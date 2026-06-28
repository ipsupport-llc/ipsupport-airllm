// Package blob is a small object-store abstraction for capture bodies. Bodies
// live here (encrypted by the caller), never in Postgres. A filesystem impl
// backs local dev; MinIO/GCS impls back deploys.
package blob

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a key does not exist in the store.
var ErrNotFound = errors.New("blob: key not found")

// Store stores opaque blobs by key.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
