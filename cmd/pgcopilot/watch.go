package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
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
	"github.com/RafayKhattak/pgcopilot/internal/tool/diagnostics"
	"github.com/RafayKhattak/pgcopilot/internal/tool/metrics"
)

const watchSystemPrompt = `You are pgcopilot, an expert PostgreSQL database AI assistant. You analyze pgwatch metrics to diagnose database performance issues. You have access to tools to fetch metric data. ALWAYS use the tools provided before answering performance questions.

You must format your final response strictly using the following Markdown structure:

**1. Evidence:** [State the hard data and metric numbers you retrieved from the tools]
**2. Likely Root Cause:** [State your diagnosis based on the evidence]
**3. Confidence Score:** [Give a percentage 0-100% of how confident you are in this diagnosis]
**4. Missing Context:** [State what data you cannot see that would increase your confidence, e.g., application logs, OS-level disk IO, etc.]`

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
		configURL := viper.GetString("PGWATCH_DB_URL")
		if configURL == "" {
			return fmt.Errorf("PGWATCH_DB_URL is not set; please add it to .env or export it")
		}

		// Graceful shutdown: cancel on SIGINT / SIGTERM.
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		metricsClient, err := db.NewClient(ctx, metricsURL)
		if err != nil {
			return fmt.Errorf("failed to connect to pgwatch metrics database: %w", err)
		}
		defer metricsClient.Close()

		configClient, err := db.NewClient(ctx, configURL)
		if err != nil {
			return fmt.Errorf("failed to connect to pgwatch config database: %w", err)
		}
		defer configClient.Close()

		llm, err := provider.New("groq", groqKey, "")
		if err != nil {
			return fmt.Errorf("failed to initialise LLM provider: %w", err)
		}

		sb := sandbox.New(sandbox.ModeReadOnly)
		trendsTool := metrics.NewTrendsTool(metricsClient)
		hypopgTool := metrics.NewHypoPGTool(metricsClient)
		activeQTool := diagnostics.NewActiveQueriesTool(configClient)

		webhookURL := viper.GetString("PGCOPILOT_WEBHOOK_URL")

		fmt.Printf("Proactive Watch Mode started for sys-id %s (db=%s) at interval %s\n", sysID, dbName, interval)
		if webhookURL != "" {
			fmt.Printf("Webhook alerts enabled → %s\n", webhookURL)
		}
		fmt.Println("Press Ctrl+C to stop.")

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		tools := []tool.Tool{trendsTool, hypopgTool, activeQTool}

		// Run the first analysis immediately, then on every tick.
		runAnalysis(ctx, llm, sb, tools, dbName, sysID, interval, webhookURL)

		for {
			select {
			case <-ctx.Done():
				fmt.Println("\nShutting down proactive watcher...")
				return nil
			case <-ticker.C:
				runAnalysis(ctx, llm, sb, tools, dbName, sysID, interval, webhookURL)
			}
		}
	},
}

// runAnalysis constructs a fresh Agent (clean conversation), sends the
// proactive prompt, and prints the result. If a webhook URL is configured
// and the response indicates an anomaly, an alert is dispatched.
// Errors are logged but never propagate — the daemon must survive transient failures.
func runAnalysis(
	ctx context.Context,
	llm provider.Provider,
	sb *sandbox.Sandbox,
	tools []tool.Tool,
	dbName, sysID string,
	interval time.Duration,
	webhookURL string,
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

	if webhookURL != "" && !strings.Contains(answer, "STATUS: OK") {
		if err := sendWebhookAlert(ctx, webhookURL, answer); err != nil {
			log.Printf("[DAEMON] webhook alert failed: %v", err)
		}
	}
}

// sendWebhookAlert POSTs a Slack-compatible JSON payload to the given URL.
// It enforces a 5-second timeout so a slow or unreachable endpoint never
// blocks the daemon loop.
func sendWebhookAlert(ctx context.Context, url, message string) error {
	payload, err := json.Marshal(map[string]string{"text": message})
	if err != nil {
		return fmt.Errorf("marshalling webhook payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending webhook request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}

	log.Printf("[DAEMON] webhook alert dispatched (HTTP %d)", resp.StatusCode)
	return nil
}

func init() {
	watchCmd.Flags().Duration("interval", 5*time.Minute, "polling interval between analysis cycles")
	watchCmd.Flags().String("sys-id", "", "system identifier of the PostgreSQL cluster to monitor")
	watchCmd.Flags().String("dbname", "", "name of the monitored database in pgwatch")
	watchCmd.Flags().String("webhook-url", "", "Slack/PagerDuty webhook URL for anomaly alerts")
	_ = viper.BindPFlag("PGCOPILOT_WEBHOOK_URL", watchCmd.Flags().Lookup("webhook-url"))

	rootCmd.AddCommand(watchCmd)
}
