package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// hypopgTool creates a hypothetical index via HypoPG on a live monitored
// database and compares the EXPLAIN cost of a query before and after, without
// mutating any real state. It uses Dual-DB Routing: the configClient resolves
// the target database's connection string from pgwatch.source.
type hypopgTool struct {
	configClient *db.Client
}

// NewHypoPGTool constructs an evaluate_hypothetical_index tool. The
// configClient must point to the pgwatch Config DB so the tool can
// dynamically resolve the target database's connection string.
func NewHypoPGTool(configClient *db.Client) tool.Tool {
	return &hypopgTool{configClient: configClient}
}

func (h *hypopgTool) Name() string { return "evaluate_hypothetical_index" }

func (h *hypopgTool) Description() string {
	return "Creates a hypothetical index using HypoPG on a live monitored database and runs EXPLAIN " +
		"on a query to see if the query planner would use the new index. Returns the before and after query costs."
}

var hypopgParamSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "db_name": {
      "type": "string",
      "description": "The name of the monitored database as registered in pgwatch (e.g. tenant3_db)."
    },
    "query": {
      "type": "string",
      "description": "The SQL SELECT query to test (e.g. SELECT * FROM orders WHERE customer_id = 42)."
    },
    "index_statement": {
      "type": "string",
      "description": "The CREATE INDEX statement to evaluate (e.g. CREATE INDEX ON orders (customer_id))."
    }
  },
  "required": ["db_name", "query", "index_statement"],
  "additionalProperties": false
}`)

func (h *hypopgTool) Parameters() json.RawMessage { return hypopgParamSchema }

func (h *hypopgTool) Permission() tool.Permission { return tool.PermissionReadOnly }

type hypopgArgs struct {
	DbName         string `json:"db_name"`
	Query          string `json:"query"`
	IndexStatement string `json:"index_statement"`
}

func (h *hypopgTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args hypopgArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("evaluate_hypothetical_index: invalid arguments: %w", err)
	}
	if args.DbName == "" || args.Query == "" || args.IndexStatement == "" {
		return "", fmt.Errorf("evaluate_hypothetical_index: db_name, query, and index_statement are all required")
	}

	dsn, err := h.configClient.GetMonitoredDBConnStr(ctx, args.DbName)
	if err != nil {
		return fmt.Sprintf("Failed to resolve connection for %q: %v", args.DbName, err), nil
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Sprintf("Failed to connect to live database %q: %v", args.DbName, err), nil
	}
	defer pool.Close()

	// All steps must run on the same connection because HypoPG hypothetical
	// indexes only exist in the creating backend's memory.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Sprintf("Failed to acquire connection on %q: %v", args.DbName, err), nil
	}
	defer conn.Release()

	costBefore, err := explainCost(ctx, conn, args.Query)
	if err != nil {
		return fmt.Sprintf(
			"Failed to run EXPLAIN on the query before creating the hypothetical index: %v", err,
		), nil
	}

	_, err = conn.Exec(ctx, "SELECT * FROM hypopg_create_index($1)", args.IndexStatement)
	if err != nil {
		return fmt.Sprintf(
			"Failed to create hypothetical index (is the HypoPG extension installed?): %v", err,
		), nil
	}

	costAfter, err := explainCost(ctx, conn, args.Query)
	if err != nil {
		_, _ = conn.Exec(ctx, "SELECT * FROM hypopg_reset()")
		return fmt.Sprintf(
			"Failed to run EXPLAIN after creating the hypothetical index: %v", err,
		), nil
	}

	_, _ = conn.Exec(ctx, "SELECT * FROM hypopg_reset()")

	var pctImprovement float64
	if costBefore != 0 {
		pctImprovement = ((costBefore - costAfter) / costBefore) * 100
	}

	verdict := "no improvement"
	if pctImprovement > 1 {
		verdict = "improvement"
	} else if pctImprovement < -1 {
		verdict = "regression"
	}

	return fmt.Sprintf(
		"Hypothetical index evaluated on %s. Cost before: %.2f. Cost after: %.2f. "+
			"Improvement: %.2f%% (%s). Index statement: %s",
		args.DbName, costBefore, costAfter,
		math.Abs(pctImprovement), verdict,
		args.IndexStatement,
	), nil
}

// costPattern matches the total-cost figure in an EXPLAIN output line.
// Example: "Seq Scan on foo  (cost=0.00..35.50 rows=2550 width=4)"
//
//	captures "35.50"
var costPattern = regexp.MustCompile(`\(cost=[\d.]+\.\.([\d.]+)`)

// explainCost runs EXPLAIN on query and extracts the total cost from the
// top-level plan node (the first line containing a cost estimate).
func explainCost(ctx context.Context, conn *pgxpool.Conn, query string) (float64, error) {
	rows, err := conn.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return 0, err
		}
		if m := costPattern.FindStringSubmatch(line); len(m) == 2 {
			cost, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return 0, fmt.Errorf("parsing cost %q: %w", m[1], err)
			}
			return cost, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no cost estimate found in EXPLAIN output")
}
