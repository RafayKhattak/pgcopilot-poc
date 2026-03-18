// Package provider defines a provider-agnostic interface for interacting with
// large language models. Concrete implementations (e.g. Gemini, OpenAI) satisfy
// the [Provider] interface so the rest of the application remains decoupled
// from any single vendor.
package provider

import (
	"context"
	"encoding/json"
)

// Role represents the sender of a [Message] in a conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSystem    Role = "system"
)

// ToolCall represents a function-call request emitted by the model.
type ToolCall struct {
	// ID is a unique identifier assigned by the model to correlate a call
	// with its corresponding tool-result message.
	ID string `json:"id"`

	// Name is the function name the model wants to invoke.
	Name string `json:"name"`

	// Arguments is the raw JSON object the model produced as input
	// parameters for the function.
	Arguments string `json:"arguments"`
}

// Message is a single turn in a conversation history.
type Message struct {
	Role      Role       `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID links a tool-result message back to the ToolCall it answers.
	// Only meaningful when Role == RoleTool.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolDefinition describes a tool the model may invoke. Parameters is a
// JSON Schema object that documents the expected arguments.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request encapsulates everything needed for a single LLM completion call.
type Request struct {
	// Messages is the ordered conversation history sent to the model.
	Messages []Message `json:"messages"`

	// Tools advertises the set of functions the model is allowed to call.
	Tools []ToolDefinition `json:"tools,omitempty"`

	// Temperature controls randomness (0 = deterministic, 1 = creative).
	// A nil value lets the provider use its own default.
	Temperature *float64 `json:"temperature,omitempty"`

	// MaxTokens caps the length of the model's reply.
	// Zero means no explicit limit.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// Usage reports token consumption for a single completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the provider-normalised result of a completion call.
type Response struct {
	Message Message `json:"message"`
	Usage   Usage   `json:"usage"`
}

// Provider is the central abstraction that every LLM backend must implement.
type Provider interface {
	// Complete sends a request to the model and returns a single response.
	// Implementations must respect context cancellation and deadlines.
	Complete(ctx context.Context, req *Request) (*Response, error)

	// ModelID returns the concrete model identifier (e.g. "gemini-2.0-flash").
	ModelID() string
}
