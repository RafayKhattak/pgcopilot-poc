// Package metrics implements agent-callable tools that retrieve and summarise
// pgwatch metric data for the LLM.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// preferredFields lists data-column keys to look for, in priority order,
// when selecting which numeric field to analyse. If none match we fall
// back to the first numeric key we encounter.
var preferredFields = []string{"tps", "commits", "rollbacks", "blks_hit", "blks_read"}

// trendsTool fetches time-series metric rows and returns a textual summary
// that the LLM can reason about.
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
	return "Fetches the trend (rate of change) for a specific metric (e.g., cpu_load) for a given monitored database over a time interval."
}

// paramSchema is the JSON Schema advertised to the LLM. Defined once and
// reused on every Parameters() call to avoid repeated allocations.
var paramSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "metric_name": {
      "type": "string",
      "description": "The pgwatch metric to query (e.g. cpu_load, backends, wal_size)."
    },
    "sys_id": {
      "type": "string",
      "description": "The pgwatch system identifier that uniquely identifies the PostgreSQL cluster."
    },
    "db_name": {
      "type": "string",
      "description": "The name of the monitored database."
    },
    "interval": {
      "type": "string",
      "description": "PostgreSQL interval expression for the lookback window (e.g. '1h', '30m', '7d')."
    }
  },
  "required": ["metric_name", "sys_id", "db_name", "interval"],
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
	Interval   string `json:"interval"`
}

func (t *trendsTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args trendsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("get_metric_trends: invalid arguments: %w", err)
	}

	if args.MetricName == "" || args.SysID == "" || args.DbName == "" || args.Interval == "" {
		return "", fmt.Errorf("get_metric_trends: all of metric_name, sys_id, db_name, and interval are required")
	}

	rows, err := t.client.FetchMetricData(ctx, args.MetricName, args.SysID, args.DbName, args.Interval)
	if err != nil {
		return "", fmt.Errorf("get_metric_trends: querying data: %w", err)
	}

	if len(rows) == 0 {
		return fmt.Sprintf(
			"No data found for metric %q (sys_id=%s, db=%s) in the last %s.",
			args.MetricName, args.SysID, args.DbName, args.Interval,
		), nil
	}

	earliest, latest := extractTimeBounds(rows)

	fieldName, values := extractNumericSeries(rows)
	if len(values) == 0 {
		return fmt.Sprintf(
			"Fetched %d rows for %s (%s to %s) but found no numeric fields in the data column.",
			len(rows), args.MetricName,
			earliest.Format(time.RFC3339), latest.Format(time.RFC3339),
		), nil
	}

	// Rows arrive DESC (newest first); reverse to chronological order so
	// the first half corresponds to the earlier portion of the interval.
	reverseFloat64(values)

	mid := len(values) / 2
	firstAvg := average(values[:mid])
	secondAvg := average(values[mid:])

	var pctChange float64
	if firstAvg != 0 {
		pctChange = ((secondAvg - firstAvg) / firstAvg) * 100
	}

	direction := "increase"
	if pctChange < 0 {
		direction = "decrease"
	}

	return fmt.Sprintf(
		"Successfully analyzed %d rows for metric '%s' (field: '%s', sys_id=%s, db=%s). "+
			"Time range: %s to %s. "+
			"In the first half of the interval (older), the average %s was %.4f. "+
			"In the second half (more recent), the average %s was %.4f. "+
			"This represents a %.2f%% %s.",
		len(rows), args.MetricName, fieldName, args.SysID, args.DbName,
		earliest.Format(time.RFC3339), latest.Format(time.RFC3339),
		fieldName, firstAvg,
		fieldName, secondAvg,
		math.Abs(pctChange), direction,
	), nil
}

// extractTimeBounds scans the slice for the earliest and latest "time" values.
// The rows are expected to arrive ordered by time DESC from FetchMetricData,
// but we scan explicitly to be resilient to ordering changes.
func extractTimeBounds(rows []map[string]any) (earliest, latest time.Time) {
	for _, r := range rows {
		ts, ok := r["time"].(time.Time)
		if !ok {
			continue
		}
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
		if latest.IsZero() || ts.After(latest) {
			latest = ts
		}
	}
	return earliest, latest
}

// extractNumericSeries picks a numeric field from each row's "data" map and
// returns its name alongside the collected values. It tries preferredFields
// first, then falls back to the first numeric key it finds.
func extractNumericSeries(rows []map[string]any) (string, []float64) {
	if len(rows) == 0 {
		return "", nil
	}

	fieldName := pickNumericField(rows[0])
	if fieldName == "" {
		return "", nil
	}

	values := make([]float64, 0, len(rows))
	for _, r := range rows {
		dataMap, ok := toStringMap(r["data"])
		if !ok {
			continue
		}
		if v, ok := toFloat64(dataMap[fieldName]); ok {
			values = append(values, v)
		}
	}
	return fieldName, values
}

// pickNumericField examines the first row's data map and returns the best
// field name to analyse.
func pickNumericField(row map[string]any) string {
	dataMap, ok := toStringMap(row["data"])
	if !ok {
		return ""
	}

	for _, pf := range preferredFields {
		if _, ok := toFloat64(dataMap[pf]); ok {
			return pf
		}
	}

	for k, v := range dataMap {
		if _, ok := toFloat64(v); ok {
			return k
		}
	}
	return ""
}

// toStringMap attempts to assert v as map[string]any, which is the standard
// shape produced by json.Unmarshal for JSON objects.
func toStringMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// toFloat64 coerces a JSON-unmarshalled numeric value to float64.
// json.Unmarshal produces float64 for all JSON numbers, but we also
// handle int/int64 defensively in case the value has been through a
// different decoder.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func reverseFloat64(s []float64) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func average(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += v
	}
	return sum / float64(len(s))
}
