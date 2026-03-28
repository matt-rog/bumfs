package cache

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/matt-rog/bumfs/internal/store"
)

type drainItem struct {
	id   string
	data []byte
}

// Connector is a write-behind cache that buffers chunks on local disk
// and asynchronously drains them to the inner backend.
type Connector struct {
	inner     store.StorageConnector
	dir       string
	maxBytes  int64
	mu        sync.Mutex
	cond      *sync.Cond
	usedBytes int64
	drainCh   chan drainItem
	stopCh    chan struct{}
	doneCh    chan struct{}
}

var _ store.StorageConnector = (*Connector)(nil)

// New creates a write-behind cache. Chunks are written to dir on local disk,
// then asynchronously drained to inner. maxBytes controls backpressure:
// writes block when the cache exceeds this size.
func New(inner store.StorageConnector, dir string, maxBytes int64) (*Connector, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}

	c := &Connector{
		inner:    inner,
		dir:      dir,
		maxBytes: maxBytes,
		drainCh:  make(chan drainItem, 256),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)

	// Crash recovery: scan dir for leftover cached chunks
	if err := c.recoverPending(); err != nil {
		return nil, fmt.Errorf("cache: recover: %w", err)
	}

	go c.drainLoop()
	return c, nil
}

func (c *Connector) Name() string { return c.inner.Name() }

func (c *Connector) Capacity() (total, used, free uint64) { return c.inner.Capacity() }

func (c *Connector) HealthCheck(ctx context.Context) error { return c.inner.HealthCheck(ctx) }

// Write caches the chunk on local disk and enqueues it for async drain.
// Blocks if the cache is full (backpressure).
func (c *Connector) Write(ctx context.Context, id string, data []byte) error {
	size := int64(len(data))

	// Backpressure: wait for space
	c.mu.Lock()
	for c.usedBytes+size > c.maxBytes {
		// Watch for context cancellation while waiting
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.cond.Broadcast()
			case <-waitDone:
			}
		}()
		c.cond.Wait()
		close(waitDone)
		if ctx.Err() != nil {
			c.mu.Unlock()
			return ctx.Err()
		}
	}
	c.usedBytes += size
	c.mu.Unlock()

	// Write to local cache dir
	path := filepath.Join(c.dir, id)
	if err := os.WriteFile(path, data, 0600); err != nil {
		c.mu.Lock()
		c.usedBytes -= size
		c.mu.Unlock()
		c.cond.Broadcast()
		return fmt.Errorf("cache write %s: %w", id, err)
	}

	// Enqueue for drain
	c.drainCh <- drainItem{id: id, data: data}
	return nil
}

// Read checks the local cache first, then falls through to inner.
func (c *Connector) Read(ctx context.Context, id string) ([]byte, error) {
	path := filepath.Join(c.dir, id)
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	}
	return c.inner.Read(ctx, id)
}

// Delete removes from cache (if present) and delegates to inner.
func (c *Connector) Delete(ctx context.Context, id string) error {
	path := filepath.Join(c.dir, id)
	if info, err := os.Stat(path); err == nil {
		size := info.Size()
		os.Remove(path)
		c.mu.Lock()
		c.usedBytes -= size
		c.mu.Unlock()
		c.cond.Broadcast()
	}
	return c.inner.Delete(ctx, id)
}

// Close signals the drain goroutine to stop and waits for it to finish
// draining all pending items.
func (c *Connector) Close() {
	close(c.stopCh)
	<-c.doneCh
}

func (c *Connector) drainLoop() {
	defer close(c.doneCh)

	for {
		select {
		case item := <-c.drainCh:
			c.drainOne(item)
		case <-c.stopCh:
			// Drain remaining items in channel
			for {
				select {
				case item := <-c.drainCh:
					c.drainOne(item)
				default:
					return
				}
			}
		}
	}
}

func (c *Connector) drainOne(item drainItem) {
	ctx := context.Background()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.inner.Write(ctx, item.id, item.data)
		if err == nil {
			break
		}
		log.Printf("cache: drain %s attempt %d failed: %v", item.id, attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	if err != nil {
		log.Printf("cache: drain %s failed after 3 attempts: %v (chunk remains in cache)", item.id, err)
		return
	}

	// Remove from local cache
	path := filepath.Join(c.dir, item.id)
	info, statErr := os.Stat(path)
	os.Remove(path)

	var size int64
	if statErr == nil {
		size = info.Size()
	}

	c.mu.Lock()
	c.usedBytes -= size
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *Connector) recoverPending() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id := entry.Name()
		path := filepath.Join(c.dir, id)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("cache: recover skip %s: %v", id, err)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		c.usedBytes += info.Size()
		c.drainCh <- drainItem{id: id, data: data}
	}
	return nil
}
