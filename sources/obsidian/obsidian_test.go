package obsidian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

const catalogJSON = `[
 {"id":"alpha","name":"Alpha","author":"Ann","description":"x","repo":"acme/alpha"},
 {"id":"alpha","name":"Alpha again","author":"Ann","repo":"acme/alpha"},
 {"id":"beta","name":"Beta","author":"Bob","repo":"bob/beta"},
 {"id":"gone","name":"Gone","author":"Gil","repo":"gil/gone"},
 {"id":"bare","name":"Bare","author":"Bea","repo":"bea/bare"},
 {"id":"broken","name":"Broken","author":"Bo","repo":"not-a-repo"}]`

const statsJSON = `{
 "alpha":{"downloads":900,"updated":1700000000000,"1.2.0":50,"1.1.0":40,"1.0.0":810},
 "beta":{"downloads":10,"updated":1600000000000,"v2.0.0":10}}`

type fake struct {
	srv      *httptest.Server
	metadata atomic.Int64
	files    atomic.Int64
}

func mkAsset(id int, name string, size int, digest string) map[string]any {
	return map[string]any{"id": id, "name": name, "size": size, "state": "uploaded", "digest": digest}
}

func release(id int, tag string, draft bool, published string, assets ...map[string]any) map[string]any {
	return map[string]any{"id": id, "tag_name": tag, "draft": draft, "prerelease": false, "created_at": published, "published_at": published, "assets": assets}
}

var sum = "sha256:" + strings.Repeat("a", 64)

func newFake(t *testing.T) *fake {
	f := &fake{}
	std := func(id int) []map[string]any {
		return []map[string]any{mkAsset(id, "main.js", 11, sum), mkAsset(id+1, "manifest.json", 5, ""), mkAsset(id+2, "styles.css", 3, "sha1:xyz"), mkAsset(id+3, "notes.txt", 9, "")}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) {
			f.metadata.Add(1)
			json.NewEncoder(w).Encode(v)
		}
		switch p := r.URL.Path; {
		case p == "/catalog/community-plugins.json":
			io.WriteString(w, catalogJSON)
		case p == "/catalog/community-plugin-stats.json":
			io.WriteString(w, statsJSON)
		case p == "/api/repos/acme/alpha/releases" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/repos/acme/alpha/releases?page=2>; rel="next"`, r.Host))
			write([]any{
				release(30, "1.3.0-draft", true, "2024-04-01T00:00:00Z", std(300)...),
				release(20, "1.2.0", false, "2024-03-01T00:00:00Z", std(200)...),
			})
		case p == "/api/repos/acme/alpha/releases":
			write([]any{
				release(10, "1.1.0", false, "2024-02-01T00:00:00Z", std(100)...),
				release(9, "1.0.1", false, "2024-01-15T00:00:00Z", mkAsset(90, "styles.css", 3, "")), // no main.js: skipped
				release(8, "1.0.0", false, "2023-01-01T00:00:00Z", std(80)...),
			})
		case p == "/api/repos/bob/beta/releases":
			write([]any{release(5, "v2.0.0", false, "2024-05-01T00:00:00Z", std(50)...)})
		case p == "/api/repos/bea/bare/releases":
			write([]any{})
		case strings.HasPrefix(p, "/api/"):
			http.NotFound(w, r)
		case strings.HasPrefix(p, "/dl/"):
			f.files.Add(1)
			io.WriteString(w, p)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) source() *Source {
	return &Source{Client: f.srv.Client(), Attempts: 1, Token: "secret", GitHubEndpoint: f.srv.URL + "/api", CatalogEndpoint: f.srv.URL + "/catalog", ReleaseEndpoint: f.srv.URL + "/dl"}
}

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return refs
}

func names(refs []registry.Reference) []string {
	got := []string{}
	for _, r := range refs {
		got = append(got, r.Name+"@"+r.Version)
	}
	return got
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil, "", "")
	for _, options := range []map[string]string{{"unknown": "x"}, {"ids": "../bad"}, {"ids": ""}, {"ids": "a,"}, {"version_depth": "0"}, {"listing_concurrency": "0"}, {"listing_concurrency": "17"}, {"listing_concurrency": "x"}} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{nil, {"ids": "obsidian-git, dataview"}, {"version_depth": "unlimited"}, {"listing_concurrency": "16"}} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	if _, err := PluginFactory(nil, "http://api.example.com", "secret")(nil); err == nil {
		t.Error("token over plaintext accepted")
	}
}

func TestDiscoverDepths(t *testing.T) {
	f := newFake(t)
	for _, tc := range []struct {
		depth string
		want  []string
	}{
		{"", []string{"alpha@1.2.0", "beta@v2.0.0"}},
		{"2", []string{"alpha@1.2.0", "alpha@1.1.0", "beta@v2.0.0"}},
		{"unlimited", []string{"alpha@1.2.0", "alpha@1.1.0", "alpha@1.0.0", "beta@v2.0.0"}},
		{"2024-02-15", []string{"alpha@1.2.0", "beta@v2.0.0"}},
		{"2100-01-01", nil},
	} {
		s := f.source()
		s.VersionDepth = tc.depth
		got := names(discover(t, s)) // "gone", "bare" and "broken" are skipped with warnings
		if len(tc.want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("depth %q: got %v want %v", tc.depth, got, tc.want)
		}
	}
}

func TestReferenceFactsMetadataAndBlobs(t *testing.T) {
	f := newFake(t)
	s := f.source()
	s.VersionDepth = "2"
	refs := discover(t, s)
	r := refs[1] // alpha@1.1.0
	if err := r.Facts.Validate(); err != nil {
		t.Fatal(err)
	}
	facts := r.Facts
	if *facts.Downloads.Total != 900 || *facts.VersionDownloads.Total != 40 || facts.Installs != nil || facts.Downloads.Week != nil {
		t.Fatalf("%+v %+v", facts.Downloads, facts.VersionDownloads)
	}
	if !facts.ReleasedAt.Equal(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)) || !facts.UpdatedAt.Equal(time.UnixMilli(1700000000000)) {
		t.Fatalf("%v %v", facts.ReleasedAt, facts.UpdatedAt)
	}
	if facts.Repository == nil || facts.Repository.URL != "https://github.com/acme/alpha" || facts.Repository.Source != registry.RepositoryDeclared {
		t.Fatalf("%+v", facts.Repository)
	}
	if r.Author == nil || r.Author.Name != "Ann" || r.Namespace != Namespace {
		t.Fatalf("%+v %q", r.Author, r.Namespace)
	}
	wantMeta := map[string]string{"plugin_name": "Alpha", "repo": "acme/alpha", "prerelease": "false", "asset_count": "3", "version_rank": "2", "release_date_source": "github-release-published"}
	if !maps.Equal(r.Metadata, wantMeta) {
		t.Fatalf("%v", r.Metadata)
	}
	// "v2.0.0" falls back to the stats key without the v prefix.
	if v := refs[2].Facts.VersionDownloads; v == nil || *v.Total != 10 {
		t.Fatalf("%+v", v)
	}

	before := f.metadata.Load()
	a, err := s.Resolve(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != "release:10" || len(a.Blobs) != 3 {
		t.Fatalf("%s %d", a.Revision, len(a.Blobs))
	}
	byName := map[string]registry.Blob{}
	for _, b := range a.Blobs {
		byName[b.Name] = b
	}
	if byName["main.js"].Digest != sum || byName["manifest.json"].Digest != "" || byName["styles.css"].Digest != "" {
		t.Fatalf("digests: %+v", a.Blobs)
	}
	body, err := byName["main.js"].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "/dl/acme/alpha/releases/download/1.1.0/main.js" || byName["main.js"].Size != 11 {
		t.Fatalf("%q", data)
	}
	if f.metadata.Load() != before {
		t.Fatal("Resolve and Open made a metadata request")
	}
}

func TestExplicitIDs(t *testing.T) {
	f := newFake(t)
	s := f.source()
	s.IDs = []string{"beta"}
	if got := names(discover(t, s)); !reflect.DeepEqual(got, []string{"beta@v2.0.0"}) {
		t.Fatal(got)
	}
	for _, ids := range [][]string{{"missing"}, {"gone"}, {"bare"}, {"broken"}, {"../x"}} {
		s.IDs = ids
		if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
			t.Errorf("%v: explicit plugin without releases was skipped", ids)
		}
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	f := newFake(t)
	s := f.source()
	good := discover(t, s)[0]
	for name, mutate := range map[string]func(*registry.Reference){
		"other registry":  func(r *registry.Reference) { r.Registry = "other" },
		"path in name":    func(r *registry.Reference) { r.Name = "../alpha" },
		"path in tag":     func(r *registry.Reference) { r.Version = "../1.0" },
		"bad repo":        func(r *registry.Reference) { r.Attributes["repo"] = "http://169.254.169.254/x" },
		"bad release id":  func(r *registry.Reference) { r.Attributes["release_id"] = "0" },
		"bad asset json":  func(r *registry.Reference) { r.Attributes["assets"] = "{" },
		"foreign file":    func(r *registry.Reference) { r.Attributes["assets"] = `[{"id":1,"name":"../../etc/passwd","size":1}]` },
		"missing main.js": func(r *registry.Reference) { r.Attributes["assets"] = `[{"id":2,"name":"manifest.json","size":1}]` },
		"bad digest": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":1,"name":"main.js","size":1,"digest":"sha256:bad"},{"id":2,"name":"manifest.json","size":1}]`
		},
		"duplicate file": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":1,"name":"main.js","size":1},{"id":3,"name":"main.js","size":1},{"id":2,"name":"manifest.json","size":1}]`
		},
		"negative size": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":1,"name":"main.js","size":-1},{"id":2,"name":"manifest.json","size":1}]`
		},
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
}

func TestCatalogErrorsAndCancellation(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"not":"a list"}`) }))
	defer bad.Close()
	s := &Source{Client: bad.Client(), Attempts: 1, CatalogEndpoint: bad.URL}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("accepted a malformed catalog")
	}
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.source().Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestListingConcurrencyOverlapsAndKeepsCatalogOrder(t *testing.T) {
	const plugins = 24
	var catalog []map[string]string
	for i := 0; i < plugins; i++ {
		catalog = append(catalog, map[string]string{"id": fmt.Sprintf("p%02d", i), "name": "P", "author": "A", "repo": fmt.Sprintf("o/p%02d", i)})
	}
	var inflight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/catalog/community-plugins.json":
			json.NewEncoder(w).Encode(catalog)
		case p == "/catalog/community-plugin-stats.json":
			io.WriteString(w, "{}")
		case strings.HasPrefix(p, "/api/repos/o/p"):
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			var n int
			fmt.Sscanf(p, "/api/repos/o/p%d/releases", &n)
			time.Sleep(time.Duration(plugins-n) * time.Millisecond) // later plugins answer first
			json.NewEncoder(w).Encode([]any{release(n+1, "1.0.0", false, "2024-01-01T00:00:00Z", mkAsset(1, "main.js", 1, sum), mkAsset(2, "manifest.json", 1, ""))})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &Source{Client: srv.Client(), Attempts: 1, GitHubEndpoint: srv.URL + "/api", CatalogEndpoint: srv.URL + "/catalog", ReleaseEndpoint: srv.URL + "/dl", Concurrency: 6}
	got := names(discover(t, s))
	if len(got) != plugins {
		t.Fatalf("got %d references, want %d", len(got), plugins)
	}
	for i, name := range got {
		if want := fmt.Sprintf("p%02d@1.0.0", i); name != want {
			t.Fatalf("reference %d = %s, want %s (catalog order)", i, name, want)
		}
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Fatalf("peak concurrent release listings %d, want overlap bounded near 6", p)
	}
}
