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

// activeLocksTool fetches blocking lock relationships from a live monitored
// database by dynamically resolving its connection string from the pgwatch
// Config DB (Dual-DB Routing).
type activeLocksTool struct {
	configClient *db.Client
}

// NewActiveLocksTool constructs a get_active_locks tool. The configClient
// must point to the pgwatch Config DB so the tool can resolve the monitored
// database's connection string at runtime.
func NewActiveLocksTool(configClient *db.Client) tool.Tool {
	return &activeLocksTool{configClient: configClient}
}

func (t *activeLocksTool) Name() string { return "get_active_locks" }

func (t *activeLocksTool) Description() string {
	return "Fetches the top 5 blocking queries currently executing on the live monitored database. " +
		"Use this to diagnose lock contention, deadlocks, or blocked transactions."
}

var activeLocksParamSchema = json.RawMessage(`{
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

func (t *activeLocksTool) Parameters() json.RawMessage { return activeLocksParamSchema }

func (t *activeLocksTool) Permission() tool.Permission { return tool.PermissionReadOnly }

type activeLocksArgs struct {
	DbName string `json:"db_name"`
}

const activeLocksSQL = `
	SELECT blocked.pid   AS blocked_pid,
	       blocker.pid   AS blocking_pid,
	       blocker.query AS blocking_statement
	FROM   pg_stat_activity blocked
	CROSS JOIN LATERAL unnest(pg_blocking_pids(blocked.pid)) AS b(pid)
	JOIN   pg_stat_activity blocker ON blocker.pid = b.pid
	WHERE  blocked.wait_event_type = 'Lock'
	LIMIT  5`

func (t *activeLocksTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args activeLocksArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("get_active_locks: invalid arguments: %w", err)
	}
	if args.DbName == "" {
		return "", fmt.Errorf("get_active_locks: db_name is required")
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

	rows, err := pool.Query(ctx, activeLocksSQL)
	if err != nil {
		return fmt.Sprintf("Failed to query pg_locks on %q: %v", args.DbName, err), nil
	}
	defer rows.Close()

	var buf strings.Builder
	fmt.Fprintf(&buf, "Lock Contention on %s:\n", args.DbName)

	count := 0
	for rows.Next() {
		var (
			blockedPID  int
			blockingPID int
			statement   string
		)
		if err := rows.Scan(&blockedPID, &blockingPID, &statement); err != nil {
			return fmt.Sprintf("Failed to read lock row from %q: %v", args.DbName, err), nil
		}
		count++

		truncated := statement
		if len(truncated) > 120 {
			truncated = truncated[:120] + "..."
		}
		fmt.Fprintf(&buf, "  Blocking PID %d is blocking PID %d with query: %s\n",
			blockingPID, blockedPID, truncated)
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("Error iterating lock results from %q: %v", args.DbName, err), nil
	}

	if count == 0 {
		return fmt.Sprintf("No active lock contention found on %s.", args.DbName), nil
	}

	return buf.String(), nil
}
