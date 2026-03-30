package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "pgcopilot",
	Short: "AI copilot for PostgreSQL observability and tuning",
	Long: `pgcopilot is a CLI tool that uses LLMs to monitor, diagnose,
and recommend optimizations for PostgreSQL databases.

It operates in three modes:
  • ask   – reactive single-shot analysis of a user prompt
  • watch – proactive continuous monitoring at a configurable interval
  • mcp   – Model Context Protocol server over stdio for IDE integration`,
}

// Execute runs the root command and exits on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
}

// initConfig wires Viper to read configuration from environment variables.
func initConfig() {
	viper.SetEnvPrefix("")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))

	// Bind the API keys so they are available via viper.GetString().
	for _, key := range []string{"GROQ_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "PGWATCH_DB_URL", "PGWATCH_METRICS_DB_URL"} {
		_ = viper.BindEnv(key)
	}
}
