// Package storage provides object storage for blobs and completion manifests.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned (wrapped) by Open when the key does not exist, so
// callers can test for it the same way on every backend.
var ErrNotFound = errors.New("object not found")

// Store must be safe for concurrent use. Put must only expose complete objects
// and must return after consuming body. Existing keys may be overwritten.
type Store interface {
	// Stat reports whether key exists and its stored size, or -1 when the
	// backend cannot report a size. Callers compare it with the expected size
	// so a truncated object is never trusted just because its key exists.
	Stat(ctx context.Context, key string) (exists bool, size int64, err error)
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error
}

// ReadableStore also reads persisted stage-one inventories for stage two.
type ReadableStore interface {
	Store
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}
