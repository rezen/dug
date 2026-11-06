// Package memstore is an in-memory storage.ReadableStore for tests and
// examples. It is safe for concurrent use.
package memstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/rezen/dug/services/storage"
)

type Store struct {
	mu      sync.Mutex
	objects map[string][]byte
}

var _ storage.ReadableStore = (*Store)(nil)

func New() *Store { return &Store{objects: make(map[string][]byte)} }

func (s *Store) Stat(ctx context.Context, key string) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, exists := s.objects[key]
	if !exists {
		return false, -1, ctx.Err()
	}
	return true, int64(len(data)), ctx.Err()
}

func (s *Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, mediaType string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Real S3 rejects a body that does not match the declared length.
	if int64(len(data)) != size {
		return fmt.Errorf("body is %d bytes, declared size is %d", len(data), size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	return nil
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

// Objects returns a copy of every stored object.
func (s *Store) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string][]byte, len(s.objects))
	for key, data := range s.objects {
		copied[key] = append([]byte(nil), data...)
	}
	return copied
}
