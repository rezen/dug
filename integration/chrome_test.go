package integration_test

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/engine"
	"github.com/rezen/dug/inventory"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage/memstore"
	"github.com/rezen/dug/sources/chrome"
)

func TestSitemapConnectInventoryAndDownload(t *testing.T) {
	idA, idB := strings.Repeat("a", 32), strings.Repeat("b", 32)
	var updates, downloads, active, peak atomic.Int32
	payload := "Cr24-fixture"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap":
			fmt.Fprintf(w, `<sitemapindex><sitemap><loc>http://%s/child.gz</loc></sitemap><sitemap><loc>http://%s/sitemap</loc></sitemap></sitemapindex>`, r.Host, r.Host)
		case "/child.gz":
			z := gzip.NewWriter(w)
			defer z.Close()
			fmt.Fprintf(z, `<urlset><url><loc>https://chromewebstore.google.com/detail/b/%s</loc></url><url><loc>https://chromewebstore.google.com/detail/a/%s</loc></url><url><loc>https://chromewebstore.google.com/detail/a/%s?hl=en</loc></url></urlset>`, idB, idA, idA)
		case "/update":
			updates.Add(1)
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p; p = peak.Load() {
				if peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			q := r.URL.Query()
			if q.Get("prodversion") != chrome.DefaultBrowserVersion || q.Get("acceptformat") != "crx3" {
				t.Error("missing update parameters")
			}
			id := strings.TrimSuffix(strings.TrimPrefix(q.Get("x"), "id="), "&uc")
			fmt.Fprintf(w, `<gupdate xmlns="http://www.google.com/update2/response"><app appid="%s" status="ok"><updatecheck status="ok" version="1" codebase="http://%s/archive.crx" hash_sha256="%s" size="%d"/></app></gupdate>`, id, r.Host, hash, len(payload))
		case "/archive.crx":
			downloads.Add(1)
			io.WriteString(w, payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	source := &chrome.Source{Client: upstream.Client(), SitemapURL: upstream.URL + "/sitemap", UpdateURL: upstream.URL + "/update", DetailURL: upstream.URL + "/detail/", ListingConcurrency: 2, Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(upstream.URL, "http://")}}
	handler, err := plugin.NewHandler(plugin.ServerOptions{Registry: "chrome", Factory: func(map[string]string) (registry.Registry, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx := context.Background()
	client, err := plugin.NewClient(ctx, server.Client(), server.URL, plugin.ClientOptions{Registry: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	store := memstore.New()
	ioOpts := inventory.DefaultOptions()
	ioOpts.TempDir = t.TempDir()
	summary, err := inventory.Create(ctx, client, store, inventory.Header{Registry: "chrome"}, ioOpts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Entries != 2 || updates.Load() != 2 || downloads.Load() != 0 || peak.Load() != 2 {
		t.Fatalf("summary=%+v updates=%d downloads=%d peak=%d", summary, updates.Load(), downloads.Load(), peak.Load())
	}
	order, _ := inventory.ParseOrder("name")
	snapshot, err := inventory.Open(ctx, store, summary.Key, order, ioOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var names []string
	if err := snapshot.Entries(ctx, func(ref registry.Reference) error { names = append(names, ref.Name); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != idA+","+idB {
		t.Fatal(names)
	}
	opts := engine.DefaultOptions()
	opts.TempDir = t.TempDir()
	opts.Concurrency = 1
	opts.Attempts = 1
	if _, err := engine.Download(ctx, snapshot.Entries, client, store, opts); err != nil {
		t.Fatal(err)
	}
	if updates.Load() != 2 || downloads.Load() != 1 {
		t.Fatalf("relisted or failed digest dedup: updates=%d downloads=%d", updates.Load(), downloads.Load())
	}
	if _, err := engine.Download(ctx, snapshot.Entries, client, store, opts); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 {
		t.Fatal("resume downloaded again")
	}
}
