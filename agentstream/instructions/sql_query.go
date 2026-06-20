package instructions

import (
	"fmt"
	"log"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/sqlwrapper"
)

// SQLQuery handles the SQL_QUERY instruction from nodeward.
func SQLQuery(jsonB64 string) (string, string, error) {
	var req sqlwrapper.QueryRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "ERROR", "", err
	}

	log.Printf("SQL_QUERY request: host=%s, type=%s, rw=%v, query_len=%d",
		req.Connection.Host, req.Connection.DatabaseType, req.ReadWrite, len(req.Query))

	if err := validateConnectionParams(req.Connection); err != nil {
		return "ERROR", "", err
	}

	if req.Query == "" {
		return "ERROR", "", fmt.Errorf("query is required")
	}

	result, err := sqlwrapper.ExecuteQuery(req.Connection, req.Query, req.ReadWrite)
	if err != nil {
		return "ERROR", "", err
	}

	respB64, err := commons.JsonB64Encode(result)
	if err != nil {
		return "ERROR", "", err
	}

	return "SQL_QUERY_RESPONSE", respB64, nil
}

func validateConnectionParams(c sqlwrapper.ConnectionParams) error {
	if c.Username == "" {
		return fmt.Errorf("username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("password is required")
	}
	if c.Host == "" {
		return fmt.Errorf("host is required")
	}
	if c.Port == 0 {
		return fmt.Errorf("port is required")
	}
	if c.DatabaseType != "postgres" && c.DatabaseType != "mysql" {
		return fmt.Errorf("databaseType must be 'postgres' or 'mysql'")
	}
	return nil
}
