package ansiblegalaxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

const archiveBytes = "tarball bytes"

var archiveDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(archiveBytes)))

type fakeVersion struct {
	version, created string
}

type fakeCollection struct {
	namespace, name string
	created         string // collection creation; the index is ordered by it, newest first
	updated         string
	downloads       int64
	deprecated      bool
	repository      string
	versions        []fakeVersion // newest first by creation, as the API lists them
	highest         string
	missingDetail   bool
}

// fakeGalaxy serves the shapes recorded from galaxy.ansible.com under /api/.
type fakeGalaxy struct {
	collections []fakeCollection
	// indexOverride replaces the index payload, to simulate shifting pages.
	indexOverride []fakeCollection
	reportedCount int
	nextOverride  string
	details       atomic.Int32
	indexPages    atomic.Int32
	downloads     atomic.Int32
}

func testCollections() []fakeCollection {
	return []fakeCollection{
		{namespace: "acme", name: "widget", created: "2026-03-01T00:00:00Z", updated: "2026-03-02T10:00:00Z", downloads: 1234, repository: "git+https://github.com/acme/widget.git",
			highest:  "3.0.0",
			versions: []fakeVersion{{"3.0.0", "2026-03-01T00:00:00Z"}, {"2.1.0", "2026-01-15T00:00:00Z"}, {"2.0.0", "2025-06-01T00:00:00Z"}}},
		{namespace: "acme", name: "gadget", created: "2025-01-01T00:00:00Z", updated: "2025-01-02T00:00:00Z", downloads: 0, deprecated: true,
			highest: "0.1.0", versions: []fakeVersion{{"0.1.0", "2025-01-01T00:00:00Z"}}},
		{namespace: "other", name: "tool", created: "2024-01-01T00:00:00Z", updated: "2024-01-02T00:00:00Z", downloads: 5,
			highest: "1.0.0", versions: []fakeVersion{{"1.0.0", "2024-01-01T00:00:00Z"}}},
	}
}

func (f *fakeGalaxy) handler(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/"), "/")
	switch {
	case parts[0] == "artifacts":
		f.downloads.Add(1)
		io.WriteString(w, archiveBytes)
	case parts[0] == "index" && len(parts) == 1:
		f.indexPages.Add(1)
		list := f.collections
		if f.indexOverride != nil {
			list = f.indexOverride
		}
		if ns := r.URL.Query().Get("namespace"); ns != "" {
			var filtered []fakeCollection
			for _, c := range list {
				if c.namespace == ns {
					filtered = append(filtered, c)
				}
			}
			list = filtered
		}
		count := len(list)
		if f.reportedCount > 0 {
			count = f.reportedCount
		}
		f.writePage(w, r, count, len(list), func(i int) any { return f.indexEntry(list[i]) }, f.nextOverride)
	case parts[0] == "index" && len(parts) == 3:
		c, ok := f.find(parts[1], parts[2])
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(f.indexEntry(c))
	case parts[0] == "index" && len(parts) == 4 && parts[3] == "versions":
		c, ok := f.find(parts[1], parts[2])
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.writePage(w, r, len(c.versions), len(c.versions), func(i int) any {
			v := c.versions[i]
			return map[string]any{"version": v.version, "created_at": v.created, "requires_ansible": ">=2.15", "marks": []string{}}
		}, "")
	case parts[0] == "index" && len(parts) == 5 && parts[3] == "versions":
		f.details.Add(1)
		c, ok := f.find(parts[1], parts[2])
		if !ok || c.missingDetail {
			http.NotFound(w, r)
			return
		}
		for _, v := range c.versions {
			if v.version == parts[4] {
				file := c.namespace + "-" + c.name + "-" + v.version + ".tar.gz"
				json.NewEncoder(w).Encode(map[string]any{
					"version": v.version, "created_at": v.created, "requires_ansible": ">=2.15", "marks": []string{"signed"},
					"download_url":   base + "/api/artifacts/" + file,
					"git_commit_sha": "abc123",
					"artifact":       map[string]any{"filename": file, "sha256": archiveDigest, "size": len(archiveBytes)},
					"metadata":       map[string]any{"license": []string{"GPL-3.0-or-later"}, "repository": c.repository, "tags": []string{"Networking", "cloud"}, "authors": []string{"Acme"}},
					"files":          []any{},
				})
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGalaxy) find(namespace, name string) (fakeCollection, bool) {
	for _, c := range f.collections {
		if c.namespace == namespace && c.name == name {
			return c, true
		}
	}
	return fakeCollection{}, false
}

func (f *fakeGalaxy) indexEntry(c fakeCollection) map[string]any {
	return map[string]any{
		"namespace": c.namespace, "name": c.name, "deprecated": c.deprecated,
		"highest_version": map[string]any{"version": c.highest},
		"created_at":      c.created, "updated_at": c.updated, "download_count": c.downloads,
	}
}

// writePage pages like pulp_ansible: limit and offset in, site-relative links out.
func (f *fakeGalaxy) writePage(w http.ResponseWriter, r *http.Request, count, n int, item func(int) any, nextOverride string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit < 1 || limit > 100 {
		limit = 100
	}
	data := []any{}
	for i := offset; i < min(offset+limit, n); i++ {
		data = append(data, item(i))
	}
	var next any
	if offset+limit < n {
		q := r.URL.Query()
		q.Set("offset", strconv.Itoa(offset+limit))
		next = r.URL.Path + "?" + q.Encode()
	}
	if nextOverride != "" && next != nil {
		next = nextOverride
	}
	json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"count": count}, "links": map[string]any{"next": next}, "data": data})
}

func newSource(t *testing.T, f *fakeGalaxy) *Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(server.Close)
	return &Source{Client: server.Client(), Endpoint: server.URL + "/api/", Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}}
}

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return refs
}

func versions(refs []registry.Reference) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.Name+"@"+r.Version)
	}
	return out
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{
		{"unknown": "x"}, {"collections": "nodot"}, {"collections": "acme."}, {"collections": "../bad.name"}, {"collections": "acme.widget,"},
		{"namespaces": "a/b"}, {"namespaces": ""}, {"page_size": "0"}, {"page_size": "101"}, {"page_size": "x"},
		{"version_depth": "0"}, {"collections": "acme.widget", "namespaces": "acme"},
		{"listing_concurrency": "0"}, {"listing_concurrency": "17"}, {"listing_concurrency": "x"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{nil, {"collections": "acme.widget, other.tool"}, {"namespaces": "acme"}, {"page_size": "100"}, {"version_depth": "unlimited"}, {"version_depth": "2025-01-01"}, {"listing_concurrency": "16"}, {"listing_concurrency": "1"}} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	_, err := factory(map[string]string{"nope": "1"})
	if err == nil || !strings.Contains(err.Error(), "collections, namespaces, page_size, version_depth") {
		t.Errorf("unknown-option error does not list supported keys: %v", err)
	}
}

func TestVersionDepth(t *testing.T) {
	for depth, want := range map[string][]string{
		"":           {"acme.widget@3.0.0"},
		"1":          {"acme.widget@3.0.0"},
		"2":          {"acme.widget@3.0.0", "acme.widget@2.1.0"},
		"unlimited":  {"acme.widget@3.0.0", "acme.widget@2.1.0", "acme.widget@2.0.0"},
		"2026-01-01": {"acme.widget@3.0.0", "acme.widget@2.1.0"},
		"2099-01-01": {},
	} {
		t.Run("depth="+depth, func(t *testing.T) {
			f := &fakeGalaxy{collections: testCollections()}
			s := newSource(t, f)
			s.Collections = []string{"acme.widget"}
			s.VersionDepth = depth
			s.PageSize = 1 // forces the version list to paginate
			refs := discover(t, s)
			if got := versions(refs); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			for i, ref := range refs {
				if ref.Metadata["version_rank"] != strconv.Itoa(i+1) {
					t.Errorf("%s version_rank = %q", ref.Version, ref.Metadata["version_rank"])
				}
			}
			if depth == "2026-01-01" && f.details.Load() != 2 {
				t.Errorf("fetched %d version documents, want 2: versions before the cutoff need no detail request", f.details.Load())
			}
			if depth == "1" && f.details.Load() != 1 {
				t.Errorf("depth 1 fetched %d version documents, want 1", f.details.Load())
			}
		})
	}
}

func TestReferenceFactsAndMetadata(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections()}
	s := newSource(t, f)
	s.Collections = []string{"acme.widget"}
	s.VersionDepth = "2"
	refs := discover(t, s)
	if len(refs) != 2 {
		t.Fatalf("got %d references", len(refs))
	}
	ref := refs[0]
	if ref.Registry != Identity || ref.Namespace != "galaxy.ansible.com" || ref.Name != "acme.widget" || ref.Version != "3.0.0" {
		t.Fatalf("identity: %+v", ref)
	}
	if err := ref.Facts.Validate(); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(ref.Raw, &raw); err != nil || raw["version"] != "3.0.0" || raw["artifact"] == nil {
		t.Fatalf("raw = %s (%v)", ref.Raw, err)
	}
	facts := ref.Facts
	if facts.Downloads == nil || facts.Downloads.Total == nil || *facts.Downloads.Total != 1234 {
		t.Errorf("downloads.total = %+v", facts.Downloads)
	}
	if d := facts.Downloads; d.Day != nil || d.Week != nil || d.Month != nil || d.Quarter != nil || d.Year != nil {
		t.Errorf("Galaxy publishes only a lifetime total; got windows %+v", d)
	}
	if facts.VersionDownloads != nil || facts.Installs != nil {
		t.Errorf("unpublished facts must stay nil: %+v", facts)
	}
	if facts.UpdatedAt == nil || !facts.UpdatedAt.Equal(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("updated_at = %v", facts.UpdatedAt)
	}
	if facts.ReleasedAt == nil || !facts.ReleasedAt.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("released_at = %v", facts.ReleasedAt)
	}
	if refs[1].Facts.ReleasedAt == nil || !refs[1].Facts.ReleasedAt.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("per-version released_at = %v", refs[1].Facts.ReleasedAt)
	}
	if refs[0].Facts == refs[1].Facts || refs[0].Facts.Downloads == refs[1].Facts.Downloads {
		t.Error("references share mutable facts")
	}
	want := &registry.Repository{URL: "https://github.com/acme/widget", Commit: "abc123", Source: registry.RepositoryDeclared}
	if !reflect.DeepEqual(facts.Repository, want) {
		t.Errorf("repository = %+v, want %+v", facts.Repository, want)
	}
	wantMetadata := map[string]string{
		"deprecated": "false", "version_rank": "1", "release_date_source": "galaxy-version-created-at",
		"requires_ansible": ">=2.15", "license": "GPL-3.0-or-later", "marks": "signed",
	}
	if !maps.Equal(ref.Metadata, wantMetadata) {
		t.Errorf("metadata = %v, want %v", ref.Metadata, wantMetadata)
	}
	for _, typed := range []string{"downloads", "download_count", "released_at", "updated_at"} {
		if _, ok := ref.Metadata[typed]; ok {
			t.Errorf("typed fact %q duplicated in metadata", typed)
		}
	}
	if !reflect.DeepEqual(ref.Tags, registry.NormalizeTags([]string{"Networking", "cloud"})) || len(ref.Tags) != 2 {
		t.Errorf("tags = %v", ref.Tags)
	}
	if ref.Author == nil || ref.Author.Username != "acme" {
		t.Errorf("author = %+v", ref.Author)
	}
	if got := ref.Attributes["sha256"]; got != "sha256:"+archiveDigest {
		t.Errorf("sha256 attribute = %q", got)
	}
	if ref.Attributes["size"] != strconv.Itoa(len(archiveBytes)) || !strings.HasSuffix(ref.Attributes["download_url"], "/artifacts/acme-widget-3.0.0.tar.gz") {
		t.Errorf("attributes = %v", ref.Attributes)
	}
	// A collection without a declared repository leaves the fact absent.
	f2 := &fakeGalaxy{collections: testCollections()}
	s2 := newSource(t, f2)
	s2.Collections = []string{"acme.gadget"}
	gadget := discover(t, s2)[0]
	if gadget.Facts.Repository != nil || gadget.Metadata["deprecated"] != "true" || gadget.Facts.Downloads == nil || *gadget.Facts.Downloads.Total != 0 {
		t.Errorf("gadget facts = %+v metadata = %v", gadget.Facts, gadget.Metadata)
	}
	if _, ok := gadget.Metadata["marks"]; ok && gadget.Metadata["marks"] == "" {
		t.Error("empty marks must be omitted")
	}
}

func TestFullWalkPaginatesAndDedupes(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections()}
	s := newSource(t, f)
	s.PageSize = 1
	refs := discover(t, s)
	if got, want := versions(refs), []string{"acme.widget@3.0.0", "acme.gadget@0.1.0", "other.tool@1.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if f.indexPages.Load() != 3 {
		t.Errorf("walked %d index pages, want 3", f.indexPages.Load())
	}

	// A collection created mid-walk shifts later offsets, repeating an entry.
	c := testCollections()
	f = &fakeGalaxy{collections: c, indexOverride: []fakeCollection{c[0], c[0], c[1], c[2]}, reportedCount: 3}
	s = newSource(t, f)
	s.PageSize = 2
	if got := versions(discover(t, s)); len(got) != 3 {
		t.Fatalf("duplicate entry emitted twice: %v", got)
	}
}

func TestNamespaceSelection(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections()}
	s := newSource(t, f)
	s.Namespaces = []string{"acme"}
	if got, want := versions(discover(t, s)), []string{"acme.widget@3.0.0", "acme.gadget@0.1.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTruncatedCatalogFails(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections(), reportedCount: 5}
	s := newSource(t, f)
	err := s.Discover(context.Background(), func(registry.Reference) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ended after 3 of 5") {
		t.Fatalf("got %v, want a truncation error", err)
	}
}

func TestMissingItems(t *testing.T) {
	c := testCollections()
	if err := newSource(t, &fakeGalaxy{collections: c}).Discover(context.Background(), func(registry.Reference) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// An explicitly requested collection that does not exist is an error.
	s := newSource(t, &fakeGalaxy{collections: c})
	s.Collections = []string{"acme.nothing"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("missing explicit collection did not fail")
	}
	// One that vanishes between the index page and its own request is skipped
	// during a full walk, and everything else is still emitted.
	c[1].missingDetail = true
	s = newSource(t, &fakeGalaxy{collections: c})
	if got, want := versions(discover(t, s)), []string{"acme.widget@3.0.0", "other.tool@1.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The same vanished collection selected explicitly is an error.
	s.Collections = []string{"acme.gadget"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("explicit collection with missing version did not fail")
	}
}

func TestPinnedDownloadMakesNoMetadataRequests(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections()}
	s := newSource(t, f)
	s.Collections = []string{"acme.widget"}
	ref := discover(t, s)[0]
	before := f.details.Load() + f.indexPages.Load()

	// A fresh context per stage: the resolution context is cancelled before
	// the first transfer in the host.
	ctx, cancel := context.WithCancel(context.Background())
	artifact, err := s.Resolve(ctx, ref)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Revision != "sha256:"+archiveDigest || len(artifact.Blobs) != 1 {
		t.Fatalf("artifact = %+v", artifact)
	}
	blob := artifact.Blobs[0]
	if blob.Name != "acme-widget-3.0.0.tar.gz" || blob.MediaType != "application/gzip" || blob.Digest != "sha256:"+archiveDigest || blob.Size != int64(len(archiveBytes)) {
		t.Fatalf("blob = %+v", blob)
	}
	for range 2 { // every Open starts at byte zero
		body, err := blob.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(body)
		body.Close()
		if string(data) != archiveBytes {
			t.Fatalf("downloaded %q", data)
		}
	}
	if f.downloads.Load() != 2 {
		t.Errorf("downloads = %d, want 2", f.downloads.Load())
	}
	if after := f.details.Load() + f.indexPages.Load(); after != before {
		t.Errorf("Resolve and Open made %d metadata requests", after-before)
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	s := &Source{}
	good := registry.Reference{Registry: Identity, Name: "acme.widget", Version: "3.0.0", Attributes: map[string]string{
		"download_url": "https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/acme-widget-3.0.0.tar.gz",
		"sha256":       "sha256:" + archiveDigest,
		"size":         "13",
	}}
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	setURL := func(raw string) func(*registry.Reference) {
		return func(r *registry.Reference) { r.Attributes["download_url"] = raw }
	}
	const artifacts = "https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/"
	for name, mutate := range map[string]func(*registry.Reference){
		"link-local URL":    setURL("http://169.254.169.254/latest/meta-data/"),
		"loopback URL":      setURL("http://127.0.0.1/artifacts/acme-widget-3.0.0.tar.gz"),
		"other host":        setURL("https://evil.example/api/artifacts/acme-widget-3.0.0.tar.gz"),
		"other version":     setURL(artifacts + "acme-widget-9.9.9.tar.gz"),
		"other collection":  setURL(artifacts + "acme-gadget-3.0.0.tar.gz"),
		"not artifact path": setURL("https://galaxy.ansible.com/api/v3/auth/token/acme-widget-3.0.0.tar.gz"),
		"query string":      setURL(artifacts + "acme-widget-3.0.0.tar.gz?x=1"),
		"missing URL":       func(r *registry.Reference) { delete(r.Attributes, "download_url") },
		"bad digest":        func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:bad" },
		"missing digest":    func(r *registry.Reference) { delete(r.Attributes, "sha256") },
		"uppercase digest":  func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:" + strings.Repeat("A", 64) },
		"zero size":         func(r *registry.Reference) { r.Attributes["size"] = "0" },
		"bad size":          func(r *registry.Reference) { r.Attributes["size"] = "big" },
		"other registry":    func(r *registry.Reference) { r.Registry = "other" },
		"path in name":      func(r *registry.Reference) { r.Name = "../acme.widget" },
		"no namespace":      func(r *registry.Reference) { r.Name = "widget" },
		"extra dot":         func(r *registry.Reference) { r.Name = "acme.widget.extra" },
		"path in version":   func(r *registry.Reference) { r.Version = "../3.0.0" },
		"empty version":     func(r *registry.Reference) { r.Version = "" },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
}

func TestPaginationStaysOnOrigin(t *testing.T) {
	s := &Source{Endpoint: "https://registry.example/api/"}
	seen := map[string]bool{}
	got, err := s.next("/api/index/?limit=100&offset=100", seen)
	if err != nil || got != "https://registry.example/api/index/?limit=100&offset=100" {
		t.Fatalf("relative link: %q, %v", got, err)
	}
	if _, err := s.next("/api/index/?limit=100&offset=100", seen); err == nil {
		t.Fatal("accepted a pagination cycle")
	}
	for _, raw := range []string{"https://other.example/api/index/", "http://registry.example/api/index/", "https://registry.example/other/", "/index/?offset=3"} {
		if _, err := s.next(raw, map[string]bool{}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestWalkRejectsCyclesAndForeignLinks(t *testing.T) {
	for name, next := range map[string]string{
		"cycle":   "/api/index/?limit=1",
		"foreign": "https://evil.example/api/index/?limit=1&offset=1",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGalaxy{collections: testCollections(), nextOverride: next}
			s := newSource(t, f)
			s.PageSize = 1
			err := s.Discover(context.Background(), func(registry.Reference) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "pagination") {
				t.Fatalf("got %v, want a pagination error", err)
			}
		})
	}
}

func TestEmitErrorsPropagate(t *testing.T) {
	f := &fakeGalaxy{collections: testCollections()}
	s := newSource(t, f)
	stop := errors.New("stop")
	calls := 0
	err := s.Discover(context.Background(), func(registry.Reference) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err = %v after %d emits", err, calls)
	}
}

func TestCancellationStopsDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, s := range []*Source{{Collections: []string{"acme.widget"}, Attempts: 1}, {Attempts: 1}} {
		if err := s.Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	}
}

func TestInvalidUpstreamMetadataFails(t *testing.T) {
	for name, mutate := range map[string]func(*fakeCollection){
		"traversal in name": func(c *fakeCollection) { c.name = ".." },
		"bad version":       func(c *fakeCollection) { c.highest = "../1"; c.versions[0].version = "../1" },
		"no versions":       func(c *fakeCollection) { c.highest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testCollections()
			mutate(&c[2])
			f := &fakeGalaxy{collections: c, indexOverride: c}
			s := newSource(t, f)
			if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
				t.Fatal("invalid upstream metadata was accepted")
			}
		})
	}
}

func TestListingConcurrencyOverlapsFetchesInIndexOrder(t *testing.T) {
	var want []string
	var collections []fakeCollection
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("c%02d", i)
		want = append(want, "acme."+name+"@1.0.0")
		collections = append(collections, fakeCollection{namespace: "acme", name: name, created: "2025-01-01T00:00:00Z", updated: "2025-01-02T00:00:00Z", highest: "1.0.0", versions: []fakeVersion{{"1.0.0", "2025-01-01T00:00:00Z"}}})
	}
	for workers, wantPeak := range map[int]int32{1: 1, 4: 0} {
		t.Run(fmt.Sprint("workers=", workers), func(t *testing.T) {
			f := &fakeGalaxy{collections: collections}
			var inflight, peak atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/versions/1.0.0/") {
					cur := inflight.Add(1)
					defer inflight.Add(-1)
					for p := peak.Load(); cur > p && !peak.CompareAndSwap(p, cur); p = peak.Load() {
					}
					// Earlier collections answer slowest, so only in-order
					// emission keeps the index order.
					name := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[3]
					idx, _ := strconv.Atoi(strings.TrimPrefix(name, "c"))
					time.Sleep(time.Duration(12-idx) * 5 * time.Millisecond)
				}
				f.handler(w, r)
			}))
			t.Cleanup(server.Close)
			s := &Source{Client: server.Client(), Endpoint: server.URL + "/api/", Attempts: 1, Concurrency: workers, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}}
			if got := versions(discover(t, s)); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want index order %v", got, want)
			}
			if wantPeak == 1 && peak.Load() != 1 {
				t.Fatalf("one worker overlapped %d detail requests", peak.Load())
			}
			if wantPeak == 0 && (peak.Load() < 2 || peak.Load() > 6) {
				t.Fatalf("peak overlap %d with 4 workers, want bounded concurrency above 1", peak.Load())
			}
		})
	}
}
