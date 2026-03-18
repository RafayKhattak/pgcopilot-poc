package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/RafayKhattak/pgcopilot/internal/agent"
	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/provider"
	_ "github.com/RafayKhattak/pgcopilot/internal/provider/groq" // register "groq" factory
	"github.com/RafayKhattak/pgcopilot/internal/sandbox"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
	"github.com/RafayKhattak/pgcopilot/internal/tool/metrics"
)

const watchSystemPrompt = `You are a proactive monitoring daemon. ` +
	`Analyze the provided metric trends. ` +
	`If everything is normal, reply with "STATUS: OK". ` +
	`If there is an anomaly, provide a detailed RCA.`

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Proactive continuous monitoring of a PostgreSQL system",
	Long: `The watch command starts a long-running loop that periodically
queries PostgreSQL system views and sends the metrics to the
configured LLM for analysis and recommendations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		interval, err := cmd.Flags().GetDuration("interval")
		if err != nil {
			return fmt.Errorf("invalid --interval value: %w", err)
		}
		sysID, err := cmd.Flags().GetString("sys-id")
		if err != nil {
			return fmt.Errorf("invalid --sys-id value: %w", err)
		}
		dbName, err := cmd.Flags().GetString("dbname")
		if err != nil {
			return fmt.Errorf("invalid --dbname value: %w", err)
		}

		if sysID == "" {
			return fmt.Errorf("--sys-id is required (run the discovery script to find it)")
		}
		if dbName == "" {
			return fmt.Errorf("--dbname is required (the pgwatch monitored database name)")
		}

		groqKey := viper.GetString("GROQ_API_KEY")
		if groqKey == "" {
			return fmt.Errorf("GROQ_API_KEY is not set; please add it to .env or export it")
		}
		metricsURL := viper.GetString("PGWATCH_METRICS_DB_URL")
		if metricsURL == "" {
			return fmt.Errorf("PGWATCH_METRICS_DB_URL is not set; please add it to .env or export it")
		}

		// Graceful shutdown: cancel on SIGINT / SIGTERM.
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		client, err := db.NewClient(ctx, metricsURL)
		if err != nil {
			return fmt.Errorf("failed to connect to pgwatch metrics database: %w", err)
		}
		defer client.Close()

		llm, err := provider.New("groq", groqKey, "")
		if err != nil {
			return fmt.Errorf("failed to initialise LLM provider: %w", err)
		}

		sb := sandbox.New(sandbox.ModeReadOnly)
		trendsTool := metrics.NewTrendsTool(client)
		hypopgTool := metrics.NewHypoPGTool(client)

		fmt.Printf("Proactive Watch Mode started for sys-id %s (db=%s) at interval %s\n", sysID, dbName, interval)
		fmt.Println("Press Ctrl+C to stop.")

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		tools := []tool.Tool{trendsTool, hypopgTool}

		// Run the first analysis immediately, then on every tick.
		runAnalysis(ctx, llm, sb, tools, dbName, sysID, interval)

		for {
			select {
			case <-ctx.Done():
				fmt.Println("\nShutting down proactive watcher...")
				return nil
			case <-ticker.C:
				runAnalysis(ctx, llm, sb, tools, dbName, sysID, interval)
			}
		}
	},
}

// runAnalysis constructs a fresh Agent (clean conversation), sends the
// proactive prompt, and prints the result. Errors are logged but never
// propagate — the daemon must survive transient failures.
func runAnalysis(
	ctx context.Context,
	llm provider.Provider,
	sb *sandbox.Sandbox,
	tools []tool.Tool,
	dbName, sysID string,
	interval time.Duration,
) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	log.Printf("[DAEMON] %s — Waking up to analyze db_stats...", ts)

	ag := agent.NewAgent(llm, sb, tools, watchSystemPrompt)

	prompt := fmt.Sprintf(
		"Please fetch and analyze the metric trends for 'db_stats' in the database '%s' "+
			"for the sys_id '%s' over the last '%s'. Are there any anomalies?",
		dbName, sysID, interval,
	)

	answer, err := ag.Run(ctx, prompt)
	if err != nil {
		log.Printf("[DAEMON] analysis failed: %v", err)
		return
	}

	fmt.Printf("\n[DAEMON REPORT @ %s]:\n%s\n\n", ts, answer)
}

func init() {
	watchCmd.Flags().Duration("interval", 5*time.Minute, "polling interval between analysis cycles")
	watchCmd.Flags().String("sys-id", "", "system identifier of the PostgreSQL cluster to monitor")
	watchCmd.Flags().String("dbname", "", "name of the monitored database in pgwatch")

	rootCmd.AddCommand(watchCmd)
}
