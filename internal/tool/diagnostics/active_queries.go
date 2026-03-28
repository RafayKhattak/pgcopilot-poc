// Package diagnostics implements agent-callable tools that query live monitored
// databases for real-time diagnostic data (e.g. pg_stat_activity).
package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// activeQueriesTool fetches the longest-running non-idle queries from a live
// monitored database by dynamically resolving its connection string from the
// pgwatch Config DB.
type activeQueriesTool struct {
	configClient *db.Client
}

// NewActiveQueriesTool constructs a get_active_queries tool. The configClient
// must point to the pgwatch Config DB (where pgwatch.source lives) so the
// tool can resolve the monitored database's connection string at runtime.
func NewActiveQueriesTool(configClient *db.Client) tool.Tool {
	return &activeQueriesTool{configClient: configClient}
}

func (t *activeQueriesTool) Name() string { return "get_active_queries" }

func (t *activeQueriesTool) Description() string {
	return "Fetches the top 5 longest-running active queries currently executing on the live monitored database. " +
		"Use this to diagnose live locks or CPU spikes."
}

var activeQueriesParamSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "db_name": {
      "type": "string",
      "description": "The name of the monitored database as registered in pgwatch."
    }
  },
  "required": ["db_name"],
  "additionalProperties": false
}`)

func (t *activeQueriesTool) Parameters() json.RawMessage { return activeQueriesParamSchema }

func (t *activeQueriesTool) Permission() tool.Permission { return tool.PermissionReadOnly }

type activeQueriesArgs struct {
	DbName string `json:"db_name"`
}

const activeQueriesSQL = `SELECT pid, state, duration, query FROM (
	SELECT pid, state,
	       extract(epoch FROM (now() - query_start)) AS duration,
	       query
	FROM pg_stat_activity
	WHERE state != 'idle'
	  AND pid != pg_backend_pid()
	ORDER BY duration DESC
	LIMIT 5
) sub`

func (t *activeQueriesTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args activeQueriesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("get_active_queries: invalid arguments: %w", err)
	}
	if args.DbName == "" {
		return "", fmt.Errorf("get_active_queries: db_name is required")
	}

	dsn, err := t.configClient.GetMonitoredDBConnStr(ctx, args.DbName)
	if err != nil {
		return fmt.Sprintf("Failed to resolve connection for %q: %v", args.DbName, err), nil
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Sprintf("Failed to connect to live database %q: %v", args.DbName, err), nil
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, activeQueriesSQL)
	if err != nil {
		return fmt.Sprintf("Failed to query pg_stat_activity on %q: %v", args.DbName, err), nil
	}
	defer rows.Close()

	var buf strings.Builder
	fmt.Fprintf(&buf, "Live Active Queries for %s:\n", args.DbName)

	count := 0
	for rows.Next() {
		var (
			pid      int
			state    string
			duration float64
			query    string
		)
		if err := rows.Scan(&pid, &state, &duration, &query); err != nil {
			return fmt.Sprintf("Failed to read query row from %q: %v", args.DbName, err), nil
		}
		count++

		truncated := query
		if len(truncated) > 120 {
			truncated = truncated[:120] + "..."
		}
		fmt.Fprintf(&buf, "  PID: %d, State: %s, Duration: %.1fs, Query: %s\n", pid, state, duration, truncated)
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("Error iterating query results from %q: %v", args.DbName, err), nil
	}

	if count == 0 {
		return fmt.Sprintf("No active (non-idle) queries found on %s.", args.DbName), nil
	}

	return buf.String(), nil
}
