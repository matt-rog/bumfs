package store

import "context"

// StorageConnector is the interface that all storage backends must implement.
type StorageConnector interface {
	Name() string
	Write(ctx context.Context, id string, data []byte) error
	Read(ctx context.Context, id string) ([]byte, error)
	Delete(ctx context.Context, id string) error
	Capacity() (total, used, free uint64)
	HealthCheck(ctx context.Context) error
}
