// Package inventory implements the durable boundary between registry listing
// and ordered downloading. Complete inventories are immutable S3 objects.
package inventory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/services/storage"
)

// Must exceed the plugin message cap (24 MiB) after JSON base64-encodes raw.
const maxRecordBytes = 32 << 20
const keyPrefix = "inventories/sha256/"

type Header struct {
	SchemaVersion int    `json:"schema_version"`
	Registry      string `json:"registry"`
	// Provider and Kind identify the selected catalog when the plugin advertises
	// provider metadata. Both are absent in inventories from older plugins.
	Provider  string            `json:"provider,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Limit     int               `json:"limit,omitempty"` // zero means full query, not a limited sample
}
type Summary struct {
	Key       string `json:"inventory_key"`
	Entries   int64  `json:"entries"`
	LocalPath string `json:"inventory_file,omitempty"`
}
type record struct {
	Type      string              `json:"type"`
	Header    *Header             `json:"header,omitempty"`
	Reference *registry.Reference `json:"reference,omitempty"`
	Count     int64               `json:"count,omitempty"`
}
type Options struct {
	TempDir string
	// LocalDir retains completed inventories as <sha256>.jsonl. Empty disables
	// retention. Incomplete files are always removed.
	LocalDir string
	// OnEntry observes validated entries as discovery progresses. An error
	// aborts publication. Displayed entries are provisional until Create succeeds.
	// The callback must not mutate the reference or its maps/slices.
	OnEntry         func(registry.Reference) error
	MaxBytes        int64
	Attempts        int
	Timeout         time.Duration // storage transfer attempt deadline, not listing duration
	SortBufferBytes int           // approximate encoded bytes per in-memory sorting run
}

func DefaultOptions() Options {
	return Options{MaxBytes: 16 << 30, Attempts: 4, Timeout: 30 * time.Minute, SortBufferBytes: 16 << 20}
}

// Validate reports invalid limits. Create and Open call it; hosts may call it
// first so bad settings fail before any listing starts.
func (o Options) Validate() error {
	if o.MaxBytes < 1 || o.MaxBytes == 1<<63-1 || o.Attempts < 1 || o.Timeout <= 0 || o.SortBufferBytes < 1 {
		return fmt.Errorf("invalid inventory limits")
	}
	return nil
}

// Create only calls Discover. No inventory is published until listing finishes
// successfully and a completion record is written. A limited query is recorded
// explicitly in the header. The returned key is relative to the store prefix.
func Create(ctx context.Context, lister registry.Lister, store storage.Store, header Header, opts Options) (Summary, error) {
	if err := opts.Validate(); err != nil {
		return Summary{}, err
	}
	if header.Registry == "" || header.Limit < 0 {
		return Summary{}, fmt.Errorf("registry is required and limit must be nonnegative")
	}
	if (header.Provider == "") != (header.Kind == "") {
		return Summary{}, fmt.Errorf("inventory provider and kind must be supplied together")
	}
	header.SchemaVersion = 1
	header.CreatedAt = time.Now().UTC()
	header.Options = maps.Clone(header.Options)
	tempDir := opts.TempDir
	if opts.LocalDir != "" {
		var err error
		tempDir, err = filepath.Abs(opts.LocalDir)
		if err != nil {
			return Summary{}, err
		}
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			return Summary{}, fmt.Errorf("create local inventory directory: %w", err)
		}
	}
	file, err := os.CreateTemp(tempDir, ".dug-list-*")
	if err != nil {
		return Summary{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	written := int64(0)
	write := func(value record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(data) > maxRecordBytes {
			return fmt.Errorf("inventory record exceeds %d bytes", maxRecordBytes)
		}
		data = append(data, '\n')
		if int64(len(data)) > opts.MaxBytes-written {
			return fmt.Errorf("inventory exceeds %d bytes", opts.MaxBytes)
		}
		n, err := io.MultiWriter(file, hash).Write(data)
		written += int64(n)
		return err
	}
	if err := write(record{Type: "header", Header: &header}); err != nil {
		return Summary{}, err
	}
	var count int64
	limitReached := errors.New("inventory listing limit reached")
	err = lister.Discover(ctx, func(ref registry.Reference) error {
		if header.Provider != "" {
			ref.Provider, ref.Kind = header.Provider, header.Kind
		}
		if err := validateReference(header.Registry, ref); err != nil {
			return err
		}
		if err := write(record{Type: "entry", Reference: &ref}); err != nil {
			return err
		}
		if opts.OnEntry != nil {
			if err := opts.OnEntry(ref); err != nil {
				return err
			}
		}
		count++
		if header.Limit > 0 && count >= int64(header.Limit) {
			return limitReached
		}
		return nil
	})
	if err != nil && !errors.Is(err, limitReached) {
		return Summary{}, err
	}
	if err := write(record{Type: "complete", Count: count}); err != nil {
		return Summary{}, err
	}
	key := keyPrefix + hex.EncodeToString(hash.Sum(nil)) + ".jsonl"
	if opts.LocalDir != "" {
		if err := file.Sync(); err != nil {
			return Summary{}, err
		}
	}
	err = retry.Do(ctx, opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return retry.Permanent(err)
		}
		return store.Put(attempt, key, file, written, "application/x-ndjson")
	})
	summary := Summary{Key: key, Entries: count}
	if err != nil || opts.LocalDir == "" {
		return summary, err
	}
	if err := file.Close(); err != nil {
		return summary, fmt.Errorf("inventory stored at %s; close local copy: %w", key, err)
	}
	localPath := filepath.Join(tempDir, filepath.Base(key))
	if err := os.Rename(file.Name(), localPath); err != nil {
		return summary, fmt.Errorf("inventory stored at %s; retain local copy: %w", key, err)
	}
	summary.LocalPath = localPath
	return summary, nil
}

type Snapshot struct {
	Header Header
	Summary
	path string
	dir  string
}

// Open verifies the entire inventory, including completion marker, count, and
// content digest, then prepares a disk-backed ordered iterator. No entries are
// exposed to the downloader until this succeeds. Close removes sorting files.
func Open(ctx context.Context, store storage.ReadableStore, key string, order Order, opts Options) (*Snapshot, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	expected := strings.TrimSuffix(strings.TrimPrefix(key, keyPrefix), ".jsonl")
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size || key != keyPrefix+hex.EncodeToString(decoded)+".jsonl" {
		return nil, fmt.Errorf("invalid inventory key")
	}
	var snapshot *Snapshot
	err = retry.Do(ctx, opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		dir, err := os.MkdirTemp(opts.TempDir, "dug-inventory-*")
		if err != nil {
			return retry.Permanent(err)
		}
		success := false
		defer func() {
			if !success {
				os.RemoveAll(dir)
			}
		}()
		body, err := store.Open(attempt, key)
		if err != nil {
			return err
		}
		defer body.Close()
		hash := sha256.New()
		scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(body, opts.MaxBytes+1), hash))
		scanner.Buffer(make([]byte, 64<<10), maxRecordBytes+1)
		sorter := newSorter(dir, order, opts.SortBufferBytes)
		defer sorter.close()
		var header Header
		var count, bytes int64
		line := 0
		complete := false
		for scanner.Scan() {
			if err := attempt.Err(); err != nil {
				return err
			}
			line++
			bytes += int64(len(scanner.Bytes()) + 1)
			if bytes > opts.MaxBytes {
				return retry.Permanent(fmt.Errorf("inventory exceeds size limit"))
			}
			var value record
			if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
				return retry.Permanent(fmt.Errorf("inventory line %d: %w", line, err))
			}
			if complete {
				return retry.Permanent(fmt.Errorf("data after inventory completion"))
			}
			if line == 1 {
				if value.Type != "header" || value.Header == nil || value.Header.SchemaVersion != 1 || value.Header.Registry == "" || value.Header.Limit < 0 || (value.Header.Provider == "") != (value.Header.Kind == "") {
					return retry.Permanent(fmt.Errorf("invalid inventory header"))
				}
				header = *value.Header
				continue
			}
			switch value.Type {
			case "entry":
				if value.Reference == nil {
					return retry.Permanent(fmt.Errorf("missing inventory reference"))
				}
				if err := validateReference(header.Registry, *value.Reference); err != nil {
					return retry.Permanent(err)
				}
				if err := sorter.add(attempt, *value.Reference, count, len(scanner.Bytes())); err != nil {
					return retry.Permanent(err)
				}
				count++
			case "complete":
				if count != value.Count {
					return retry.Permanent(fmt.Errorf("inventory count mismatch"))
				}
				complete = true
			default:
				return retry.Permanent(fmt.Errorf("unexpected inventory record %q", value.Type))
			}
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				// Re-downloading cannot shrink the line; fail once.
				return retry.Permanent(fmt.Errorf("inventory line %d exceeds %d bytes", line+1, maxRecordBytes))
			}
			return err
		}
		if err := body.Close(); err != nil {
			return err
		}
		if !complete || hex.EncodeToString(hash.Sum(nil)) != expected {
			return retry.Permanent(fmt.Errorf("incomplete inventory or checksum mismatch"))
		}
		path, err := sorter.finish(attempt)
		if err != nil {
			return err
		}
		snapshot = &Snapshot{Header: header, Summary: Summary{Key: key, Entries: count}, path: path, dir: dir}
		success = true
		return nil
	})
	return snapshot, err
}

func (s *Snapshot) Close() error { return os.RemoveAll(s.dir) }
func (s *Snapshot) Entries(ctx context.Context, emit func(registry.Reference) error) error {
	file, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item sortItem
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err := emit(item.Reference); err != nil {
			return err
		}
	}
}
func validateReference(id string, ref registry.Reference) error {
	if ref.Registry != id || ref.Name == "" || ref.Version == "" {
		return fmt.Errorf("invalid inventory reference for registry %q", id)
	}
	if (ref.Provider == "") != (ref.Kind == "") {
		return fmt.Errorf("inventory reference provider and kind must be supplied together")
	}
	if err := ref.ValidateListing(); err != nil {
		return fmt.Errorf("invalid inventory reference %s@%s: %w", ref.Name, ref.Version, err)
	}
	return nil
}
