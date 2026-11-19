package datastore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// claimCallSite is one production reference to the token-claim entry point
// outside this package, found by scanning source files.
type claimCallSite struct {
	location      string
	permitCapable bool
}

// discoverClaimCallSites mechanically finds every production reference to
// MarkUploadTokenUsed outside this package: direct qualified calls
// (datastore.MarkUploadTokenUsed(...)) and calls through its sole
// production indirection, webhook's claimUploadToken(...). Review finding
// 1239 wants the claim-caller list proved against the source, not
// hand-copied, so a new call site - or an existing one that stops being
// able to carry a permit deadline - changes what this returns, and
// TestMarkUploadTokenUsedClaimCallSites_MatchExpectedShape fails until a
// human reconciles it.
//
// This is a mechanical, textual scan, not a type-aware one: a call site is
// "permit-capable" when its second argument is anything other than the
// literal nil. Today that is true for exactly one call site, the
// scoped-or-legacy app upload claim (webhook/cli_deploy_handler.go): it
// passes a variable that is non-nil only when the token is scoped.
func discoverClaimCallSites(t *testing.T) []claimCallSite {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	pattern := regexp.MustCompile(`\b(?:claimUploadToken|datastore\.MarkUploadTokenUsed)\(([^)]*)\)`)
	var sites []claimCallSite
	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "datastore", ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}
		for i, line := range strings.Split(string(data), "\n") {
			// The indirection's own declaration assigns the function value;
			// it is not a call and has no argument list to classify.
			if strings.Contains(line, "claimUploadToken = ") {
				continue
			}
			for _, m := range pattern.FindAllStringSubmatch(line, -1) {
				args := strings.Split(m[1], ",")
				permitCapable := len(args) == 2 && strings.TrimSpace(args[1]) != "nil"
				sites = append(sites, claimCallSite{
					location:      fmt.Sprintf("%s:%d", rel, i+1),
					permitCapable: permitCapable,
				})
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scan repo for claim call sites: %v", walkErr)
	}
	return sites
}

// TestMarkUploadTokenUsedClaimCallSites_MatchExpectedShape pins review
// finding 1239's derived caller list: MarkUploadTokenUsed has exactly three
// production call sites outside this package (build-image upload, scoped-
// or-legacy app upload, and pull), and exactly one of them (the scoped-or-
// legacy app upload) is capable of passing a non-nil permit deadline. A
// fourth call site appearing, or the permit-capable one changing, changes
// these counts and fails this test. It needs no Postgres connection and
// always runs; TestMarkUploadTokenUsed_PostgresDeadlineMatrix below re-runs
// the same check before trusting its own hand-written row list.
func TestMarkUploadTokenUsedClaimCallSites_MatchExpectedShape(t *testing.T) {
	sites := discoverClaimCallSites(t)
	if len(sites) != 3 {
		t.Fatalf("found %d production claim call sites, want exactly 3: %+v", len(sites), sites)
	}
	permitCapable := 0
	for _, s := range sites {
		if s.permitCapable {
			permitCapable++
		}
	}
	if permitCapable != 1 {
		t.Fatalf("found %d permit-capable claim call sites, want exactly 1: %+v", permitCapable, sites)
	}
}

// claimDeadlineCase is one (named caller, deadline kind) row of review
// finding 1239's mandated matrix: 4 named callers (build-image upload,
// scoped app upload, legacy app upload, pull), one row per applicable
// deadline - token for every caller, permit only for the scoped caller.
type claimDeadlineCase struct {
	caller             string
	deadlineKind       string // "token" or "permit"
	callerHasPermitArg bool   // whether this caller's call site can pass a non-nil permit at all
}

var claimDeadlineMatrix = []claimDeadlineCase{
	{caller: "build-image upload", deadlineKind: "token"},
	{caller: "legacy app upload", deadlineKind: "token"},
	{caller: "pull", deadlineKind: "token"},
	{caller: "scoped app upload", deadlineKind: "token", callerHasPermitArg: true},
	{caller: "scoped app upload", deadlineKind: "permit", callerHasPermitArg: true},
}

// TestMarkUploadTokenUsed_PostgresDeadlineMatrix pins review findings 1239
// and 1244 (repeats of 1168/1128/1076): every applicable deadline - the
// token's own expires_at for every caller, and the scoped caller's fresh
// permit deadline too - must be enforced by the claim itself, evaluated
// strictly after a conflicting row lock is released, even when that lock
// left the row's content unchanged (review finding 1239's specific
// counter-example to a plain clock_timestamp()-in-the-WHERE-clause fix: see
// markUploadTokenUsedPostgres's doc comment). Each row below holds the row
// lock across its deadline with a SELECT ... FOR UPDATE that is released
// without modifying the row, then asserts the token is left unused - which
// also proves no archive persist or build start can follow, since
// webhook/cli_deploy_handler.go only proceeds past the claim call on a nil
// error.
//
// Needs a real PostgreSQL connection (see
// TestMarkUploadTokenUsed_PostgresClockTimestampSurvivesLockWait's doc for
// how to run it); skipped unless CLUSTERAGENT_TEST_POSTGRES_DSN is set.
func TestMarkUploadTokenUsed_PostgresDeadlineMatrix(t *testing.T) {
	dsn := os.Getenv("CLUSTERAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERAGENT_TEST_POSTGRES_DSN not set; skipping the PostgreSQL-backed deadline matrix for review finding 1239")
	}

	// Re-derive the caller shape before trusting the hand-written matrix
	// below: if the source has drifted (a new call site, or the permit
	// moved), this fails here instead of silently testing the wrong thing.
	sites := discoverClaimCallSites(t)
	if len(sites) != 3 {
		t.Fatalf("this matrix assumes exactly 3 claim call sites outside datastore, found %d: %+v - update the matrix", len(sites), sites)
	}
	permitCapable := 0
	for _, s := range sites {
		if s.permitCapable {
			permitCapable++
		}
	}
	if permitCapable != 1 {
		t.Fatalf("this matrix assumes exactly 1 permit-capable claim call site, found %d: %+v - update the matrix", permitCapable, sites)
	}

	appDB := setupPostgresTestDB(t, dsn)

	for i, c := range claimDeadlineMatrix {
		c := c
		t.Run(fmt.Sprintf("%s/%s_deadline", c.caller, c.deadlineKind), func(t *testing.T) {
			raw := fmt.Sprintf("pg-matrix-row-%d-%s-%s", i, c.caller, c.deadlineKind)

			farFuture := time.Now().Add(5 * time.Minute)
			tight := time.Now().Add(700 * time.Millisecond)

			var tokenExpiry time.Time
			var permitArg *time.Time
			switch {
			case c.deadlineKind == "token" && !c.callerHasPermitArg:
				tokenExpiry = tight
			case c.deadlineKind == "token" && c.callerHasPermitArg:
				tokenExpiry = tight
				permitValue := farFuture
				permitArg = &permitValue
			case c.deadlineKind == "permit":
				tokenExpiry = farFuture
				permitValue := tight
				permitArg = &permitValue
			}

			if err := CreatePullToken(raw, "osid-1", "upload-"+c.deadlineKind, tokenExpiry); err != nil {
				t.Fatalf("create token: %v", err)
			}

			lockDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatalf("open second postgres connection: %v", err)
			}
			t.Cleanup(func() {
				if sqlDB, err := lockDB.DB(); err == nil {
					sqlDB.Close()
				}
			})

			tx := lockDB.Begin()
			if tx.Error != nil {
				t.Fatalf("begin lock tx: %v", tx.Error)
			}
			// A lock that leaves the row unchanged (review finding 1239):
			// SELECT ... FOR UPDATE, never an UPDATE, so there is no new
			// row version for PostgreSQL to re-check quals against once
			// the lock is released.
			var locked struct{ ID uint }
			if err := tx.Raw(
				"SELECT id FROM "+tablePrefix+"upload_tokens WHERE token_hash = ? FOR UPDATE",
				hashToken(raw),
			).Scan(&locked).Error; err != nil {
				t.Fatalf("take the row lock: %v", err)
			}

			claimDone := make(chan error, 1)
			go func() {
				claimDone <- MarkUploadTokenUsed(raw, permitArg) // blocks on tx's row lock
			}()

			// The contended deadline passes while the claim is still
			// blocked on the lock, well before the 1.3s hold below
			// releases it.
			time.Sleep(1300 * time.Millisecond)
			if err := tx.Commit().Error; err != nil {
				t.Fatalf("release the row lock: %v", err)
			}

			claimErr := <-claimDone
			if claimErr == nil {
				t.Fatalf("expected the claim to refuse once its %s deadline passed while the lock was held", c.deadlineKind)
			}

			tok, err := getPullTokenUsingDB(appDB, raw)
			if err != nil {
				t.Fatalf("read back the contended token: %v", err)
			}
			if tok.Used {
				t.Fatal("the contended token must not be left marked used by the refused claim")
			}
		})
	}

	// The finding's "include a timely claimant that proceeds exactly once":
	// no contention, plenty of time left on both deadlines.
	t.Run("timely claimant with no contention proceeds exactly once", func(t *testing.T) {
		raw := "pg-matrix-timely-claimant"
		permitValue := time.Now().Add(5 * time.Minute)
		if err := CreatePullToken(raw, "osid-1", "upload-timely", time.Now().Add(5*time.Minute)); err != nil {
			t.Fatalf("create token: %v", err)
		}
		if err := MarkUploadTokenUsed(raw, &permitValue); err != nil {
			t.Fatalf("a timely claimant with no contention must still succeed: %v", err)
		}
		if err := MarkUploadTokenUsed(raw, &permitValue); err == nil {
			t.Fatal("a second claim of the same token must still fail (single use)")
		}
	})
}

// TestMarkUploadTokenUsed_PostgresClockTimestampSurvivesLockWait pins
// review finding 1168 (repeat of 1076/1128): PostgreSQL fixes
// CURRENT_TIMESTAMP (and NOW()) at TRANSACTION START, not at statement
// execution
// (postgresql.org/docs/15/functions-datetime.html#FUNCTIONS-DATETIME-CURRENT).
// A wait for a row lock before the claim actually takes effect can leave
// CURRENT_TIMESTAMP before the token's real deadline even though
// wall-clock time has already passed it. clock_timestamp() is PostgreSQL's
// real wall-clock function; markUploadTokenUsedPostgres reads it only
// after the row's lock is confirmed held (review finding 1239's further
// repeat: see that function's doc comment for why clock_timestamp() alone,
// inside a single UPDATE's WHERE clause, was not sufficient either).
//
// This needs a real PostgreSQL connection: SQLite has no clock_timestamp()
// and does not reproduce PostgreSQL's transaction-start timestamp
// semantics. It is skipped unless CLUSTERAGENT_TEST_POSTGRES_DSN names a
// reachable database, for example:
//
//	CLUSTERAGENT_TEST_POSTGRES_DSN="host=127.0.0.1 port=5432 user=postgres password=... dbname=... sslmode=disable" \
//	  go test ./datastore/... -run TestMarkUploadTokenUsed_Postgres -v
//
// Measured against a real PostgreSQL 15 instance while fixing finding
// 1168: holding the row's lock in one session for 1.3s while a second
// session's UPDATE ... WHERE expires_at > CURRENT_TIMESTAMP waits on it
// let a token that expired 0.6s into that wait still be claimed
// (used=true) once the lock released, because CURRENT_TIMESTAMP had been
// captured before the wait began. The same experiment with
// expires_at > clock_timestamp() correctly left the token unclaimed
// (used=false).
func TestMarkUploadTokenUsed_PostgresClockTimestampSurvivesLockWait(t *testing.T) {
	dsn := os.Getenv("CLUSTERAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERAGENT_TEST_POSTGRES_DSN not set; skipping the PostgreSQL-backed deadline test for review finding 1168")
	}

	appDB := setupPostgresTestDB(t, dsn)

	const raw = "pg-lock-wait-probe-token-timely-claimant-does-not-interfere"
	const rawBlocked = "pg-lock-wait-probe-token-blocked-by-lock-contention"

	// Timely claimant (the finding's "include a timely claimant that
	// proceeds exactly once"): no contention, plenty of time left.
	liveExpiry := time.Now().Add(5 * time.Minute)
	if err := CreatePullToken(raw, "osid-1", "upload-live", liveExpiry); err != nil {
		t.Fatalf("CreatePullToken (timely): %v", err)
	}
	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("a timely claimant with no contention must still succeed: %v", err)
	}
	if err := MarkUploadTokenUsed(raw, nil); err == nil {
		t.Fatal("a second claim of the same token must still fail (single use)")
	}

	// The contended claimant: expires in 700ms, held by a lock for 1.3s.
	tightExpiry := time.Now().Add(700 * time.Millisecond)
	if err := CreatePullToken(rawBlocked, "osid-1", "upload-blocked", tightExpiry); err != nil {
		t.Fatalf("CreatePullToken (contended): %v", err)
	}

	// A second, independent connection holds the row lock, modeling
	// contention: another session, a slow write, a stalled connection.
	lockDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open second postgres connection: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := lockDB.DB(); err == nil {
			sqlDB.Close()
		}
	})

	tx := lockDB.Begin()
	if tx.Error != nil {
		t.Fatalf("begin lock tx: %v", tx.Error)
	}
	if err := tx.Exec(
		"UPDATE "+tablePrefix+"upload_tokens SET used = used WHERE token_hash = ?",
		hashToken(rawBlocked),
	).Error; err != nil {
		t.Fatalf("take the row lock: %v", err)
	}

	claimDone := make(chan error, 1)
	go func() {
		claimDone <- MarkUploadTokenUsed(rawBlocked, nil) // blocks on tx's row lock
	}()

	// The token's 700ms deadline passes while the claim is still blocked on
	// the lock, well before the 1.3s hold below releases it.
	time.Sleep(1300 * time.Millisecond)
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("release the row lock: %v", err)
	}

	claimErr := <-claimDone
	if claimErr == nil {
		t.Fatal("expected MarkUploadTokenUsed to refuse a claim that only takes effect after the token's deadline passed while waiting on a lock")
	}

	tok, err := getPullTokenUsingDB(appDB, rawBlocked)
	if err != nil {
		t.Fatalf("read back the contended token: %v", err)
	}
	if tok.Used {
		t.Fatal("the contended token must not be left marked used by the refused claim")
	}
}

// setupPostgresTestDB installs a real PostgreSQL connection as the active
// datastore handle for the duration of one test, auto-migrates the schema,
// and restores the previous handle on cleanup. It returns the installed
// *gorm.DB so a test can read rows back directly.
func setupPostgresTestDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()

	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{
			TablePrefix: tablePrefix,
		},
	})
	if err != nil {
		t.Fatalf("open postgres %s: %v", dsn, err)
	}
	if err := gdb.AutoMigrate(allModels()...); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	// This database persists across runs (unlike setupTestDB's fresh SQLite
	// file per test), so start from an empty table every time.
	if err := gdb.Exec("TRUNCATE TABLE " + tablePrefix + "upload_tokens").Error; err != nil {
		t.Fatalf("truncate %supload_tokens: %v", tablePrefix, err)
	}

	prev := setHandle(gdb)
	t.Cleanup(func() {
		if cur := setHandle(prev); cur != nil {
			if sqlDB, err := cur.DB(); err == nil {
				sqlDB.Close()
			}
		}
	})
	return gdb
}

// getPullTokenUsingDB reads a token's used state back through the given
// handle directly, bypassing activeDB(), so the assertion is not fooled by
// a cleanup swap racing the read.
func getPullTokenUsingDB(gdb *gorm.DB, token string) (*UploadToken, error) {
	var m UploadTokenModel
	if err := gdb.Where("token_hash = ? AND purpose = ?", hashToken(token), PurposePull).First(&m).Error; err != nil {
		return nil, err
	}
	return &UploadToken{Used: m.Used}, nil
}
