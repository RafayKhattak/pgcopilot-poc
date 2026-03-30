package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const tenantCount = 5

func main() {
	_ = godotenv.Load()

	superuserURL := os.Getenv("POSTGRES_SUPERUSER_URL")
	if superuserURL == "" {
		log.Fatal("POSTGRES_SUPERUSER_URL is not set (e.g. postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable)")
	}
	configURL := os.Getenv("PGWATCH_DB_URL")
	if configURL == "" {
		log.Fatal("PGWATCH_DB_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Superuser pool for CREATE DATABASE and CREATE EXTENSION.
	adminPool, err := pgxpool.New(ctx, superuserURL)
	if err != nil {
		log.Fatalf("Failed to connect as superuser: %v", err)
	}
	defer adminPool.Close()
	log.Println("[OK] Connected as superuser")

	configPool, err := pgxpool.New(ctx, configURL)
	if err != nil {
		log.Fatalf("Failed to connect to config DB: %v", err)
	}
	defer configPool.Close()
	log.Println("[OK] Connected to pgwatch config DB")

	for i := 1; i <= tenantCount; i++ {
		dbName := fmt.Sprintf("tenant%d_db", i)

		// CREATE DATABASE cannot run inside a transaction block.
		_, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", dbName))
		if err != nil {
			if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "42P04" {
				log.Printf("[SKIP] Database %s already exists", dbName)
			} else {
				log.Fatalf("[FAIL] Could not create database %s: %v", dbName, err)
			}
		} else {
			log.Printf("[OK] Created database %s", dbName)
		}
	}

	// Parse the superuser URL so we can swap the database path for each tenant.
	baseURL, err := url.Parse(superuserURL)
	if err != nil {
		log.Fatalf("Failed to parse POSTGRES_SUPERUSER_URL: %v", err)
	}

	for i := 1; i <= tenantCount; i++ {
		dbName := fmt.Sprintf("tenant%d_db", i)

		tenantURL := *baseURL
		tenantURL.Path = "/" + dbName
		dsn := tenantURL.String()

		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			log.Fatalf("[FAIL] Could not connect to %s: %v", dbName, err)
		}

		if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
			pool.Close()
			log.Fatalf("[FAIL] pg_stat_statements on %s: %v", dbName, err)
		}
		log.Printf("[OK] pg_stat_statements enabled on %s", dbName)

		if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS hypopg"); err != nil {
			log.Printf("[WARN] hypopg not available on %s (install the OS package to enable): %v", dbName, err)
		} else {
			log.Printf("[OK] hypopg enabled on %s", dbName)
		}

		pool.Close()
	}

	// Register each tenant as a monitored source in pgwatch.
	// The connstr host is "postgres" — the Docker-internal DNS name that
	// the pgwatch container uses to reach the PostgreSQL server.
	const upsertSQL = `
		INSERT INTO pgwatch.source (name, connstr, preset_config, is_enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO NOTHING`

	for i := 1; i <= tenantCount; i++ {
		dbName := fmt.Sprintf("tenant%d_db", i)
		connstr := fmt.Sprintf("postgresql://postgres:postgres@postgres:5432/%s?sslmode=disable", dbName)

		tag, err := configPool.Exec(ctx, upsertSQL, dbName, connstr, "basic", true)
		if err != nil {
			log.Fatalf("[FAIL] Could not register %s in pgwatch.source: %v", dbName, err)
		}
		if tag.RowsAffected() == 0 {
			log.Printf("[SKIP] %s already registered in pgwatch.source", dbName)
		} else {
			log.Printf("[OK] Registered %s in pgwatch.source (preset=basic, enabled=true)", dbName)
		}
	}

	log.Println("\n=== Multi-tenant environment setup complete ===")
	log.Printf("  Databases created  : %d (tenant1_db … tenant%d_db)\n", tenantCount, tenantCount)
	log.Printf("  Extensions enabled : pg_stat_statements, hypopg\n")
	log.Printf("  pgwatch sources    : registered with Docker-internal host 'postgres'\n")
}
