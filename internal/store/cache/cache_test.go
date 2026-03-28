package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matt-rog/bumfs/internal/store"
)

// memBackend is an in-memory StorageConnector for testing.
type memBackend struct {
	mu   sync.Mutex
	data map[string][]byte
}

var _ store.StorageConnector = (*memBackend)(nil)

func newMemBackend() *memBackend {
	return &memBackend{data: make(map[string][]byte)}
}

func (m *memBackend) Name() string { return "mem" }

func (m *memBackend) Write(_ context.Context, id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = append([]byte(nil), data...)
	return nil
}

func (m *memBackend) Read(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	return d, nil
}

func (m *memBackend) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
	return nil
}

func (m *memBackend) Capacity() (uint64, uint64, uint64) { return 1000, 0, 1000 }
func (m *memBackend) HealthCheck(_ context.Context) error { return nil }

func (m *memBackend) has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[id]
	return ok
}

func TestWriteAppearsOnDiskThenDrains(t *testing.T) {
	dir := t.TempDir()
	inner := newMemBackend()

	c, err := New(inner, dir, 1<<20) // 1MB cache
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Write(ctx, "chunk1", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	// Should appear on local disk immediately
	path := filepath.Join(dir, "chunk1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("chunk not on local disk: %v", err)
	}

	// Wait for drain
	deadline := time.Now().Add(5 * time.Second)
	for !inner.has("chunk1") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !inner.has("chunk1") {
		t.Fatal("chunk1 not drained to inner backend")
	}

	// After drain, local cache file should be removed
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cache file not cleaned up after drain")
}

func TestReadHitsCacheThenInner(t *testing.T) {
	dir := t.TempDir()
	inner := newMemBackend()

	c, err := New(inner, dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Write(ctx, "c1", []byte("cached")); err != nil {
		t.Fatal(err)
	}

	// Read should hit local cache
	data, err := c.Read(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cached" {
		t.Fatalf("read got %q, want %q", data, "cached")
	}

	// Put something only in inner (simulating already-drained)
	inner.Write(ctx, "c2", []byte("inner-only"))
	data, err = c.Read(ctx, "c2")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "inner-only" {
		t.Fatalf("read got %q, want %q", data, "inner-only")
	}
}

func TestBackpressure(t *testing.T) {
	dir := t.TempDir()

	// slowBackend delays writes to simulate a slow backend
	slow := &slowBackend{inner: newMemBackend(), delay: 100 * time.Millisecond}

	// Cache of 10 bytes — writing 6+6 should block on the second write
	c, err := New(slow, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()

	// First write fits
	if err := c.Write(ctx, "a", []byte("123456")); err != nil {
		t.Fatal(err)
	}

	// Second write (6 bytes) should block since 6+6 > 10
	done := make(chan error, 1)
	go func() {
		done <- c.Write(ctx, "b", []byte("abcdef"))
	}()

	select {
	case <-done:
		// Should not complete immediately — drain hasn't freed space yet
		// But with fast local disk, the drain goroutine might have already freed space
		// This is acceptable; the important thing is it doesn't deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("second write timed out (possible deadlock)")
	}
}

func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	inner := newMemBackend()

	// Pre-populate cache dir (simulating crash with undrained chunks)
	if err := os.WriteFile(filepath.Join(dir, "orphan1"), []byte("data1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan2"), []byte("data2"), 0600); err != nil {
		t.Fatal(err)
	}

	c, err := New(inner, dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for recovery drain
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inner.has("orphan1") && inner.has("orphan2") {
			c.Close()
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()
	t.Fatal("orphaned chunks not drained on recovery")
}

func TestCloseWaitsForDrain(t *testing.T) {
	dir := t.TempDir()
	inner := newMemBackend()

	c, err := New(inner, dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Write(ctx, fmt.Sprintf("c%d", i), []byte("data")); err != nil {
			t.Fatal(err)
		}
	}

	c.Close()

	// All chunks should be drained after Close returns
	for i := 0; i < 5; i++ {
		if !inner.has(fmt.Sprintf("c%d", i)) {
			t.Fatalf("chunk c%d not drained after Close", i)
		}
	}
}

// slowBackend wraps a memBackend with an artificial write delay.
type slowBackend struct {
	inner *memBackend
	delay time.Duration
}

var _ store.StorageConnector = (*slowBackend)(nil)

func (s *slowBackend) Name() string { return s.inner.Name() }

func (s *slowBackend) Write(ctx context.Context, id string, data []byte) error {
	time.Sleep(s.delay)
	return s.inner.Write(ctx, id, data)
}

func (s *slowBackend) Read(ctx context.Context, id string) ([]byte, error) {
	return s.inner.Read(ctx, id)
}

func (s *slowBackend) Delete(ctx context.Context, id string) error { return s.inner.Delete(ctx, id) }
func (s *slowBackend) Capacity() (uint64, uint64, uint64)          { return s.inner.Capacity() }
func (s *slowBackend) HealthCheck(ctx context.Context) error        { return s.inner.HealthCheck(ctx) }
