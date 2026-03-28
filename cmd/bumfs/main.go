package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/matt-rog/bumfs/internal/chunk"
	"github.com/matt-rog/bumfs/internal/config"
	"github.com/matt-rog/bumfs/internal/crypto"
	bumfs "github.com/matt-rog/bumfs/internal/fs"
	"github.com/matt-rog/bumfs/internal/meta"
	"github.com/matt-rog/bumfs/internal/store"
	"github.com/matt-rog/bumfs/internal/store/cache"
	"github.com/matt-rog/bumfs/internal/store/caplimit"
	"github.com/matt-rog/bumfs/internal/store/local"
	"github.com/matt-rog/bumfs/internal/store/multi"
	"github.com/matt-rog/bumfs/internal/store/ratelimit"
	"github.com/matt-rog/bumfs/internal/store/telegram"
	"github.com/matt-rog/bumfs/internal/store/wandb"
	"github.com/winfsp/cgofuse/fuse"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "mount":
		cmdMount()
	case "unmount", "umount":
		cmdUnmount()
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: bumfs <command> [args]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  mount  [mountpoint]  Mount the filesystem\n")
	fmt.Fprintf(os.Stderr, "  unmount [mountpoint]  Unmount the filesystem\n")
}

func cmdMount() {
	// Load or create config
	cfg := loadConfig()

	// Override mount point from first positional arg (skip flags)
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "--config" {
			i++ // skip flag value
			continue
		}
		if strings.HasPrefix(os.Args[i], "--") {
			continue
		}
		cfg.BumFS.MountPoint = os.Args[i]
		break
	}

	if cfg.BumFS.MountPoint == "" {
		log.Fatal("bumfs: no mount point specified")
	}

	// Ensure mount point exists
	if err := os.MkdirAll(cfg.BumFS.MountPoint, 0755); err != nil {
		log.Fatalf("bumfs: create mount point: %v", err)
	}

	// Init encryption key
	encKey, err := resolveEncryptionKey(cfg)
	if err != nil {
		log.Fatalf("bumfs: encryption key: %v", err)
	}
	enc, err := crypto.NewEncryptor(encKey)
	if err != nil {
		log.Fatalf("bumfs: create encryptor: %v", err)
	}

	// Build all backends with rate limiting
	var backends []store.StorageConnector
	for name, backendCfg := range cfg.Backends {
		var raw store.StorageConnector
		switch backendCfg.Type {
		case "local":
			raw, err = local.New(backendCfg.Path)
		case "telegram":
			dbPath := filepath.Join(filepath.Dir(cfg.BumFS.MetadataDB), "telegram_index.json")
			raw, err = telegram.New(backendCfg.BotToken, backendCfg.ChatID, dbPath)
		case "wandb":
			dbPath := filepath.Join(filepath.Dir(cfg.BumFS.MetadataDB), "wandb_index.json")
			raw, err = wandb.New(backendCfg.ApiKey, backendCfg.Entity, backendCfg.Project, dbPath)
		default:
			log.Fatalf("bumfs: unknown backend type %q for %q", backendCfg.Type, name)
		}
		if err != nil {
			log.Fatalf("bumfs: create backend %q: %v", name, err)
		}
		wrapped := ratelimit.New(raw, backendCfg.RateLimit)
		wrapped = caplimit.New(wrapped, backendCfg.MaxCapacity)
		backends = append(backends, wrapped)
	}
	if len(backends) == 0 {
		log.Fatal("bumfs: no backends configured")
	}

	// Multi-backend router
	indexPath := filepath.Join(filepath.Dir(cfg.BumFS.MetadataDB), "multi_index.json")
	var connector store.StorageConnector
	connector, err = multi.New(backends, indexPath)
	if err != nil {
		log.Fatalf("bumfs: create multi connector: %v", err)
	}

	// Optional write-behind cache
	var cacheConn *cache.Connector
	if cfg.BumFS.CacheSize > 0 {
		cacheConn, err = cache.New(connector, cfg.BumFS.CacheDir, cfg.BumFS.CacheSize)
		if err != nil {
			log.Fatalf("bumfs: create cache: %v", err)
		}
		connector = cacheConn
		log.Printf("bumfs: write cache enabled (%d bytes at %s)", cfg.BumFS.CacheSize, cfg.BumFS.CacheDir)
	}

	// Init metadata store
	metaStore, err := meta.Open(cfg.BumFS.MetadataDB)
	if err != nil {
		log.Fatalf("bumfs: open metadata: %v", err)
	}

	// Init chunk manager
	chunkMgr := chunk.NewManager(cfg.BumFS.ChunkSize, enc, connector)

	// Create FUSE filesystem
	fsys := bumfs.New(metaStore, chunkMgr, connector)
	host := fuse.NewFileSystemHost(fsys)
	host.SetCapReaddirPlus(true)

	// Signal handling for clean unmount
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("bumfs: received %v, unmounting...", sig)
		host.Unmount()
	}()

	log.Printf("bumfs: mounting at %s", cfg.BumFS.MountPoint)
	log.Printf("bumfs: metadata db: %s", cfg.BumFS.MetadataDB)
	log.Printf("bumfs: backends: %d configured", len(backends))

	// Mount (blocks until unmount)
	if !host.Mount(cfg.BumFS.MountPoint, nil) {
		log.Fatal("bumfs: mount failed")
	}

	// Cleanup
	if cacheConn != nil {
		log.Println("bumfs: draining write cache...")
		cacheConn.Close()
	}
	metaStore.Close()
	log.Println("bumfs: unmounted cleanly")
}

func cmdUnmount() {
	mountpoint := "/tmp/bumfs"
	if len(os.Args) >= 3 {
		mountpoint = os.Args[2]
	}

	fmt.Printf("Unmounting %s...\n", mountpoint)
	// Use fusermount on Linux
	err := syscall.Exec("/bin/fusermount", []string{"fusermount", "-u", mountpoint}, os.Environ())
	if err != nil {
		log.Fatalf("bumfs: unmount failed: %v", err)
	}
}

func loadConfig() *config.Config {
	cfgPath := config.DefaultConfigPath()

	// Check for --config flag
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
			break
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// Use defaults if config doesn't exist
		if os.IsNotExist(err) || true {
			log.Printf("bumfs: using default config (no config at %s)", cfgPath)
			return config.Default()
		}
		log.Fatalf("bumfs: load config: %v", err)
	}
	return cfg
}

func resolveEncryptionKey(cfg *config.Config) ([]byte, error) {
	keyBytes, hasKey := cfg.EncryptionKeyBytes()
	if !hasKey {
		// Generate a new key and store it
		key, err := crypto.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		log.Printf("bumfs: generated encryption key: %s", hex.EncodeToString(key))
		log.Println("bumfs: save this key in your config file to persist across mounts!")
		return key, nil
	}

	// If it's 32 bytes, use as-is (raw key)
	if len(keyBytes) == 32 {
		return keyBytes, nil
	}

	// Otherwise treat as passphrase — derive key with Argon2id
	// Use a fixed salt derived from the passphrase for deterministic key derivation
	// (In production, the salt should be stored alongside the metadata)
	salt := make([]byte, crypto.SaltLen)
	copy(salt, keyBytes)
	return crypto.DeriveKey(string(keyBytes), salt), nil
}
