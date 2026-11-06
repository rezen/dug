package n8n

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{
		{"unknown": "x"},
		{"packages": "../bad"},
		{"packages": ""},
		{"packages": "a,,b"},
		{"packages": "@scope"},
		{"keyword": "Has Space"},
		{"keyword": "a,b"},
		{"keyword": ""},
		{"enumerate": "everything"},
		{"version_depth": "0"},
		{"packages": "a", "keyword": "node-red"},
		{"packages": "a", "enumerate": "search"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{
		nil,
		{"packages": "n8n-nodes-x,@acme/n8n-nodes-y"},
		{"keyword": "node-red", "enumerate": "ecosystems"},
		{"enumerate": "search", "version_depth": "unlimited"},
		{"version_depth": "2025-01-01"},
	} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	_, err := factory(map[string]string{"nope": "x"})
	if err == nil || !strings.Contains(err.Error(), "packages, keyword, enumerate, version_depth") {
		t.Errorf("unknown-option error does not list supported keys: %v", err)
	}
}

// fixture is a fake npm registry, download-count API and keyword index on one
// server. Tarball bytes are derived from the package and version so their
// SHA-512 can be published in the packument.
type fixture struct {
	server   *httptest.Server
	source   *Source
	meta     atomic.Int32 // requests that were not tarball downloads
	searches atomic.Int32
	indexes  atomic.Int32
	mu       sync.Mutex
	paths    []string
}

type testVersion struct {
	version    string
	published  string
	postinst   bool
	deprecated bool
	provenance bool
	user       string
	noIntegr   bool // publish only a legacy shasum
}

type testPackage struct {
	name     string
	latest   string
	keywords []string
	repo     any
	versions []testVersion
}

func tarball(name, version string) []byte { return []byte("tarball of " + name + "@" + version) }

func integrityOf(name, version string) string {
	sum := sha512.Sum512(tarball(name, version))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func (f *fixture) packument(p testPackage) map[string]any {
	times := map[string]any{"created": "2024-12-31T00:00:00.000Z", "modified": "2025-03-02T10:00:00.250Z"}
	versions := map[string]any{}
	for _, v := range p.versions {
		times[v.version] = v.published
		scripts := map[string]string{"build": "tsc"}
		if v.postinst {
			scripts["postinstall"] = "node setup.js"
		}
		dist := map[string]any{"tarball": "ignored", "unpackedSize": 1234}
		if v.noIntegr {
			dist["shasum"] = strings.Repeat("ab", 20)
		} else {
			dist["integrity"] = integrityOf(p.name, v.version)
		}
		if v.provenance {
			dist["attestations"] = map[string]any{"provenance": map[string]any{"predicateType": "https://slsa.dev/provenance/v1"}}
		}
		version := map[string]any{"name": p.name, "version": v.version, "scripts": scripts, "dist": dist}
		if v.deprecated {
			version["deprecated"] = "use a newer release"
		}
		if v.user != "" {
			version["_npmUser"] = map[string]any{"name": v.user, "email": v.user + "@example.com"}
		}
		versions[v.version] = version
	}
	doc := map[string]any{
		"name":        p.name,
		"dist-tags":   map[string]string{"latest": p.latest},
		"keywords":    p.keywords,
		"license":     "MIT",
		"time":        times,
		"versions":    versions,
		"maintainers": []any{map[string]any{"name": "acme"}},
	}
	if p.repo != nil {
		doc["repository"] = p.repo
	}
	return doc
}

var (
	alpha = testPackage{
		name: "n8n-nodes-alpha", latest: "1.1.0",
		keywords: []string{"n8n-community-node-package", "Slack", "chat"},
		repo:     map[string]any{"type": "git", "url": "git+https://github.com/acme/alpha.git", "directory": "packages/alpha"},
		versions: []testVersion{
			{version: "1.0.0", published: "2025-01-01T00:00:00.000Z", postinst: true, deprecated: true, user: "mallory"},
			{version: "1.1.0", published: "2025-02-01T00:00:00.000Z", provenance: true, user: "acme"},
			{version: "1.2.0-beta.1", published: "2025-03-01T00:00:00.000Z", user: "acme"},
		},
	}
	beta = testPackage{
		name: "@acme/n8n-nodes-beta", latest: "0.3.0",
		keywords: []string{"n8n-community-node-package"},
		repo:     "github:acme/beta",
		versions: []testVersion{{version: "0.3.0", published: "2025-04-01T00:00:00.000Z", noIntegr: true}},
	}
	other = testPackage{
		name: "n8n-nodes-unkeyworded", latest: "1.0.0", keywords: []string{"something-else"},
		versions: []testVersion{{version: "1.0.0", published: "2025-01-01T00:00:00.000Z"}},
	}
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	packages := map[string]testPackage{alpha.name: alpha, beta.name: beta, other.name: other}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		if strings.HasSuffix(path, ".tgz") {
			// /NAME/-/BASE-VERSION.tgz
			name, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/-/")
			base := name[strings.LastIndex(name, "/")+1:]
			version := strings.TrimSuffix(strings.TrimPrefix(rest, base+"-"), ".tgz")
			if _, ok := packages[name]; !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(tarball(name, version))
			return
		}
		f.meta.Add(1)
		switch {
		case path == "/-/v1/search":
			f.searches.Add(1)
			objects := []any{}
			if r.URL.Query().Get("from") == "0" {
				for _, p := range []testPackage{beta, alpha} {
					objects = append(objects, map[string]any{"package": map[string]any{"name": p.name, "version": p.latest}})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"total": 5, "objects": objects})
		case path == "/index/keywords/"+DefaultKeyword:
			f.indexes.Add(1)
			entry := func(name, ecosystem string) map[string]any {
				return map[string]any{"name": name, "ecosystem": ecosystem}
			}
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode(map[string]any{"packages": []any{entry(alpha.name, "npm"), entry(other.name, "npm"), entry("n8n-nodes-unpublished", "npm")}})
				return
			}
			w.Header().Add("Link", fmt.Sprintf(`<%s/index/keywords/%s?page=1&per_page=1000>; rel="first", <%s/index/keywords/%s?page=2&per_page=1000>; rel="next"`, f.server.URL, DefaultKeyword, f.server.URL, DefaultKeyword))
			json.NewEncoder(w).Encode(map[string]any{"packages": []any{entry(alpha.name, "npm"), entry("n8n-nodes-gone", "npm"), entry("some-go-module", "go")}})
		case strings.HasPrefix(path, "/downloads/point/last-month/"):
			spec := strings.TrimPrefix(path, "/downloads/point/last-month/")
			counts := map[string]int{alpha.name: 4200, beta.name: 17}
			if !strings.Contains(spec, ",") {
				if n, ok := counts[spec]; ok {
					fmt.Fprintf(w, `{"downloads": %d, "package": %q}`, n, spec)
					return
				}
				http.NotFound(w, r)
				return
			}
			doc := map[string]any{}
			for _, name := range strings.Split(spec, ",") {
				if n, ok := counts[name]; ok {
					doc[name] = map[string]any{"downloads": n}
				} else {
					doc[name] = nil
				}
			}
			json.NewEncoder(w).Encode(doc)
		case path == "/n8n-nodes-unpublished":
			io.WriteString(w, `{"name":"n8n-nodes-unpublished","time":{"unpublished":{"time":"2025-05-18T01:39:59.066Z"}}}`)
		default:
			if p, ok := packages[strings.TrimPrefix(path, "/")]; ok {
				json.NewEncoder(w).Encode(f.packument(p))
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	f.source = &Source{
		Client:        f.server.Client(),
		Registry:      f.server.URL,
		Downloads:     f.server.URL,
		Index:         f.server.URL + "/index/",
		Attempts:      1,
		DownloadHosts: []string{strings.TrimPrefix(f.server.URL, "http://")},
	}
	return f
}

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	err := s.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func versionsOf(refs []registry.Reference, name string) []string {
	var out []string
	for _, r := range refs {
		if r.Name == name {
			out = append(out, r.Version)
		}
	}
	return out
}

func TestDiscoverCatalogWalk(t *testing.T) {
	f := newFixture(t)
	var warnings bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnings, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	refs := discover(t, f.source)
	// Index order first (alpha), then what only search knew (beta). The
	// index's gone, unkeyworded, unpublished and non-npm entries are skipped.
	if got := []string{refs[0].Name, refs[1].Name}; len(refs) != 2 || got[0] != alpha.name || got[1] != beta.name {
		t.Fatalf("refs = %v", refs)
	}
	for _, skipped := range []string{"n8n-nodes-gone", "n8n-nodes-unkeyworded", "n8n-nodes-unpublished", "some-go-module"} {
		if !strings.Contains(warnings.String(), skipped) && skipped != "some-go-module" {
			t.Errorf("no warning for skipped package %s: %s", skipped, warnings.String())
		}
		if len(versionsOf(refs, skipped)) != 0 {
			t.Errorf("emitted %s", skipped)
		}
	}
	// npm search reports 5 but only 2 survived verification: the gap is announced.
	if !strings.Contains(warnings.String(), "fewer packages than npm search reports") {
		t.Errorf("gap not reported: %s", warnings.String())
	}

	a := refs[0]
	if a.Registry != Identity || a.Namespace != Namespace || a.Version != "1.1.0" {
		t.Fatalf("alpha = %+v", a)
	}
	if want := map[string]string{
		"download_url": f.server.URL + "/n8n-nodes-alpha/-/n8n-nodes-alpha-1.1.0.tgz",
		"integrity":    integrityOf(alpha.name, "1.1.0"),
	}; !maps.Equal(a.Attributes, want) {
		t.Errorf("attributes = %v, want %v", a.Attributes, want)
	}
	if want := map[string]string{"version_rank": "1", "release_date_source": "npm-time", "license": "MIT", "provenance": "true"}; !maps.Equal(a.Metadata, want) {
		t.Errorf("metadata = %v, want %v", a.Metadata, want)
	}
	var raw map[string]any
	if err := json.Unmarshal(a.Raw, &raw); err != nil || raw["version"] != "1.1.0" || raw["name"] != alpha.name {
		t.Errorf("raw = %s (%v)", a.Raw, err)
	}
	if !slices.Equal(a.Tags, []string{"chat", "n8n-community-node-package", "slack"}) {
		t.Errorf("tags = %v", a.Tags)
	}
	if a.Author == nil || a.Author.Username != "acme" || a.Author.Email != "acme@example.com" {
		t.Errorf("author = %+v", a.Author)
	}
	facts := a.Facts
	if facts == nil || facts.Validate() != nil {
		t.Fatalf("facts = %+v (%v)", facts, facts.Validate())
	}
	if facts.Downloads == nil || facts.Downloads.Month == nil || *facts.Downloads.Month != 4200 {
		t.Errorf("monthly downloads = %+v", facts.Downloads)
	}
	if facts.Downloads.Total != nil || facts.Downloads.Week != nil || facts.Downloads.Day != nil || facts.Downloads.Quarter != nil || facts.Downloads.Year != nil {
		t.Errorf("derived or invented download windows: %+v", facts.Downloads)
	}
	if facts.VersionDownloads != nil || facts.Installs != nil {
		t.Errorf("invented facts: %+v", facts)
	}
	if facts.UpdatedAt == nil || !facts.UpdatedAt.Equal(time.Date(2025, 3, 2, 10, 0, 0, 250_000_000, time.UTC)) {
		t.Errorf("updated = %v", facts.UpdatedAt)
	}
	if facts.ReleasedAt == nil || !facts.ReleasedAt.Equal(time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("released = %v", facts.ReleasedAt)
	}
	if r := facts.Repository; r == nil || r.URL != "https://github.com/acme/alpha" || r.Directory != "packages/alpha" || r.Source != registry.RepositoryDeclared {
		t.Errorf("repository = %+v", r)
	}
	for _, key := range []string{"downloads", "monthly_downloads", "released_at", "updated_at", "repository"} {
		if _, ok := a.Metadata[key]; ok {
			t.Errorf("typed fact %q duplicated in metadata", key)
		}
	}

	// A scoped package published without an integrity field pins its SHA-1.
	b := refs[1]
	if b.Name != beta.name || b.Attributes["integrity"] != "sha1-"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 20)) {
		t.Errorf("beta attributes = %v", b.Attributes)
	}
	if b.Author != nil || b.Facts.Repository == nil || b.Facts.Repository.URL != "https://github.com/acme/beta" {
		t.Errorf("beta author=%+v repository=%+v", b.Author, b.Facts.Repository)
	}
	if b.Facts.Downloads == nil || *b.Facts.Downloads.Month != 17 {
		t.Errorf("beta downloads = %+v", b.Facts.Downloads)
	}
	// Both references must be resolvable: listing self-resolves.
	for _, ref := range refs {
		if _, err := f.source.Resolve(context.Background(), ref); err != nil {
			t.Errorf("%s@%s: %v", ref.Name, ref.Version, err)
		}
	}
}

func TestVersionDepth(t *testing.T) {
	f := newFixture(t)
	for depth, want := range map[string][]string{
		"":            {"1.1.0"},
		"1":           {"1.1.0"},
		"2":           {"1.1.0", "1.2.0-beta.1"}, // current first, then newest published
		"unlimited":   {"1.1.0", "1.2.0-beta.1", "1.0.0"},
		"2025-01-15":  {"1.1.0", "1.2.0-beta.1"},
		"2025-02-15":  {"1.2.0-beta.1"}, // the current release predates the cutoff
		"2025-03-01":  {"1.2.0-beta.1"},
		"2025-03-02":  {},
		"2099-01-01":  {},
		"2020-01-01":  {"1.1.0", "1.2.0-beta.1", "1.0.0"},
		"unlimited ":  nil,
		"not-a-depth": nil,
	} {
		s := *f.source
		s.Packages = []string{alpha.name}
		s.VersionDepth = depth
		var got []string
		err := s.Discover(context.Background(), func(r registry.Reference) error {
			got = append(got, r.Version)
			if r.Metadata["version_rank"] != fmt.Sprint(len(got)) {
				t.Errorf("depth %q: %s has version_rank %s", depth, r.Version, r.Metadata["version_rank"])
			}
			return nil
		})
		if want == nil {
			if err == nil {
				t.Errorf("depth %q accepted", depth)
			}
			continue
		}
		if err != nil {
			t.Errorf("depth %q: %v", depth, err)
			continue
		}
		if !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
			t.Errorf("depth %q: got %v, want %v", depth, got, want)
		}
	}
	// Per-version fields: deprecated install-script release by another publisher.
	s := *f.source
	s.Packages = []string{alpha.name}
	s.VersionDepth = "unlimited"
	var oldest registry.Reference
	if err := s.Discover(context.Background(), func(r registry.Reference) error { oldest = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if oldest.Version != "1.0.0" || oldest.Metadata["install_script"] != "true" || oldest.Metadata["deprecated"] != "true" || oldest.Metadata["provenance"] != "" {
		t.Errorf("1.0.0 metadata = %v", oldest.Metadata)
	}
	if oldest.Author == nil || oldest.Author.Username != "mallory" {
		t.Errorf("1.0.0 publisher = %+v", oldest.Author)
	}
	if !oldest.Facts.ReleasedAt.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("1.0.0 released = %v", oldest.Facts.ReleasedAt)
	}
}

func TestEnumerationModes(t *testing.T) {
	f := newFixture(t)
	for mode, want := range map[string][]string{
		EnumerateEcosystems: {alpha.name},
		EnumerateSearch:     {beta.name, alpha.name},
		EnumerateAll:        {alpha.name, beta.name},
	} {
		f.searches.Store(0)
		f.indexes.Store(0)
		s := *f.source
		s.Enumerate = mode
		var got []string
		for _, ref := range discover(t, &s) {
			got = append(got, ref.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", mode, got, want)
		}
		if (mode == EnumerateEcosystems) != (f.searches.Load() == 0) || (mode == EnumerateSearch) != (f.indexes.Load() == 0) {
			t.Errorf("%s: searches=%d index pages=%d", mode, f.searches.Load(), f.indexes.Load())
		}
	}
	s := *f.source
	s.Enumerate = "bogus"
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Error("accepted a bogus enumerate mode")
	}
}

func TestExplicitPackages(t *testing.T) {
	f := newFixture(t)
	s := *f.source
	s.Packages = []string{beta.name, alpha.name, beta.name} // duplicate listed twice
	var names []string
	for _, r := range discover(t, &s) {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{beta.name, alpha.name}) {
		t.Fatalf("names = %v", names)
	}
	if f.searches.Load() != 0 || f.indexes.Load() != 0 {
		t.Error("explicit packages consulted an index")
	}
	// An explicitly requested package that is gone is an error, not a skip.
	s.Packages = []string{"n8n-nodes-gone"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "n8n-nodes-gone") {
		t.Fatalf("missing explicit package error = %v", err)
	}
	// Explicit selection does not require the keyword.
	s.Packages = []string{other.name}
	if got := discover(t, &s); len(got) != 1 || got[0].Name != other.name {
		t.Fatalf("explicit non-keyword package = %v", got)
	}
	s.Packages = []string{"../escape"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("accepted an invalid package name")
	}
}

func TestIndexPaginationSafety(t *testing.T) {
	for name, link := range map[string]func(self string) string{
		"cycle":         func(self string) string { return self },
		"cross-origin":  func(string) string { return "http://169.254.169.254/api/v1/keywords/x?page=2" },
		"other-path":    func(self string) string { return strings.Replace(self, "/index/", "/elsewhere/", 1) },
		"other-scheme":  func(self string) string { return strings.Replace(self, "http://", "https://", 1) },
		"relative-away": func(string) string { return "/not-the-api" },
	} {
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				self := server.URL + r.URL.RequestURI()
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, link(self)))
				io.WriteString(w, `{"packages": []}`)
			}))
			defer server.Close()
			s := &Source{Client: server.Client(), Index: server.URL + "/index/", Attempts: 1, Enumerate: EnumerateEcosystems}
			err := s.Discover(context.Background(), func(registry.Reference) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "pagination") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if got := nextLink([]string{`<https://x/a?page=1>; rel="first", <https://x/a?page=2>; rel="next"`}); got != "https://x/a?page=2" {
		t.Errorf("nextLink = %q", got)
	}
	if got := nextLink([]string{`<https://x/a?page=1>; rel="first"`, `<https://x/b>; rel=prev`}); got != "" {
		t.Errorf("nextLink without next = %q", got)
	}
}

func TestResolvePinnedDownload(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f.source)
	ref := refs[0]
	f.meta.Store(0)
	artifact, err := f.source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Revision != integrityOf(alpha.name, "1.1.0") || len(artifact.Blobs) != 1 {
		t.Fatalf("artifact = %+v", artifact)
	}
	blob := artifact.Blobs[0]
	if blob.Name != "n8n-nodes-alpha-1.1.0.tgz" || blob.Digest != "" || blob.Size != -1 || blob.MediaType != "application/gzip" {
		t.Fatalf("blob = %+v", blob)
	}
	// Open uses its own context and may be called repeatedly from byte zero.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 2 {
		body, err := blob.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Equal(data, tarball(alpha.name, "1.1.0")) {
			t.Fatalf("download = %q, %v", data, err)
		}
	}
	if _, err := blob.Open(ctx); err == nil {
		t.Error("Open ignored its cancelled context")
	}
	if f.meta.Load() != 0 {
		t.Errorf("Resolve and Open made %d metadata requests", f.meta.Load())
	}
	// A scoped package's tarball lives under its scope but is named by its basename.
	scoped, err := f.source.Resolve(context.Background(), refs[1])
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Blobs[0].Name != "n8n-nodes-beta-0.3.0.tgz" {
		t.Errorf("scoped blob name = %q", scoped.Blobs[0].Name)
	}
	body, err := scoped.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
}

func TestOpenRejectsTamperedBytes(t *testing.T) {
	f := newFixture(t)
	ref := discover(t, f.source)[0]
	// The registry's published hash is the only authority: a different
	// tarball behind the same URL must fail the transfer, not be archived.
	ref.Attributes = maps.Clone(ref.Attributes)
	ref.Attributes["integrity"] = integrityOf(alpha.name, "9.9.9")
	artifact, err := f.source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifact.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("read of mismatching tarball = %v", err)
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	f := newFixture(t)
	good := discover(t, f.source)[0]
	if _, err := f.source.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	offHost := func(raw string) func(*registry.Reference) {
		return func(r *registry.Reference) { r.Attributes["download_url"] = raw }
	}
	sha256SRI := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	for name, mutate := range map[string]func(*registry.Reference){
		"link-local URL":      offHost("http://169.254.169.254/latest/meta-data/"),
		"loopback URL":        offHost("http://127.0.0.1:1/n8n-nodes-alpha/-/n8n-nodes-alpha-1.1.0.tgz"),
		"credentials in URL":  offHost(strings.Replace(good.Attributes["download_url"], "http://", "http://user:pw@", 1)),
		"other package path":  offHost(f.server.URL + "/n8n-nodes-beta/-/n8n-nodes-beta-0.3.0.tgz"),
		"other version path":  offHost(f.server.URL + "/n8n-nodes-alpha/-/n8n-nodes-alpha-1.0.0.tgz"),
		"query appended":      offHost(good.Attributes["download_url"] + "?redirect=1"),
		"missing URL":         func(r *registry.Reference) { delete(r.Attributes, "download_url") },
		"missing integrity":   func(r *registry.Reference) { delete(r.Attributes, "integrity") },
		"sha256 integrity":    func(r *registry.Reference) { r.Attributes["integrity"] = sha256SRI },
		"truncated integrity": func(r *registry.Reference) { r.Attributes["integrity"] = r.Attributes["integrity"][:40] },
		"integrity list":      func(r *registry.Reference) { r.Attributes["integrity"] += " " + r.Attributes["integrity"] },
		"other registry":      func(r *registry.Reference) { r.Registry = "npm" },
		"path in name":        func(r *registry.Reference) { r.Name = "../n8n-nodes-alpha" },
		"empty name":          func(r *registry.Reference) { r.Name = "" },
		"path in version":     func(r *registry.Reference) { r.Version = "1.1.0/../../x" },
		"empty version":       func(r *registry.Reference) { r.Version = "" },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := f.source.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
	// With the production allowlist, a test-server URL is off host.
	if _, err := (&Source{}).Resolve(context.Background(), good); err == nil {
		t.Error("the default allowlist accepted a non-npm host")
	}
	// And the default host with the right path resolves with no network.
	ref := registry.Reference{Registry: Identity, Name: "@acme/pkg", Version: "2.0.0", Attributes: map[string]string{
		"download_url": "https://registry.npmjs.org/@acme/pkg/-/pkg-2.0.0.tgz",
		"integrity":    integrityOf("@acme/pkg", "2.0.0"),
	}}
	if _, err := (&Source{}).Resolve(context.Background(), ref); err != nil {
		t.Errorf("default-host resolve: %v", err)
	}
}

func TestDiscoverHonorsCancellationAndEmitErrors(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.source.Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery = %v", err)
	}
	// Cancelling from inside emit stops before the next package.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	var seen int
	err := f.source.Discover(ctx, func(registry.Reference) error { seen++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("cancel in emit: seen=%d err=%v", seen, err)
	}
	stop := errors.New("host stopped")
	if err := f.source.Discover(context.Background(), func(registry.Reference) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("emit error = %v", err)
	}
}

func TestDiscoverRejectsInvalidMetadata(t *testing.T) {
	f := newFixture(t)
	// A package document whose version entry names a different package is an
	// error, never a guess.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := f.packument(alpha)
		doc["versions"].(map[string]any)["1.1.0"].(map[string]any)["name"] = "someone-elses-package"
		json.NewEncoder(w).Encode(doc)
	}))
	defer bad.Close()
	s := &Source{Client: bad.Client(), Registry: bad.URL, Downloads: bad.URL, Attempts: 1, Packages: []string{alpha.name},
		DownloadHosts: []string{strings.TrimPrefix(bad.URL, "http://")}}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Error("accepted a version entry for another package")
	}
	// A date cutoff over a version with no publish time cannot be honored.
	noTime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			io.WriteString(w, `{"downloads": 1}`)
			return
		}
		doc := f.packument(alpha)
		delete(doc["time"].(map[string]any), "1.1.0")
		json.NewEncoder(w).Encode(doc)
	}))
	defer noTime.Close()
	s = &Source{Client: noTime.Client(), Registry: noTime.URL, Downloads: noTime.URL, Attempts: 1, Packages: []string{alpha.name}, VersionDepth: "2025-01-01",
		DownloadHosts: []string{strings.TrimPrefix(noTime.URL, "http://")}}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Error("honored a date cutoff without a publish time")
	}
	s.VersionDepth = "unlimited"
	var got []string
	if err := s.Discover(context.Background(), func(r registry.Reference) error { got = append(got, r.Version); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "1.1.0") {
		t.Errorf("version without a publish time dropped: %v", got)
	}
}
