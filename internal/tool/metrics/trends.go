// Package metrics implements agent-callable tools that retrieve and summarise
// pgwatch metric data for the LLM.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// trendsTool fetches a dynamic baseline comparison for a metric field and
// returns a deterministic summary string for the LLM.
type trendsTool struct {
	client *db.Client
}

// NewTrendsTool constructs a get_metric_trends tool backed by the given
// database client. The caller is responsible for registering it via
// [tool.Register] if desired.
func NewTrendsTool(client *db.Client) tool.Tool {
	return &trendsTool{client: client}
}

func (t *trendsTool) Name() string { return "get_metric_trends" }

func (t *trendsTool) Description() string {
	return "Compares the last 1-hour average of a metric field against its 24-hour baseline to detect deviations. " +
		"All math is performed server-side in PostgreSQL for efficiency."
}

// paramSchema is the JSON Schema advertised to the LLM. Defined once and
// reused on every Parameters() call to avoid repeated allocations.
var paramSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "metric_name": {
      "type": "string",
      "description": "The pgwatch metric table to query (e.g. db_stats, cpu_load, wal_size)."
    },
    "sys_id": {
      "type": "string",
      "description": "The pgwatch system identifier that uniquely identifies the PostgreSQL cluster."
    },
    "db_name": {
      "type": "string",
      "description": "The name of the monitored database."
    },
    "field_name": {
      "type": "string",
      "description": "The numeric JSONB field inside the data column to analyse (e.g. tps, blks_hit, commits)."
    }
  },
  "required": ["metric_name", "sys_id", "db_name", "field_name"],
  "additionalProperties": false
}`)

func (t *trendsTool) Parameters() json.RawMessage { return paramSchema }

func (t *trendsTool) Permission() tool.Permission { return tool.PermissionReadOnly }

// trendsArgs mirrors the JSON Schema so we can unmarshal the LLM's arguments
// into a typed struct.
type trendsArgs struct {
	MetricName string `json:"metric_name"`
	SysID      string `json:"sys_id"`
	DbName     string `json:"db_name"`
	FieldName  string `json:"field_name"`
}

func (t *trendsTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args trendsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("get_metric_trends: invalid arguments: %w", err)
	}

	if args.MetricName == "" || args.SysID == "" || args.DbName == "" || args.FieldName == "" {
		return "", fmt.Errorf("get_metric_trends: all of metric_name, sys_id, db_name, and field_name are required")
	}

	currentAvg, baselineAvg, err := t.client.FetchMetricBaseline(
		ctx, args.MetricName, args.SysID, args.DbName, args.FieldName,
	)
	if err != nil {
		return "", fmt.Errorf("get_metric_trends: %w", err)
	}

	if baselineAvg == 0 && currentAvg == 0 {
		return fmt.Sprintf(
			"Analyzed %s.%s (sys_id=%s, db=%s). No data found in the last 24 hours — "+
				"both the baseline and current averages are 0.",
			args.MetricName, args.FieldName, args.SysID, args.DbName,
		), nil
	}

	var deviation float64
	if baselineAvg != 0 {
		deviation = ((currentAvg - baselineAvg) / baselineAvg) * 100
	}

	return fmt.Sprintf(
		"Analyzed %s.%s. The 24-hour baseline average is %.4f. "+
			"The last 1-hour average is %.4f. "+
			"This represents a deviation of %+.2f%%.",
		args.MetricName, args.FieldName,
		baselineAvg, currentAvg, math.Round(deviation*100)/100,
	), nil
}
