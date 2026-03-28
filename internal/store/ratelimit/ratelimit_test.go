package ratelimit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-rog/bumfs/internal/store"
)

// mockBackend is a minimal StorageConnector for testing.
type mockBackend struct {
	writes atomic.Int64
}

var _ store.StorageConnector = (*mockBackend)(nil)

func (m *mockBackend) Name() string                                     { return "mock" }
func (m *mockBackend) Write(_ context.Context, _ string, _ []byte) error { m.writes.Add(1); return nil }
func (m *mockBackend) Read(_ context.Context, _ string) ([]byte, error)  { return nil, nil }
func (m *mockBackend) Delete(_ context.Context, _ string) error          { return nil }
func (m *mockBackend) Capacity() (uint64, uint64, uint64)                { return 100, 0, 100 }
func (m *mockBackend) HealthCheck(_ context.Context) error               { return nil }

func TestRateLimitThrottles(t *testing.T) {
	mock := &mockBackend{}
	// 5 requests/sec with burst=5: first 5 use burst tokens instantly,
	// then each additional write waits ~200ms. 11 writes = 5 burst + 6 waited
	// = 6 * 200ms = ~1.2s minimum.
	conn := New(mock, 5)

	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 11; i++ {
		if err := conn.Write(ctx, "chunk", []byte("data")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	if elapsed < 1*time.Second {
		t.Fatalf("11 writes at 5/sec should take >= 1s, took %v", elapsed)
	}
	if mock.writes.Load() != 11 {
		t.Fatalf("expected 11 writes, got %d", mock.writes.Load())
	}
}

func TestContextCancellation(t *testing.T) {
	mock := &mockBackend{}
	// Very low rate to ensure the second call blocks
	conn := New(mock, 1)

	ctx := context.Background()
	// Use the burst token
	if err := conn.Write(ctx, "chunk", []byte("data")); err != nil {
		t.Fatal(err)
	}

	// Cancel context immediately for the next call
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := conn.Write(ctx, "chunk", []byte("data"))
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestZeroRPSReturnsUnwrapped(t *testing.T) {
	mock := &mockBackend{}
	conn := New(mock, 0)
	// Should be the same pointer — no wrapping
	if conn != mock {
		t.Fatal("expected rps=0 to return inner directly")
	}

	conn = New(mock, -1)
	if conn != mock {
		t.Fatal("expected rps<0 to return inner directly")
	}
}
