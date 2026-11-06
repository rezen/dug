package chrome

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestMetadataFailuresAndCancellation(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, body := range []string{
		`<gupdate><app appid="` + id + `"><updatecheck status="noupdate"/></app></gupdate>`,
		`<gupdate><app appid="` + id + `"><updatecheck status="ok" version="1" codebase="https://example.com/a.crx" hash_sha256="bad" size="1"/></app></gupdate>`,
		`<gupdate/>`, `<html/>`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			s := &Source{Client: server.Client(), UpdateURL: server.URL, IDs: []string{id}, Attempts: 1, SkipDetails: true}
			if err := s.Discover(context.Background(), func(registry.Reference) error { t.Fatal("emitted invalid metadata"); return nil }); err == nil {
				t.Fatal("expected error")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestSitemapAndOptionValidation(t *testing.T) {
	for _, body := range []string{`<html/>`, `<urlset>`, `<sitemapindex><sitemap><loc>https://another.example/sitemap</loc></sitemap></sitemapindex>`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		s := &Source{Client: server.Client(), SitemapURL: server.URL, Attempts: 1}
		if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
			t.Errorf("accepted %q", body)
		}
		server.Close()
	}
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{{"ids": "bad"}, {"ids": strings.Repeat("a", 32) + ","}, {"browser_version": "bad"}, {"listing_concurrency": "0"}, {"unknown": "x"}} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	if _, err := factory(map[string]string{"ids": strings.Repeat("a", 32), "listing_concurrency": "2"}); err != nil {
		t.Fatal(err)
	}
}

func TestLookupsOverlapAcrossBatchBoundaries(t *testing.T) {
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)}
	thirdStarted := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("x"), "id="), "&uc")
		switch id {
		case ids[2]:
			once.Do(func() { close(thirdStarted) })
		case ids[0]:
			// The first lookup finishes only once the third has started. A
			// batch-at-a-time walk never starts the third and stalls here.
			select {
			case <-thirdStarted:
			case <-time.After(2 * time.Second):
				t.Error("third lookup never started while the first was in flight")
			}
		}
		fmt.Fprintf(w, `<gupdate><app appid="%s" status="ok"><updatecheck status="ok" version="1" codebase="http://%s/%s.crx" hash_sha256="%s" size="1"/></app></gupdate>`, id, r.Host, id, strings.Repeat("0", 64))
	}))
	defer server.Close()
	s := &Source{Client: server.Client(), UpdateURL: server.URL, IDs: ids, ListingConcurrency: 2, Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}, SkipDetails: true}
	var got []string
	if err := s.Discover(context.Background(), func(ref registry.Reference) error { got = append(got, ref.Name); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("emission order changed: %v", got)
	}
}

func TestDownloadURLsMustUseGoogleHosts(t *testing.T) {
	s := &Source{}
	ref := registry.Reference{Registry: "chrome", Name: strings.Repeat("a", 32), Version: "1", Attributes: map[string]string{"download_url": "http://169.254.169.254/latest/meta-data/", "sha256": strings.Repeat("0", 64), "size": "1"}}
	if _, err := s.Resolve(context.Background(), ref); err == nil {
		t.Fatal("resolved a CRX URL off Google's download hosts")
	}
	ref.Attributes["download_url"] = "https://clients2.googleusercontent.com/crx/blobs/x/extension_1.crx"
	if _, err := s.Resolve(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}
