// Package datastore is the cluster agent's local SQLite store. It persists
// build jobs and their logs, build args, one-shot job audit records, presigned
// upload tokens, and the SQL schema cache, and runs idempotent schema
// migrations on startup.
package datastore

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

// Initialize creates the database connection and sets up tables
func Initialize() error {
	// Ensure /data directory exists
	dataDir := "/data"
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, "builds.db")
	var err error
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}

	// Set connection pool settings
	db.SetMaxOpenConns(1) // SQLite works best with a single connection
	db.SetMaxIdleConns(1)

	// Create tables
	if err := createTables(); err != nil {
		return err
	}

	log.Println("Datastore initialized successfully at", dbPath)
	return nil
}

// createTables sets up the database schema
func createTables() error {
	schema := `
	CREATE TABLE IF NOT EXISTS buildkit_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT UNIQUE NOT NULL,
		osid TEXT NOT NULL,
		repo TEXT NOT NULL,
		branch TEXT NOT NULL,
		commit_hash TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME DEFAULT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_buildkit_jobs_job_id ON buildkit_jobs(job_id);
	CREATE INDEX IF NOT EXISTS idx_buildkit_jobs_status ON buildkit_jobs(status);
	CREATE INDEX IF NOT EXISTS idx_buildkit_jobs_created_at ON buildkit_jobs(created_at);

	CREATE TABLE IF NOT EXISTS buildkit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL,
		log_entry TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(job_id) REFERENCES buildkit_jobs(job_id)
	);

	CREATE INDEX IF NOT EXISTS idx_buildkit_logs_job_id ON buildkit_logs(job_id);
	CREATE INDEX IF NOT EXISTS idx_buildkit_logs_created_at ON buildkit_logs(created_at);

	CREATE TABLE IF NOT EXISTS buildkit_job_args (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		build_job_id TEXT NOT NULL,
		arg_key TEXT NOT NULL,
		arg_value TEXT NOT NULL,
		source TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(build_job_id) REFERENCES buildkit_jobs(job_id)
	);

	CREATE INDEX IF NOT EXISTS idx_buildkit_job_args_build_job_id ON buildkit_job_args(build_job_id);

	CREATE TABLE IF NOT EXISTS upload_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT UNIQUE NOT NULL,
		deploy_config TEXT NOT NULL,
		dockerfile TEXT NOT NULL DEFAULT '',
		build_args TEXT NOT NULL DEFAULT '[]',
		build_target TEXT NOT NULL DEFAULT '',
		expires_at DATETIME NOT NULL,
		used INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		purpose TEXT NOT NULL DEFAULT 'upload'
	);

	CREATE INDEX IF NOT EXISTS idx_upload_tokens_token ON upload_tokens(token);
	CREATE INDEX IF NOT EXISTS idx_upload_tokens_expires_at ON upload_tokens(expires_at);

	CREATE TABLE IF NOT EXISTS sql_schema_cache (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		connection_hash TEXT NOT NULL,
		schema_json TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_sql_schema_cache_hash ON sql_schema_cache(connection_hash);

	CREATE TABLE IF NOT EXISTS one_shot_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT UNIQUE NOT NULL,
		osid TEXT NOT NULL,
		namespace TEXT NOT NULL,
		image TEXT NOT NULL,
		command TEXT NOT NULL,
		k8s_job_name TEXT NOT NULL,
		status TEXT NOT NULL,
		exit_code INTEGER DEFAULT NULL,
		actor TEXT NOT NULL DEFAULT '',
		timeout_seconds INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME DEFAULT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_one_shot_jobs_run_id ON one_shot_jobs(run_id);
	CREATE INDEX IF NOT EXISTS idx_one_shot_jobs_osid ON one_shot_jobs(osid);
	CREATE INDEX IF NOT EXISTS idx_one_shot_jobs_status ON one_shot_jobs(status);
	`

	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if err := migrateUploadTokensPurpose(); err != nil {
		return err
	}
	if err := migrateUploadTokensDockerfile(); err != nil {
		return err
	}
	if err := migrateUploadTokensBuildArgs(); err != nil {
		return err
	}
	return migrateUploadTokensBuildTarget()
}

// migrateUploadTokensPurpose adds the purpose column to upload_tokens if it
// is not already present. SQLite's CREATE TABLE IF NOT EXISTS does not run
// when the table already has a schema from a prior version of the agent.
func migrateUploadTokensPurpose() error {
	rows, err := db.Query("PRAGMA table_info(upload_tokens)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "purpose" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE upload_tokens ADD COLUMN purpose TEXT NOT NULL DEFAULT 'upload'`)
	return err
}

// migrateUploadTokensDockerfile adds the dockerfile column to upload_tokens
// if it is not already present. Closes the schema half of iter-27 I27-Y:
// the column carries the Dockerfile path inside the tarball (relative to
// the tarball root) so monorepo CLI deploys can build at non-root
// Dockerfile locations. Existing rows default to empty (= "Dockerfile" at
// the root), preserving the pre-monorepo single-app behaviour for tokens
// minted before the column existed.
func migrateUploadTokensDockerfile() error {
	rows, err := db.Query("PRAGMA table_info(upload_tokens)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "dockerfile" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE upload_tokens ADD COLUMN dockerfile TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateUploadTokensBuildArgs adds the build_args column to upload_tokens if
// it is not already present. The column carries a JSON array of effective
// build args ([{key,value,source}]) captured at token-creation time and read
// back at tarball-upload time, since the CLI build runs in a later request
// than the one that mints the token. Existing rows default to '[]' (no build
// args), preserving the pre-feature behaviour for tokens minted before the
// column existed.
func migrateUploadTokensBuildArgs() error {
	rows, err := db.Query("PRAGMA table_info(upload_tokens)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "build_args" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE upload_tokens ADD COLUMN build_args TEXT NOT NULL DEFAULT '[]'`)
	return err
}

// migrateUploadTokensBuildTarget adds the build_target column to
// upload_tokens if it is not already present (obj-47). The column carries a
// JSON BuildTargetSpec ({repo, tags}) for app-less build-image tokens,
// captured at token-creation time and read back at tarball-upload time.
// Existing rows and app-deploy/pull tokens default to ” (no explicit
// target), preserving the pre-feature behaviour for tokens minted before
// the column existed.
func migrateUploadTokensBuildTarget() error {
	rows, err := db.Query("PRAGMA table_info(upload_tokens)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "build_target" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE upload_tokens ADD COLUMN build_target TEXT NOT NULL DEFAULT ''`)
	return err
}

// GetDB returns the database connection
func GetDB() *sql.DB {
	return db
}

// Close closes the database connection
func Close() error {
	if db != nil {
		return db.Close()
	}
	return nil
}
