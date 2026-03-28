package main

import (
	"fmt"
	"strings"

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

const systemPrompt = `You are pgcopilot, an expert PostgreSQL database AI assistant. You analyze pgwatch metrics to diagnose database performance issues. You have access to tools to fetch metric data. ALWAYS use the tools provided before answering performance questions.

You must format your final response strictly using the following Markdown structure:

**1. Evidence:** [State the hard data and metric numbers you retrieved from the tools]
**2. Likely Root Cause:** [State your diagnosis based on the evidence]
**3. Confidence Score:** [Give a percentage 0-100% of how confident you are in this diagnosis]
**4. Missing Context:** [State what data you cannot see that would increase your confidence, e.g., application logs, OS-level disk IO, etc.]`

var askCmd = &cobra.Command{
	Use:   "ask [prompt]",
	Short: "Reactive single-shot analysis of a user prompt",
	Long: `The ask command sends a single natural-language prompt to the
configured LLM and returns an analysis based on the current
PostgreSQL state.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := strings.Join(args, " ")
		fmt.Printf("Reactive Ask Mode: %s\n\n", prompt)

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

		metricsClient, err := db.NewClient(cmd.Context(), metricsURL)
		if err != nil {
			return fmt.Errorf("failed to connect to pgwatch metrics database: %w", err)
		}
		defer metricsClient.Close()

		configClient, err := db.NewClient(cmd.Context(), configURL)
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
		ag := agent.NewAgent(llm, sb, []tool.Tool{trendsTool, hypopgTool, activeQTool}, systemPrompt)

		fmt.Println("Thinking...")
		answer, err := ag.Run(cmd.Context(), prompt)
		if err != nil {
			return fmt.Errorf("agent error: %w", err)
		}

		fmt.Printf("\n%s\n", answer)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(askCmd)
}
