//go:build fuse3

package local

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestNewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir", "chunks")
	_, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestName(t *testing.T) {
	b := testBackend(t)
	if b.Name() != "local" {
		t.Fatalf("Name() = %q, want %q", b.Name(), "local")
	}
}

func TestWriteAndRead(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	data := []byte("hello world")
	if err := b.Write(ctx, "test-id", data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := b.Read(ctx, "test-id")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestReadNonExistent(t *testing.T) {
	b := testBackend(t)
	_, err := b.Read(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error reading non-existent blob")
	}
}

func TestDeleteExisting(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	if err := b.Write(ctx, "del-me", []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Delete(ctx, "del-me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Read(ctx, "del-me")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestDeleteNonExistentSilent(t *testing.T) {
	b := testBackend(t)
	if err := b.Delete(context.Background(), "nope"); err != nil {
		t.Fatalf("Delete non-existent should return nil, got: %v", err)
	}
}

func TestWriteOverwrite(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	if err := b.Write(ctx, "ow", []byte("first")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if err := b.Write(ctx, "ow", []byte("second")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	got, err := b.Read(ctx, "ow")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, []byte("second")) {
		t.Fatalf("got %q, want %q", got, "second")
	}
}

func TestCapacity(t *testing.T) {
	b := testBackend(t)
	total, _, free := b.Capacity()
	if total == 0 {
		t.Fatal("total capacity should be > 0")
	}
	if free == 0 {
		t.Fatal("free capacity should be > 0")
	}
}

func TestHealthCheck(t *testing.T) {
	b := testBackend(t)
	if err := b.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck should pass: %v", err)
	}

	// Create a backend pointing at a non-existent dir, then remove it
	dir := filepath.Join(t.TempDir(), "gone")
	b2, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	os.RemoveAll(dir)
	if err := b2.HealthCheck(context.Background()); err == nil {
		t.Fatal("HealthCheck should fail after dir removal")
	}
}

func TestLargeBlob(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("X"), 5*1024*1024)
	if err := b.Write(ctx, "big", data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := b.Read(ctx, "big")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("large blob round-trip mismatch")
	}
}
