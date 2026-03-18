// Package tool defines the interface every agent-callable tool must satisfy
// and provides a registry for discovering tools by name at runtime.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Permission classifies the side-effect level of a tool so the security
// sandbox can gate execution without understanding each tool's internals.
type Permission int

const (
	// PermissionReadOnly indicates the tool only reads data — no mutations.
	PermissionReadOnly Permission = iota

	// PermissionWrite indicates the tool mutates state (e.g. CREATE INDEX).
	PermissionWrite

	// PermissionDangerous indicates the tool performs irreversible or
	// high-impact operations (e.g. DROP TABLE, VACUUM FULL).
	PermissionDangerous
)

func (p Permission) String() string {
	switch p {
	case PermissionReadOnly:
		return "read-only"
	case PermissionWrite:
		return "write"
	case PermissionDangerous:
		return "dangerous"
	default:
		return fmt.Sprintf("Permission(%d)", int(p))
	}
}

// Tool is the contract every agent-callable function must implement.
// The LLM sees Name, Description, and Parameters (a JSON Schema object)
// when deciding which tool to invoke; the sandbox inspects Permission
// before allowing Execute to run.
type Tool interface {
	// Name returns a unique, snake_case identifier (e.g. "fetch_metric_data").
	Name() string

	// Description is a concise explanation shown to the LLM so it can
	// decide when to call this tool.
	Description() string

	// Parameters returns a JSON Schema document describing the expected
	// arguments. This is passed directly to the LLM as a function schema.
	Parameters() json.RawMessage

	// Permission declares the side-effect level of this tool.
	Permission() Permission

	// Execute runs the tool with the given JSON arguments and returns a
	// human-readable (and LLM-readable) result string.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

var (
	mu       sync.RWMutex
	registry = make(map[string]Tool)
)

// Register adds a tool to the global registry. It panics on duplicate names
// so wiring errors surface immediately at startup.
func Register(t Tool) {
	mu.Lock()
	defer mu.Unlock()

	name := t.Name()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("tool: duplicate registration for %q", name))
	}
	registry[name] = t
}

// Get returns the tool registered under name, or nil if none exists.
func Get(name string) Tool {
	mu.RLock()
	defer mu.RUnlock()
	return registry[name]
}

// All returns a snapshot of every registered tool, keyed by name.
func All() map[string]Tool {
	mu.RLock()
	defer mu.RUnlock()

	snap := make(map[string]Tool, len(registry))
	for k, v := range registry {
		snap[k] = v
	}
	return snap
}
