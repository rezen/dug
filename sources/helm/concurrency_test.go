package helm

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

func chartID(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

// manyCharts serves a catalog of n charts whose package lookups take longer
// for earlier charts, so a concurrent walk finishes them out of order.
func manyCharts(t *testing.T, n int) (*Source, *atomic.Int32) {
	t.Helper()
	var inflight, peak atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		switch {
		case path == "packages/search":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			page := []map[string]any{}
			for i := offset; i < min(offset+60, n); i++ {
				page = append(page, map[string]any{"package_id": chartID(i), "name": fmt.Sprintf("c%d", i), "normalized_name": fmt.Sprintf("c%d", i), "repository": map[string]any{"name": "acme", "url": server.URL + "/helm", "kind": 0}})
			}
			w.Header().Set("Pagination-Total-Count", strconv.Itoa(n))
			json.NewEncoder(w).Encode(map[string]any{"packages": page})
		case strings.HasPrefix(path, "packages/helm/acme/c"):
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			defer inflight.Add(-1)
			name := strings.TrimPrefix(path, "packages/helm/acme/")
			i, _ := strconv.Atoi(strings.TrimPrefix(name, "c"))
			time.Sleep(time.Duration(min(n-i, 8)) * 3 * time.Millisecond)
			json.NewEncoder(w).Encode(map[string]any{
				"package_id": chartID(i), "name": name, "normalized_name": name, "version": "1.0.0", "ts": release100.Unix(),
				"digest": digestOf([]byte(name)), "content_url": server.URL + "/charts/" + name + ".tgz",
				"repository": map[string]any{"name": "acme", "url": server.URL + "/helm", "kind": 0},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	return &Source{Client: server.Client(), Endpoint: server.URL + "/api/v1/", Attempts: 1, Interval: time.Nanosecond, SearchInterval: time.Nanosecond, DownloadHosts: []string{host}}, &peak
}

func TestWalkLooksUpChartsConcurrentlyInSearchOrder(t *testing.T) {
	const n = 24
	s, peak := manyCharts(t, n)
	s.Concurrency = 6
	refs := collect(t, s)
	if len(refs) != n {
		t.Fatalf("got %d references, want %d", len(refs), n)
	}
	for i, r := range refs {
		if want := fmt.Sprintf("acme/c%d", i); r.Name != want {
			t.Fatalf("reference %d is %s, want %s: emission must follow search order", i, r.Name, want)
		}
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Fatalf("peak concurrent lookups %d, want overlap bounded near 6", p)
	}
}

func TestWalkConcurrencyOfOneIsSerial(t *testing.T) {
	s, peak := manyCharts(t, 8)
	s.Concurrency = 1
	collect(t, s)
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak concurrent lookups %d with listing_concurrency 1", p)
	}
}

func TestWalkEmitsFirstReferenceBeforeWalkFinishes(t *testing.T) {
	s, _ := manyCharts(t, 200) // four search pages
	s.Concurrency = 4
	var searches atomic.Int32
	client, inner := *s.Client, s.Client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/packages/search") {
			searches.Add(1)
		}
		return inner.RoundTrip(r)
	})
	s.Client = &client
	first := int32(-1)
	err := s.Discover(context.Background(), func(registry.Reference) error {
		if first < 0 {
			first = searches.Load()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first < 1 || first >= 4 {
		t.Fatalf("first reference emitted after %d searches, want it before the walk ends", first)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
	s := &Source{Concurrency: 17}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Error("Discover accepted concurrency 17")
	}
}
