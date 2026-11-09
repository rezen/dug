package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezen/dug/events"
	"github.com/rezen/dug/inventory"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage"
	"github.com/rezen/dug/services/storage/memstore"
)

type discoveryFixture struct{ registry.Registry }

func (discoveryFixture) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	for i := 0; i < 10; i++ {
		if err := emit(registry.Reference{Registry: "wordpress", Name: "example", Version: "1"}); err != nil {
			return err
		}
	}
	return nil
}

type stagedFixture struct {
	mu        sync.Mutex
	listCalls int
	downloads []string
}

func (s *stagedFixture) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	s.mu.Lock()
	s.listCalls++
	calls := s.listCalls
	s.mu.Unlock()
	if calls != 1 {
		return fmt.Errorf("stage two must not rediscover")
	}
	for _, name := range []string{"z", "a", "m"} {
		facts := &registry.Facts{Downloads: &registry.DownloadStats{Total: registry.Int64(map[string]int64{"z": 10, "a": 30, "m": 20}[name])}, UpdatedAt: registry.Time(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))}
		if err := emit(registry.Reference{Registry: "test", Name: name, Version: "1", Facts: facts}); err != nil {
			return err
		}
	}
	return nil
}

func TestSyncFiltersAndLimitsAfterGlobalOrdering(t *testing.T) {
	source := &stagedFixture{}
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	store := memstore.New()
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }
	var out, logs bytes.Buffer
	args := []string{"sync", "test", "--bucket", "test", "--plugin-url", server.URL, "--where", "downloads > 15 AND updated >= 2025-01-01", "--order", "downloads", "--limit", "1", "--concurrency", "1"}
	if err := executeWithStore(context.Background(), args, &out, &logs, factory); err != nil {
		t.Fatal(err)
	}
	var report struct {
		InventoryKey string `json:"inventory_key"`
		Scheduled    int    `json:"scheduled"`
	}
	if err := json.NewDecoder(&out).Decode(&report); err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Open(context.Background(), store, report.InventoryKey, inventory.Order{}, inventory.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot.Summary.Entries != 3 || report.Scheduled != 1 {
		t.Fatalf("inventory=%d scheduled=%d", snapshot.Summary.Entries, report.Scheduled)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !slices.Equal(source.downloads, []string{"a"}) {
		t.Fatalf("wrong selection %v", source.downloads)
	}
}
func (s *stagedFixture) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	return registry.Artifact{Reference: ref, Revision: ref.Version, Blobs: []registry.Blob{{Name: ref.Name, Size: 1, Open: func(context.Context) (io.ReadCloser, error) {
		s.mu.Lock()
		s.downloads = append(s.downloads, ref.Name)
		s.mu.Unlock()
		return io.NopCloser(strings.NewReader(ref.Name)), nil
	}}}}, nil
}

func TestSeparateStagesDownloadInOrderWithoutRediscovery(t *testing.T) {
	source := &stagedFixture{}
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(options map[string]string) (registry.Registry, error) {
		if options["query"] != "saved" {
			return nil, fmt.Errorf("saved options missing")
		}
		return source, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	store := memstore.New()
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }
	var out, logs bytes.Buffer
	args := []string{"list", "test", "--format", "summary", "--inventory-dir", t.TempDir(), "--bucket", "test", "--plugin-url", server.URL, "--option", "query=saved"}
	if err := executeWithStore(context.Background(), args, &out, &logs, factory); err != nil {
		t.Fatal(err)
	}
	var summary inventory.Summary
	if err := json.NewDecoder(&out).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	downloaded := len(source.downloads)
	source.mu.Unlock()
	// One inventory part, its complete.json, and the last_run pointer.
	if downloaded != 0 || len(store.Objects()) != 3 {
		t.Fatal("stage one downloaded artifacts")
	}
	args = []string{"download", "test", "--bucket", "test", "--plugin-url", server.URL, "--inventory", summary.Key, "--order", "name", "--concurrency", "1", "--downloads", "1"}
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := executeWithStore(context.Background(), args, &out, &logs, factory); err != nil {
			t.Fatal(err)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.listCalls != 1 || !slices.Equal(source.downloads, []string{"a", "m", "z"}) {
		t.Fatalf("listing=%d download order=%v", source.listCalls, source.downloads)
	}
}

func TestListUsesExternalPluginOptionsAndLimit(t *testing.T) {
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "wordpress", Factory: func(options map[string]string) (registry.Registry, error) {
		if options["search"] != "shop" {
			return nil, fmt.Errorf("missing plugin query option")
		}
		return discoveryFixture{}, nil
	}, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	t.Setenv("DUG_PLUGIN_TOKEN", "test-token")
	var out, logs bytes.Buffer
	store := memstore.New()
	if err := executeWithStore(context.Background(), []string{"list", "wordpress", "--format", "summary", "--inventory-dir", t.TempDir(), "--bucket", "test", "--plugin-url", server.URL, "--option", "search=shop", "--limit", "2"}, &out, &logs, func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }); err != nil {
		t.Fatal(err)
	}
	var summary inventory.Summary
	if err := json.NewDecoder(&out).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.Entries != 2 || summary.Key == "" {
		t.Fatalf("summary=%+v", summary)
	}
	opts := inventory.DefaultOptions()
	opts.TempDir = t.TempDir()
	snapshot, err := inventory.Open(context.Background(), store, summary.Key, inventory.Order{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	count := 0
	if err := snapshot.Entries(context.Background(), func(registry.Reference) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if snapshot.Header.Options["search"] != "shop" || snapshot.Header.Limit != 2 {
		t.Fatalf("header=%+v", snapshot.Header)
	}
	if count != 2 {
		t.Fatalf("expected two resources, got %d", count)
	}
}

func TestInvalidCLI(t *testing.T) {
	t.Setenv("DUG_BUCKET", "")
	for _, args := range [][]string{{"sync", "wordpress", "--concurrency", "0"}, {"list", "wordpress", "--option", "invalid"}, {"list", "wordpress", "--option", "search=x", "--option", "search=y"}, {"sync", "wordpress"}, {"download", "wordpress", "--bucket", "test"}, {"download", "wordpress", "--bucket", "test", "--inventory", "x", "--option", "search=x"}} {
		var out bytes.Buffer
		err := execute(context.Background(), args, &out, &out)
		var misuse usageError
		if err == nil || !errors.As(err, &misuse) {
			t.Fatalf("expected a usage error for %v, got %v", args, err)
		}
	}
}

func TestLocalDefaultsAndExplicitBucket(t *testing.T) {
	t.Setenv("DUG_BUCKET", "production")
	t.Setenv("DUG_S3_ENDPOINT", "https://production.example.com")
	t.Setenv("AWS_REGION", "eu-west-1")
	for _, bucket := range []string{"dug-e2e", "my-local-corpus"} {
		args := []string{"local", "list", "test"}
		if bucket != "dug-e2e" {
			args = append(args, "--bucket", bucket)
		}
		called := false
		stop := fmt.Errorf("stop before discovery")
		err := executeWithStore(context.Background(), args, io.Discard, io.Discard, func(_ context.Context, opts storage.S3Options) (storage.ReadableStore, error) {
			called = true
			if opts.Bucket != bucket || opts.Endpoint != "http://127.0.0.1:9000" || opts.Region != "us-east-1" || !opts.PathStyle {
				t.Fatalf("wrong local options: %+v", opts)
			}
			return nil, stop
		})
		if !called || err != stop {
			t.Fatalf("called=%v error=%v", called, err)
		}
	}
}

func TestLocalHelpAndInvalidArgumentsDoNotStartS3(t *testing.T) {
	for _, args := range [][]string{
		{"local"}, {"local", "--help"}, {"local", "sync", "wordpress", "--help"},
		{"local", "sync", "wordpress", "--endpoint", "https://example.com"},
		{"local", "sync", "wordpress", "--region", "eu-west-1"},
		{"local", "sync", "wordpress", "--path-style=false"},
		{"local", "sync", "wordpress", "--where", "invalid"},
		{"local", "list", "wordpress", "--format", "invalid"},
		{"local", "list", "wordpress", "--inventory-dir="},
	} {
		_ = executeWithStore(context.Background(), args, io.Discard, io.Discard, func(context.Context, storage.S3Options) (storage.ReadableStore, error) {
			t.Fatalf("started S3 for %v", args)
			return nil, nil
		})
	}
}

func TestSyncReportsProgressUnlessQuiet(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		source := &stagedFixture{}
		handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		store := memstore.New()
		factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }
		args := []string{"sync", "test", "--bucket", "test", "--plugin-url", server.URL, "--concurrency", "1"}
		if quiet {
			args = append(args, "--quiet")
		}
		var out, logs bytes.Buffer
		err = executeWithStore(context.Background(), args, &out, &logs, factory)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		text := logs.String()
		for _, want := range []string{"listing started", "download started"} {
			if strings.Contains(text, want) == quiet {
				t.Fatalf("quiet=%v: %q presence wrong in logs:\n%s", quiet, want, text)
			}
		}
		if !strings.Contains(text, "inventory complete") {
			t.Fatalf("quiet=%v: inventory key must always be logged:\n%s", quiet, text)
		}
	}
}

type countingFixture struct {
	mu    sync.Mutex
	lists int
}

func (c *countingFixture) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return emit(registry.Reference{Registry: "test", Name: "a", Version: "1"})
}

func (c *countingFixture) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	return registry.Artifact{Reference: ref, Revision: ref.Version, Blobs: []registry.Blob{{Name: ref.Name, Size: 1, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("a")), nil
	}}}}, nil
}

func TestSyncReusesLastRunUnlessRediscovering(t *testing.T) {
	source := &countingFixture{}
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	store := memstore.New()
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }
	sync := func(extra ...string) string {
		t.Helper()
		var out, logs bytes.Buffer
		args := append([]string{"sync", "test", "--bucket", "test", "--plugin-url", server.URL, "--concurrency", "1"}, extra...)
		if err := executeWithStore(context.Background(), args, &out, &logs, factory); err != nil {
			t.Fatal(err)
		}
		return logs.String()
	}
	lists := func() int {
		source.mu.Lock()
		defer source.mu.Unlock()
		return source.lists
	}
	sync()
	if lists() != 1 {
		t.Fatalf("first sync lists=%d", lists())
	}
	if logs := sync(); lists() != 1 || !strings.Contains(logs, "reusing last run") {
		t.Fatalf("second sync should reuse last run: lists=%d", lists())
	}
	// A different query must not pick up another query's pointer.
	sync("--option", "query=other")
	if lists() != 2 {
		t.Fatalf("different options lists=%d", lists())
	}
	sync("--rediscover")
	if lists() != 3 {
		t.Fatalf("--rediscover lists=%d", lists())
	}
	// The refreshed pointer is the new default.
	sync()
	if lists() != 3 {
		t.Fatalf("sync after rediscover lists=%d", lists())
	}
}

func TestRediscoverRejectedOutsideSync(t *testing.T) {
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return memstore.New(), nil }
	for _, args := range [][]string{
		{"list", "test", "--bucket", "test", "--rediscover"},
		{"download", "test", "--bucket", "test", "--inventory", "x", "--rediscover"},
		{"sync", "test", "--bucket", "test", "--inventory", "x", "--rediscover"},
	} {
		if err := executeWithStore(context.Background(), args, io.Discard, io.Discard, factory); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestSyncRecordsDiscoveryAndDownloadEvents(t *testing.T) {
	source := &stagedFixture{}
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "test", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	var hooked sync.Map
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Events []events.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		for _, event := range payload.Events {
			hooked.Store(string(event.Kind)+"/"+event.Reference.Name, true)
		}
	}))
	defer hook.Close()
	store := memstore.New()
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }
	db := filepath.Join(t.TempDir(), "events.db")
	var out, logs bytes.Buffer
	args := []string{"sync", "test", "--bucket", "test", "--plugin-url", server.URL, "--concurrency", "1", "--events-db", db, "--events-webhook", hook.URL}
	if err := executeWithStore(context.Background(), args, &out, &logs, factory); err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	sink, err := events.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	counts := map[string]int64{}
	var rows []events.Row
	if err := sink.DB().Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		counts[row.Kind]++
	}
	if counts["discovered"] != 3 || counts["downloaded"] != 3 || len(rows) != 6 {
		t.Fatalf("sqlite counts = %v", counts)
	}
	for _, key := range []string{"discovered/a", "downloaded/z"} {
		if _, ok := hooked.Load(key); !ok {
			t.Errorf("webhook missed %s", key)
		}
	}
}

func TestInvalidEventSettings(t *testing.T) {
	factory := func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return memstore.New(), nil }
	for _, args := range [][]string{
		{"list", "test", "--bucket", "b", "--events-batch-size", "0"},
		{"list", "test", "--bucket", "b", "--events-webhook", "ftp://x"},
	} {
		err := executeWithStore(context.Background(), args, io.Discard, io.Discard, factory)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("%v: err = %v, want usage error", args, err)
		}
	}
}
