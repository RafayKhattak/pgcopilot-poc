package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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

// GetMonitoredDBConnStr resolves the connection string of an enabled monitored
// database by looking it up in the pgwatch.source config table. The dbName
// parameter is fully parameterised ($1) to prevent SQL injection.
func (c *Client) GetMonitoredDBConnStr(ctx context.Context, dbName string) (string, error) {
	const query = `SELECT connstr FROM pgwatch.source WHERE name = $1 AND is_enabled = true LIMIT 1`

	var connStr string
	err := c.pool.QueryRow(ctx, query, dbName).Scan(&connStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("db: monitored database %q not found or disabled in pgwatch.source", dbName)
	}
	if err != nil {
		return "", fmt.Errorf("db: resolving connection string for %q: %w", dbName, err)
	}

	return connStr, nil
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
