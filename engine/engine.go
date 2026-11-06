// Package engine downloads an ordered inventory with bounded concurrency.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/services/storage"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// copyBufferBytes sizes the staging copy buffer. io.Copy's default 32 KiB
// would cost one write syscall per 32 KiB of a multi-gigabyte blob.
const copyBufferBytes = 1 << 20

type Options struct {
	Concurrency  int // concurrently resolved/archived resources
	Downloads    int // global blob transfers, including upload and retries
	QueueSize    int
	Attempts     int
	Timeout      time.Duration // per resolution or complete blob attempt
	MaxBlobBytes int64
	TempDir      string
	Refresh      bool
	FailFast     bool
	// OnEvent is called concurrently; callbacks must be concurrency safe.
	OnEvent func(Event)
}

func DefaultOptions() Options {
	return Options{Concurrency: 16, Downloads: 32, QueueSize: 64, Attempts: 4, Timeout: 30 * time.Minute, MaxBlobBytes: 4 << 30}
}

// Validate reports invalid limits. Download calls it; hosts may call it first
// so bad settings fail before any listing or storage work starts.
func (o Options) Validate() error {
	if o.Concurrency < 1 || o.Downloads < 1 || o.QueueSize < 0 || o.Attempts < 1 || o.Timeout <= 0 || o.MaxBlobBytes < 1 || o.MaxBlobBytes == math.MaxInt64 {
		return fmt.Errorf("concurrency, downloads, attempts, timeout and max blob bytes must be positive; queue must be nonnegative")
	}
	return nil
}

type Event struct {
	Reference registry.Reference
	Status    string
	Key       string
	Error     error
}

type Report struct {
	Scheduled     int64 `json:"scheduled"`
	Completed     int64 `json:"completed"`
	Skipped       int64 `json:"skipped"`
	Failed        int64 `json:"failed"`
	UploadedBlobs int64 `json:"uploaded_blobs"`
	UploadedBytes int64 `json:"uploaded_bytes"`
}

type StoredBlob struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Key       string `json:"key"`
}

type Manifest struct {
	SchemaVersion int                `json:"schema_version"`
	Reference     registry.Reference `json:"reference"`
	Revision      string             `json:"revision"`
	CompletedAt   time.Time          `json:"completed_at"`
	Blobs         []StoredBlob       `json:"blobs"`
}

type run struct {
	store                                               storage.Store
	opts                                                Options
	slots                                               chan struct{}
	inflight                                            singleflight.Group
	completed, skipped, failed, uploaded, uploadedBytes atomic.Int64
}

// Download consumes saved references in scheduling order. It never calls registry
// discovery. With concurrent workers, start and completion times may differ from
// queue order; use Concurrency=1 for strict sequential processing.
// Individual failures are collected unless FailFast is set; any failure returns
// an error. Reruns skip completed manifests and stored content-addressed blobs.
func Download(ctx context.Context, entries registry.Entries, source registry.Resolver, store storage.Store, opts Options) (Report, error) {
	var report Report
	if entries == nil || source == nil || store == nil {
		return report, fmt.Errorf("entries, resolver and store are required")
	}
	if err := opts.Validate(); err != nil {
		return report, err
	}
	r := &run{store: store, opts: opts, slots: make(chan struct{}, opts.Downloads)}
	group, ctx := errgroup.WithContext(ctx)
	queue := make(chan registry.Reference, opts.QueueSize)
	var discovered atomic.Int64
	var firstErr error
	var firstMu sync.Mutex
	group.Go(func() error {
		defer close(queue)
		return entries(ctx, func(ref registry.Reference) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case queue <- ref:
				discovered.Add(1)
				return nil
			}
		})
	})
	for i := 0; i < opts.Concurrency; i++ {
		group.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ref, ok := <-queue:
					if !ok {
						return nil
					}
					err := r.archive(ctx, source, ref)
					if err != nil {
						r.failed.Add(1)
						err = fmt.Errorf("%s/%s@%s: %w", ref.Registry, ref.Name, ref.Version, err)
						firstMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						firstMu.Unlock()
						r.event(Event{Reference: ref, Status: "failed", Error: err})
						if opts.FailFast {
							return err
						}
					}
				}
			}
		})
	}
	err := group.Wait()
	report = Report{Scheduled: discovered.Load(), Completed: r.completed.Load(), Skipped: r.skipped.Load(), Failed: r.failed.Load(), UploadedBlobs: r.uploaded.Load(), UploadedBytes: r.uploadedBytes.Load()}
	if err != nil {
		return report, err
	}
	if firstErr != nil {
		return report, fmt.Errorf("%d resources failed; first failure: %w", report.Failed, firstErr)
	}
	return report, nil
}

// ManifestKey is deterministic, including source namespace, version, discovery
// attributes and resolved revision, so new resource versions get new manifests.
func ManifestKey(artifact registry.Artifact) string {
	ref := artifact.Reference
	// Listing popularity, repositories, timestamps, tags, authorship and raw records must
	// not trigger re-downloads.
	ref.Metadata, ref.Facts, ref.Tags, ref.Author, ref.Raw = nil, nil, nil, nil, nil
	identity, _ := json.Marshal(struct {
		Reference registry.Reference
		Revision  string
	}{ref, artifact.Revision})
	digest := sha256.Sum256(identity)
	return "manifests/" + hex.EncodeToString(digest[:]) + ".json"
}

func BlobKey(digest string) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	decoded, err := hex.DecodeString(hexDigest)
	if !strings.HasPrefix(digest, "sha256:") || err != nil || len(decoded) != sha256.Size || strings.ToLower(hexDigest) != hexDigest {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	return "blobs/sha256/" + hexDigest, nil
}

func (r *run) event(event Event) {
	if r.opts.OnEvent != nil {
		r.opts.OnEvent(event)
	}
}

func (r *run) archive(ctx context.Context, source registry.Resolver, ref registry.Reference) error {
	var artifact registry.Artifact
	err := retry.Do(ctx, r.opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
		var err error
		artifact, err = source.Resolve(attempt, ref)
		return err
	})
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	if artifact.Reference.Registry == "" || artifact.Reference.Name == "" || artifact.Revision == "" || len(artifact.Blobs) == 0 {
		return fmt.Errorf("adapter returned incomplete artifact")
	}
	key := ManifestKey(artifact)
	if !r.opts.Refresh {
		exists, _, err := r.store.Stat(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			r.skipped.Add(1)
			r.event(Event{Reference: ref, Status: "skipped", Key: key})
			return nil
		}
	}
	manifest := Manifest{SchemaVersion: 1, Reference: artifact.Reference, Revision: artifact.Revision, Blobs: make([]StoredBlob, len(artifact.Blobs))}
	var transfers errgroup.Group
	transfers.SetLimit(r.opts.Downloads)
	for i, blob := range artifact.Blobs {
		transfers.Go(func() error {
			stored, err := r.blob(ctx, blob)
			if err != nil {
				return fmt.Errorf("blob %s: %w", blob.Name, err)
			}
			stored.Name, stored.MediaType = blob.Name, blob.MediaType
			manifest.Blobs[i] = stored
			return nil
		})
	}
	if err := transfers.Wait(); err != nil {
		return err
	}
	manifest.CompletedAt = time.Now().UTC()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	err = retry.Do(ctx, r.opts.Attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
		return r.store.Put(attempt, key, bytes.NewReader(body), int64(len(body)), "application/json")
	})
	if err != nil {
		return fmt.Errorf("commit manifest: %w", err)
	}
	r.completed.Add(1)
	r.event(Event{Reference: ref, Status: "completed", Key: key})
	return nil
}

func (r *run) blob(ctx context.Context, blob registry.Blob) (StoredBlob, error) {
	if blob.Open == nil || blob.Size < -1 {
		return StoredBlob{}, fmt.Errorf("invalid blob descriptor")
	}
	if blob.Size > r.opts.MaxBlobBytes {
		return StoredBlob{}, fmt.Errorf("size %d exceeds maximum %d", blob.Size, r.opts.MaxBlobBytes)
	}
	transfer := func() (any, error) {
		var stored StoredBlob
		err := retry.Do(ctx, r.opts.Attempts, func() error {
			// A download slot is held only while an attempt is active, so a
			// blob waiting out its retry backoff does not idle global capacity.
			select {
			case r.slots <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			defer func() { <-r.slots }()
			attempt, cancel := context.WithTimeout(ctx, r.opts.Timeout)
			defer cancel()
			var err error
			stored, err = r.transfer(attempt, blob)
			return err
		})
		return stored, err
	}
	var result any
	var err error
	if blob.Digest != "" {
		if _, err := BlobKey(blob.Digest); err != nil {
			return StoredBlob{}, err
		}
		result, err, _ = r.inflight.Do(blob.Digest, transfer)
	} else {
		result, err = transfer()
	}
	if err != nil {
		return StoredBlob{}, err
	}
	stored := result.(StoredBlob)
	if blob.Size >= 0 && stored.Size >= 0 && blob.Size != stored.Size {
		return StoredBlob{}, fmt.Errorf("conflicting sizes for digest %s", blob.Digest)
	}
	return stored, nil
}

func (r *run) transfer(ctx context.Context, blob registry.Blob) (StoredBlob, error) {
	if blob.Digest != "" && blob.Size >= 0 {
		key, _ := BlobKey(blob.Digest)
		exists, size, err := r.store.Stat(ctx, key)
		if err != nil {
			return StoredBlob{}, err
		}
		if exists && (size < 0 || size == blob.Size) {
			return StoredBlob{Digest: blob.Digest, Size: blob.Size, Key: key}, nil
		}
		// A stored object with the wrong size is not trusted; it is replaced below.
	}
	file, err := os.CreateTemp(r.opts.TempDir, "dug-blob-*")
	if err != nil {
		return StoredBlob{}, retry.Permanent(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	body, err := blob.Open(ctx)
	if err != nil {
		return StoredBlob{}, err
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(body, r.opts.MaxBlobBytes+1), make([]byte, copyBufferBytes))
	closeErr := body.Close()
	if err != nil {
		return StoredBlob{}, err
	}
	if closeErr != nil {
		return StoredBlob{}, closeErr
	}
	if err := ctx.Err(); err != nil {
		return StoredBlob{}, err
	}
	if n > r.opts.MaxBlobBytes {
		return StoredBlob{}, retry.Permanent(fmt.Errorf("blob exceeds maximum %d bytes", r.opts.MaxBlobBytes))
	}
	if blob.Size >= 0 && n != blob.Size {
		return StoredBlob{}, fmt.Errorf("size mismatch: expected %d, received %d", blob.Size, n)
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if blob.Digest != "" && blob.Digest != digest {
		return StoredBlob{}, fmt.Errorf("checksum mismatch: expected %s, received %s", blob.Digest, digest)
	}
	key, _ := BlobKey(digest)
	exists, size, err := r.store.Stat(ctx, key)
	if err != nil {
		return StoredBlob{}, err
	}
	if !exists || (size >= 0 && size != n) {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return StoredBlob{}, err
		}
		if err := r.store.Put(ctx, key, file, n, "application/octet-stream"); err != nil {
			return StoredBlob{}, err
		}
		r.uploaded.Add(1)
		r.uploadedBytes.Add(n)
	}
	return StoredBlob{Digest: digest, Size: n, Key: key}, nil
}
