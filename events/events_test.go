package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func ref(name string) registry.Reference {
	return registry.Reference{Registry: "npm", Name: name, Version: "1.0.0", Raw: json.RawMessage(`{"big":true}`)}
}

type collector struct {
	mu      sync.Mutex
	batches [][]Event
}

func (c *collector) Handle(_ context.Context, batch []Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches = append(c.batches, append([]Event(nil), batch...))
	return nil
}

func (c *collector) all() (events []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.batches {
		events = append(events, b...)
	}
	return
}

func TestBatchesByKindAndDrainsOnClose(t *testing.T) {
	bus := New(nil)
	all, failed := &collector{}, &collector{}
	if err := bus.Subscribe("all", all, SubscribeOptions{BatchSize: 3, FlushInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe("failed", failed, SubscribeOptions{Kinds: []Kind{Failed}}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe("all", all, SubscribeOptions{}); err == nil {
		t.Fatal("duplicate subscriber accepted")
	}
	for i := range 7 {
		kind := Discovered
		if i == 6 {
			kind = Failed
		}
		bus.Publish(Event{Kind: kind, Reference: ref("a")})
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(all.all()); got != 7 {
		t.Fatalf("all got %d events, want 7", got)
	}
	if len(all.batches) != 3 || len(all.batches[0]) != 3 || len(all.batches[2]) != 1 {
		t.Fatalf("batches = %v", len(all.batches))
	}
	if got := failed.all(); len(got) != 1 || got[0].Kind != Failed || got[0].Time.IsZero() {
		t.Fatalf("failed subscriber got %+v", got)
	}
	bus.Publish(Event{Kind: Discovered}) // after Close: ignored, must not panic
}

func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	release := make(chan struct{})
	var delivered atomic.Int64
	bus := New(nil)
	slow := HandlerFunc(func(ctx context.Context, batch []Event) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		delivered.Add(int64(len(batch)))
		return nil
	})
	if err := bus.Subscribe("slow", slow, SubscribeOptions{BatchSize: 1, Buffer: 5}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range 1000 {
		bus.Publish(Event{Kind: Discovered, Reference: ref("a")})
	}
	if time.Since(start) > time.Second {
		t.Fatalf("publishing blocked for %s", time.Since(start))
	}
	close(release)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := bus.Stats()[0]
	if stats.Dropped == 0 || stats.Delivered+stats.Dropped != 1000 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestCloseDeadlineCancelsStuckHandler(t *testing.T) {
	bus := New(nil)
	stuck := HandlerFunc(func(ctx context.Context, _ []Event) error { <-ctx.Done(); return ctx.Err() })
	if err := bus.Subscribe("stuck", stuck, SubscribeOptions{BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	bus.Publish(Event{Kind: Discovered})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := bus.Close(ctx); err == nil {
		t.Fatal("Close should report the deadline")
	}
}

func TestHandlerErrorIsReportedAndCounted(t *testing.T) {
	var lost atomic.Int64
	bus := New(func(_ string, _ error, n int) { lost.Add(int64(n)) })
	bad := HandlerFunc(func(context.Context, []Event) error { return io.ErrUnexpectedEOF })
	if err := bus.Subscribe("bad", bad, SubscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	bus.Publish(Event{Kind: Failed})
	bus.Publish(Event{Kind: Failed})
	bus.Close(context.Background())
	if lost.Load() != 2 || bus.Stats()[0].Failed != 2 {
		t.Fatalf("lost=%d stats=%+v", lost.Load(), bus.Stats())
	}
}

func TestNilBusIsNoop(t *testing.T) {
	var bus *Bus
	bus.Publish(Event{})
	if bus.Close(context.Background()) != nil || bus.Stats() != nil {
		t.Fatal("nil bus should be inert")
	}
}

func TestSQLiteStoresBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	sink, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	bus := New(func(_ string, err error, _ int) { t.Error(err) })
	if err := bus.Subscribe("sqlite", sink, SubscribeOptions{BatchSize: 250}); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		bus.Publish(Event{Kind: Discovered, Reference: ref("pkg")})
	}
	bus.Publish(Event{Kind: Downloaded, Reference: ref("pkg"), Key: "dug/m.json"})
	bus.Publish(Event{Kind: Failed, Reference: ref("bad"), Error: "boom"})
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var rows []Row
	if err := reopened.DB().Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1002 {
		t.Fatalf("rows = %d, want 1002", len(rows))
	}
	last := rows[1001]
	if last.Kind != "failed" || last.Name != "bad" || last.Error != "boom" || last.Registry != "npm" {
		t.Fatalf("last row = %+v", last)
	}
	if rows[0].Reference == "" || !json.Valid([]byte(rows[0].Reference)) || strings.Contains(rows[0].Reference, "big") {
		t.Fatalf("reference should be JSON without raw: %s", rows[0].Reference)
	}
}

func TestWebhookSignsAndRetries(t *testing.T) {
	var calls atomic.Int64
	var gotBody []byte
	var gotSig string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "try later", http.StatusServiceUnavailable)
			return
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
	}))
	defer server.Close()
	hook, err := NewWebhook(server.URL, "s3cret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.Handle(context.Background(), []Event{{Kind: Discovered, Reference: ref("a")}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want a retry after 503", calls.Load())
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(gotBody)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); gotSig != want {
		t.Fatalf("signature %q, want %q", gotSig, want)
	}
	var payload struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil || len(payload.Events) != 1 || payload.Events[0].Reference.Raw != nil {
		t.Fatalf("payload %s (%v)", gotBody, err)
	}
}

func TestWebhookClientErrorIsPermanent(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "no", http.StatusBadRequest)
	}))
	defer server.Close()
	hook, _ := NewWebhook(server.URL, "", server.Client())
	if err := hook.Handle(context.Background(), []Event{{Kind: Failed}}); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	if _, err := NewWebhook("ftp://x", "", nil); err == nil {
		t.Fatal("non-http URL accepted")
	}
}
