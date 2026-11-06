// Package inventory implements the durable boundary between registry listing
// and ordered downloading. Complete inventories are immutable S3 objects.
package inventory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/rezen/dug/internal/layout"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/services/storage"
)

// Must exceed the plugin message cap (24 MiB) after JSON base64-encodes raw.
const maxRecordBytes = 32 << 20

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
	// Digest is the SHA-256 of every line before the complete record, so a
	// truncated or edited inventory is rejected even though its key is a ULID.
	Digest string `json:"sha256,omitempty"`
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
	// PartEntries and PartBytes bound one uploaded segment: Create uploads a
	// part as soon as either is reached, so an interrupted listing keeps
	// everything saved up to the last full part.
	PartEntries int
	PartBytes   int
	// AllowPartial lets Open read an inventory that has no complete.json by
	// taking every consecutive part that exists. Entries are validated but the
	// listing is known to be missing its tail.
	AllowPartial bool
}

func DefaultOptions() Options {
	return Options{MaxBytes: 16 << 30, Attempts: 4, Timeout: 30 * time.Minute, SortBufferBytes: 16 << 20, PartEntries: 5000, PartBytes: 8 << 20}
}

// Validate reports invalid limits. Create and Open call it; hosts may call it
// first so bad settings fail before any listing starts.
func (o Options) Validate() error {
	if o.MaxBytes < 1 || o.MaxBytes == 1<<63-1 || o.Attempts < 1 || o.Timeout <= 0 || o.SortBufferBytes < 1 || o.PartEntries < 1 || o.PartBytes < 1 {
		return fmt.Errorf("invalid inventory limits")
	}
	return nil
}

// Part describes one uploaded segment in complete.json.
type Part struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Entries int64  `json:"entries"`
	SHA256  string `json:"sha256"`
}

// Completion is complete.json, the commit point of an inventory. It is written
// only after every part has uploaded, so its presence means the listing
// finished and the parts it names are the whole inventory.
type Completion struct {
	SchemaVersion int       `json:"schema_version"`
	Registry      string    `json:"registry"`
	CreatedAt     time.Time `json:"created_at"`
	Entries       int64     `json:"entries"`
	Parts         []Part    `json:"parts"`
}

const completeName = "complete.json"

func partName(n int) string { return fmt.Sprintf("part-%06d.jsonl", n) }

// Create only calls Discover. Records are uploaded in parts while listing runs,
// and complete.json is written last, so no inventory is complete until listing
// finishes. If listing fails, the parts uploaded so far stay in the bucket; the
// returned summary carries their key so the caller can report it, and Open reads
// them only with AllowPartial. A limited query is recorded in the header, so a
// sample is never mistaken for the whole catalog.
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
	id, err := layout.NewULID(header.CreatedAt)
	if err != nil {
		return Summary{}, err
	}
	key, err := layout.InventoryKey(header.Registry, id)
	if err != nil {
		return Summary{}, err
	}
	tempDir := opts.TempDir
	if opts.LocalDir != "" {
		tempDir, err = filepath.Abs(opts.LocalDir)
		if err != nil {
			return Summary{}, err
		}
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			return Summary{}, fmt.Errorf("create local inventory directory: %w", err)
		}
	}
	// The local copy is the same records as the parts, concatenated.
	var file *os.File
	if opts.LocalDir != "" {
		file, err = os.CreateTemp(tempDir, ".dug-list-*")
		if err != nil {
			return Summary{}, err
		}
		defer os.Remove(file.Name())
		defer file.Close()
	}
	put := func(name string, body []byte, mediaType string) error {
		return retry.Do(ctx, opts.Attempts, func() error {
			attempt, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
			return store.Put(attempt, key+"/"+name, bytes.NewReader(body), int64(len(body)), mediaType)
		})
	}
	var (
		part         bytes.Buffer
		partEntries  int64
		parts        []Part
		written      int64
		count        int64
		partialError = func(err error) (Summary, error) {
			return Summary{Key: key, Entries: count}, err
		}
	)
	flush := func() error {
		if part.Len() == 0 {
			return nil
		}
		name := partName(len(parts) + 1)
		sum := sha256.Sum256(part.Bytes())
		if err := put(name, part.Bytes(), "application/x-ndjson"); err != nil {
			return fmt.Errorf("upload inventory %s: %w", name, err)
		}
		parts = append(parts, Part{Name: name, Size: int64(part.Len()), Entries: partEntries, SHA256: hex.EncodeToString(sum[:])})
		part.Reset()
		partEntries = 0
		return nil
	}
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
		if file != nil {
			if _, err := file.Write(data); err != nil {
				return err
			}
		}
		part.Write(data)
		written += int64(len(data))
		if value.Type == "entry" {
			partEntries++
		}
		if partEntries >= int64(opts.PartEntries) || part.Len() >= opts.PartBytes {
			return flush()
		}
		return nil
	}
	if err := write(record{Type: "header", Header: &header}); err != nil {
		return partialError(err)
	}
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
		return partialError(err)
	}
	if err := flush(); err != nil {
		return partialError(err)
	}
	body, err := json.MarshalIndent(Completion{SchemaVersion: 1, Registry: header.Registry, CreatedAt: header.CreatedAt, Entries: count, Parts: parts}, "", "  ")
	if err != nil {
		return partialError(err)
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return partialError(err)
		}
	}
	if err := put(completeName, body, "application/json"); err != nil {
		return partialError(fmt.Errorf("commit inventory: %w", err))
	}
	summary := Summary{Key: key, Entries: count}
	if file == nil {
		return summary, nil
	}
	if err := file.Close(); err != nil {
		return summary, fmt.Errorf("inventory stored at %s; close local copy: %w", key, err)
	}
	localPath := filepath.Join(tempDir, path.Base(key)+".jsonl")
	if err := os.Rename(file.Name(), localPath); err != nil {
		return summary, fmt.Errorf("inventory stored at %s; retain local copy: %w", key, err)
	}
	summary.LocalPath = localPath
	return summary, nil
}

type Snapshot struct {
	Header Header
	Summary
	// Partial is true when the inventory had no complete.json and was read
	// with AllowPartial: the listing is missing everything after the last part.
	Partial bool
	path    string
	dir     string
}

// ErrIncomplete is returned by Open for an inventory without complete.json
// unless AllowPartial is set.
var ErrIncomplete = errors.New("inventory is incomplete: listing was interrupted before complete.json was written")

// Open verifies the whole inventory, including each part's size and SHA-256 and
// the entry counts in complete.json, then prepares a disk-backed ordered
// iterator. No entries are exposed to the downloader until this succeeds.
// Close removes sorting files.
func Open(ctx context.Context, store storage.ReadableStore, key string, order Order, opts Options) (*Snapshot, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	keyDir, _, ok := layout.ParseInventoryKey(key)
	if !ok {
		return nil, fmt.Errorf("invalid inventory key")
	}
	var snapshot *Snapshot
	err := retry.Do(ctx, opts.Attempts, func() error {
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
		completion, complete, err := loadCompletion(attempt, store, key)
		if err != nil {
			return err
		}
		if !complete && !opts.AllowPartial {
			return retry.Permanent(fmt.Errorf("%s: %w; pass --allow-partial to use the entries that were saved", key, ErrIncomplete))
		}
		if complete {
			if d, err := layout.Dir(completion.Registry); err != nil || d != keyDir {
				return retry.Permanent(fmt.Errorf("inventory %s does not belong to registry %q", key, completion.Registry))
			}
			for i, p := range completion.Parts {
				if p.Name != partName(i+1) || p.Size < 0 || p.Entries < 0 {
					return retry.Permanent(fmt.Errorf("invalid part %d in %s", i+1, completeName))
				}
			}
		}
		sorter := newSorter(dir, order, opts.SortBufferBytes)
		defer sorter.close()
		var header Header
		var count, total int64
		line := 0
		readPart := func(name string, want *Part) (found bool, err error) {
			body, err := store.Open(attempt, key+"/"+name)
			if errors.Is(err, storage.ErrNotFound) {
				if complete {
					return false, retry.Permanent(fmt.Errorf("inventory part %s is missing", name))
				}
				return false, nil
			}
			if err != nil {
				return false, err
			}
			defer body.Close()
			hash := sha256.New()
			var size, entries int64
			scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(body, opts.MaxBytes+1), hash))
			scanner.Buffer(make([]byte, 64<<10), maxRecordBytes+1)
			for scanner.Scan() {
				if err := attempt.Err(); err != nil {
					return false, err
				}
				line++
				size += int64(len(scanner.Bytes()) + 1)
				total += int64(len(scanner.Bytes()) + 1)
				if total > opts.MaxBytes {
					return false, retry.Permanent(fmt.Errorf("inventory exceeds size limit"))
				}
				var value record
				if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
					return false, retry.Permanent(fmt.Errorf("inventory %s line %d: %w", name, line, err))
				}
				if line == 1 {
					if value.Type != "header" || value.Header == nil || value.Header.SchemaVersion != 1 || value.Header.Registry == "" || value.Header.Limit < 0 || (value.Header.Provider == "") != (value.Header.Kind == "") {
						return false, retry.Permanent(fmt.Errorf("invalid inventory header"))
					}
					if d, err := layout.Dir(value.Header.Registry); err != nil || d != keyDir {
						return false, retry.Permanent(fmt.Errorf("inventory %s does not belong to registry %q", key, value.Header.Registry))
					}
					if complete && value.Header.Registry != completion.Registry {
						return false, retry.Permanent(fmt.Errorf("inventory header registry differs from %s", completeName))
					}
					header = *value.Header
					continue
				}
				if value.Type != "entry" || value.Reference == nil {
					return false, retry.Permanent(fmt.Errorf("unexpected inventory record %q in %s", value.Type, name))
				}
				if err := validateReference(header.Registry, *value.Reference); err != nil {
					return false, retry.Permanent(err)
				}
				if err := sorter.add(attempt, *value.Reference, count, len(scanner.Bytes())); err != nil {
					return false, retry.Permanent(err)
				}
				count++
				entries++
			}
			if err := scanner.Err(); err != nil {
				if errors.Is(err, bufio.ErrTooLong) {
					// Re-downloading cannot shrink the line; fail once.
					return false, retry.Permanent(fmt.Errorf("inventory line %d exceeds %d bytes", line+1, maxRecordBytes))
				}
				return false, err
			}
			if want != nil && (size != want.Size || entries != want.Entries || hex.EncodeToString(hash.Sum(nil)) != want.SHA256) {
				return false, retry.Permanent(fmt.Errorf("inventory part %s does not match %s (size, entry count or checksum)", name, completeName))
			}
			return true, nil
		}
		for i := 0; ; i++ {
			var want *Part
			if complete {
				if i >= len(completion.Parts) {
					break
				}
				want = &completion.Parts[i]
			}
			found, err := readPart(partName(i+1), want)
			if err != nil {
				return err
			}
			if !found {
				break
			}
		}
		if line == 0 {
			return retry.Permanent(fmt.Errorf("inventory %s has no parts", key))
		}
		if complete && count != completion.Entries {
			return retry.Permanent(fmt.Errorf("inventory count mismatch"))
		}
		path, err := sorter.finish(attempt)
		if err != nil {
			return err
		}
		snapshot = &Snapshot{Header: header, Summary: Summary{Key: key, Entries: count}, Partial: !complete, path: path, dir: dir}
		success = true
		return nil
	})
	return snapshot, err
}

// loadCompletion reads complete.json. A missing file reports complete=false.
func loadCompletion(ctx context.Context, store storage.ReadableStore, key string) (Completion, bool, error) {
	body, err := store.Open(ctx, key+"/"+completeName)
	if errors.Is(err, storage.ErrNotFound) {
		return Completion{}, false, nil
	}
	if err != nil {
		return Completion{}, false, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxRecordBytes))
	if err != nil {
		return Completion{}, false, err
	}
	var c Completion
	if err := json.Unmarshal(data, &c); err != nil || c.SchemaVersion != 1 || c.Registry == "" {
		return Completion{}, false, retry.Permanent(fmt.Errorf("invalid %s", completeName))
	}
	return c, true, nil
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
