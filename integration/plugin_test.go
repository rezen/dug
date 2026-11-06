package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rezen/dug/engine"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage/memstore"
)

type fixture struct {
	discover func(context.Context, func(registry.Reference) error) error
	resolve  func(context.Context, registry.Reference) (registry.Artifact, error)
}

func (s fixture) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	return s.discover(ctx, emit)
}
func (s fixture) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	return s.resolve(ctx, ref)
}

func reference() registry.Reference {
	return registry.Reference{Registry: "test", Name: "archive", Version: "1"}
}
func artifact(data string) registry.Artifact {
	sum := sha256.Sum256([]byte(data))
	return registry.Artifact{Reference: reference(), Revision: "1", Blobs: []registry.Blob{{Name: "archive.zip", Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data)), Open: func(ctx context.Context) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(data)), nil
	}}}}
}
func serve(t *testing.T, source registry.Registry, token string) (*plugin.Client, *httptest.Server) {
	t.Helper()
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := plugin.NewClient(context.Background(), server.Client(), server.URL, plugin.ClientOptions{Registry: "test", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestRemoteEngineArchiveAndResume(t *testing.T) {
	data := strings.Repeat("zip bytes", 100000) // spans multiple protocol chunks
	var opens atomic.Int64
	source := fixture{
		discover: func(ctx context.Context, emit func(registry.Reference) error) error { return emit(reference()) },
		resolve: func(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
			a := artifact(data)
			a.Blobs[0].Open = func(ctx context.Context) (io.ReadCloser, error) {
				opens.Add(1)
				return io.NopCloser(strings.NewReader(data)), ctx.Err()
			}
			return a, nil
		},
	}
	client, _ := serve(t, source, "secret")
	store := memstore.New()
	opts := engine.DefaultOptions()
	opts.TempDir = t.TempDir()
	opts.Attempts = 1
	for i := 0; i < 2; i++ {
		report, err := engine.Download(context.Background(), func(ctx context.Context, emit func(registry.Reference) error) error { return emit(reference()) }, client, store, opts)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (report.Completed != 1 || report.UploadedBytes != int64(len(data))) {
			t.Fatalf("report=%+v", report)
		}
		if i == 1 && report.Skipped != 1 {
			t.Fatalf("resume=%+v", report)
		}
	}
	if opens.Load() != 1 {
		t.Fatalf("downloads=%d", opens.Load())
	}
	key, _ := engine.BlobKey(artifact(data).Blobs[0].Digest)
	if string(store.Objects()[key]) != data {
		t.Fatal("stored data differs")
	}
}

type failedReader struct{ sent bool }

func (r *failedReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("upstream interrupted")
	}
	r.sent = true
	return copy(p, "partial"), nil
}
func (r *failedReader) Close() error { return nil }

func TestStreamErrorNeverCommitsPartialArtifact(t *testing.T) {
	source := fixture{
		discover: func(ctx context.Context, emit func(registry.Reference) error) error { return emit(reference()) },
		resolve: func(context.Context, registry.Reference) (registry.Artifact, error) {
			a := artifact("")
			a.Blobs[0].Size = -1
			a.Blobs[0].Digest = ""
			a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) { return &failedReader{}, nil }
			return a, nil
		},
	}
	client, _ := serve(t, source, "")
	store := memstore.New()
	opts := engine.DefaultOptions()
	opts.TempDir = t.TempDir()
	opts.Attempts = 1
	report, err := engine.Download(context.Background(), func(ctx context.Context, emit func(registry.Reference) error) error { return emit(reference()) }, client, store, opts)
	if err == nil || report.Failed != 1 || len(store.Objects()) != 0 {
		t.Fatalf("report=%+v err=%v objects=%d", report, err, len(store.Objects()))
	}
}
