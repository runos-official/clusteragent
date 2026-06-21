package sqlwrapper

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// schemaQueryTimeout bounds each schema-introspection query so a slow or
// unresponsive tenant database can't hang the SQL_SCHEMA handler (and the
// stream worker behind it) indefinitely. Schema reads previously ran with no
// deadline at all.
const schemaQueryTimeout = 30 * time.Second

// FetchSchema introspects the target database and returns all schemas, databases, and tables.
func FetchSchema(params ConnectionParams) ([]SchemaDatabase, error) {
	switch params.DatabaseType {
	case "postgres":
		return fetchPostgresSchema(params)
	case "mysql":
		return fetchMySQLSchema(params)
	default:
		return nil, fmt.Errorf("unsupported database type: %s", params.DatabaseType)
	}
}

func fetchPostgresSchema(params ConnectionParams) ([]SchemaDatabase, error) {
	// If a specific database is requested, only introspect that one
	if params.DatabaseName != "" {
		db, err := GetConnection(params)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to database %s: %w", params.DatabaseName, err)
		}
		tables, err := fetchPostgresTables(db)
		if err != nil {
			return nil, err
		}
		return []SchemaDatabase{{Name: params.DatabaseName, Tables: tables}}, nil
	}

	// Connect to the default "postgres" database to list all databases
	listParams := params
	listParams.DatabaseName = "postgres"
	db, err := GetConnection(listParams)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to postgres database: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datistemplate = false ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}
	defer rows.Close()

	var dbNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan database name: %w", err)
		}
		dbNames = append(dbNames, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var databases []SchemaDatabase
	for _, dbName := range dbNames {
		dbParams := params
		dbParams.DatabaseName = dbName
		conn, err := GetConnection(dbParams)
		if err != nil {
			log.Printf("sqlwrapper: skipping database %s: %v", dbName, err)
			continue
		}
		tables, err := fetchPostgresTables(conn)
		if err != nil {
			log.Printf("sqlwrapper: skipping database %s: %v", dbName, err)
			continue
		}
		databases = append(databases, SchemaDatabase{Name: dbName, Tables: tables})
	}

	return databases, nil
}

func fetchPostgresTables(db *sql.DB) ([]SchemaTable, error) {
	query := `
		SELECT
			t.table_schema,
			t.table_name,
			c.column_name,
			c.data_type,
			c.is_nullable,
			COALESCE(c.column_default, '') as column_default,
			CASE WHEN kcu.column_name IS NOT NULL THEN true ELSE false END as is_primary_key
		FROM information_schema.tables t
		JOIN information_schema.columns c
			ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		LEFT JOIN information_schema.table_constraints tc
			ON tc.table_schema = t.table_schema
			AND tc.table_name = t.table_name
			AND tc.constraint_type = 'PRIMARY KEY'
		LEFT JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_name = tc.constraint_name
			AND kcu.table_schema = tc.table_schema
			AND kcu.column_name = c.column_name
		WHERE t.table_schema NOT IN ('pg_catalog', 'information_schema', 'ag_catalog')
			AND t.table_type = 'BASE TABLE'
			AND t.table_schema NOT IN (
				SELECT table_schema FROM information_schema.tables
				WHERE table_name = '_ag_label_vertex'
			)
		ORDER BY t.table_schema, t.table_name, c.ordinal_position`

	ctx, cancel := context.WithTimeout(context.Background(), schemaQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query postgres schema: %w", err)
	}
	defer rows.Close()

	return scanSchemaRows(rows)
}

func fetchMySQLSchema(params ConnectionParams) ([]SchemaDatabase, error) {
	// If a specific database is requested, only introspect that one
	if params.DatabaseName != "" {
		db, err := GetConnection(params)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to database %s: %w", params.DatabaseName, err)
		}
		tables, err := fetchMySQLTables(db, params.DatabaseName)
		if err != nil {
			return nil, err
		}
		return []SchemaDatabase{{Name: params.DatabaseName, Tables: tables}}, nil
	}

	// Connect without a specific database to list all
	db, err := GetConnection(params)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mysql: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SHOW DATABASES`)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}
	defer rows.Close()

	systemDBs := map[string]bool{
		"information_schema": true,
		"mysql":              true,
		"performance_schema": true,
		"sys":                true,
	}

	var dbNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan database name: %w", err)
		}
		if !systemDBs[name] {
			dbNames = append(dbNames, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var databases []SchemaDatabase
	for _, dbName := range dbNames {
		tables, err := fetchMySQLTables(db, dbName)
		if err != nil {
			log.Printf("sqlwrapper: skipping database %s: %v", dbName, err)
			continue
		}
		databases = append(databases, SchemaDatabase{Name: dbName, Tables: tables})
	}

	return databases, nil
}

func fetchMySQLTables(db *sql.DB, databaseName string) ([]SchemaTable, error) {
	query := `
		SELECT
			t.TABLE_SCHEMA,
			t.TABLE_NAME,
			c.COLUMN_NAME,
			c.DATA_TYPE,
			c.IS_NULLABLE,
			COALESCE(c.COLUMN_DEFAULT, '') as COLUMN_DEFAULT,
			CASE WHEN c.COLUMN_KEY = 'PRI' THEN true ELSE false END as IS_PRIMARY_KEY
		FROM information_schema.TABLES t
		JOIN information_schema.COLUMNS c
			ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		WHERE t.TABLE_SCHEMA = ?
			AND t.TABLE_TYPE = 'BASE TABLE'
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME, c.ORDINAL_POSITION`

	ctx, cancel := context.WithTimeout(context.Background(), schemaQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, query, databaseName)
	if err != nil {
		return nil, fmt.Errorf("failed to query mysql schema: %w", err)
	}
	defer rows.Close()

	return scanSchemaRows(rows)
}

// scanSchemaRows reads schema rows (shared between PG and MySQL) and groups them
// into SchemaTable structs with nested columns.
func scanSchemaRows(rows *sql.Rows) ([]SchemaTable, error) {
	type tableKey struct {
		schema string
		name   string
	}

	tableOrder := []tableKey{}
	tableMap := map[tableKey]*SchemaTable{}

	for rows.Next() {
		var (
			schema       string
			tableName    string
			colName      string
			dataType     string
			isNullable   string
			defaultValue string
			isPrimaryKey bool
		)
		if err := rows.Scan(&schema, &tableName, &colName, &dataType, &isNullable, &defaultValue, &isPrimaryKey); err != nil {
			return nil, fmt.Errorf("failed to scan schema row: %w", err)
		}

		key := tableKey{schema: schema, name: tableName}
		tbl, exists := tableMap[key]
		if !exists {
			tbl = &SchemaTable{
				Schema: schema,
				Name:   tableName,
			}
			tableMap[key] = tbl
			tableOrder = append(tableOrder, key)
		}

		tbl.Columns = append(tbl.Columns, SchemaColumn{
			Name:         colName,
			DataType:     dataType,
			IsNullable:   isNullable == "YES",
			IsPrimaryKey: isPrimaryKey,
			DefaultValue: defaultValue,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	tables := make([]SchemaTable, 0, len(tableOrder))
	for _, key := range tableOrder {
		tables = append(tables, *tableMap[key])
	}
	return tables, nil
}
