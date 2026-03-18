// Package sandbox enforces permission checks before any agent-invoked tool
// is allowed to execute. It acts as the single security chokepoint between
// the LLM's tool-call decisions and the real side-effects those tools
// perform, ensuring the operator's chosen safety policy is never bypassed.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/RafayKhattak/pgcopilot/internal/tool"
)

// Mode controls how the sandbox handles tools whose permission level
// exceeds [tool.PermissionReadOnly].
type Mode int

const (
	// ModeReadOnly unconditionally blocks any tool that is not read-only.
	ModeReadOnly Mode = iota

	// ModeConfirm is reserved for a future interactive flow where the
	// operator is prompted for approval before a write/dangerous tool
	// runs. Currently it behaves identically to ModeReadOnly.
	ModeConfirm
)

func (m Mode) String() string {
	switch m {
	case ModeReadOnly:
		return "read-only"
	case ModeConfirm:
		return "confirm"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Sandbox gates tool execution according to a configurable [Mode].
type Sandbox struct {
	mode Mode
}

// New creates a Sandbox operating in the given mode.
func New(mode Mode) *Sandbox {
	return &Sandbox{mode: mode}
}

// Execute checks the tool's declared permission against the sandbox policy
// and, if allowed, delegates to [tool.Tool.Execute].
//
// In ModeReadOnly any tool whose Permission() > PermissionReadOnly is
// rejected with a descriptive error that the LLM can relay to the user.
func (s *Sandbox) Execute(ctx context.Context, t tool.Tool, args json.RawMessage) (string, error) {
	perm := t.Permission()

	switch s.mode {
	case ModeReadOnly:
		if perm > tool.PermissionReadOnly {
			return "", fmt.Errorf(
				"sandbox: blocked %q (permission %s) — current policy is %s",
				t.Name(), perm, s.mode,
			)
		}

	case ModeConfirm:
		// Future: prompt the operator for confirmation when perm > ReadOnly.
		// Until that flow is implemented, fall back to read-only behaviour
		// to avoid accidental mutations.
		if perm > tool.PermissionReadOnly {
			return "", fmt.Errorf(
				"sandbox: blocked %q (permission %s) — confirm mode not yet implemented, defaulting to %s",
				t.Name(), perm, ModeReadOnly,
			)
		}
	}

	return t.Execute(ctx, args)
}
