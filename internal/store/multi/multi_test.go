package multi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/matt-rog/bumfs/internal/store"
)

// memBackend is an in-memory StorageConnector for testing.
type memBackend struct {
	name    string
	data    map[string][]byte
	total   uint64
	usedVal uint64
}

var _ store.StorageConnector = (*memBackend)(nil)

func newMemBackend(name string, total, used uint64) *memBackend {
	return &memBackend{name: name, data: make(map[string][]byte), total: total, usedVal: used}
}

func (m *memBackend) Name() string { return m.name }

func (m *memBackend) Write(_ context.Context, id string, data []byte) error {
	m.data[id] = append([]byte(nil), data...)
	return nil
}

func (m *memBackend) Read(_ context.Context, id string) ([]byte, error) {
	d, ok := m.data[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	return d, nil
}

func (m *memBackend) Delete(_ context.Context, id string) error {
	delete(m.data, id)
	return nil
}

func (m *memBackend) Capacity() (uint64, uint64, uint64) {
	free := m.total - m.usedVal
	return m.total, m.usedVal, free
}

func (m *memBackend) HealthCheck(_ context.Context) error { return nil }

func TestWriteRoutesToMostFreeSpace(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	small := newMemBackend("small", 100, 80) // 20 free
	large := newMemBackend("large", 100, 10) // 90 free

	mc, err := New([]store.StorageConnector{small, large}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := mc.Write(ctx, "chunk1", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	// Should have routed to "large" (more free space)
	if _, ok := large.data["chunk1"]; !ok {
		t.Fatal("expected chunk1 on large backend")
	}
	if _, ok := small.data["chunk1"]; ok {
		t.Fatal("chunk1 should not be on small backend")
	}
}

func TestReadRoutesToCorrectBackend(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	b1 := newMemBackend("b1", 100, 0)
	b2 := newMemBackend("b2", 100, 50)

	mc, err := New([]store.StorageConnector{b1, b2}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Write goes to b1 (more free)
	if err := mc.Write(ctx, "c1", []byte("data1")); err != nil {
		t.Fatal(err)
	}

	data, err := mc.Read(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data1" {
		t.Fatalf("read got %q, want %q", data, "data1")
	}
}

func TestDeleteRemovesFromBackendAndIndex(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	b := newMemBackend("b", 100, 0)
	mc, err := New([]store.StorageConnector{b}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := mc.Write(ctx, "c1", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := mc.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.data["c1"]; ok {
		t.Fatal("expected chunk deleted from backend")
	}

	_, err = mc.Read(ctx, "c1")
	if err == nil {
		t.Fatal("expected error reading deleted chunk")
	}
}

func TestIndexPersistsAndLoads(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	b := newMemBackend("b", 100, 0)
	mc, err := New([]store.StorageConnector{b}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := mc.Write(ctx, "c1", []byte("data")); err != nil {
		t.Fatal(err)
	}

	// Verify index file exists
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("index file not created: %v", err)
	}

	// Create a new Connector — should load existing index
	mc2, err := New([]store.StorageConnector{b}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	name, ok := mc2.index.Get("c1")
	if !ok || name != "b" {
		t.Fatalf("index not loaded: ok=%v name=%q", ok, name)
	}
}

func TestSingleBackendWorks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	b := newMemBackend("solo", 100, 0)
	mc, err := New([]store.StorageConnector{b}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := mc.Write(ctx, "c1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := mc.Read(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q, want %q", data, "hello")
	}
}

func TestCapacityAggregates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")

	b1 := newMemBackend("b1", 100, 20)
	b2 := newMemBackend("b2", 200, 50)
	mc, err := New([]store.StorageConnector{b1, b2}, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	total, used, free := mc.Capacity()
	if total != 300 || used != 70 || free != 230 {
		t.Fatalf("Capacity = (%d, %d, %d), want (300, 70, 230)", total, used, free)
	}
}

func TestNoBackendsReturnsError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.json")
	_, err := New(nil, dbPath)
	if err == nil {
		t.Fatal("expected error with no backends")
	}
}
