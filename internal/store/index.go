package store

import (
	"encoding/json"
	"os"
	"sync"
)

// Index is a thread-safe, JSON-persisted map.
// It replaces the per-backend loadIndex/saveIndex boilerplate.
type Index[V any] struct {
	mu     sync.RWMutex
	data   map[string]V
	dbPath string
}

// NewIndex creates or loads an Index from the given JSON file path.
// If the file does not exist, the index starts empty.
func NewIndex[V any](dbPath string) (*Index[V], error) {
	idx := &Index[V]{
		data:   make(map[string]V),
		dbPath: dbPath,
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &idx.data); err != nil {
		return nil, err
	}
	return idx, nil
}

// Get returns the value for key and whether it exists.
func (idx *Index[V]) Get(key string) (V, bool) {
	idx.mu.RLock()
	v, ok := idx.data[key]
	idx.mu.RUnlock()
	return v, ok
}

// Set stores a key-value pair.
func (idx *Index[V]) Set(key string, val V) {
	idx.mu.Lock()
	idx.data[key] = val
	idx.mu.Unlock()
}

// Delete removes a key.
func (idx *Index[V]) Delete(key string) {
	idx.mu.Lock()
	delete(idx.data, key)
	idx.mu.Unlock()
}

// Len returns the number of entries.
func (idx *Index[V]) Len() int {
	idx.mu.RLock()
	n := len(idx.data)
	idx.mu.RUnlock()
	return n
}

// Save persists the index to disk as JSON.
func (idx *Index[V]) Save() error {
	idx.mu.RLock()
	raw, err := json.Marshal(idx.data)
	idx.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(idx.dbPath, raw, 0600)
}
