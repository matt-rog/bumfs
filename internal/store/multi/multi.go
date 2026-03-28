package multi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/matt-rog/bumfs/internal/store"
)

// Connector routes chunks across multiple backends, tracking which backend
// holds each chunk via a JSON-persisted index.
type Connector struct {
	backends []store.StorageConnector
	mu       sync.RWMutex
	index    map[string]string // chunkID → backend name
	dbPath   string
}

var _ store.StorageConnector = (*Connector)(nil)

// New creates a multi-backend connector. backends must not be empty.
// dbPath is the path to the JSON index file for crash recovery.
func New(backends []store.StorageConnector, dbPath string) (*Connector, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("multi: no backends provided")
	}
	c := &Connector{
		backends: backends,
		index:    make(map[string]string),
		dbPath:   dbPath,
	}
	if err := c.loadIndex(); err != nil {
		return nil, fmt.Errorf("multi: load index: %w", err)
	}
	return c, nil
}

func (c *Connector) Name() string { return "multi" }

// Write stores a chunk on the backend with the most free capacity.
func (c *Connector) Write(ctx context.Context, id string, data []byte) error {
	backend := c.pickBackend()
	if err := backend.Write(ctx, id, data); err != nil {
		return err
	}

	c.mu.Lock()
	c.index[id] = backend.Name()
	c.mu.Unlock()

	return c.saveIndex()
}

// Read retrieves a chunk from the backend that holds it.
func (c *Connector) Read(ctx context.Context, id string) ([]byte, error) {
	backend, err := c.lookup(id)
	if err != nil {
		return nil, err
	}
	return backend.Read(ctx, id)
}

// Delete removes a chunk from the backend that holds it.
func (c *Connector) Delete(ctx context.Context, id string) error {
	backend, err := c.lookup(id)
	if err != nil {
		// Not in index — nothing to delete
		return nil
	}

	if err := backend.Delete(ctx, id); err != nil {
		return err
	}

	c.mu.Lock()
	delete(c.index, id)
	c.mu.Unlock()

	return c.saveIndex()
}

// Capacity returns the aggregate capacity across all backends.
func (c *Connector) Capacity() (total, used, free uint64) {
	for _, b := range c.backends {
		t, u, f := b.Capacity()
		total += t
		used += u
		free += f
	}
	return
}

// HealthCheck checks all backends and returns the first error encountered.
func (c *Connector) HealthCheck(ctx context.Context) error {
	for _, b := range c.backends {
		if err := b.HealthCheck(ctx); err != nil {
			return fmt.Errorf("multi healthcheck %s: %w", b.Name(), err)
		}
	}
	return nil
}

// pickBackend selects the backend with the most free space.
func (c *Connector) pickBackend() store.StorageConnector {
	type candidate struct {
		backend store.StorageConnector
		free    uint64
		idx     int // original order for stable sorting
	}
	candidates := make([]candidate, len(c.backends))
	for i, b := range c.backends {
		_, _, f := b.Capacity()
		candidates[i] = candidate{backend: b, free: f, idx: i}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].free != candidates[j].free {
			return candidates[i].free > candidates[j].free
		}
		return candidates[i].idx < candidates[j].idx
	})
	return candidates[0].backend
}

// lookup finds the backend holding a chunk by its index entry.
func (c *Connector) lookup(id string) (store.StorageConnector, error) {
	c.mu.RLock()
	name, ok := c.index[id]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("multi: chunk %s not in index", id)
	}

	for _, b := range c.backends {
		if b.Name() == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("multi: backend %q for chunk %s not found", name, id)
}

func (c *Connector) loadIndex() error {
	data, err := os.ReadFile(c.dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Unmarshal(data, &c.index)
}

func (c *Connector) saveIndex() error {
	c.mu.RLock()
	data, err := json.Marshal(c.index)
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(c.dbPath, data, 0600)
}
