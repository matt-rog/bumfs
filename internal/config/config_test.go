//go:build fuse3

package config

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultValues(t *testing.T) {
	cfg := Default()
	if cfg.BumFS.MountPoint != "/tmp/bumfs" {
		t.Fatalf("MountPoint = %q, want /tmp/bumfs", cfg.BumFS.MountPoint)
	}
	if cfg.BumFS.ChunkSize != 1<<20 {
		t.Fatalf("ChunkSize = %d, want %d", cfg.BumFS.ChunkSize, 1<<20)
	}
	be, ok := cfg.Backends["local"]
	if !ok {
		t.Fatal("expected 'local' backend")
	}
	if be.Type != "local" {
		t.Fatalf("backend type = %q, want %q", be.Type, "local")
	}
}

func TestLoadValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[bumfs]
mount_point = "/mnt/test"
metadata_db = "/tmp/meta.db"
encryption_key = "mykey"
chunk_size = 512

[backends.mylocal]
type = "local"
path = "/tmp/chunks"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BumFS.MountPoint != "/mnt/test" {
		t.Fatalf("MountPoint = %q", cfg.BumFS.MountPoint)
	}
	if cfg.BumFS.ChunkSize != 512 {
		t.Fatalf("ChunkSize = %d", cfg.BumFS.ChunkSize)
	}
	if cfg.BumFS.EncryptionKey != "mykey" {
		t.Fatalf("EncryptionKey = %q", cfg.BumFS.EncryptionKey)
	}
	be, ok := cfg.Backends["mylocal"]
	if !ok {
		t.Fatal("expected 'mylocal' backend")
	}
	if be.Path != "/tmp/chunks" {
		t.Fatalf("backend path = %q", be.Path)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestEncryptionKeyBytesEmpty(t *testing.T) {
	cfg := Default()
	b, ok := cfg.EncryptionKeyBytes()
	if ok {
		t.Fatal("expected ok=false for empty key")
	}
	if b != nil {
		t.Fatal("expected nil bytes for empty key")
	}
}

func TestEncryptionKeyBytesHex(t *testing.T) {
	cfg := Default()
	// 64 hex chars = 32 bytes
	hexKey := strings.Repeat("ab", 32)
	cfg.BumFS.EncryptionKey = hexKey
	b, ok := cfg.EncryptionKeyBytes()
	if !ok {
		t.Fatal("expected ok=true")
	}
	expected, _ := hex.DecodeString(hexKey)
	if !bytes.Equal(b, expected) {
		t.Fatal("hex key mismatch")
	}
	if len(b) != 32 {
		t.Fatalf("key length = %d, want 32", len(b))
	}
}

func TestEncryptionKeyBytesPassphrase(t *testing.T) {
	cfg := Default()
	cfg.BumFS.EncryptionKey = "my secret passphrase"
	b, ok := cfg.EncryptionKeyBytes()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !bytes.Equal(b, []byte("my secret passphrase")) {
		t.Fatalf("passphrase bytes mismatch: %q", b)
	}
}

func TestExpandHomePaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[bumfs]
mount_point = "~/mnt"
metadata_db = "~/meta.db"

[backends.local]
type = "local"
path = "~/chunks"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(cfg.BumFS.MountPoint, home) {
		t.Fatalf("MountPoint not expanded: %q", cfg.BumFS.MountPoint)
	}
	if !strings.HasPrefix(cfg.BumFS.MetadataDB, home) {
		t.Fatalf("MetadataDB not expanded: %q", cfg.BumFS.MetadataDB)
	}
	if !strings.HasPrefix(cfg.Backends["local"].Path, home) {
		t.Fatalf("Backend path not expanded: %q", cfg.Backends["local"].Path)
	}
}
