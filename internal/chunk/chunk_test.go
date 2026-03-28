//go:build fuse3

package chunk

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/matt-rog/bumfs/internal/crypto"
)

// memBackend is an in-memory storage backend for testing.
type memBackend struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemBackend() *memBackend {
	return &memBackend{data: make(map[string][]byte)}
}

func (m *memBackend) Name() string { return "mem" }

func (m *memBackend) Write(_ context.Context, id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.data[id] = cp
	return nil
}

func (m *memBackend) Read(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[id]
	if !ok {
		return nil, fmt.Errorf("mem: not found: %s", id)
	}
	cp := make([]byte, len(d))
	copy(cp, d)
	return cp, nil
}

func (m *memBackend) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
	return nil
}

func (m *memBackend) Capacity() (total, used, free uint64) { return 1 << 30, 0, 1 << 30 }

func (m *memBackend) HealthCheck(_ context.Context) error { return nil }

func (m *memBackend) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

func testManager(t *testing.T, chunkSize int) (*Manager, *memBackend) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	enc, err := crypto.NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	be := newMemBackend()
	mgr := NewManager(chunkSize, enc, be)
	return mgr, be
}

func TestWriteReadRoundTrip(t *testing.T) {
	mgr, _ := testManager(t, 0)
	ctx := context.Background()
	data := bytes.Repeat([]byte("Z"), 100)
	refs, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := mgr.ReadFile(ctx, refs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip mismatch")
	}
}

func TestEmptyFile(t *testing.T) {
	mgr, _ := testManager(t, 0)
	ctx := context.Background()
	for _, input := range [][]byte{nil, {}} {
		refs, err := mgr.WriteFile(ctx, input)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if refs != nil {
			t.Fatalf("expected nil refs for empty data, got %d", len(refs))
		}
	}
}

func TestContentAddressing(t *testing.T) {
	mgr, _ := testManager(t, 0)
	ctx := context.Background()
	data := []byte("deterministic content")
	refs1, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile 1: %v", err)
	}
	refs2, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	if len(refs1) != len(refs2) {
		t.Fatalf("ref count mismatch: %d vs %d", len(refs1), len(refs2))
	}
	for i := range refs1 {
		if refs1[i].ChunkID != refs2[i].ChunkID {
			t.Fatalf("chunk %d: ID mismatch %s vs %s", i, refs1[i].ChunkID, refs2[i].ChunkID)
		}
	}
}

func TestContentAddressingDifferentData(t *testing.T) {
	mgr, _ := testManager(t, 0)
	ctx := context.Background()
	refs1, err := mgr.WriteFile(ctx, []byte("alpha"))
	if err != nil {
		t.Fatalf("WriteFile 1: %v", err)
	}
	refs2, err := mgr.WriteFile(ctx, []byte("beta"))
	if err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	if refs1[0].ChunkID == refs2[0].ChunkID {
		t.Fatal("different data should produce different ChunkIDs")
	}
}

func TestMultiChunkFile(t *testing.T) {
	mgr, _ := testManager(t, 64)
	ctx := context.Background()
	data := bytes.Repeat([]byte("M"), 200)
	refs, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 200 / 64 = 3 full + 1 partial = 4 chunks (ceil(200/64))
	if len(refs) != 4 {
		t.Fatalf("expected 4 refs, got %d", len(refs))
	}
	got, err := mgr.ReadFile(ctx, refs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("multi-chunk round-trip mismatch")
	}
}

func TestChunkSizeRespected(t *testing.T) {
	mgr, _ := testManager(t, 100)
	ctx := context.Background()
	data := bytes.Repeat([]byte("S"), 250)
	refs, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(refs))
	}
	if refs[0].Size != 100 || refs[1].Size != 100 || refs[2].Size != 50 {
		t.Fatalf("sizes: %d, %d, %d", refs[0].Size, refs[1].Size, refs[2].Size)
	}
}

func TestDeleteChunks(t *testing.T) {
	mgr, be := testManager(t, 0)
	ctx := context.Background()
	refs, err := mgr.WriteFile(ctx, []byte("delete me"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if be.count() == 0 {
		t.Fatal("backend should have chunks")
	}
	if err := mgr.DeleteChunks(ctx, refs); err != nil {
		t.Fatalf("DeleteChunks: %v", err)
	}
	if be.count() != 0 {
		t.Fatalf("backend should be empty, has %d", be.count())
	}
}

func TestDefaultChunkSize(t *testing.T) {
	mgr, _ := testManager(t, 0)
	if mgr.ChunkSize() != DefaultChunkSize {
		t.Fatalf("ChunkSize() = %d, want %d", mgr.ChunkSize(), DefaultChunkSize)
	}
}

func TestLargeFile(t *testing.T) {
	mgr, _ := testManager(t, 1<<20)
	ctx := context.Background()
	data := bytes.Repeat([]byte("L"), 5*1024*1024)
	refs, err := mgr.WriteFile(ctx, data)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(refs) != 5 {
		t.Fatalf("expected 5 refs, got %d", len(refs))
	}
	got, err := mgr.ReadFile(ctx, refs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("large file round-trip mismatch")
	}
}
