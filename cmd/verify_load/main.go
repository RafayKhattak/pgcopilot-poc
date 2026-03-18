package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	metricsURL := os.Getenv("PGWATCH_METRICS_DB_URL")
	if metricsURL == "" {
		log.Fatal("PGWATCH_METRICS_DB_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, metricsURL)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer pool.Close()

	const query = `
		SELECT COUNT(*),
		       MAX((data->>'xact_commit')::numeric),
		       MIN((data->>'xact_commit')::numeric),
		       MAX((data->>'blks_hit')::numeric),
		       MAX((data->>'tup_fetched')::numeric),
		       MAX((data->>'tup_updated')::numeric),
		       MAX((data->>'numbackends')::numeric)
		FROM   public.db_stats
		WHERE  dbname = 'my_test_db'
		  AND  data->>'sys_id' = '7618200670381232155'
		  AND  time >= NOW() - interval '5 minutes'`

	var (
		count                                       int64
		maxCommits, minCommits                      *float64
		maxBlksHit, maxTupFetched, maxTupUpdated    *float64
		maxBackends                                 *float64
	)
	if err := pool.QueryRow(ctx, query).Scan(
		&count, &maxCommits, &minCommits,
		&maxBlksHit, &maxTupFetched, &maxTupUpdated, &maxBackends,
	); err != nil {
		log.Fatalf("Query failed: %v", err)
	}

	fmt.Println("=== pgwatch Load Spike Verification ===")
	fmt.Printf("Time window       : last 5 minutes\n")
	fmt.Printf("Row count         : %d\n", count)

	fmtVal := func(label string, v *float64) {
		if v != nil {
			fmt.Printf("%-18s: %.0f\n", label, *v)
		} else {
			fmt.Printf("%-18s: <no data>\n", label)
		}
	}

	fmtVal("Max xact_commit", maxCommits)
	fmtVal("Min xact_commit", minCommits)
	fmtVal("Max blks_hit", maxBlksHit)
	fmtVal("Max tup_fetched", maxTupFetched)
	fmtVal("Max tup_updated", maxTupUpdated)
	fmtVal("Max numbackends", maxBackends)

	if maxCommits != nil && minCommits != nil {
		delta := *maxCommits - *minCommits
		fmt.Printf("\nCommit delta      : %.0f transactions across the window\n", delta)
	}

	if count > 0 && maxCommits != nil && *maxCommits > 1000 {
		fmt.Println("\nVERDICT: SPIKE CONFIRMED — pgwatch captured the load generation event.")
	} else if count > 0 {
		fmt.Println("\nVERDICT: Data present but spike is modest. pgwatch may need more scrape time.")
	} else {
		fmt.Println("\nVERDICT: No data found in the last 5 minutes. pgwatch may not have scraped yet.")
	}
}
