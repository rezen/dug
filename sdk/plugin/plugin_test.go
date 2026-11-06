package plugin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
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

func TestDiscoveryCallbackCancelsRemoteStream(t *testing.T) {
	stopped := make(chan struct{})
	source := fixture{discover: func(ctx context.Context, emit func(registry.Reference) error) error {
		defer close(stopped)
		if err := emit(reference()); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	client, _ := serve(t, source, "")
	stop := errors.New("enough")
	if err := client.Discover(context.Background(), func(registry.Reference) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("remote discovery was not cancelled")
	}
}

type waitingReader struct {
	ctx              context.Context
	started, stopped chan struct{}
	sent             bool
}

func (r *waitingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "first chunk"), nil
	}
	close(r.started)
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r *waitingReader) Close() error { close(r.stopped); return nil }

func TestDownloadCancellationClosesUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	source := fixture{resolve: func(context.Context, registry.Reference) (registry.Artifact, error) {
		a := artifact("")
		a.Blobs[0].Open = func(ctx context.Context) (io.ReadCloser, error) {
			return &waitingReader{ctx: ctx, started: started, stopped: stopped}, nil
		}
		return a, nil
	}}
	client, _ := serve(t, source, "")
	a, err := client.Resolve(context.Background(), reference())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, err := a.Blobs[0].Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected stream cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client read blocked")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream body not closed")
	}
}

func TestRevisionDriftAndHandshake(t *testing.T) {
	var revision atomic.Int64
	revision.Store(1)
	source := fixture{resolve: func(context.Context, registry.Reference) (registry.Artifact, error) {
		a := artifact("data")
		a.Revision = fmt.Sprint(revision.Load())
		return a, nil
	}}
	client, server := serve(t, source, "secret")
	for _, opts := range []plugin.ClientOptions{{Registry: "test"}, {Registry: "wrong", Token: "secret"}} {
		if _, err := plugin.NewClient(context.Background(), server.Client(), server.URL, opts); err == nil {
			t.Fatalf("handshake accepted %+v", opts)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := client.Resolve(ctx, reference())
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	// Open uses the new transfer context, even though resolution was cancelled.
	body, err := a.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(data) != "data" {
		t.Fatalf("data=%s err=%v", data, err)
	}
	revision.Store(2)
	body, err = a.Blobs[0].Open(context.Background())
	if err == nil {
		_, err = io.ReadAll(body)
		body.Close()
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected revision drift failure, got %v", err)
	}
}

func TestPermanentUpstreamErrorsAreNotRetriedAcrossTheWire(t *testing.T) {
	var opens atomic.Int64
	source := fixture{resolve: func(context.Context, registry.Reference) (registry.Artifact, error) {
		a := artifact("data")
		a.Blobs[0].Open = func(context.Context) (io.ReadCloser, error) {
			opens.Add(1)
			return nil, retry.Permanent(fmt.Errorf("GET https://example.invalid/x.zip: HTTP 404"))
		}
		return a, nil
	}}
	client, _ := serve(t, source, "")
	a, err := client.Resolve(context.Background(), reference())
	if err != nil {
		t.Fatal(err)
	}
	err = retry.Do(context.Background(), 4, func() error {
		body, err := a.Blobs[0].Open(context.Background())
		if err != nil {
			return err
		}
		_, err = io.ReadAll(body)
		body.Close()
		return err
	})
	if err == nil || !retry.IsPermanent(err) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("permanent verdict lost across the wire: %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("upstream opened %d times for a permanent failure", opens.Load())
	}
}

func TestBlobNamesMustBePlainFileNames(t *testing.T) {
	for _, name := range []string{"../escape.zip", "sub/dir.zip", `back\slash.zip`, ".", ".."} {
		source := fixture{resolve: func(context.Context, registry.Reference) (registry.Artifact, error) {
			a := artifact("data")
			a.Blobs[0].Name = name
			return a, nil
		}}
		client, _ := serve(t, source, "")
		if _, err := client.Resolve(context.Background(), reference()); err == nil {
			t.Errorf("accepted blob name %q", name)
		}
	}
}

func TestTokenIsNotSentInPlaintextToRemoteHosts(t *testing.T) {
	_, err := plugin.NewClient(context.Background(), &http.Client{}, "http://plugin.internal:7781", plugin.ClientOptions{Registry: "test", Token: "secret"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("token accepted over plaintext to a remote host: %v", err)
	}
}

func TestTypedFactsSurviveTheWireAndAreValidated(t *testing.T) {
	released := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	facts := &registry.Facts{
		Downloads:        &registry.DownloadStats{Total: registry.Int64(1000), Week: registry.Int64(0)},
		VersionDownloads: &registry.DownloadStats{Total: registry.Int64(12)},
		Installs:         registry.Int64(300),
		Repository:       &registry.Repository{URL: "https://github.com/acme/widget", Directory: "packages/widget", Commit: "0123abc", Source: registry.RepositoryAttested},
		ReleasedAt:       registry.Time(released),
		UpdatedAt:        registry.Time(released.Add(time.Hour)),
	}
	var served registry.Reference
	source := fixture{
		discover: func(ctx context.Context, emit func(registry.Reference) error) error {
			ref := reference()
			ref.Facts, ref.Tags, ref.Author, ref.Raw = served.Facts, served.Tags, served.Author, served.Raw
			return emit(ref)
		},
		resolve: func(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
			a := artifact("payload")
			a.Reference = ref
			return a, nil
		},
	}
	client, _ := serve(t, source, "")
	served.Facts = facts
	served.Tags = []string{"privacy", "security"}
	served.Author = &registry.Author{Name: "Acme", Username: "acme", ID: "7", URL: "https://github.com/acme", Email: "dev@example.org"}
	served.Raw = json.RawMessage(`{"slug": "widget", "extra": [1, 2]}`)
	var got []registry.Reference
	if err := client.Discover(context.Background(), func(ref registry.Reference) error { got = append(got, ref); return nil }); err != nil {
		t.Fatal(err)
	}
	want := reference()
	want.Facts, want.Tags, want.Author, want.Raw = facts, served.Tags, served.Author, served.Raw
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got[0].Facts == facts || got[0].Facts.Downloads == facts.Downloads || got[0].Author == served.Author || &got[0].Tags[0] == &served.Tags[0] {
		t.Fatal("wire conversion aliased the plugin's facts")
	}
	resolved, err := client.Resolve(context.Background(), want)
	if err != nil || !reflect.DeepEqual(resolved.Reference, want) {
		t.Fatalf("resolve reference %+v err=%v", resolved.Reference, err)
	}
	served.Raw = json.RawMessage(`<not json>`)
	if err := client.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "raw") {
		t.Fatalf("non-JSON raw record crossed the wire: %v", err)
	}
	served.Raw = want.Raw
	served.Tags = []string{"security", "privacy"}
	if err := client.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("unsorted tags crossed the wire: %v", err)
	}
	served.Tags = nil
	served.Author = &registry.Author{}
	if err := client.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "author") {
		t.Fatalf("empty author crossed the wire: %v", err)
	}
	served.Author = nil
	served.Facts = &registry.Facts{Downloads: &registry.DownloadStats{Total: registry.Int64(-1)}}
	if err := client.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative counts crossed the wire: %v", err)
	}
	served.Facts = &registry.Facts{Repository: &registry.Repository{URL: "https://example.org/r", Source: "rumor"}}
	if err := client.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("unknown repository source crossed the wire: %v", err)
	}
}
