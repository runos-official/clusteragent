package datastore

import (
	"fmt"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// provisionAsAdmin connects to the "postgres" maintenance database as the admin
// role and idempotently ensures the application database, role, and schema
// privileges exist, setting the app role's password to appPassword.
//
// Identifiers (db/role names) are validated against identRe by the caller and
// re-validated here, then quoted; the password is never string-concatenated
// into DDL: CREATE/ALTER ROLE uses a parameterised format via quoting helpers
// that escape per Postgres rules.
func provisionAsAdmin(cfg dbConfig, appPassword string) error {
	if !identRe.MatchString(cfg.name) {
		return fmt.Errorf("invalid database name %q", cfg.name)
	}
	if !identRe.MatchString(cfg.user) {
		return fmt.Errorf("invalid role name %q", cfg.user)
	}

	adminDSN := dsnFor(cfg.host, cfg.port, cfg.adminUser, cfg.adminPassword, "postgres")
	admin, err := openAdmin(adminDSN)
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer closeGorm(admin)

	// 1. CREATE DATABASE if it does not exist. Postgres has no
	//    "CREATE DATABASE IF NOT EXISTS", so check pg_database first.
	var dbExists bool
	if err := admin.Raw(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = ?)`, cfg.name).Scan(&dbExists).Error; err != nil {
		return fmt.Errorf("check database existence: %w", err)
	}
	if !dbExists {
		if err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %s`, quoteIdent(cfg.name))).Error; err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	}

	// 2. CREATE or ALTER the app role, always (re)setting the password to the
	//    one we will store in the Secret.
	var roleExists bool
	if err := admin.Raw(`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = ?)`, cfg.user).Scan(&roleExists).Error; err != nil {
		return fmt.Errorf("check role existence: %w", err)
	}
	quotedRole := quoteIdent(cfg.user)
	quotedPass := quoteLiteral(appPassword)
	if roleExists {
		if err := admin.Exec(fmt.Sprintf(`ALTER ROLE %s WITH LOGIN PASSWORD %s`, quotedRole, quotedPass)).Error; err != nil {
			return fmt.Errorf("alter role: %w", err)
		}
	} else {
		if err := admin.Exec(fmt.Sprintf(`CREATE ROLE %s WITH LOGIN PASSWORD %s`, quotedRole, quotedPass)).Error; err != nil {
			return fmt.Errorf("create role: %w", err)
		}
	}

	// 3. Grant the role rights on the database.
	if err := admin.Exec(fmt.Sprintf(`GRANT ALL PRIVILEGES ON DATABASE %s TO %s`, quoteIdent(cfg.name), quotedRole)).Error; err != nil {
		return fmt.Errorf("grant database privileges: %w", err)
	}

	// Close the maintenance connection before connecting to the new DB to set
	// schema-level privileges.
	closeGorm(admin)

	// 4. Connect to the freshly created app DB as admin and ensure the role can
	//    create + own the public schema (so AutoMigrate can build the
	//    cluster_agent_ tables).
	appAdminDSN := dsnFor(cfg.host, cfg.port, cfg.adminUser, cfg.adminPassword, cfg.name)
	appAdmin, err := openAdmin(appAdminDSN)
	if err != nil {
		return fmt.Errorf("connect to app db as admin: %w", err)
	}
	defer closeGorm(appAdmin)

	stmts := []string{
		fmt.Sprintf(`GRANT ALL ON SCHEMA public TO %s`, quotedRole),
		// On PG15+ the public schema is owned by the bootstrap superuser and the
		// app role needs explicit ownership to CREATE objects it then owns.
		fmt.Sprintf(`ALTER SCHEMA public OWNER TO %s`, quotedRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO %s`, quotedRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO %s`, quotedRole),
	}
	for _, s := range stmts {
		if err := appAdmin.Exec(s).Error; err != nil {
			return fmt.Errorf("grant schema privileges (%q): %w", s, err)
		}
	}

	return nil
}

// openAdmin opens a short-lived GORM connection for administrative DDL. It does
// not apply the table-prefix naming strategy (no models are migrated here).
func openAdmin(dsn string) (*gorm.DB, error) {
	return gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
}

func closeGorm(g *gorm.DB) {
	if g == nil {
		return
	}
	if sqlDB, err := g.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// quoteIdent quotes a Postgres identifier by wrapping in double quotes and
// doubling any embedded double quote. Callers must still validate against
// identRe; this is defence in depth.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// quoteLiteral quotes a Postgres string literal by wrapping in single quotes
// and doubling any embedded single quote. Used for the generated password so it
// is never naively concatenated.
func quoteLiteral(literal string) string {
	return `'` + strings.ReplaceAll(literal, `'`, `''`) + `'`
}
