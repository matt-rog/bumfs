package wandb

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/matt-rog/bumfs/internal/config"
)

func TestIntegration(t *testing.T) {
	cfg, err := config.Load(config.DefaultConfigPath())
	if err != nil {
		t.Skipf("skipping: cannot load config: %v", err)
	}

	bc, ok := cfg.Backends["wandb"]
	if !ok || bc.Type != "wandb" {
		t.Skip("skipping: no wandb backend configured")
	}

	dbPath := filepath.Join(t.TempDir(), "index.json")
	b, err := New(bc.ApiKey, bc.Entity, bc.Project, dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// HealthCheck
	if err := b.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	chunkID := "integration_test_chunk"
	payload := []byte("bumfs wandb integration test payload")

	t.Cleanup(func() {
		b.Delete(ctx, chunkID)
	})

	// Write
	if err := b.Write(ctx, chunkID, payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read back and verify
	got, err := b.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Read mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Capacity should reflect usage
	_, used, _ := b.Capacity()
	if used == 0 {
		t.Error("Capacity used=0 after write, expected >0")
	}

	// Delete
	if err := b.Delete(ctx, chunkID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Read after delete should fail
	if _, err := b.Read(ctx, chunkID); err == nil {
		t.Fatal("Read after Delete: expected error, got nil")
	}
}
