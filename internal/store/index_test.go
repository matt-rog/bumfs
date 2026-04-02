package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexStringRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idx.json")

	idx, err := NewIndex[string](dbPath)
	if err != nil {
		t.Fatal(err)
	}

	idx.Set("k1", "v1")
	idx.Set("k2", "v2")

	if v, ok := idx.Get("k1"); !ok || v != "v1" {
		t.Fatalf("Get(k1) = %q, %v; want v1, true", v, ok)
	}
	if idx.Len() != 2 {
		t.Fatalf("Len = %d, want 2", idx.Len())
	}

	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload from disk
	idx2, err := NewIndex[string](dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := idx2.Get("k1"); !ok || v != "v1" {
		t.Fatalf("after reload Get(k1) = %q, %v; want v1, true", v, ok)
	}
	if idx2.Len() != 2 {
		t.Fatalf("after reload Len = %d, want 2", idx2.Len())
	}
}

func TestIndexInt64RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idx.json")

	idx, err := NewIndex[int64](dbPath)
	if err != nil {
		t.Fatal(err)
	}

	idx.Set("asset1", 42)
	idx.Set("asset2", 99)

	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}

	idx2, err := NewIndex[int64](dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := idx2.Get("asset1"); !ok || v != 42 {
		t.Fatalf("Get(asset1) = %d, %v; want 42, true", v, ok)
	}
}

func TestIndexDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idx.json")

	idx, err := NewIndex[string](dbPath)
	if err != nil {
		t.Fatal(err)
	}

	idx.Set("k1", "v1")
	idx.Delete("k1")

	if _, ok := idx.Get("k1"); ok {
		t.Fatal("expected k1 to be deleted")
	}
	if idx.Len() != 0 {
		t.Fatalf("Len = %d, want 0", idx.Len())
	}
}

func TestIndexMissingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nonexistent.json")

	idx, err := NewIndex[string](dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Len() != 0 {
		t.Fatalf("Len = %d, want 0 for missing file", idx.Len())
	}
}

func TestIndexCorruptFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bad.json")

	if err := os.WriteFile(dbPath, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := NewIndex[string](dbPath)
	if err == nil {
		t.Fatal("expected error for corrupt JSON")
	}
}
