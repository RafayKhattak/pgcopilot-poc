package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/RafayKhattak/pgcopilot/internal/db"
	"github.com/RafayKhattak/pgcopilot/internal/sandbox"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
	"github.com/RafayKhattak/pgcopilot/internal/tool/diagnostics"
	"github.com/RafayKhattak/pgcopilot/internal/tool/metrics"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP server over stdio for external AI clients",
	Long: `The mcp command starts a Model Context Protocol (MCP) server that
exposes pgcopilot's tools over standard I/O. External AI clients such
as Cursor and Claude Desktop can connect and invoke tools directly.

All tool calls pass through the read-only sandbox — the same security
boundary that protects the ask and watch commands.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		metricsURL := viper.GetString("PGWATCH_METRICS_DB_URL")
		if metricsURL == "" {
			return fmt.Errorf("PGWATCH_METRICS_DB_URL is not set; please add it to .env or export it")
		}
		configURL := viper.GetString("PGWATCH_DB_URL")
		if configURL == "" {
			return fmt.Errorf("PGWATCH_DB_URL is not set; please add it to .env or export it")
		}

		ctx := cmd.Context()

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

		sb := sandbox.New(sandbox.ModeReadOnly)

		tools := []tool.Tool{
			metrics.NewTrendsTool(metricsClient),
			metrics.NewHypoPGTool(configClient),
			diagnostics.NewActiveQueriesTool(configClient),
			diagnostics.NewActiveLocksTool(configClient),
		}

		s := server.NewMCPServer(
			"pgcopilot",
			"0.1.0",
			server.WithToolCapabilities(false),
			server.WithRecovery(),
		)

		for _, t := range tools {
			mcpTool, convErr := convertToMCPTool(t)
			if convErr != nil {
				return fmt.Errorf("registering tool %q: %w", t.Name(), convErr)
			}
			s.AddTool(mcpTool, makeMCPHandler(sb, t))
		}

		return server.ServeStdio(s)
	},
}

// convertToMCPTool maps our tool.Tool interface to the mcp-go Tool struct.
// It unmarshals the JSON Schema from tool.Parameters() into the MCP
// ToolInputSchema and propagates the read-only annotation from Permission().
func convertToMCPTool(t tool.Tool) (mcp.Tool, error) {
	var schema mcp.ToolInputSchema
	if err := json.Unmarshal(t.Parameters(), &schema); err != nil {
		return mcp.Tool{}, fmt.Errorf("parsing input schema: %w", err)
	}

	readOnly := t.Permission() == tool.PermissionReadOnly
	return mcp.Tool{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: schema,
		Annotations: mcp.ToolAnnotation{
			ReadOnlyHint:    mcp.ToBoolPtr(readOnly),
			DestructiveHint: mcp.ToBoolPtr(false),
		},
	}, nil
}

// makeMCPHandler returns an MCP handler that bridges the MCP request into
// our sandbox → tool.Execute pipeline. Errors are returned as MCP tool
// errors so the calling AI client sees them as tool output, not crashes.
func makeMCPHandler(sb *sandbox.Sandbox, t tool.Tool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rawArgs, err := json.Marshal(req.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid arguments: %v", err)), nil
		}

		result, err := sb.Execute(ctx, t, json.RawMessage(rawArgs))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(result), nil
	}
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
