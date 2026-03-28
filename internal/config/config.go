package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	BumFS   BumFSConfig                `toml:"bumfs"`
	Backends map[string]BackendConfig   `toml:"backends"`
}

// BumFSConfig holds core settings.
type BumFSConfig struct {
	MountPoint    string `toml:"mount_point"`
	MetadataDB    string `toml:"metadata_db"`
	EncryptionKey string `toml:"encryption_key"`
	ChunkSize     int    `toml:"chunk_size"`
	CacheSize     int64  `toml:"cache_size"` // bytes, 0 = sync writes (no cache)
	CacheDir      string `toml:"cache_dir"`  // default ~/.bumfs/cache
}

// BackendConfig holds settings for a storage backend.
type BackendConfig struct {
	Type      string  `toml:"type"`
	Path      string  `toml:"path"`
	BotToken  string  `toml:"bot_token"`
	ChatID    int64   `toml:"chat_id"`
	ApiKey    string  `toml:"api_key"`  // W&B API key
	Entity    string  `toml:"entity"`   // W&B entity (username or team)
	Project   string  `toml:"project"`  // W&B project name
	RateLimit   float64 `toml:"rate_limit"`   // requests/sec, 0 = no limit
	MaxCapacity int64   `toml:"max_capacity"` // bytes, 0 = unlimited
	Priority    int     `toml:"priority"`     // lower = preferred for writes
}

// DefaultConfigPath returns ~/.bumfs/config.toml.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".bumfs", "config.toml")
}

// Load reads a config file from disk.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: load %s: %w", path, err)
	}
	cfg.expandPaths()
	return &cfg, nil
}

// Default returns a config with sensible defaults.
func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		BumFS: BumFSConfig{
			MountPoint:    "/tmp/bumfs",
			MetadataDB:    filepath.Join(home, ".bumfs", "metadata.db"),
			EncryptionKey: "",
			ChunkSize:     1 << 20, // 1MB
			CacheSize:     0,       // sync writes by default
			CacheDir:      filepath.Join(home, ".bumfs", "cache"),
		},
		Backends: map[string]BackendConfig{
			"local": {
				Type: "local",
				Path: filepath.Join(home, ".bumfs", "chunks"),
			},
		},
	}
}

// EncryptionKeyBytes returns the encryption key as bytes.
// If the key looks like a hex string, it decodes it; otherwise treats it as a passphrase.
func (c *Config) EncryptionKeyBytes() ([]byte, bool) {
	key := c.BumFS.EncryptionKey
	if key == "" {
		return nil, false
	}
	// Try hex decode
	if b, err := hex.DecodeString(key); err == nil && len(b) == 32 {
		return b, true
	}
	return []byte(key), true
}

func (c *Config) expandPaths() {
	c.BumFS.MountPoint = expandHome(c.BumFS.MountPoint)
	c.BumFS.MetadataDB = expandHome(c.BumFS.MetadataDB)
	c.BumFS.CacheDir = expandHome(c.BumFS.CacheDir)
	for k, v := range c.Backends {
		v.Path = expandHome(v.Path)
		c.Backends[k] = v
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
