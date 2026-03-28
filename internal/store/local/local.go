package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/matt-rog/bumfs/internal/store"
)

// Backend stores chunks as individual files in a directory.
type Backend struct {
	dir string
}

var _ store.StorageConnector = (*Backend)(nil)

// New creates a local storage backend at the given directory.
func New(dir string) (*Backend, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("local backend: mkdir %s: %w", dir, err)
	}
	return &Backend{dir: dir}, nil
}

func (b *Backend) Name() string { return "local" }

func (b *Backend) path(id string) string {
	return filepath.Join(b.dir, id)
}

func (b *Backend) Write(_ context.Context, id string, data []byte) error {
	return os.WriteFile(b.path(id), data, 0600)
}

func (b *Backend) Read(_ context.Context, id string) ([]byte, error) {
	data, err := os.ReadFile(b.path(id))
	if err != nil {
		return nil, fmt.Errorf("local read %s: %w", id, err)
	}
	return data, nil
}

func (b *Backend) Delete(_ context.Context, id string) error {
	err := os.Remove(b.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (b *Backend) Capacity() (total, used, free uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(b.dir, &stat); err != nil {
		return 0, 0, 0
	}
	total = stat.Blocks * uint64(stat.Bsize)
	free = stat.Bavail * uint64(stat.Bsize)
	used = total - free
	return
}

func (b *Backend) HealthCheck(_ context.Context) error {
	info, err := os.Stat(b.dir)
	if err != nil {
		return fmt.Errorf("local healthcheck: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local healthcheck: %s is not a directory", b.dir)
	}
	return nil
}
