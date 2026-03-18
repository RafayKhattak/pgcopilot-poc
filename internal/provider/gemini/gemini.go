// Package gemini implements [provider.Provider] for Google's Gemini models
// using the official google.golang.org/genai SDK.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/RafayKhattak/pgcopilot/internal/provider"
)

// Default model used when the caller does not specify one.
const DefaultModel = "gemini-2.0-flash"

// GeminiProvider wraps the Google GenAI client and satisfies [provider.Provider].
type GeminiProvider struct {
	client *genai.Client
	model  string
}

// NewGeminiProvider constructs a [GeminiProvider]. It is also the
// [provider.Factory] registered under the name "gemini".
func NewGeminiProvider(apiKey, model string) (provider.Provider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: API key must not be empty")
	}
	if model == "" {
		model = DefaultModel
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: creating client: %w", err)
	}

	return &GeminiProvider{client: client, model: model}, nil
}

func init() {
	provider.Register("gemini", NewGeminiProvider)
}

// ModelID returns the Gemini model identifier (e.g. "gemini-2.0-flash").
func (g *GeminiProvider) ModelID() string { return g.model }

// Complete maps the provider-agnostic [provider.Request] to the Gemini SDK,
// executes the call, and maps the response back.
func (g *GeminiProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	contents, config, err := buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: building request: %w", err)
	}

	resp, err := g.client.Models.GenerateContent(ctx, g.model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("gemini: GenerateContent: %w", err)
	}

	return parseResponse(resp)
}

// ---------------------------------------------------------------------------
// Request mapping
// ---------------------------------------------------------------------------

// buildRequest converts our agnostic types into the Gemini SDK types.
func buildRequest(req *provider.Request) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	config := &genai.GenerateContentConfig{}

	if req.Temperature != nil {
		t := float32(*req.Temperature)
		config.Temperature = &t
	}
	if req.MaxTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxTokens)
	}

	// Extract system instruction from the message list.
	config.SystemInstruction = extractSystemInstruction(req.Messages)

	// Map tool definitions → Gemini FunctionDeclarations.
	if len(req.Tools) > 0 {
		decls, err := convertToolDefs(req.Tools)
		if err != nil {
			return nil, nil, err
		}
		config.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	// Map conversation messages (excluding system) → Gemini Contents.
	contents, err := convertMessages(req.Messages)
	if err != nil {
		return nil, nil, err
	}

	return contents, config, nil
}

// extractSystemInstruction pulls all system messages, concatenates their
// content, and returns a single Gemini Content for the system instruction.
func extractSystemInstruction(msgs []provider.Message) *genai.Content {
	var parts []string
	for _, m := range msgs {
		if m.Role == provider.RoleSystem {
			parts = append(parts, m.Content)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &genai.Content{
		Parts: []*genai.Part{{Text: strings.Join(parts, "\n")}},
		Role:  "user",
	}
}

// convertMessages maps our Message slice into Gemini Content objects,
// skipping system messages (handled separately).
func convertMessages(msgs []provider.Message) ([]*genai.Content, error) {
	var contents []*genai.Content
	for _, m := range msgs {
		if m.Role == provider.RoleSystem {
			continue
		}

		c, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		contents = append(contents, c)
	}
	return contents, nil
}

// convertMessage maps a single agnostic Message to a Gemini Content.
func convertMessage(m provider.Message) (*genai.Content, error) {
	c := &genai.Content{Role: mapRole(m.Role)}

	switch m.Role {
	case provider.RoleTool:
		// Tool result → FunctionResponse part.
		resp, err := toolContentToMap(m.Content)
		if err != nil {
			return nil, fmt.Errorf("parsing tool response for call %q: %w", m.ToolCallID, err)
		}
		c.Parts = []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       m.ToolCallID,
				Name:     resp.name,
				Response: resp.data,
			},
		}}

	case provider.RoleAssistant:
		// The assistant turn may contain text, tool calls, or both.
		if m.Content != "" {
			c.Parts = append(c.Parts, &genai.Part{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			args, err := argsStringToMap(tc.Arguments)
			if err != nil {
				return nil, fmt.Errorf("parsing tool-call args for %q: %w", tc.Name, err)
			}
			c.Parts = append(c.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Name,
					Args: args,
				},
			})
		}

	default:
		// User or any other role: plain text.
		c.Parts = []*genai.Part{{Text: m.Content}}
	}

	return c, nil
}

// mapRole translates our Role constants to Gemini role strings.
func mapRole(r provider.Role) string {
	switch r {
	case provider.RoleAssistant:
		return "model"
	case provider.RoleTool:
		return "user"
	default:
		return "user"
	}
}

// convertToolDefs maps our ToolDefinition slice to Gemini FunctionDeclarations.
func convertToolDefs(defs []provider.ToolDefinition) ([]*genai.FunctionDeclaration, error) {
	decls := make([]*genai.FunctionDeclaration, 0, len(defs))
	for _, d := range defs {
		var schema any
		if len(d.Parameters) > 0 {
			if err := json.Unmarshal(d.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("unmarshalling parameters for tool %q: %w", d.Name, err)
			}
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 d.Name,
			Description:          d.Description,
			ParametersJsonSchema: schema,
		})
	}
	return decls, nil
}

// ---------------------------------------------------------------------------
// Response mapping
// ---------------------------------------------------------------------------

// parseResponse converts a Gemini GenerateContentResponse back to our
// provider-agnostic Response.
func parseResponse(resp *genai.GenerateContentResponse) (*provider.Response, error) {
	if resp == nil || len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: empty response (no candidates)")
	}

	candidate := resp.Candidates[0]
	msg := provider.Message{Role: provider.RoleAssistant}

	if candidate.Content != nil {
		for _, p := range candidate.Content.Parts {
			if p.Text != "" {
				msg.Content += p.Text
			}
			if p.FunctionCall != nil {
				argsJSON, err := json.Marshal(p.FunctionCall.Args)
				if err != nil {
					return nil, fmt.Errorf("gemini: marshalling function-call args: %w", err)
				}
				msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
					ID:        p.FunctionCall.ID,
					Name:      p.FunctionCall.Name,
					Arguments: string(argsJSON),
				})
			}
		}
	}

	usage := provider.Usage{}
	if m := resp.UsageMetadata; m != nil {
		usage.PromptTokens = int(m.PromptTokenCount)
		usage.CompletionTokens = int(m.CandidatesTokenCount)
		usage.TotalTokens = int(m.TotalTokenCount)
	}

	return &provider.Response{Message: msg, Usage: usage}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// toolResult holds the parsed content of a tool-response message.
type toolResult struct {
	name string
	data map[string]any
}

// toolContentToMap parses the tool message content. It expects either a JSON
// object with an optional "name" key, or falls back to wrapping plain text
// in {"output": content}.
func toolContentToMap(content string) (toolResult, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(content), &raw); err == nil {
		name, _ := raw["name"].(string)
		delete(raw, "name")
		return toolResult{name: name, data: raw}, nil
	}

	return toolResult{data: map[string]any{"output": content}}, nil
}

// argsStringToMap deserialises a JSON-encoded arguments string.
func argsStringToMap(s string) (map[string]any, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}
