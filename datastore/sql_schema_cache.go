package datastore

import (
	"database/sql"
	"fmt"
	"time"
)

type SQLSchemaCache struct {
	ID             int64     `json:"id"`
	ConnectionHash string    `json:"connectionHash"`
	SchemaJSON     string    `json:"schemaJson"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// GetSQLSchemaCache retrieves a cached schema entry by connection hash.
// Returns nil, nil if no entry is found.
func GetSQLSchemaCache(connectionHash string) (*SQLSchemaCache, error) {
	row := db.QueryRow(
		`SELECT id, connection_hash, schema_json, created_at, updated_at
		 FROM sql_schema_cache WHERE connection_hash = ?`,
		connectionHash,
	)

	var entry SQLSchemaCache
	err := row.Scan(&entry.ID, &entry.ConnectionHash, &entry.SchemaJSON, &entry.CreatedAt, &entry.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query schema cache: %w", err)
	}
	return &entry, nil
}

// UpsertSQLSchemaCache inserts or replaces a cached schema entry.
func UpsertSQLSchemaCache(connectionHash, schemaJSON string) error {
	_, err := db.Exec(
		`INSERT INTO sql_schema_cache (connection_hash, schema_json, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(connection_hash) DO UPDATE SET
		   schema_json = excluded.schema_json,
		   updated_at = CURRENT_TIMESTAMP`,
		connectionHash, schemaJSON,
	)
	if err != nil {
		return fmt.Errorf("failed to upsert schema cache: %w", err)
	}
	return nil
}

// DeleteSQLSchemaCache removes a cached schema entry by connection hash.
func DeleteSQLSchemaCache(connectionHash string) error {
	_, err := db.Exec(`DELETE FROM sql_schema_cache WHERE connection_hash = ?`, connectionHash)
	if err != nil {
		return fmt.Errorf("failed to delete schema cache: %w", err)
	}
	return nil
}
