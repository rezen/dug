package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rezen/dug/internal/layout"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/services/storage"
)

// LastRun points at the most recent completed inventory for one query. A query
// is the registry, its plugin options, and the listing limit, so a pointer is
// never reused for a different catalog slice.
type LastRun struct {
	InventoryKey string            `json:"inventory_key"`
	Registry     string            `json:"registry"`
	Options      map[string]string `json:"options,omitempty"`
	Limit        int               `json:"limit,omitempty"`
	Entries      int64             `json:"entries"`
	CreatedAt    time.Time         `json:"created_at"`
}

func lastRunKey(registry string, options map[string]string, limit int) (string, error) {
	prefix, err := layout.LastRunPrefix(registry)
	if err != nil {
		return "", err
	}
	// encoding/json sorts map keys, so equal queries hash identically.
	query, _ := json.Marshal(struct {
		Registry string            `json:"registry"`
		Options  map[string]string `json:"options"`
		Limit    int               `json:"limit"`
	}{registry, options, limit})
	sum := sha256.Sum256(query)
	return prefix + hex.EncodeToString(sum[:]) + ".json", nil
}

// SaveLastRun records summary as the latest inventory for header's query,
// replacing any earlier pointer. Call it only after Create succeeds.
func SaveLastRun(ctx context.Context, store storage.Store, header Header, summary Summary, opts Options) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(LastRun{
		InventoryKey: summary.Key,
		Registry:     header.Registry,
		Options:      header.Options,
		Limit:        header.Limit,
		Entries:      summary.Entries,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	key, err := lastRunKey(header.Registry, header.Options, header.Limit)
	if err != nil {
		return err
	}
	return retry.Do(ctx, opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		return store.Put(attempt, key, bytes.NewReader(body), int64(len(body)), "application/json")
	})
}

// LoadLastRun returns the pointer for the query, or false when none has been
// recorded yet. The inventory it names is verified later by Open.
func LoadLastRun(ctx context.Context, store storage.ReadableStore, registry string, options map[string]string, limit int, opts Options) (LastRun, bool, error) {
	if err := opts.Validate(); err != nil {
		return LastRun{}, false, err
	}
	key, err := lastRunKey(registry, options, limit)
	if err != nil {
		return LastRun{}, false, err
	}
	var run LastRun
	found := false
	err = retry.Do(ctx, opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		reader, err := store.Open(attempt, key)
		if errors.Is(err, storage.ErrNotFound) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &run); err != nil {
			return retry.Permanent(fmt.Errorf("decode %s: %w", key, err))
		}
		found = true
		return nil
	})
	if err != nil || !found {
		return LastRun{}, false, err
	}
	invDir, _, ok := layout.ParseInventoryKey(run.InventoryKey)
	if dir, _ := layout.Dir(registry); run.Registry != registry || !ok || invDir != dir {
		return LastRun{}, false, fmt.Errorf("last run pointer %s is invalid", key)
	}
	return run, true, nil
}
