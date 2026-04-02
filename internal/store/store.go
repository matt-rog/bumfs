package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/matt-rog/bumfs/internal/config"
)

// StorageConnector is the interface that all storage backends must implement.
type StorageConnector interface {
	Name() string
	Write(ctx context.Context, id string, data []byte) error
	Read(ctx context.Context, id string) ([]byte, error)
	Delete(ctx context.Context, id string) error
	Capacity() (total, used, free uint64)
	HealthCheck(ctx context.Context) error
}

// Factory creates a StorageConnector from a backend config and data directory.
type Factory func(cfg config.BackendConfig, dataDir string) (StorageConnector, error)

var (
	registryMu sync.Mutex
	registry   = map[string]Factory{}
)

// Register adds a backend factory under the given type name.
// Typically called from an init() function in each backend package.
func Register(typeName string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[typeName] = factory
}

// Create instantiates a backend by its registered type name.
func Create(typeName string, cfg config.BackendConfig, dataDir string) (StorageConnector, error) {
	registryMu.Lock()
	f, ok := registry[typeName]
	registryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("store: unknown backend type %q", typeName)
	}
	return f(cfg, dataDir)
}
