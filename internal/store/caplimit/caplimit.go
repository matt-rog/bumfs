package caplimit

import (
	"context"

	"github.com/matt-rog/bumfs/internal/store"
)

// Connector wraps a StorageConnector with an artificial capacity limit.
type Connector struct {
	inner    store.StorageConnector
	maxBytes uint64
}

var _ store.StorageConnector = (*Connector)(nil)

// New wraps inner with a capacity cap of maxBytes.
// If maxBytes <= 0, returns inner unwrapped (no cap).
func New(inner store.StorageConnector, maxBytes int64) store.StorageConnector {
	if maxBytes <= 0 {
		return inner
	}
	return &Connector{
		inner:    inner,
		maxBytes: uint64(maxBytes),
	}
}

func (c *Connector) Name() string { return c.inner.Name() }

func (c *Connector) Capacity() (total, used, free uint64) {
	_, used, _ = c.inner.Capacity()
	total = c.maxBytes
	if used >= total {
		return total, used, 0
	}
	return total, used, total - used
}

func (c *Connector) Write(ctx context.Context, id string, data []byte) error {
	return c.inner.Write(ctx, id, data)
}

func (c *Connector) Read(ctx context.Context, id string) ([]byte, error) {
	return c.inner.Read(ctx, id)
}

func (c *Connector) Delete(ctx context.Context, id string) error {
	return c.inner.Delete(ctx, id)
}

func (c *Connector) HealthCheck(ctx context.Context) error {
	return c.inner.HealthCheck(ctx)
}
