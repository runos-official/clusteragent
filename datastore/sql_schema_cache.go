package datastore

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	var m SQLSchemaCacheModel
	err = gdb.Where("connection_hash = ?", connectionHash).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query schema cache: %w", err)
	}
	return &SQLSchemaCache{
		ID:             int64(m.ID),
		ConnectionHash: m.ConnectionHash,
		SchemaJSON:     m.SchemaJSON,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}, nil
}

// UpsertSQLSchemaCache inserts or replaces a cached schema entry. On a
// connection_hash conflict it overwrites schema_json and bumps updated_at,
// matching the original ON CONFLICT DO UPDATE behaviour.
func UpsertSQLSchemaCache(connectionHash, schemaJSON string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	entry := SQLSchemaCacheModel{
		ConnectionHash: connectionHash,
		SchemaJSON:     schemaJSON,
		UpdatedAt:      time.Now(),
	}
	err = gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "connection_hash"}},
		DoUpdates: clause.Assignments(map[string]any{
			"schema_json": schemaJSON,
			"updated_at":  gorm.Expr("CURRENT_TIMESTAMP"),
		}),
	}).Create(&entry).Error
	if err != nil {
		return fmt.Errorf("failed to upsert schema cache: %w", err)
	}
	return nil
}

// DeleteSQLSchemaCache removes a cached schema entry by connection hash.
func DeleteSQLSchemaCache(connectionHash string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	err = gdb.Where("connection_hash = ?", connectionHash).Delete(&SQLSchemaCacheModel{}).Error
	if err != nil {
		return fmt.Errorf("failed to delete schema cache: %w", err)
	}
	return nil
}
