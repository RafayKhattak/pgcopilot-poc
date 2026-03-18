package provider

import (
	"fmt"
	"sync"
)

// Factory is a constructor that creates a [Provider] given an API key and model
// identifier. Each concrete package (e.g. gemini, openai) registers one of
// these via [Register].
type Factory func(apiKey, model string) (Provider, error)

var (
	mu       sync.RWMutex
	registry = make(map[string]Factory)
)

// Register makes a provider factory available under the given name.
// It is intended to be called from an init() function in each concrete
// provider package.
//
//	func init() {
//	    provider.Register("gemini", NewGeminiProvider)
//	}
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("provider: duplicate registration for %q", name))
	}
	registry[name] = f
}

// New looks up a registered provider by name and constructs it with the
// supplied credentials. It returns an error if the name is unknown or the
// factory itself fails.
func New(name, apiKey, model string) (Provider, error) {
	mu.RLock()
	f, ok := registry[name]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("provider: unknown provider %q (registered: %v)", name, registeredNames())
	}
	return f(apiKey, model)
}

// registeredNames returns a sorted-ish slice of all registered provider names
// for use in error messages.
func registeredNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	return names
}
