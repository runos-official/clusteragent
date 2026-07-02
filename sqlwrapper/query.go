// Package sqlwrapper executes SQL_QUERY and SQL_SCHEMA instructions against a
// tenant's Postgres or MySQL database. It pools connections per target
// (pool.go), enforces a read-only session unless the caller explicitly asks
// for read-write (query.go), caps returned rows, and introspects schema
// (schema.go).
package sqlwrapper

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	maxRows      = 100
	queryTimeout = 30 * time.Second
)

// ExecuteQuery runs a SQL query against the target database.
// When readWrite is false, the session is set to read-only so write statements will be rejected by the database.
func ExecuteQuery(params ConnectionParams, query string, readWrite bool) (*QueryResponse, error) {
	db, err := GetConnection(params)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	// Pin a single physical connection for the whole operation. The read-only
	// session mode is a per-session setting, so it MUST be applied and queried
	// on the same connection. Running the mode statement on the *sql.DB pool
	// and then the query separately could land the query on a different pooled
	// connection (MaxOpenConns>1), silently bypassing the read-only guard.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Close()

	start := time.Now()

	// Set session mode before executing. This persists for the lifetime of the
	// pinned connection; we always set it explicitly to handle reuse from a
	// different mode.
	//
	// Read-only guarantee differs by engine, and this difference is the reason
	// isReadStatement exists as more than a query/exec router:
	//   - Postgres: `default_transaction_read_only = on` is a robust,
	//     server-enforced guard. Any write statement (INSERT/UPDATE/DELETE/DDL)
	//     is rejected by the database itself even under autocommit, regardless
	//     of which code path we route it through. The engine is the
	//     authoritative gate here.
	//   - MySQL: `SET SESSION TRANSACTION READ ONLY` only constrains the next
	//     explicit transaction; it does NOT reliably block autocommit DML/DDL
	//     the way Postgres does. So for MySQL the authoritative read-only gate is
	//     the isReadStatement keyword classification: when readWrite is false a
	//     non-read statement is REJECTED outright (rejectNonReadUnderReadOnly
	//     below) before it can reach executeWriteQuery, so a write can never hit
	//     the database on a read-only connection regardless of engine. The SET
	//     statement remains defense-in-depth.
	var modeStmt string
	switch params.DatabaseType {
	case "postgres":
		if readWrite {
			modeStmt = "SET default_transaction_read_only = off"
		} else {
			modeStmt = "SET default_transaction_read_only = on"
		}
	case "mysql":
		if readWrite {
			modeStmt = "SET SESSION TRANSACTION READ WRITE"
		} else {
			modeStmt = "SET SESSION TRANSACTION READ ONLY"
		}
	}
	if _, err := conn.ExecContext(ctx, modeStmt); err != nil {
		return nil, fmt.Errorf("failed to set transaction mode: %w", err)
	}

	// Defense-in-depth read-only gate: refuse a non-read statement outright when
	// the caller asked for read-only, rather than routing it to the write path.
	// On MySQL this is the AUTHORITATIVE gate (the SET above does not block
	// autocommit DML/DDL); on Postgres it backs up the server-enforced
	// default_transaction_read_only.
	if rejectNonReadUnderReadOnly(readWrite, query) {
		return nil, fmt.Errorf("read-only connection: refusing to execute a non-read statement")
	}

	if isReadStatement(query) {
		return executeReadQuery(ctx, conn, query, start)
	}
	return executeWriteQuery(ctx, conn, query, start)
}

// rejectNonReadUnderReadOnly reports whether a statement must be refused before
// execution: a non-read statement on a read-only (readWrite=false) connection.
// This makes read-only a hard block instead of relying on per-engine session
// settings — the authoritative gate for MySQL (whose SET SESSION READ ONLY does
// not block autocommit DML), and defense-in-depth on Postgres. A statement that
// isReadStatement does not recognise as a read (incl. a comment- or
// whitespace-prefixed write, SET, or DDL) is treated as a write and rejected.
func rejectNonReadUnderReadOnly(readWrite bool, query string) bool {
	return !readWrite && !isReadStatement(query)
}

// isReadStatement checks the first keyword of the query to determine if it's a read operation.
func isReadStatement(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return true
	}

	firstWord := strings.ToUpper(strings.Fields(trimmed)[0])

	readKeywords := map[string]bool{
		"SELECT":   true,
		"SHOW":     true,
		"DESCRIBE": true,
		"DESC":     true,
		"EXPLAIN":  true,
		"WITH":     true,
	}

	return readKeywords[firstWord]
}

func executeReadQuery(ctx context.Context, conn *sql.Conn, query string, start time.Time) (*QueryResponse, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query execution failed: %w", err)
	}
	defer rows.Close()

	columnNames, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("failed to get column types: %w", err)
	}
	typeNames := make([]string, len(columnTypes))
	for i, ct := range columnTypes {
		typeNames[i] = ct.DatabaseTypeName()
	}

	var resultRows [][]any
	truncated := false
	count := 0

	for rows.Next() {
		if count >= maxRows {
			truncated = true
			break
		}
		values := make([]any, len(columnNames))
		valuePtrs := make([]any, len(columnNames))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		// Convert []byte to string for safe JSON serialization
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				values[i] = string(b)
			}
		}
		resultRows = append(resultRows, values)
		count++
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	durationMs := time.Since(start).Milliseconds()

	return &QueryResponse{
		Columns:     columnNames,
		ColumnTypes: typeNames,
		Rows:        resultRows,
		RowCount:    len(resultRows),
		DurationMs:  durationMs,
		Truncated:   truncated,
	}, nil
}

func executeWriteQuery(ctx context.Context, conn *sql.Conn, query string, start time.Time) (*QueryResponse, error) {
	result, err := conn.ExecContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query execution failed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	durationMs := time.Since(start).Milliseconds()

	return &QueryResponse{
		RowsAffected: rowsAffected,
		DurationMs:   durationMs,
	}, nil
}
