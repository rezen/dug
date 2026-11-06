package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage"
	"github.com/rezen/dug/services/storage/memstore"
)

type source struct {
	artifacts []registry.Artifact
	err       error
}

func (s source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	for _, a := range s.artifacts {
		if err := emit(a.Reference); err != nil {
			return err
		}
	}
	return s.err
}
func (s source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	for _, a := range s.artifacts {
		if a.Reference.Name == ref.Name {
			return a, nil
		}
	}
	return registry.Artifact{}, fmt.Errorf("not found")
}
func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func artifact(name, data string, opens *atomic.Int64) registry.Artifact {
	return registry.Artifact{Reference: registry.Reference{Registry: "test", Name: name, Version: "1"}, Revision: "1", Blobs: []registry.Blob{{Name: name, Size: int64(len(data)), Digest: digest(data), Open: func(ctx context.Context) (io.ReadCloser, error) {
		opens.Add(1)
		return io.NopCloser(strings.NewReader(data)), nil
	}}}}
}
func manifestKey(t *testing.T, a registry.Artifact) string {
	t.Helper()
	key, err := ManifestKey(a)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func blobKey(t *testing.T, a registry.Artifact) string {
	t.Helper()
	dir, err := ArtifactDir(a)
	if err != nil {
		t.Fatal(err)
	}
	return dir + "/" + a.Blobs[0].Name
}

func options(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.TempDir = t.TempDir()
	opts.Attempts = 1
	return opts
}

func TestResumeAndManifest(t *testing.T) {
	var opens atomic.Int64
	s := source{artifacts: []registry.Artifact{artifact("a", "shared payload", &opens), artifact("b", "shared payload", &opens)}}
	store := memstore.New()
	opts := options(t)
	report, err := runFixture(context.Background(), s, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Completed != 2 || report.UploadedBlobs != 2 || opens.Load() != 2 {
		t.Fatalf("report=%+v opens=%d", report, opens.Load())
	}
	for _, a := range s.artifacts {
		var m Manifest
		if err := json.Unmarshal(store.Objects()[manifestKey(t, a)], &m); err != nil {
			t.Fatal(err)
		}
		if m.SchemaVersion != 1 || len(m.Blobs) != 1 || m.Blobs[0].Name != a.Reference.Name || m.Blobs[0].Key != blobKey(t, a) || string(store.Objects()[m.Blobs[0].Key]) != "shared payload" {
			t.Fatalf("bad manifest %+v", m)
		}
	}
	report, err = runFixture(context.Background(), s, store, opts)
	if err != nil || report.Skipped != 2 || opens.Load() != 2 {
		t.Fatalf("resume: %+v %v opens=%d", report, err, opens.Load())
	}
}

func TestCorruptionAndOversizeNeverCommit(t *testing.T) {
	for _, kind := range []string{"checksum", "truncated", "oversize", "invalid-digest"} {
		t.Run(kind, func(t *testing.T) {
			var opens atomic.Int64
			a := artifact("bad", "payload", &opens)
			opts := options(t)
			switch kind {
			case "checksum":
				a.Blobs[0].Digest = digest("other")
			case "truncated":
				a.Blobs[0].Size++
			case "oversize":
				a.Blobs[0].Size = -1
				opts.MaxBlobBytes = 3
			case "invalid-digest":
				a.Blobs[0].Digest = "sha256:../../bad"
			}
			store := memstore.New()
			report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, opts)
			if err == nil || report.Failed != 1 || len(store.Objects()) != 0 {
				t.Fatalf("report=%+v err=%v objects=%v", report, err, store.Objects())
			}
		})
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenReader) Close() error             { return nil }
func TestRetriesInterruptedBody(t *testing.T) {
	var opens atomic.Int64
	a := artifact("retry", "complete", &opens)
	a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) {
		if opens.Add(1) == 1 {
			return brokenReader{}, nil
		}
		return io.NopCloser(strings.NewReader("complete")), nil
	}
	opts := options(t)
	opts.Attempts = 2
	report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, memstore.New(), opts)
	if err != nil || report.Completed != 1 || opens.Load() != 2 {
		t.Fatalf("report=%+v err=%v opens=%d", report, err, opens.Load())
	}
}

func TestGlobalConcurrencyBound(t *testing.T) {
	var active, peak, opens atomic.Int64
	s := source{}
	for i := 0; i < 16; i++ {
		data := fmt.Sprint(i)
		a := artifact(data, data, &opens)
		a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) {
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			return io.NopCloser(strings.NewReader(data)), nil
		}
		s.artifacts = append(s.artifacts, a)
	}
	opts := options(t)
	opts.Downloads = 3
	report, err := runFixture(context.Background(), s, memstore.New(), opts)
	if err != nil || report.Completed != 16 || peak.Load() > 3 || peak.Load() < 2 {
		t.Fatalf("report=%+v err=%v peak=%d", report, err, peak.Load())
	}
}

func TestCancellationStopsBlockedDiscovery(t *testing.T) {
	var opens atomic.Int64
	a := artifact("cancel", "payload", &opens)
	a.Blobs[0].Open = func(ctx context.Context) (io.ReadCloser, error) { <-ctx.Done(); return nil, ctx.Err() }
	s := source{artifacts: make([]registry.Artifact, 100)}
	for i := range s.artifacts {
		s.artifacts[i] = a
	}
	opts := options(t)
	opts.Concurrency = 1
	opts.QueueSize = 0
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runFixture(ctx, s, memstore.New(), opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestPartialFailureCanResume(t *testing.T) {
	var opens atomic.Int64
	a := artifact("one", "first", &opens)
	b := artifact("two", "second", &opens)
	valid := b.Blobs[0].Digest
	b.Blobs[0].Digest = digest("wrong")
	store := memstore.New()
	opts := options(t)
	report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a, b}}, store, opts)
	if err == nil || report.Completed != 1 || report.Failed != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	b.Blobs[0].Digest = valid
	report, err = runFixture(context.Background(), source{artifacts: []registry.Artifact{a, b}}, store, opts)
	if err != nil || report.Completed != 1 || report.Skipped != 1 {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestRefreshUnknownDigest(t *testing.T) {
	var opens atomic.Int64
	a := artifact("mutable", "old", &opens)
	a.Blobs[0].Digest = ""
	store := memstore.New()
	opts := options(t)
	if _, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, opts); err != nil {
		t.Fatal(err)
	}
	a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("new")), nil }
	opts.Refresh = true
	if _, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, opts); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(store.Objects()[manifestKey(t, a)], &m); err != nil {
		t.Fatal(err)
	}
	if m.Blobs[0].Digest != digest("new") {
		t.Fatalf("refresh did not replace manifest: %+v", m)
	}
}

// Engine fixtures supply saved references; the production downloader has no discovery capability.
func runFixture(ctx context.Context, source registry.Registry, store storage.Store, opts Options) (Report, error) {
	return Download(ctx, source.Discover, source, store, opts)
}

func TestSchedulingMetadataDoesNotChangeCompletionIdentity(t *testing.T) {
	var opens atomic.Int64
	a := artifact("same-version", "payload", &opens)
	key := manifestKey(t, a)
	a.Reference.Metadata = map[string]string{"stars": "999", "rating": "80"}
	if manifestKey(t, a) != key {
		t.Fatal("mutable scheduling metadata changed resource identity")
	}
	a.Reference.Facts = &registry.Facts{Downloads: &registry.DownloadStats{Total: registry.Int64(999)}, Installs: registry.Int64(5), Repository: &registry.Repository{URL: "https://github.com/acme/widget", Source: registry.RepositoryRegistry}, UpdatedAt: registry.Time(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))}
	if manifestKey(t, a) != key {
		t.Fatal("mutable typed facts changed resource identity")
	}
	a.Reference.Tags = []string{"security", "seo"}
	a.Reference.Author = &registry.Author{Username: "acme", ID: "7"}
	if manifestKey(t, a) != key {
		t.Fatal("tags or author changed resource identity")
	}
	a.Reference.Raw = []byte(`{"downloads":999}`)
	if manifestKey(t, a) != key {
		t.Fatal("raw record changed resource identity")
	}
}

func TestTruncatedStoredBlobIsReplaced(t *testing.T) {
	for _, sizeKnown := range []bool{true, false} {
		t.Run(fmt.Sprintf("size-known=%v", sizeKnown), func(t *testing.T) {
			var opens atomic.Int64
			a := artifact("trusted", "complete payload", &opens)
			if !sizeKnown {
				a.Blobs[0].Size = -1
			}
			store := memstore.New()
			key := blobKey(t, a)
			if err := store.Put(context.Background(), key, strings.NewReader("complete"), 8, "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, options(t))
			if err != nil || report.Completed != 1 || report.UploadedBlobs != 1 || opens.Load() != 1 {
				t.Fatalf("report=%+v err=%v opens=%d", report, err, opens.Load())
			}
			if string(store.Objects()[key]) != "complete payload" {
				t.Fatal("a truncated stored blob was trusted on key existence alone")
			}
		})
	}
}

// gatedSource delays one resource's resolution until gate closes, so a test
// can control the order in which resources compete for download slots.
type gatedSource struct {
	source
	gate  chan struct{}
	gated string
}

func (g gatedSource) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Name == g.gated {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return registry.Artifact{}, ctx.Err()
		}
	}
	return g.source.Resolve(ctx, ref)
}

func TestSlotReleasedDuringRetryBackoff(t *testing.T) {
	var opens, aOpens, aRetry, bOpen atomic.Int64
	aFailed := make(chan struct{})
	a := artifact("a", "payload a", &opens)
	a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) {
		if aOpens.Add(1) == 1 {
			close(aFailed)
			return nil, errors.New("transient upstream failure")
		}
		aRetry.Store(time.Now().UnixNano())
		return io.NopCloser(strings.NewReader("payload a")), nil
	}
	b := artifact("b", "payload b", &opens)
	b.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) {
		bOpen.Store(time.Now().UnixNano())
		return io.NopCloser(strings.NewReader("payload b")), nil
	}
	opts := options(t)
	opts.Downloads, opts.Concurrency, opts.Attempts = 1, 2, 2
	src := gatedSource{source: source{artifacts: []registry.Artifact{a, b}}, gate: aFailed, gated: "b"}
	report, err := runFixture(context.Background(), src, memstore.New(), opts)
	if err != nil || report.Completed != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if bOpen.Load() == 0 || aRetry.Load() == 0 || bOpen.Load() > aRetry.Load() {
		t.Fatalf("b opened at %d, after a's retry at %d: the only download slot was held through a's backoff", bOpen.Load(), aRetry.Load())
	}
}

func TestLargeBlobSpansCopyBuffers(t *testing.T) {
	var opens atomic.Int64
	data := strings.Repeat("0123456789abcdef", 3<<16) // 3 MiB, several 1 MiB copy buffers
	a := artifact("large", data, &opens)
	store := memstore.New()
	report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, options(t))
	if err != nil || report.UploadedBytes != int64(len(data)) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	key := blobKey(t, a)
	if string(store.Objects()[key]) != data {
		t.Fatal("stored bytes differ")
	}
}

func TestObjectsLiveUnderRegistryTypeDirectory(t *testing.T) {
	data := "zip bytes"
	open := func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil }
	cases := []struct {
		ref  registry.Reference
		want string
	}{
		{registry.Reference{Registry: "wordpress", Name: "akismet", Version: "5.3"}, "wordpress/plugins/artifacts/akismet/5.3/"},
		{registry.Reference{Registry: "wordpress-themes", Name: "astra", Version: "4.0"}, "wordpress/themes/artifacts/astra/4.0/"},
		{registry.Reference{Registry: "github", Name: "cli/cli", Version: "feature/x"}, "github/repos/artifacts/cli/cli/feature%2Fx/"},
	}
	for _, c := range cases {
		a := registry.Artifact{Reference: c.ref, Revision: "r", Blobs: []registry.Blob{{Name: "pkg.zip", Size: int64(len(data)), Open: open}}}
		store := memstore.New()
		if _, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, options(t)); err != nil {
			t.Fatal(err)
		}
		if string(store.Objects()[c.want+"pkg.zip"]) != data || len(store.Objects()[c.want+"manifest.json"]) == 0 || len(store.Objects()) != 2 {
			t.Fatalf("%s: objects=%v", c.want, store.Objects())
		}
	}
}

func TestChangedRevisionReplacesVersionInPlace(t *testing.T) {
	var opens atomic.Int64
	a := artifact("moving", "old", &opens)
	store := memstore.New()
	opts := options(t)
	if _, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, opts); err != nil {
		t.Fatal(err)
	}
	b := artifact("moving", "newer", &opens)
	b.Revision = "2"
	report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{b}}, store, opts)
	if err != nil || report.Completed != 1 || report.Skipped != 0 {
		t.Fatalf("%+v %v", report, err)
	}
	if string(store.Objects()[blobKey(t, b)]) != "newer" {
		t.Fatal("changed revision kept the old bytes")
	}
}

func TestUnsafeKeyPartsAreRejected(t *testing.T) {
	var opens atomic.Int64
	for name, mutate := range map[string]func(*registry.Artifact){
		"dotdot-name":     func(a *registry.Artifact) { a.Reference.Name = "a/../b" },
		"dotdot-version":  func(a *registry.Artifact) { a.Reference.Version = ".." },
		"reserved-blob":   func(a *registry.Artifact) { a.Blobs[0].Name = "manifest.json" },
		"empty-blob":      func(a *registry.Artifact) { a.Blobs[0].Name = "" },
		"dotdot-registry": func(a *registry.Artifact) { a.Reference.Registry = ".." },
	} {
		t.Run(name, func(t *testing.T) {
			a := artifact("ok", "payload", &opens)
			mutate(&a)
			store := memstore.New()
			report, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, options(t))
			if err == nil || report.Failed != 1 || len(store.Objects()) != 0 {
				t.Fatalf("report=%+v err=%v objects=%v", report, err, store.Objects())
			}
		})
	}
}

func TestBlobNameCannotAddPathLevels(t *testing.T) {
	var opens atomic.Int64
	a := artifact("ok", "payload", &opens)
	a.Blobs[0].Name = "../x"
	store := memstore.New()
	if _, err := runFixture(context.Background(), source{artifacts: []registry.Artifact{a}}, store, options(t)); err != nil {
		t.Fatal(err)
	}
	dir, _ := ArtifactDir(a)
	if _, ok := store.Objects()[dir+"/..%2Fx"]; !ok || len(store.Objects()) != 2 {
		t.Fatalf("objects=%v", store.Objects())
	}
}
