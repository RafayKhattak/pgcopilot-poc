// Package groq implements [provider.Provider] for Groq's inference API
// using the OpenAI-compatible client from github.com/sashabaranov/go-openai.
package groq

import (
	"context"
	"encoding/json"
	"fmt"

	openai "github.com/sashabaranov/go-openai"

	"github.com/RafayKhattak/pgcopilot/internal/provider"
)

const (
	DefaultModel = "llama-3.3-70b-versatile"
	groqBaseURL  = "https://api.groq.com/openai/v1"
)

// GroqProvider wraps the OpenAI-compatible client pointed at Groq's API
// and satisfies [provider.Provider].
type GroqProvider struct {
	client *openai.Client
	model  string
}

// NewGroqProvider constructs a [GroqProvider]. It is also the
// [provider.Factory] registered under the name "groq".
func NewGroqProvider(apiKey, model string) (provider.Provider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("groq: API key must not be empty")
	}
	if model == "" {
		model = DefaultModel
	}

	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = groqBaseURL

	return &GroqProvider{
		client: openai.NewClientWithConfig(cfg),
		model:  model,
	}, nil
}

func init() {
	provider.Register("groq", NewGroqProvider)
}

// ModelID returns the model identifier (e.g. "llama3-70b-8192").
func (g *GroqProvider) ModelID() string { return g.model }

// Complete maps the provider-agnostic [provider.Request] to the OpenAI
// ChatCompletion format, sends it to Groq, and maps the response back.
func (g *GroqProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	oaiReq, err := buildRequest(g.model, req)
	if err != nil {
		return nil, fmt.Errorf("groq: building request: %w", err)
	}

	resp, err := g.client.CreateChatCompletion(ctx, oaiReq)
	if err != nil {
		return nil, fmt.Errorf("groq: CreateChatCompletion: %w", err)
	}

	return parseResponse(resp)
}

// ---------------------------------------------------------------------------
// Request mapping
// ---------------------------------------------------------------------------

func buildRequest(model string, req *provider.Request) (openai.ChatCompletionRequest, error) {
	msgs := make([]openai.ChatCompletionMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, convertMessage(m))
	}

	oaiReq := openai.ChatCompletionRequest{
		Model:    model,
		Messages: msgs,
	}

	if req.Temperature != nil {
		oaiReq.Temperature = float32(*req.Temperature)
	}
	if req.MaxTokens > 0 {
		oaiReq.MaxTokens = req.MaxTokens
	}

	if len(req.Tools) > 0 {
		tools, err := convertToolDefs(req.Tools)
		if err != nil {
			return openai.ChatCompletionRequest{}, err
		}
		oaiReq.Tools = tools
	}

	return oaiReq, nil
}

// convertMessage maps a single provider.Message to the OpenAI struct.
func convertMessage(m provider.Message) openai.ChatCompletionMessage {
	msg := openai.ChatCompletionMessage{
		Content: m.Content,
	}

	switch m.Role {
	case provider.RoleSystem:
		msg.Role = openai.ChatMessageRoleSystem
	case provider.RoleUser:
		msg.Role = openai.ChatMessageRoleUser
	case provider.RoleAssistant:
		msg.Role = openai.ChatMessageRoleAssistant
		msg.ToolCalls = convertToolCallsOut(m.ToolCalls)
	case provider.RoleTool:
		msg.Role = openai.ChatMessageRoleTool
		msg.ToolCallID = m.ToolCallID
	default:
		msg.Role = openai.ChatMessageRoleUser
	}

	return msg
}

// convertToolCallsOut maps our outgoing tool calls (from a prior assistant
// message being replayed in conversation history) to OpenAI ToolCalls.
func convertToolCallsOut(calls []provider.ToolCall) []openai.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]openai.ToolCall, len(calls))
	for i, tc := range calls {
		out[i] = openai.ToolCall{
			ID:   tc.ID,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		}
	}
	return out
}

// convertToolDefs maps our ToolDefinition slice to OpenAI Tool objects.
func convertToolDefs(defs []provider.ToolDefinition) ([]openai.Tool, error) {
	tools := make([]openai.Tool, 0, len(defs))
	for _, d := range defs {
		var params any
		if len(d.Parameters) > 0 {
			if err := json.Unmarshal(d.Parameters, &params); err != nil {
				return nil, fmt.Errorf("unmarshalling parameters for tool %q: %w", d.Name, err)
			}
		}
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  params,
			},
		})
	}
	return tools, nil
}

// ---------------------------------------------------------------------------
// Response mapping
// ---------------------------------------------------------------------------

func parseResponse(resp openai.ChatCompletionResponse) (*provider.Response, error) {
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("groq: empty response (no choices)")
	}

	choice := resp.Choices[0]
	msg := provider.Message{
		Role:    provider.RoleAssistant,
		Content: choice.Message.Content,
	}

	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	usage := provider.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return &provider.Response{Message: msg, Usage: usage}, nil
}
