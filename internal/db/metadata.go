package db

import (
	"context"
	"fmt"
)

// Queries target the pgwatch catalog schema which stores metric definitions
// (pgwatch.metric) and monitored data-source registrations (pgwatch.source).
const (
	queryAvailableMetrics = `SELECT name FROM pgwatch.metric ORDER BY name`
	queryMonitoredDBs     = `SELECT name FROM pgwatch.source WHERE is_enabled = true ORDER BY name`
)

// GetAvailableMetrics returns the names of all metric definitions registered
// in the pgwatch.metric catalog. Each name corresponds to a collector whose
// SQL is executed against monitored databases on a schedule.
func (c *Client) GetAvailableMetrics(ctx context.Context) ([]string, error) {
	return c.queryStringColumn(ctx, queryAvailableMetrics)
}

// GetMonitoredDBs returns the names of all enabled data sources registered in
// the pgwatch.source catalog. Each source represents a PostgreSQL database
// (or instance) that pgwatch actively collects metrics from.
func (c *Client) GetMonitoredDBs(ctx context.Context) ([]string, error) {
	return c.queryStringColumn(ctx, queryMonitoredDBs)
}

// queryStringColumn executes a query that returns a single text column and
// collects every row into a string slice.
func (c *Client) queryStringColumn(ctx context.Context, query string) ([]string, error) {
	rows, err := c.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("db: executing query: %w", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			return nil, fmt.Errorf("db: scanning row: %w", err)
		}
		results = append(results, val)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterating rows: %w", err)
	}

	return results, nil
}
