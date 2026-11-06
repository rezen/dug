package wordpress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestPaginationAndDownload(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive.zip" {
			io.WriteString(w, "zip content")
			return
		}
		pages++
		page := r.URL.Query().Get("request[page]")
		if page != fmt.Sprint(pages) {
			t.Errorf("page=%s requests=%d", page, pages)
		}
		json.NewEncoder(w).Encode(map[string]any{"info": map[string]int{"pages": 2}, "plugins": []map[string]string{{"slug": "plugin-" + page, "version": "1.0", "download_link": "http://" + r.Host + "/archive.zip"}}})
	}))
	defer server.Close()
	s := &Source{Client: server.Client(), Endpoint: server.URL, PageSize: 1, Attempts: 1, DownloadHosts: testHosts(server)}
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || pages != 2 {
		t.Fatalf("refs=%v pages=%d", refs, pages)
	}
	for i, ref := range refs {
		var raw map[string]any
		if err := json.Unmarshal(ref.Raw, &raw); err != nil || raw["slug"] != fmt.Sprintf("plugin-%d", i+1) || raw["download_link"] == nil {
			t.Fatalf("raw = %s (%v)", ref.Raw, err)
		}
	}
	a, err := s.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	body, err := a.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "zip content" {
		t.Fatalf("data=%s err=%v", data, err)
	}
}

func TestPluginAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"error":"Plugin not found."}`) }))
	defer server.Close()
	s := &Source{Client: server.Client(), Endpoint: server.URL, PageSize: 100, Slugs: []string{"missing"}, Attempts: 1}
	if err := s.Discover(context.Background(), func(registry.Reference) error { t.Fatal("unexpected plugin"); return nil }); err == nil {
		t.Fatal("expected API error")
	}
}

// testHosts allows fixture servers as download hosts, mirroring how an
// embedding deployment would configure a mirror.
func testHosts(server *httptest.Server) []string {
	return []string{strings.TrimPrefix(server.URL, "http://")}
}

func TestDownloadURLsMustUseWordPressHost(t *testing.T) {
	s := &Source{}
	ref := registry.Reference{Registry: "wordpress", Name: "akismet", Version: "1", Attributes: map[string]string{"download_url": "http://169.254.169.254/latest/meta-data/"}}
	if _, err := s.Resolve(context.Background(), ref); err == nil {
		t.Fatal("resolved a ZIP URL off the WordPress download host")
	}
	ref.Attributes["download_url"] = "https://downloads.wordpress.org/plugin/akismet.1.zip"
	if _, err := s.Resolve(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	// Discovery rejects a foreign catalog entry too, so it fails at listing time.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"slug": "akismet", "version": "1", "download_link": "https://evil.example/akismet.1.zip"})
	}))
	defer server.Close()
	s = &Source{Client: server.Client(), Endpoint: server.URL, PageSize: 100, Slugs: []string{"akismet"}, Attempts: 1}
	if err := s.Discover(context.Background(), func(registry.Reference) error { t.Fatal("emitted a foreign download URL"); return nil }); err == nil {
		t.Fatal("listing accepted a foreign download URL")
	}
}

func TestThemesUseTheirOwnDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("request[fields][downloadlink]") != "1" {
			t.Errorf("theme requests must ask for downloadlink: %v", q)
		}
		switch q.Get("action") {
		case "query_themes":
			if q.Get("request[browse]") != "updated" {
				t.Errorf("browse=%q", q.Get("request[browse]"))
			}
			json.NewEncoder(w).Encode(map[string]any{"info": map[string]int{"pages": 1}, "themes": []map[string]any{{"slug": "child", "version": "1.0", "download_link": "http://" + r.Host + "/theme/child.1.0.zip", "template": "parent", "last_updated": "2026-11-06", "last_updated_time": "2026-11-06 17:21:56", "active_installs": 50000, "downloaded": 7, "rating": 80, "num_ratings": 2, "tags": map[string]string{"two-columns": "Two Columns", "Blog": "Blog"}, "author": map[string]any{"user_nicename": "acme", "display_name": "Acme &amp; Co", "profile": "https://profiles.wordpress.org/acme/"}}}})
		case "theme_information":
			if q.Get("request[fields][extended_author]") != "1" || q.Get("request[fields][tags]") != "1" {
				t.Errorf("author and tag fields not requested: %v", q)
			}
			json.NewEncoder(w).Encode(map[string]any{"slug": q.Get("request[slug]"), "version": "2.0", "download_link": "http://" + r.Host + "/theme/parent.2.0.zip", "last_updated": "2026-11-05", "author": "parentdev", "tags": []string{}})
		default:
			t.Errorf("unexpected action %q", q.Get("action"))
		}
	}))
	defer server.Close()
	collect := func(s *Source) []registry.Reference {
		t.Helper()
		var refs []registry.Reference
		if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
			t.Fatal(err)
		}
		return refs
	}
	s := &Source{Kind: Themes, Client: server.Client(), Endpoint: server.URL, PageSize: 100, Attempts: 1, DownloadHosts: testHosts(server)}
	refs := collect(s)
	if len(refs) != 1 || refs[0].Registry != "wordpress-themes" || refs[0].Namespace != "wordpress.org" || refs[0].Name != "child" || refs[0].Version != "1.0" {
		t.Fatalf("refs=%+v", refs)
	}
	m, f := refs[0].Metadata, refs[0].Facts
	if m["template"] != "parent" || m["rating"] != "80" || m["updated_at"] != "" || m["active_installs"] != "" || m["downloads"] != "" {
		t.Fatalf("metadata=%v", m)
	}
	if f == nil || f.UpdatedAt == nil || f.UpdatedAt.Format(time.RFC3339) != "2026-11-06T17:21:56Z" || f.Installs == nil || *f.Installs != 50000 || f.Downloads == nil || f.Downloads.Total == nil || *f.Downloads.Total != 7 || f.Downloads.Week != nil || f.ReleasedAt != nil {
		t.Fatalf("facts=%+v", f)
	}
	if f.Repository == nil || f.Repository.URL != "https://themes.svn.wordpress.org/child" || f.Repository.Source != registry.RepositoryRegistry {
		t.Fatalf("repository=%+v", f.Repository)
	}
	if !reflect.DeepEqual(refs[0].Tags, []string{"blog", "two-columns"}) || !reflect.DeepEqual(refs[0].Author, &registry.Author{Name: "Acme & Co", Username: "acme", URL: "https://profiles.wordpress.org/acme/"}) {
		t.Fatalf("tags=%v author=%+v", refs[0].Tags, refs[0].Author)
	}
	s.Slugs = []string{"parent"}
	refs = collect(s)
	if len(refs) != 1 || refs[0].Name != "parent" || refs[0].Version != "2.0" || refs[0].Facts == nil || refs[0].Facts.UpdatedAt == nil || refs[0].Facts.UpdatedAt.Format(time.RFC3339) != "2026-11-05T00:00:00Z" {
		t.Fatalf("refs=%+v", refs)
	}
	if refs[0].Facts.Installs != nil || refs[0].Facts.Downloads != nil {
		t.Fatalf("absent directory metrics became facts: %+v", refs[0].Facts)
	}
	if refs[0].Tags != nil || !reflect.DeepEqual(refs[0].Author, &registry.Author{Username: "parentdev"}) {
		t.Fatalf("bare theme author or empty tags mishandled: tags=%v author=%+v", refs[0].Tags, refs[0].Author)
	}
	if _, ok := refs[0].Metadata["template"]; ok {
		t.Fatalf("parent theme gained a template: %v", refs[0].Metadata)
	}
	if err := (&Source{Kind: "widgets", PageSize: 1}).Discover(context.Background(), nil); err == nil {
		t.Fatal("unknown directory was accepted")
	}
}

// The themes API encodes purely numeric versions as JSON numbers.
func TestNumericVersionsDecodeVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"info":{"pages":1},"themes":[{"slug":"plain","version":1.0,"download_link":"http://%s/theme/plain.1.0.zip"},{"slug":"two","version":2,"download_link":"http://%s/theme/two.2.zip"}]}`, r.Host, r.Host)
	}))
	defer server.Close()
	s := &Source{Kind: Themes, Client: server.Client(), Endpoint: server.URL, PageSize: 100, Attempts: 1, DownloadHosts: testHosts(server)}
	var got []string
	if err := s.Discover(context.Background(), func(ref registry.Reference) error { got = append(got, ref.Version); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "1.0,2" {
		t.Fatalf("versions=%v", got)
	}
	var v text
	for _, bad := range []string{`true`, `{}`, `[1]`} {
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Fatalf("accepted %s as a version", bad)
		}
	}
}
