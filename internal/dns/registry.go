package dns

import (
	"fmt"
	"sync"

	"github.com/go-logr/logr"
)

// Factory is a constructor function that providers register to create themselves.
type Factory func(log logr.Logger, settings map[string]string) (Provider, error)

var (
	mu        sync.Mutex
	factories = make(map[string]Factory)
)

// Register is called by provider packages in their init() to self-register.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("dns: provider %q already registered", name))
	}
	factories[name] = f
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	fmt.Printf("registered DNS providers: %v\n", names)
}

// NewProvider looks up the named provider in the registry and creates it.
func NewProvider(name string, log logr.Logger, settings map[string]string) (Provider, error) {
	mu.Lock()
	f, ok := factories[name]
	mu.Unlock()
	if !ok {
		names := make([]string, 0, len(factories))
		for n := range factories {
			names = append(names, n)
		}
		return nil, fmt.Errorf("unsupported DNS provider: %q (registered: %v)", name, names)
	}
	return f(log, settings)
}
