// Package datastore is the cluster agent's persistent store, backed by GORM on
// PostgreSQL. It persists build jobs and their logs, build args, one-shot job
// audit records, single-use upload/pull tokens, and the SQL schema cache.
//
// # Discovery via a control-plane pointer ConfigMap
//
// The system Postgres can be switched at runtime, so its host and admin
// credentials are NOT baked into the agent's environment. Instead a well-known
// "pointer" ConfigMap names the CURRENT system Postgres, and the agent re-reads
// it on every reconcile tick:
//
//	RUNOS_SYSTEM_DB_REF       Name of the pointer ConfigMap (default
//	                          "runos-system-db").
//	RUNOS_SYSTEM_DB_NAMESPACE Namespace of the pointer ConfigMap (default: the
//	                          agent namespace, "runos").
//
// The ConfigMap's data carries the current system Postgres coordinates:
//
//	host                  Postgres host.
//	port                  Postgres port (default "5432").
//	adminSecretNamespace  Namespace of the admin Secret.
//	adminSecretName       Name of the admin Secret.
//
// The referenced admin Secret (CNPG-style) holds the admin/superuser
// credentials used ONLY for provisioning:
//
//	username  Admin role (default "postgres" if the key is absent).
//	password  Admin password.
//
// The agent is cluster-admin, so it reads that Secret cross-namespace.
//
// As an OPTIONAL fallback only (the pointer ConfigMap is the primary path): if
// the ConfigMap is absent but RUNOS_DB_HOST + RUNOS_DB_ADMIN_USER/PASSWORD are
// set in the environment, those are used instead.
//
// The application database/role are still owned by the agent:
//
//	RUNOS_DB_NAME           Application database name (default "runos").
//	RUNOS_DB_USER           Application role name (default "runos").
//	RUNOS_DB_CRED_SECRET    Name of the Secret holding the app credentials
//	                        (default "cluster-agent-db").
//	RUNOS_DB_CRED_NAMESPACE Namespace of that Secret (default: the agent
//	                        namespace, "runos").
//
// No real host, secret name, or password is ever hardcoded; the agent
// SELF-PROVISIONS its application database + role and stores the generated app
// password in the credential Secret, which also records the host it belongs to.
//
// # Self-healing reconcile model
//
// Initialize() does NOT connect synchronously and never fails the process when
// Postgres or the pointer ConfigMap is absent. It starts a background reconcile
// goroutine and returns nil immediately. The agent therefore always boots; while
// no connection is live, datastore operations return ErrNotReady and recover
// automatically once a connection is established.
//
// On each tick the reconcile loop:
//
//  1. Resolves the target system Postgres from the pointer ConfigMap (or the
//     env fallback) and builds a "target signature" from {host, port,
//     adminSecretNamespace, adminSecretName, dbname, user}.
//  2. If a handle is already live, the target signature is unchanged, AND a
//     Ping succeeds, the connection is healthy: it sleeps the healthy interval
//     and loops.
//  3. Otherwise (no handle / unhealthy / target changed) it (re)connects: it
//     reuses the app password from the credential Secret when that Secret
//     records the SAME host and carries a password; if that connect fails, or
//     the Secret records a DIFFERENT host (the system Postgres was switched) or
//     is absent, it PROVISIONS on the current instance as admin (fresh
//     password, CREATE DATABASE if missing, CREATE-or-ALTER ROLE, GRANT) and
//     overwrites the credential Secret with the CURRENT host. It then opens
//     GORM as the app role, AutoMigrates all models, and atomically swaps the
//     new handle in (closing the previous one).
//  4. On ANY error it logs, backs off exponentially (capped), and retries. The
//     loop never returns until Close() cancels its context.
//
// Net effect: tolerant of Postgres being down at startup or going down later,
// retries indefinitely, auto-creates the DB/schema on whatever the current
// system Postgres is, and when an operator switches the system Postgres to a
// different instance the loop detects the changed target, reconnects, and
// re-provisions the schema on the new instance, all automatically.
//
// Tables are prefixed cluster_agent_ via a GORM NamingStrategy so the runos DB
// can be shared with other components.
package datastore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// ErrNotReady is returned by every query function when no live database handle
// has been established yet (e.g. Postgres is unreachable, or the reconcile loop
// is mid-reconnect). It is a normal, transient condition: the reconcile loop
// recovers automatically, so callers should treat it as "try again later".
var ErrNotReady = errors.New("datastore: system database not connected yet")

// handle is the concurrency-safe current GORM connection. The reconcile loop
// swaps it atomically; query functions load it via activeDB(). A nil pointer
// means "no live connection" -> ErrNotReady.
var handle atomic.Pointer[gorm.DB]

// activeDB returns the current live GORM handle, or ErrNotReady if none is set.
// Every exported query function begins by calling this so a missing/booting
// connection surfaces cleanly instead of panicking on a nil handle.
func activeDB() (*gorm.DB, error) {
	if gdb := handle.Load(); gdb != nil {
		return gdb, nil
	}
	return nil, ErrNotReady
}

// setHandle atomically installs gdb as the active handle and returns the
// previous one (nil if none). The caller is responsible for closing the
// returned previous handle.
func setHandle(gdb *gorm.DB) *gorm.DB {
	return handle.Swap(gdb)
}

// tablePrefix namespaces the agent's physical tables inside the shared runos DB.
const tablePrefix = "cluster_agent_"

// defaultNamespace is the agent's own namespace, used for the pointer ConfigMap
// and credential Secret when their *_NAMESPACE env is unset. Mirrors
// agentstream.Namespace; kept as a local constant so the datastore does not
// import agentstream (cycle-free).
const defaultNamespace = "runos"

// Reconcile loop timing.
const (
	healthyInterval = 25 * time.Second // sleep between checks while connected + healthy
	backoffMin      = 2 * time.Second  // first retry delay after a failed reconcile
	backoffMax      = 30 * time.Second // cap on the exponential backoff
)

// identRe validates a Postgres identifier (db/role name) before it is ever
// interpolated into DDL. Config-sourced, but we defend anyway: only the classic
// unquoted-identifier shape is allowed, so quoting it is safe.
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// dbConfig is the resolved connection configuration for one reconcile pass: the
// app DB/role/credential settings (from env) plus the CURRENT system Postgres
// coordinates (from the pointer ConfigMap or the env fallback).
type dbConfig struct {
	host          string
	port          string
	name          string
	user          string
	adminUser     string
	adminPassword string
	credSecret    string
	credNamespace string
}

// signature is the stable key used to detect a system-Postgres switch. Admin
// password is deliberately excluded: a rotated admin password must not, by
// itself, force a reprovision; host/instance + identity changes do.
func (c dbConfig) signature(adminSecretNamespace, adminSecretName string) string {
	return strings.Join([]string{
		c.host, c.port, adminSecretNamespace, adminSecretName, c.name, c.user,
	}, "\x1f")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// appEnvConfig loads the env-sourced app DB/role/credential settings. The system
// Postgres coordinates (host/port/admin) are filled in per-tick by resolveTarget.
func appEnvConfig() dbConfig {
	return dbConfig{
		port:          env("RUNOS_DB_PORT", "5432"),
		name:          env("RUNOS_DB_NAME", "runos"),
		user:          env("RUNOS_DB_USER", "runos"),
		credSecret:    env("RUNOS_DB_CRED_SECRET", "cluster-agent-db"),
		credNamespace: env("RUNOS_DB_CRED_NAMESPACE", defaultNamespace),
	}
}

// ----------------------------------------------------------------------------
// Reconcile loop lifecycle
// ----------------------------------------------------------------------------

// reconciler owns the background loop's cancellation. Guarded by reconcileMu so
// Initialize/Close are safe to call concurrently and idempotently.
var (
	reconcileMu     sync.Mutex
	reconcileCancel context.CancelFunc
	reconcileDone   chan struct{}
)

// Initialize starts the self-healing reconcile loop in the background and
// returns nil IMMEDIATELY. It never blocks the agent's startup and never fails
// the process when Postgres or the pointer ConfigMap is absent; datastore
// operations return ErrNotReady until the loop establishes a connection.
//
// Calling Initialize again while a loop is already running is a no-op.
func Initialize() error {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()

	if reconcileCancel != nil {
		// Already running.
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reconcileCancel = cancel
	reconcileDone = done

	go reconcileLoop(ctx, done)

	log.Printf("Datastore: reconcile loop started (system DB discovered via ConfigMap %q in namespace %q)",
		env("RUNOS_SYSTEM_DB_REF", "runos-system-db"),
		env("RUNOS_SYSTEM_DB_NAMESPACE", defaultNamespace))
	return nil
}

// Close cancels the reconcile loop, waits for it to exit (no goroutine leak),
// and closes the live handle if one is set.
func Close() error {
	reconcileMu.Lock()
	cancel := reconcileCancel
	done := reconcileDone
	reconcileCancel = nil
	reconcileDone = nil
	reconcileMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	if prev := setHandle(nil); prev != nil {
		closeGorm(prev)
	}
	return nil
}

// reconcileLoop is the infinite background loop. It never returns until ctx is
// cancelled; on any reconcile error it logs, backs off, and retries.
func reconcileLoop(ctx context.Context, done chan struct{}) {
	defer close(done)

	cfg := appEnvConfig()
	if !identRe.MatchString(cfg.name) {
		log.Printf("Datastore: invalid RUNOS_DB_NAME %q; reconcile loop will not connect until fixed", cfg.name)
	}
	if !identRe.MatchString(cfg.user) {
		log.Printf("Datastore: invalid RUNOS_DB_USER %q; reconcile loop will not connect until fixed", cfg.user)
	}

	// currentSig is the target signature of the handle currently installed.
	var currentSig string
	backoff := backoffMin

	for {
		wait, err := reconcileOnce(cfg, &currentSig)
		if err != nil {
			log.Printf("Datastore: reconcile failed (%v), retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		backoff = backoffMin
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// sleepCtx sleeps for d unless ctx is cancelled first. Returns false if ctx was
// cancelled (the loop should exit), true if the sleep completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// reconcileOnce performs a single reconcile pass. It returns the duration to
// sleep before the next tick on success, or an error to trigger backoff.
//
//   - Resolves the current target system Postgres.
//   - If the existing handle matches the target signature and Pings OK, it is
//     healthy: return the healthy interval.
//   - Otherwise it (re)connects: reuse the cred-Secret password when it records
//     the same host, else provision as admin on the current instance, then open
//     GORM as the app role, AutoMigrate, and atomically swap the handle in.
func reconcileOnce(cfg dbConfig, currentSig *string) (time.Duration, error) {
	k8s, kerr := newK8sClient()
	if kerr != nil {
		// Not in a cluster (or no API access): fall back to env discovery.
		// resolveTarget tolerates a nil client (it just can't read the
		// ConfigMap or the credential/admin Secrets).
		k8s = nil
	}

	host, port, adminNS, adminName, adminUser, adminPass, err := resolveTarget(k8s)
	if err != nil {
		return 0, err
	}

	cfg.host = host
	cfg.port = port
	cfg.adminUser = adminUser
	cfg.adminPassword = adminPass
	if cfg.port == "" {
		cfg.port = "5432"
	}

	if !identRe.MatchString(cfg.name) {
		return 0, fmt.Errorf("invalid RUNOS_DB_NAME %q", cfg.name)
	}
	if !identRe.MatchString(cfg.user) {
		return 0, fmt.Errorf("invalid RUNOS_DB_USER %q", cfg.user)
	}

	targetSig := cfg.signature(adminNS, adminName)

	// Fast path: handle live, target unchanged, ping OK -> healthy.
	if live := handle.Load(); live != nil && targetSig == *currentSig {
		if sqlDB, derr := live.DB(); derr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			perr := sqlDB.PingContext(ctx)
			cancel()
			if perr == nil {
				return healthyInterval, nil
			}
			log.Printf("Datastore: health ping failed (%v); reconnecting", perr)
		}
		// fallthrough to reconnect
	}

	// (Re)connect path.
	if k8s == nil {
		return 0, errors.New("kubernetes client unavailable for credential resolution")
	}

	gdb, err := connect(k8s, cfg)
	if err != nil {
		return 0, err
	}
	if err := gdb.AutoMigrate(allModels()...); err != nil {
		closeGorm(gdb)
		return 0, fmt.Errorf("automigrate: %w", err)
	}

	prev := setHandle(gdb)
	if prev != nil && prev != gdb {
		closeGorm(prev)
	}
	*currentSig = targetSig
	log.Printf("Datastore: connected (postgres %s:%s/%s as %q)", cfg.host, cfg.port, cfg.name, cfg.user)
	return healthyInterval, nil
}

// connect resolves the app password and opens a GORM connection to the current
// target instance. It reuses the credential Secret's password when that Secret
// records the SAME host and connecting with it succeeds; otherwise (different
// host, absent Secret, or a failed connect) it provisions as admin on the
// current instance and rewrites the Secret with the current host.
func connect(k8s kubernetes.Interface, cfg dbConfig) (*gorm.DB, error) {
	pw, recordedHost, ok, err := readCredentialSecret(k8s, cfg)
	if err != nil {
		return nil, err
	}

	if ok && recordedHost == cfg.host {
		// Same instance the Secret was written for: try the stored password.
		if gdb, derr := openGORM(cfg, pw); derr == nil {
			return gdb, nil
		} else {
			log.Printf("Datastore: stored credentials failed for %s (%v); reprovisioning", cfg.host, derr)
		}
	} else if ok {
		log.Printf("Datastore: credential Secret records host %q but target is %q (system DB switched); reprovisioning", recordedHost, cfg.host)
	}

	// Provision (or repair) on the CURRENT instance as admin.
	if cfg.adminUser == "" || cfg.adminPassword == "" {
		return nil, errors.New("admin credentials unavailable for provisioning the current system DB")
	}
	newPW, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	if err := provisionAsAdmin(cfg, newPW); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	if err := writeCredentialSecret(k8s, cfg, newPW); err != nil {
		return nil, fmt.Errorf("write credential Secret: %w", err)
	}
	log.Printf("Datastore: provisioned DB %q + role %q on %s, wrote Secret %q", cfg.name, cfg.user, cfg.host, cfg.credSecret)

	gdb, err := openGORM(cfg, newPW)
	if err != nil {
		return nil, err
	}
	return gdb, nil
}

// resolveTarget reads the pointer ConfigMap to discover the CURRENT system
// Postgres and its admin Secret. If the ConfigMap is absent it falls back to
// the RUNOS_DB_HOST + RUNOS_DB_ADMIN_USER/PASSWORD env (optional fallback path).
// Returns host, port, adminSecretNamespace, adminSecretName, adminUser,
// adminPassword.
func resolveTarget(k8s kubernetes.Interface) (host, port, adminNS, adminName, adminUser, adminPass string, err error) {
	refName := env("RUNOS_SYSTEM_DB_REF", "runos-system-db")
	refNS := env("RUNOS_SYSTEM_DB_NAMESPACE", defaultNamespace)

	var cm *corev1.ConfigMap
	if k8s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, gerr := k8s.CoreV1().ConfigMaps(refNS).Get(ctx, refName, metav1.GetOptions{})
		cancel()
		if gerr != nil && !apierrors.IsNotFound(gerr) {
			return "", "", "", "", "", "", fmt.Errorf("get pointer ConfigMap %q/%q: %w", refNS, refName, gerr)
		}
		if gerr == nil {
			cm = got
		}
	}

	if cm != nil {
		host = strings.TrimSpace(cm.Data["host"])
		port = strings.TrimSpace(cm.Data["port"])
		adminNS = strings.TrimSpace(cm.Data["adminSecretNamespace"])
		adminName = strings.TrimSpace(cm.Data["adminSecretName"])
		if host == "" {
			return "", "", "", "", "", "", fmt.Errorf("pointer ConfigMap %q/%q has no host", refNS, refName)
		}
		if port == "" {
			port = "5432"
		}
		if adminName == "" {
			return "", "", "", "", "", "", fmt.Errorf("pointer ConfigMap %q/%q has no adminSecretName", refNS, refName)
		}
		if adminNS == "" {
			adminNS = refNS
		}
		adminUser, adminPass, err = readAdminSecret(k8s, adminNS, adminName)
		if err != nil {
			return "", "", "", "", "", "", err
		}
		return host, port, adminNS, adminName, adminUser, adminPass, nil
	}

	// Fallback: env-provided host + admin creds (optional path).
	host = os.Getenv("RUNOS_DB_HOST")
	if host == "" {
		return "", "", "", "", "", "", fmt.Errorf("pointer ConfigMap %q/%q absent and RUNOS_DB_HOST not set", refNS, refName)
	}
	port = env("RUNOS_DB_PORT", "5432")
	adminUser = os.Getenv("RUNOS_DB_ADMIN_USER")
	adminPass = os.Getenv("RUNOS_DB_ADMIN_PASSWORD")
	// Synthetic signature inputs so an env switch of host still re-signs.
	adminNS = "env"
	adminName = "env"
	return host, port, adminNS, adminName, adminUser, adminPass, nil
}

// readAdminSecret reads the CNPG-style admin Secret referenced by the pointer
// ConfigMap. The username key defaults to "postgres" when absent.
func readAdminSecret(client kubernetes.Interface, ns, name string) (user, password string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret, gerr := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if gerr != nil {
		return "", "", fmt.Errorf("get admin Secret %q/%q: %w", ns, name, gerr)
	}
	user = strings.TrimSpace(string(secret.Data["username"]))
	if user == "" {
		user = "postgres"
	}
	password = string(secret.Data["password"])
	if strings.TrimSpace(password) == "" {
		return "", "", fmt.Errorf("admin Secret %q/%q has no password", ns, name)
	}
	return user, password, nil
}

// openGORM dials the application database as the application role and verifies
// the connection with a single Ping. Unlike the previous implementation it does
// NOT loop internally: the reconcile loop owns retry/backoff, so a transient
// failure here just returns an error and the loop retries.
func openGORM(cfg dbConfig, password string) (*gorm.DB, error) {
	dsn := dsnFor(cfg.host, cfg.port, cfg.user, password, cfg.name)
	gormCfg := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		NamingStrategy: schema.NamingStrategy{
			TablePrefix: tablePrefix,
		},
	}

	gdb, err := gorm.Open(postgres.Open(dsn), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres %s:%s/%s: %w", cfg.host, cfg.port, cfg.name, err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		closeGorm(gdb)
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		closeGorm(gdb)
		return nil, fmt.Errorf("ping postgres %s:%s/%s: %w", cfg.host, cfg.port, cfg.name, err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	return gdb, nil
}

// dsnFor builds a libpq DSN string. Values are passed as key=value pairs so the
// password is never URL-encoded into a connection URL; quote any value that
// could contain whitespace defensively.
func dsnFor(host, port, user, password, dbname string) string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password='%s' dbname=%s sslmode=prefer connect_timeout=5",
		host, port, user, pgEscape(password), dbname,
	)
}

// pgEscape escapes a value for a single-quoted libpq DSN field.
func pgEscape(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `'`, `\'`)
}

// generatePassword returns a strong, URL-safe random password (32 bytes of
// entropy, hex-encoded).
func generatePassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ----------------------------------------------------------------------------
// Token hashing
// ----------------------------------------------------------------------------

// hashToken maps a raw, high-entropy token to its stored representation. The
// tokens are crypto-random (>=256 bits), so a plain SHA-256 is the correct,
// standard choice: no salt or KDF is needed. Centralised here so swapping to an
// HMAC-with-pepper later is a single-line change.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqualHash compares two token hashes without leaking timing.
func constantTimeEqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ----------------------------------------------------------------------------
// Kubernetes credential Secret
// ----------------------------------------------------------------------------

// newK8sClient builds an in-cluster Kubernetes clientset. The pod's
// ServiceAccount is cluster-admin, so it can read the pointer ConfigMap, the
// admin Secret cross-namespace, and read/write the credential Secret. Outside a
// cluster this returns an error; the caller treats a nil client as "ConfigMap
// unreadable" and the env fallback still applies for discovery.
func newK8sClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return cs, nil
}

// readCredentialSecret returns (password, recordedHost, true, nil) when the
// credential Secret exists and carries a non-empty password key. recordedHost is
// the host the Secret was last written for, used to detect a system-Postgres
// switch. A missing Secret (or one with no usable password) returns
// ("", "", false, nil) so the caller provisions.
func readCredentialSecret(client kubernetes.Interface, cfg dbConfig) (password, recordedHost string, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret, gerr := client.CoreV1().Secrets(cfg.credNamespace).Get(ctx, cfg.credSecret, metav1.GetOptions{})
	if gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("get Secret %q: %w", cfg.credSecret, gerr)
	}
	pw := strings.TrimSpace(string(secret.Data["password"]))
	if pw == "" {
		// Secret exists but has no usable password: treat as absent so the
		// admin path can repair it.
		return "", "", false, nil
	}
	recordedHost = strings.TrimSpace(string(secret.Data["host"]))
	return pw, recordedHost, true, nil
}

// writeCredentialSecret stores the resolved app credentials so restarts and
// healthy reconnects reuse them. host/port/user/dbname are recorded alongside
// the password; host is also how a later tick detects the system Postgres was
// switched to a different instance.
func writeCredentialSecret(client kubernetes.Interface, cfg dbConfig, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.credSecret,
			Namespace: cfg.credNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"host":     cfg.host,
			"port":     cfg.port,
			"user":     cfg.user,
			"password": password,
			"dbname":   cfg.name,
		},
	}
	_, err := client.CoreV1().Secrets(cfg.credNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		// Raced with another writer (or a prior partial run): update in place.
		existing, gerr := client.CoreV1().Secrets(cfg.credNamespace).Get(ctx, cfg.credSecret, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		existing.StringData = secret.StringData
		_, uerr := client.CoreV1().Secrets(cfg.credNamespace).Update(ctx, existing, metav1.UpdateOptions{})
		return uerr
	}
	return err
}
