package n8n

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

// manyPackages serves n explicit packages whose packument requests take longer
// for earlier packages, so a concurrent walk finishes them out of order. It
// also records the peak packument concurrency and the bulk count requests.
func manyPackages(t *testing.T, n int) (s *Source, names []string, peak, bulk *atomic.Int32) {
	t.Helper()
	var inflight atomic.Int32
	peak, bulk = new(atomic.Int32), new(atomic.Int32)
	for i := range n {
		names = append(names, "n8n-nodes-p"+strconv.Itoa(i))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/downloads/point/last-month/"):
			bulk.Add(1)
			doc := map[string]any{}
			for _, name := range strings.Split(strings.TrimPrefix(path, "/downloads/point/last-month/"), ",") {
				doc[name] = map[string]any{"downloads": 5}
			}
			json.NewEncoder(w).Encode(doc)
		case strings.HasPrefix(path, "/n8n-nodes-p"):
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			name := strings.TrimPrefix(path, "/")
			i, _ := strconv.Atoi(strings.TrimPrefix(name, "n8n-nodes-p"))
			time.Sleep(time.Duration(min(n-i, 8)) * 3 * time.Millisecond)
			p := testPackage{name: name, latest: "1.0.0", keywords: []string{DefaultKeyword}, versions: []testVersion{{version: "1.0.0", published: "2025-01-01T00:00:00.000Z"}}}
			json.NewEncoder(w).Encode((&fixture{}).packument(p))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	s = &Source{Client: server.Client(), Registry: server.URL, Downloads: server.URL, Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}, Packages: names}
	return s, names, peak, bulk
}

func TestPackumentsFetchConcurrentlyAndEmitInOrder(t *testing.T) {
	const n = 300 // three bulk chunks
	s, names, peak, bulk := manyPackages(t, n)
	s.Concurrency = 6
	refs := discover(t, s)
	if len(refs) != n {
		t.Fatalf("got %d references, want %d", len(refs), n)
	}
	for i, r := range refs {
		if r.Name != names[i] {
			t.Fatalf("reference %d is %s, want %s: emission must follow package order", i, r.Name, names[i])
		}
		if r.Facts == nil || r.Facts.Downloads == nil || r.Facts.Downloads.Month == nil || *r.Facts.Downloads.Month != 5 {
			t.Fatalf("%s lost its download count: %+v", r.Name, r.Facts)
		}
	}
	if got := bulk.Load(); got != 3 {
		t.Errorf("made %d bulk count requests, want 3", got)
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Fatalf("peak concurrent packument fetches %d, want overlap bounded near 6", p)
	}
}

func TestConcurrencyOfOneIsSerial(t *testing.T) {
	s, _, peak, _ := manyPackages(t, 10)
	s.Concurrency = 1
	discover(t, s)
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak concurrent fetches %d with listing_concurrency 1", p)
	}
}

func TestEmitErrorStopsConcurrentWalk(t *testing.T) {
	s, _, _, _ := manyPackages(t, 50)
	stop := fmt.Errorf("enough")
	count := 0
	err := s.Discover(context.Background(), func(registry.Reference) error {
		if count++; count == 3 {
			return stop
		}
		return nil
	})
	if err != stop || count != 3 {
		t.Fatalf("err = %v after %d references, want the emit error after 3", err, count)
	}
}

func TestListingConcurrencyOption(t *testing.T) {
	factory := PluginFactory(nil)
	for _, v := range []string{"0", "17", "x", ""} {
		if _, err := factory(map[string]string{"listing_concurrency": v}); err == nil {
			t.Errorf("accepted listing_concurrency %q", v)
		}
	}
	for _, v := range []string{"1", "16"} {
		if _, err := factory(map[string]string{"listing_concurrency": v}); err != nil {
			t.Errorf("rejected listing_concurrency %q: %v", v, err)
		}
	}
	s := &Source{Concurrency: 17, Packages: []string{"n8n-nodes-x"}}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Error("Discover accepted concurrency 17")
	}
}
