package ratelimit

import (
	"context"
	"fmt"

	"golang.org/x/time/rate"

	"github.com/matt-rog/bumfs/internal/store"
)

// Connector wraps a StorageConnector with a token-bucket rate limiter.
type Connector struct {
	inner   store.StorageConnector
	limiter *rate.Limiter
}

var _ store.StorageConnector = (*Connector)(nil)

// New wraps inner with a rate limiter at rps requests per second.
// If rps <= 0, returns inner unwrapped (no rate limiting).
func New(inner store.StorageConnector, rps float64) store.StorageConnector {
	if rps <= 0 {
		return inner
	}
	burst := int(rps)
	if burst < 1 {
		burst = 1
	}
	return &Connector{
		inner:   inner,
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
	}
}

func (c *Connector) Name() string { return c.inner.Name() }

func (c *Connector) Capacity() (total, used, free uint64) { return c.inner.Capacity() }

func (c *Connector) Write(ctx context.Context, id string, data []byte) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("ratelimit write %s: %w", id, err)
	}
	return c.inner.Write(ctx, id, data)
}

func (c *Connector) Read(ctx context.Context, id string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("ratelimit read %s: %w", id, err)
	}
	return c.inner.Read(ctx, id)
}

func (c *Connector) Delete(ctx context.Context, id string) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("ratelimit delete %s: %w", id, err)
	}
	return c.inner.Delete(ctx, id)
}

func (c *Connector) HealthCheck(ctx context.Context) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("ratelimit healthcheck: %w", err)
	}
	return c.inner.HealthCheck(ctx)
}
