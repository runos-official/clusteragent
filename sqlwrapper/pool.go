package sqlwrapper

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

type poolEntry struct {
	db       *sql.DB
	lastUsed time.Time
	mu       sync.Mutex
}

var (
	pools  sync.Map
	poolMu sync.Mutex
)

const (
	maxIdleConns      = 2
	maxOpenConns      = 5
	connMaxLifetime   = 10 * time.Minute
	connMaxIdleTime   = 5 * time.Minute
	poolEvictInterval = 2 * time.Minute
	poolIdleTimeout   = 10 * time.Minute
)

// GetConnection returns a *sql.DB for the given connection parameters,
// reusing an existing pool if one exists for the same connection hash.
func GetConnection(params ConnectionParams) (*sql.DB, error) {
	hash := ConnectionHash(params)

	if val, ok := pools.Load(hash); ok {
		entry := val.(*poolEntry)
		entry.mu.Lock()
		entry.lastUsed = time.Now()
		entry.mu.Unlock()

		if err := entry.db.Ping(); err == nil {
			return entry.db, nil
		}
		// Stale connection — remove and recreate
		entry.db.Close()
		pools.Delete(hash)
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	// Double-check after acquiring lock
	if val, ok := pools.Load(hash); ok {
		entry := val.(*poolEntry)
		entry.mu.Lock()
		entry.lastUsed = time.Now()
		entry.mu.Unlock()
		return entry.db, nil
	}

	dsn, driver, err := buildDSN(params)
	if err != nil {
		return nil, fmt.Errorf("failed to build DSN: %w", err)
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open connection: %w", err)
	}

	db.SetMaxIdleConns(maxIdleConns)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	pools.Store(hash, &poolEntry{
		db:       db,
		lastUsed: time.Now(),
	})

	log.Printf("sqlwrapper: new connection pool created for %s@%s:%d/%s (%s)",
		params.Username, params.Host, params.Port, params.DatabaseName, params.DatabaseType)

	return db, nil
}

// pqEscape renders a value safe for a lib/pq keyword=value DSN. lib/pq parses
// `key=value` pairs and treats whitespace as a separator, so an unescaped
// password containing a space or quote could inject extra connection
// parameters (e.g. `sslmode=disable` -> `... password=foo sslmode=require`).
// Wrapping the value in single quotes and backslash-escaping `'` and `\`
// matches lib/pq's documented quoting rules.
func pqEscape(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

func buildDSN(params ConnectionParams) (dsn string, driver string, err error) {
	switch params.DatabaseType {
	case "postgres":
		driver = "postgres"
		dbName := params.DatabaseName
		if dbName == "" {
			dbName = "postgres"
		}
		dsn = fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			pqEscape(params.Host),
			params.Port,
			pqEscape(params.Username),
			pqEscape(params.Password),
			pqEscape(dbName),
		)
	case "mysql":
		driver = "mysql"
		cfg := mysql.NewConfig()
		cfg.User = params.Username
		cfg.Passwd = params.Password
		cfg.Net = "tcp"
		cfg.Addr = fmt.Sprintf("%s:%d", params.Host, params.Port)
		cfg.DBName = params.DatabaseName
		dsn = cfg.FormatDSN()
	default:
		return "", "", fmt.Errorf("unsupported database type: %s", params.DatabaseType)
	}
	return dsn, driver, nil
}

// StartPoolCleanup runs a background goroutine that evicts idle connection pools.
func StartPoolCleanup() {
	ticker := time.NewTicker(poolEvictInterval)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		pools.Range(func(key, value any) bool {
			entry := value.(*poolEntry)
			entry.mu.Lock()
			idle := now.Sub(entry.lastUsed)
			entry.mu.Unlock()
			if idle > poolIdleTimeout {
				log.Printf("sqlwrapper: evicting idle connection pool (idle %s)", idle)
				entry.db.Close()
				pools.Delete(key)
			}
			return true
		})
	}
}

// CloseAllPools closes every connection pool. Called on shutdown.
func CloseAllPools() {
	pools.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		entry.db.Close()
		pools.Delete(key)
		return true
	})
	log.Println("sqlwrapper: all connection pools closed")
}
