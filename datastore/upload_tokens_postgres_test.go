package datastore

import (
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// TestMarkUploadTokenUsed_PostgresClockTimestampSurvivesLockWait pins
// review finding 1168 (repeat of 1076/1128): PostgreSQL fixes
// CURRENT_TIMESTAMP (and NOW()) at TRANSACTION START, not at statement
// execution
// (postgresql.org/docs/15/functions-datetime.html#FUNCTIONS-DATETIME-CURRENT).
// A wait for a row lock before the claim UPDATE actually takes effect can
// leave CURRENT_TIMESTAMP before the token's real deadline even though
// wall-clock time has already passed it. clock_timestamp() is PostgreSQL's
// real wall-clock function, re-evaluated when the statement actually runs
// after the lock wait ends, which is what MarkUploadTokenUsed now uses.
//
// This needs a real PostgreSQL connection: SQLite has no clock_timestamp()
// and does not reproduce PostgreSQL's transaction-start timestamp
// semantics. It is skipped unless CLUSTERAGENT_TEST_POSTGRES_DSN names a
// reachable database, for example:
//
//	CLUSTERAGENT_TEST_POSTGRES_DSN="host=127.0.0.1 port=5432 user=postgres password=... dbname=... sslmode=disable" \
//	  go test ./datastore/... -run TestMarkUploadTokenUsed_Postgres -v
//
// Measured against a real PostgreSQL 15 instance while fixing this
// finding: holding the row's lock in one session for 1.3s while a second
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
	if err := MarkUploadTokenUsed(raw); err != nil {
		t.Fatalf("a timely claimant with no contention must still succeed: %v", err)
	}
	if err := MarkUploadTokenUsed(raw); err == nil {
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
		claimDone <- MarkUploadTokenUsed(rawBlocked) // blocks on tx's row lock
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
