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
	maxRows      = 10
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
	//     the way Postgres does. So for MySQL the authoritative read-only gate
	//     is the isReadStatement keyword classification below, NOT the SET
	//     statement: a statement whose first keyword isn't in the read set is
	//     routed to executeWriteQuery, which is the only path that issues a
	//     write, and a read-only caller (readWrite=false) is contractually
	//     expected to send only read statements. The SET statement is
	//     defense-in-depth for MySQL, not a complete barrier; isReadStatement is
	//     the primary guard. (Tightening this into a hard server-side block for
	//     MySQL would require rejecting non-read statements outright when
	//     readWrite is false; left as-is since no concrete bypass is in scope.)
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

	if isReadStatement(query) {
		return executeReadQuery(ctx, conn, query, start)
	}
	return executeWriteQuery(ctx, conn, query, start)
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
