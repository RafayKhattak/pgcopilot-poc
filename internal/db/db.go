// Package db provides a thin wrapper around pgxpool for PostgreSQL connectivity.
// It centralises pool creation, health-checking, and graceful shutdown so the
// rest of the application can depend on a single [Client] value.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Client is the application's handle to a PostgreSQL connection pool.
type Client struct {
	pool *pgxpool.Pool
}

// NewClient parses connString, creates a connection pool, and verifies
// reachability with a ping before returning. The caller owns the returned
// Client and must call [Client.Close] when done.
func NewClient(ctx context.Context, connString string) (*Client, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("db: parsing connection string: %w", err)
	}

	// Sensible pool defaults — callers can override via the DSN
	// (e.g. pool_max_conns=10) which ParseConfig already handles.
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 10
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: creating pool: %w", err)
	}

	// Verify the database is reachable before handing the pool to callers.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: pinging database: %w", err)
	}

	return &Client{pool: pool}, nil
}

// Pool exposes the underlying pgxpool.Pool for callers that need direct
// access (e.g. running raw queries or using pgx-specific features).
func (c *Client) Pool() *pgxpool.Pool { return c.pool }

// Close drains and shuts down the connection pool. It is safe to call
// multiple times; subsequent calls are no-ops.
func (c *Client) Close() { c.pool.Close() }

// Ping verifies the database is still reachable. Useful for health-check
// endpoints or readiness probes.
func (c *Client) Ping(ctx context.Context) error {
	return c.pool.Ping(ctx)
}
