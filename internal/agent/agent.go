// Package agent implements the agentic loop that connects an LLM provider to
// a set of tools gated by a security sandbox. Each call to [Agent.Run]
// sends a user prompt, lets the model invoke tools iteratively, and returns
// the final natural-language answer.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/RafayKhattak/pgcopilot/internal/provider"
	"github.com/RafayKhattak/pgcopilot/internal/sandbox"
	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// maxIterations caps the number of LLM round-trips in a single Run call to
// prevent runaway loops (e.g. the model repeatedly calling tools that return
// errors it cannot recover from).
const maxIterations = 10

// Agent orchestrates a multi-turn conversation between the LLM and a set of
// tools, with every tool invocation passing through the sandbox.
type Agent struct {
	llm          provider.Provider
	sb           *sandbox.Sandbox
	tools        []tool.Tool
	toolIndex    map[string]tool.Tool
	conversation []provider.Message
	systemPrompt string
}

// NewAgent creates an Agent wired to the given provider, sandbox, and tools.
// The system prompt is prepended to every LLM request as a RoleSystem message.
func NewAgent(
	llm provider.Provider,
	sb *sandbox.Sandbox,
	tools []tool.Tool,
	sysPrompt string,
) *Agent {
	idx := make(map[string]tool.Tool, len(tools))
	for _, t := range tools {
		idx[t.Name()] = t
	}
	return &Agent{
		llm:          llm,
		sb:           sb,
		tools:        tools,
		toolIndex:    idx,
		systemPrompt: sysPrompt,
	}
}

// Run executes the full agentic loop for a single user prompt:
//
//  1. Append the user message to the conversation.
//  2. Send the conversation + tool definitions to the LLM.
//  3. If the LLM returns tool calls, execute each one through the sandbox
//     and feed the results back as RoleTool messages.
//  4. Repeat until the LLM responds with plain text (no tool calls) or the
//     iteration limit is reached.
func (a *Agent) Run(ctx context.Context, userPrompt string) (string, error) {
	a.conversation = append(a.conversation, provider.Message{
		Role:    provider.RoleUser,
		Content: userPrompt,
	})

	toolDefs := a.buildToolDefs()

	for i := range maxIterations {
		req := &provider.Request{
			Messages: a.buildMessages(),
			Tools:    toolDefs,
		}

		resp, err := a.llm.Complete(ctx, req)
		if err != nil {
			return "", fmt.Errorf("agent: LLM call failed on iteration %d: %w", i, err)
		}

		a.conversation = append(a.conversation, resp.Message)
		log.Printf("[agent] iteration %d  model=%s  tool_calls=%d  usage=%+v",
			i, a.llm.ModelID(), len(resp.Message.ToolCalls), resp.Usage)

		if len(resp.Message.ToolCalls) == 0 {
			return resp.Message.Content, nil
		}

		a.handleToolCalls(ctx, resp.Message.ToolCalls)
	}

	return "", fmt.Errorf("agent: reached maximum iterations (%d) without a final answer", maxIterations)
}

// handleToolCalls executes every tool call through the sandbox and appends
// the results (or errors) to the conversation as RoleTool messages.
func (a *Agent) handleToolCalls(ctx context.Context, calls []provider.ToolCall) {
	for _, tc := range calls {
		result := a.executeSingleTool(ctx, tc)
		a.conversation = append(a.conversation, provider.Message{
			Role:       provider.RoleTool,
			Content:    result,
			ToolCallID: tc.ID,
		})
	}
}

// executeSingleTool looks up a tool by name, runs it through the sandbox,
// and returns the result string. Errors are returned as descriptive strings
// rather than Go errors so the LLM can see and react to them.
func (a *Agent) executeSingleTool(ctx context.Context, tc provider.ToolCall) string {
	t, ok := a.toolIndex[tc.Name]
	if !ok {
		msg := fmt.Sprintf("Error: tool %q not found. Available tools: %s", tc.Name, a.toolNames())
		log.Printf("[agent] %s", msg)
		return msg
	}

	log.Printf("[agent] executing tool %q (id=%s)", tc.Name, tc.ID)

	result, err := a.sb.Execute(ctx, t, json.RawMessage(tc.Arguments))
	if err != nil {
		msg := fmt.Sprintf("Error executing %q: %s", tc.Name, err)
		log.Printf("[agent] %s", msg)
		return msg
	}

	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildMessages constructs the full message list for an LLM request:
// system prompt first, then the accumulated conversation.
func (a *Agent) buildMessages() []provider.Message {
	msgs := make([]provider.Message, 0, 1+len(a.conversation))
	if a.systemPrompt != "" {
		msgs = append(msgs, provider.Message{
			Role:    provider.RoleSystem,
			Content: a.systemPrompt,
		})
	}
	msgs = append(msgs, a.conversation...)
	return msgs
}

// buildToolDefs converts the agent's tool slice into the provider-agnostic
// ToolDefinition slice that the LLM request expects.
func (a *Agent) buildToolDefs() []provider.ToolDefinition {
	defs := make([]provider.ToolDefinition, 0, len(a.tools))
	for _, t := range a.tools {
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

// toolNames returns a comma-separated list of registered tool names for use
// in error messages.
func (a *Agent) toolNames() string {
	if len(a.tools) == 0 {
		return "(none)"
	}
	names := ""
	for i, t := range a.tools {
		if i > 0 {
			names += ", "
		}
		names += t.Name()
	}
	return names
}
