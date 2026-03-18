package db

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// safeIdentifier matches strings that are valid as unquoted PostgreSQL
// identifiers: letters, digits, and underscores only. This is intentionally
// strict — no dots, dashes, spaces, or quotes — so that a metric name can
// never escape the "public".<metric> table reference when interpolated into
// the query string.
//
// Why not use pgx parameterisation ($1)?  PostgreSQL does not allow
// parameterised identifiers (table/column names). The only safe path is
// to validate the identifier against a strict allow-list regex *before*
// formatting it into the SQL text.
var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// FetchMetricData retrieves time-series rows from a pgwatch metric table.
//
// # Table naming
//
// pgwatch stores each metric in its own table under the public schema
// (e.g. public.cpu_utilization, public.wal_stats). Because table names
// cannot be parameterised in SQL, the metric argument is validated against
// [safeIdentifier] before being interpolated into the query.
//
// # Tenant / cluster scoping
//
// A single pgwatch metrics database may contain data from many monitored
// clusters. To prevent cross-cluster data leaks every query is scoped by
// two dimensions:
//
//   - dbname ($1) — the monitored database name.
//   - data->>'sys_id' ($2) — the pgwatch system identifier that uniquely
//     identifies a PostgreSQL cluster. Filtering on sys_id inside the JSONB
//     "data" column ensures a caller can only see rows belonging to their
//     own cluster, even when multiple clusters report the same dbname.
//
// # Time windowing
//
// The interval argument ($3) is cast to a PostgreSQL interval and used in
// a >= NOW() - $3::interval predicate so only recent data is returned.
//
// The method returns each row as a map with keys "time", "data", and
// "tag_data", preserving the original JSONB structure for downstream
// consumers (e.g. the LLM prompt builder).
func (c *Client) FetchMetricData(
	ctx context.Context,
	metric string,
	sysID string,
	dbName string,
	interval string,
) ([]map[string]any, error) {

	// ---- 1. Validate the metric identifier ----
	if metric == "" {
		return nil, fmt.Errorf("db: metric name must not be empty")
	}
	if !safeIdentifier.MatchString(metric) {
		return nil, fmt.Errorf("db: invalid metric name %q: must match %s", metric, safeIdentifier.String())
	}

	// ---- 2. Build the query with the safe identifier ----
	//
	// Only the table name is interpolated; all user-supplied filter values
	// are bound via $1/$2/$3 so they are never interpreted as SQL.
	query := fmt.Sprintf(`
		SELECT time, data, tag_data
		FROM   public.%s
		WHERE  dbname          = $1
		  AND  data->>'sys_id' = $2
		  AND  time           >= NOW() - $3::interval
		ORDER BY time DESC`, metric)

	// ---- 3. Execute ----
	rows, err := c.pool.Query(ctx, query, dbName, sysID, interval)
	if err != nil {
		return nil, fmt.Errorf("db: querying metric %q: %w", metric, err)
	}
	defer rows.Close()

	// ---- 4. Collect results ----
	var results []map[string]any
	for rows.Next() {
		var (
			ts      time.Time
			dataRaw json.RawMessage
			tagRaw  json.RawMessage
		)
		if err := rows.Scan(&ts, &dataRaw, &tagRaw); err != nil {
			return nil, fmt.Errorf("db: scanning metric %q row: %w", metric, err)
		}

		var data, tagData any
		if err := json.Unmarshal(dataRaw, &data); err != nil {
			return nil, fmt.Errorf("db: unmarshalling data column for metric %q: %w", metric, err)
		}
		if err := json.Unmarshal(tagRaw, &tagData); err != nil {
			return nil, fmt.Errorf("db: unmarshalling tag_data column for metric %q: %w", metric, err)
		}

		results = append(results, map[string]any{
			"time":     ts,
			"data":     data,
			"tag_data": tagData,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterating metric %q rows: %w", metric, err)
	}

	return results, nil
}
