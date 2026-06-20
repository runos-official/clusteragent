package instructions

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/sqlwrapper"
)

// SQLSchema handles the SQL_SCHEMA instruction from nodeward.
func SQLSchema(jsonB64 string) (string, string, error) {
	var req sqlwrapper.SchemaRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "ERROR", "", err
	}

	log.Printf("SQL_SCHEMA request: host=%s, type=%s, refresh=%v",
		req.Connection.Host, req.Connection.DatabaseType, req.Refresh)

	if err := validateConnectionParams(req.Connection); err != nil {
		return "ERROR", "", err
	}

	connHash := sqlwrapper.ConnectionHash(req.Connection)

	// Check cache first (unless refresh is requested)
	if !req.Refresh {
		cached, err := datastore.GetSQLSchemaCache(connHash)
		if err == nil && cached != nil {
			var databases []sqlwrapper.SchemaDatabase
			if err := json.Unmarshal([]byte(cached.SchemaJSON), &databases); err == nil {
				resp := sqlwrapper.SchemaResponse{
					Databases:  databases,
					FromCache:  true,
					DurationMs: 0,
				}
				respB64, err := commons.JsonB64Encode(resp)
				if err != nil {
					return "ERROR", "", err
				}
				return "SQL_SCHEMA_RESPONSE", respB64, nil
			}
			log.Printf("SQL_SCHEMA: cached data is corrupt, fetching fresh: %v", err)
		}
	}

	// Fetch fresh from the target database
	start := time.Now()
	databases, err := sqlwrapper.FetchSchema(req.Connection)
	if err != nil {
		return "ERROR", "", fmt.Errorf("schema fetch failed: %w", err)
	}
	durationMs := time.Since(start).Milliseconds()

	// Cache the result
	schemaBytes, err := json.Marshal(databases)
	if err != nil {
		log.Printf("SQL_SCHEMA: failed to marshal schema for caching: %v", err)
	} else {
		if err := datastore.UpsertSQLSchemaCache(connHash, string(schemaBytes)); err != nil {
			log.Printf("SQL_SCHEMA: failed to cache schema: %v", err)
		}
	}

	resp := sqlwrapper.SchemaResponse{
		Databases:  databases,
		FromCache:  false,
		DurationMs: durationMs,
	}
	respB64, err := commons.JsonB64Encode(resp)
	if err != nil {
		return "ERROR", "", err
	}

	return "SQL_SCHEMA_RESPONSE", respB64, nil
}
