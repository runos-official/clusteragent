package sqlwrapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ConnectionParams identifies a target database for all SQL operations.
type ConnectionParams struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	DatabaseType string `json:"databaseType"` // "postgres" or "mysql"
	DatabaseName string `json:"databaseName,omitempty"`
}

// --- SQL_SCHEMA types ---

type SchemaRequest struct {
	Connection ConnectionParams `json:"connection"`
	Refresh    bool             `json:"refresh"`
}

type SchemaDatabase struct {
	Name   string        `json:"name"`
	Tables []SchemaTable `json:"tables"`
}

type SchemaTable struct {
	Schema  string         `json:"schema,omitempty"` // e.g. "public" for PG, empty for MySQL
	Name    string         `json:"name"`
	Columns []SchemaColumn `json:"columns"`
}

type SchemaColumn struct {
	Name         string `json:"name"`
	DataType     string `json:"dataType"`
	IsNullable   bool   `json:"isNullable"`
	IsPrimaryKey bool   `json:"isPrimaryKey"`
	DefaultValue string `json:"defaultValue,omitempty"`
}

type SchemaResponse struct {
	Databases  []SchemaDatabase `json:"databases"`
	FromCache  bool             `json:"fromCache"`
	DurationMs int64            `json:"durationMs"`
}

// --- SQL_QUERY types ---

type QueryRequest struct {
	Connection ConnectionParams `json:"connection"`
	Query      string           `json:"query"`
	ReadWrite  bool             `json:"readWrite"`
}

type QueryResponse struct {
	Columns      []string `json:"columns"`
	ColumnTypes  []string `json:"columnTypes"`
	Rows         [][]any  `json:"rows"`
	RowCount     int      `json:"rowCount"`
	RowsAffected int64    `json:"rowsAffected"`
	DurationMs   int64    `json:"durationMs"`
	Truncated    bool     `json:"truncated"`
}

// ConnectionHash produces a deterministic SHA-256 hex digest for the given connection parameters.
// Used as the cache key and pool map key.
func ConnectionHash(params ConnectionParams) string {
	raw := fmt.Sprintf("%s:%s:%s@%s:%d/%s",
		params.DatabaseType,
		params.Username,
		params.Password,
		params.Host,
		params.Port,
		params.DatabaseName,
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
