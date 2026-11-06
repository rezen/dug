package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

const packageID = "7918f2cb-0742-4993-b58a-673129604cf6"

var (
	archive110 = []byte("widget 1.1.0 archive")
	archive120 = []byte("widget 1.2.0 archive")
	release120 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	release110 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	release100 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fake serves the Artifact Hub routes this source uses, with real field names.
type fake struct {
	server    *httptest.Server
	metadata  atomic.Int32 // Artifact Hub requests, everything except archives
	downloads atomic.Int32
	searches  []string
	total     int  // overrides Pagination-Total-Count when positive
	dupes     bool // repeat an entry on a later page, as a shifting catalog does
	noDigest  map[string]bool
	ociURL    bool // content_url points at an OCI registry, as Bitnami's does
}

func (f *fake) doc(version string, ts time.Time, digest string) map[string]any {
	doc := map[string]any{
		"package_id": packageID, "name": "widget", "normalized_name": "widget",
		"category_display_name": "Monitoring and logging", "keywords": []string{"Widget", "metrics"},
		"version": version, "app_version": "v" + version, "deprecated": false, "prerelease": false,
		"signed": true, "license": "Apache-2.0", "ts": ts.Unix(),
		"digest": digest, "content_url": f.server.URL + "/charts/widget-" + version + ".tgz",
		"links": []map[string]string{{"url": "https://github.com/acme/charts.git", "name": "Chart Source"}, {"url": "https://example.com", "name": "Homepage"}},
		"repository": map[string]any{
			"name": "acme", "url": f.server.URL + "/helm", "kind": 0, "official": true, "verified_publisher": true, "cncf": false,
			"organization_name": "acme-org", "organization_display_name": "Acme",
		},
	}
	return doc
}

func (f *fake) handler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case strings.HasPrefix(r.URL.Path, "/charts/"):
		f.downloads.Add(1)
		switch r.URL.Path {
		case "/charts/widget-1.2.0.tgz":
			w.Write(archive120)
		case "/charts/widget-1.1.0.tgz":
			w.Write(archive110)
		default:
			http.NotFound(w, r)
		}
		return
	case path == "packages/search":
		f.metadata.Add(1)
		f.searches = append(f.searches, r.URL.RawQuery)
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		repo := func(name, repoURL string) map[string]any {
			return map[string]any{"name": name, "url": repoURL, "kind": 0}
		}
		all := []map[string]any{
			{"package_id": packageID, "name": "widget", "normalized_name": "widget", "stars": 42, "repository": repo("acme", f.server.URL+"/helm")},
			{"package_id": "11111111-1111-1111-1111-111111111111", "name": "ocichart", "normalized_name": "ocichart", "stars": 7, "repository": repo("ociacme", "oci://ghcr.io/acme/charts/ocichart")},
		}
		if f.dupes {
			all = append(all, all[0])
		}
		end := min(offset+limit, len(all))
		page := []map[string]any{}
		if offset < len(all) {
			page = all[offset:end]
		}
		total := 2 // Artifact Hub counts unique packages
		if f.total > 0 {
			total = f.total
		}
		w.Header().Set("Pagination-Total-Count", strconv.Itoa(total))
		json.NewEncoder(w).Encode(map[string]any{"packages": page})
		return
	case strings.HasPrefix(path, "packages/helm/acme/widget"):
		f.metadata.Add(1)
		version := strings.TrimPrefix(strings.TrimPrefix(path, "packages/helm/acme/widget"), "/")
		digest := func(data []byte) string {
			if f.noDigest[version] || (version == "" && f.noDigest["1.2.0"]) {
				return ""
			}
			return digestOf(data)
		}
		var doc map[string]any
		switch version {
		case "", "1.2.0":
			doc = f.doc("1.2.0", release120, digest(archive120))
			doc["available_versions"] = []map[string]any{
				{"version": "1.2.0", "app_version": "v1.2.0", "ts": release120.Unix()},
				{"version": "1.1.0", "app_version": "v1.1.0", "ts": release110.Unix()},
				{"version": "1.0.0", "app_version": "v1.0.0", "ts": release100.Unix(), "prerelease": true},
			}
		case "1.1.0":
			doc = f.doc("1.1.0", release110, digest(archive110))
		case "1.0.0":
			// Published without a digest: cannot be pinned.
			doc = f.doc("1.0.0", release100, "")
			doc["prerelease"] = true
		default:
			http.NotFound(w, r)
			return
		}
		if f.ociURL {
			doc["content_url"] = "oci://registry-1.docker.io/acme/widget:" + doc["version"].(string)
		}
		json.NewEncoder(w).Encode(doc)
		return
	}
	http.NotFound(w, r)
}

func newFake(t *testing.T) (*fake, *Source) {
	t.Helper()
	f := &fake{noDigest: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.server.Close)
	host := strings.TrimPrefix(f.server.URL, "http://")
	return f, &Source{Client: f.server.Client(), Endpoint: f.server.URL + "/api/v1/", Attempts: 1, Interval: time.Nanosecond, SearchInterval: time.Nanosecond, DownloadHosts: []string{host}}
}

func collect(t *testing.T, s *Source) []registry.Reference {
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
		out = append(out, r.Version)
	}
	return out
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{
		{"unknown": "x"}, {"charts": "widget"}, {"charts": "../bad/chart"}, {"charts": "acme/"}, {"charts": ""},
		{"repos": "bad repo"}, {"search": ""}, {"search": strings.Repeat("x", 101)},
		{"page_size": "60"},
		{"charts": "acme/widget", "repos": "acme"}, {"charts": "acme/widget", "search": "x"},
		{"version_depth": "0"}, {"version_depth": "soon"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{
		nil, {"charts": "acme/widget, acme/other"}, {"repos": "acme,bitnami", "search": "metrics"},
		{"version_depth": "unlimited"}, {"version_depth": "2026-01-01"},
	} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	err := func() error { _, err := factory(map[string]string{"nope": "1"}); return err }()
	if err == nil || !strings.Contains(err.Error(), "supported: charts, repos, search, version_depth") {
		t.Errorf("unknown-option error does not list the keys: %v", err)
	}
}

func TestDiscoverVersionDepths(t *testing.T) {
	for depth, want := range map[string][]string{
		"1":          {"1.2.0"},
		"2":          {"1.2.0", "1.1.0"},
		"5":          {"1.2.0", "1.1.0"}, // 1.0.0 has no digest and is skipped
		"unlimited":  {"1.2.0", "1.1.0"},
		"2026-10-15": {"1.2.0"},
		"2026-09-15": {"1.2.0", "1.1.0"},
		"2099-01-01": {},
	} {
		t.Run(depth, func(t *testing.T) {
			_, s := newFake(t)
			s.Charts = []string{"acme/widget"}
			s.VersionDepth = depth
			if got := versions(collect(t, s)); !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestDiscoverFactsAndMetadata(t *testing.T) {
	_, s := newFake(t)
	s.Charts = []string{"acme/widget"}
	s.VersionDepth = "2"
	refs := collect(t, s)
	if len(refs) != 2 {
		t.Fatalf("got %d references", len(refs))
	}
	current, old := refs[0], refs[1]
	if current.Registry != Identity || current.Namespace != Namespace || current.Name != "acme/widget" {
		t.Errorf("identity: %+v", current)
	}
	for _, ref := range refs {
		if err := ref.Facts.Validate(); err != nil {
			t.Error(err)
		}
		f := ref.Facts
		if f.Downloads != nil || f.VersionDownloads != nil || f.Installs != nil {
			t.Errorf("unpublished facts must stay nil: %+v", f)
		}
		if f.UpdatedAt == nil || !f.UpdatedAt.Equal(release120) {
			t.Errorf("updated_at: %v", f.UpdatedAt)
		}
		if f.Repository == nil || f.Repository.URL != "https://github.com/acme/charts" || f.Repository.Source != registry.RepositoryDeclared {
			t.Errorf("repository: %+v", f.Repository)
		}
		if !slices.Equal(ref.Tags, []string{"metrics", "monitoring-and-logging", "widget"}) {
			t.Errorf("tags: %v", ref.Tags)
		}
		if ref.Author == nil || ref.Author.Username != "acme-org" || ref.Author.Name != "Acme" {
			t.Errorf("author: %+v", ref.Author)
		}
		if _, ok := ref.Metadata["stars"]; ok {
			t.Error("stars are only in search results; explicit charts must not invent them")
		}
	}
	if current.Facts.ReleasedAt == nil || !current.Facts.ReleasedAt.Equal(release120) || !old.Facts.ReleasedAt.Equal(release110) {
		t.Errorf("released_at: %v %v", current.Facts.ReleasedAt, old.Facts.ReleasedAt)
	}
	wantCurrent := map[string]string{
		"repository": "acme", "official": "true", "verified_publisher": "true", "cncf": "false",
		"category": "Monitoring and logging", "version_rank": "1", "prerelease": "false", "app_version": "v1.2.0",
		"deprecated": "false", "signed": "true", "license": "Apache-2.0", "release_date_source": "artifacthub-ts",
	}
	if !maps.Equal(current.Metadata, wantCurrent) {
		t.Errorf("metadata: %v", current.Metadata)
	}
	if old.Metadata["version_rank"] != "2" || old.Metadata["app_version"] != "v1.1.0" {
		t.Errorf("old metadata: %v", old.Metadata)
	}
	if current.Attributes["sha256"] != "sha256:"+digestOf(archive120) || current.Attributes["package_id"] != packageID {
		t.Errorf("attributes: %v", current.Attributes)
	}
	if _, ok := current.Attributes["size"]; ok {
		t.Error("Artifact Hub publishes no size")
	}
	current.Facts.Repository.URL = "mutated"
	if old.Facts.Repository.URL == "mutated" {
		t.Error("references share mutable facts")
	}
}

func TestCatalogWalkFiltersAndSkipsOCI(t *testing.T) {
	f, s := newFake(t)
	s.Repos = []string{"acme", "ociacme"}
	s.Search = "widget metrics"
	refs := collect(t, s)
	if got := versions(refs); !slices.Equal(got, []string{"1.2.0"}) {
		t.Fatalf("got %v: the duplicate and the OCI chart must be dropped", got)
	}
	if refs[0].Metadata["stars"] != "42" {
		t.Errorf("stars: %v", refs[0].Metadata)
	}
	if len(f.searches) != 1 {
		t.Fatalf("made %d search requests, want 1: %v", len(f.searches), f.searches)
	}
	q, _ := url.ParseQuery(f.searches[0])
	if q.Get("offset") != "0" || q.Get("limit") != "60" || q.Get("kind") != "0" || q.Get("deprecated") != "true" || q.Get("sort") != "last_updated" {
		t.Errorf("search query: %v", q)
	}
	if !slices.Equal(q["repo"], []string{"acme", "ociacme"}) || q.Get("ts_query_web") != "widget metrics" {
		t.Errorf("filters not forwarded: %v", q)
	}
}

func TestCatalogWalkDedupesShiftedEntries(t *testing.T) {
	f, s := newFake(t)
	f.dupes = true
	f.total = 3
	if got := versions(collect(t, s)); !slices.Equal(got, []string{"1.2.0"}) {
		t.Fatalf("got %v: a chart repeated across pages must be emitted once", got)
	}
}

func TestCatalogShortfall(t *testing.T) {
	f, s := newFake(t)
	f.total = 50 // the search promises far more than it serves
	err := s.Discover(context.Background(), func(registry.Reference) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ended early") {
		t.Fatalf("got %v, want an early-end error", err)
	}
	f, s = newFake(t)
	f.total = 3 // one chart moved while walking: tolerated with a warning
	if got := versions(collect(t, s)); !slices.Equal(got, []string{"1.2.0"}) {
		t.Fatalf("got %v", got)
	}
}

func TestExplicitChartMustBeArchivable(t *testing.T) {
	f, s := newFake(t)
	f.noDigest["1.2.0"] = true
	s.Charts = []string{"acme/widget"}
	err := s.Discover(context.Background(), func(registry.Reference) error { return nil })
	if !errors.Is(err, errUnarchivable) {
		t.Fatalf("got %v, want errUnarchivable", err)
	}
	// During a catalog walk the same chart is skipped, not fatal.
	s.Charts = nil
	if got := collect(t, s); len(got) != 0 {
		t.Fatalf("emitted %v", versions(got))
	}
	s.Charts = []string{"acme/missing"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("a missing explicit chart must be an error")
	}
	s.Charts = []string{"acme/widget"}
	s.VersionDepth = "unlimited"
	f.noDigest["1.2.0"] = false
	f.noDigest["1.1.0"] = true
	if got := versions(collect(t, s)); !slices.Equal(got, []string{"1.2.0"}) {
		t.Fatalf("an older unarchivable version must be skipped, got %v", got)
	}
}

func TestOCIContentIsSkippedWholeChart(t *testing.T) {
	f, s := newFake(t)
	f.ociURL = true
	s.VersionDepth = "unlimited"
	before := f.metadata.Load()
	if got := collect(t, s); len(got) != 0 {
		t.Fatalf("emitted %v", versions(got))
	}
	// Search (2 pages of one request each at most) plus one document: no per-version requests.
	if n := f.metadata.Load() - before; n > 3 {
		t.Errorf("%d requests: an OCI chart must not be probed version by version", n)
	}
	s.Charts = []string{"acme/widget"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); !errors.Is(err, errOCIChart) {
		t.Fatalf("got %v, want errOCIChart for an explicit chart", err)
	}
}

func TestResolveOpensPinnedDownloadWithoutMetadataRequests(t *testing.T) {
	f, s := newFake(t)
	s.Charts = []string{"acme/widget"}
	s.VersionDepth = "2"
	refs := collect(t, s)
	before := f.metadata.Load()
	for i, want := range [][]byte{archive120, archive110} {
		artifact, err := s.Resolve(context.Background(), refs[i])
		if err != nil {
			t.Fatal(err)
		}
		blob := artifact.Blobs[0]
		wantDigest := "sha256:" + digestOf(want)
		if artifact.Revision != wantDigest || blob.Digest != wantDigest || blob.Size != -1 {
			t.Errorf("artifact: revision %s digest %s size %d", artifact.Revision, blob.Digest, blob.Size)
		}
		if blob.Name != fmt.Sprintf("acme-widget-%s.tgz", refs[i].Version) || blob.MediaType != "application/gzip" {
			t.Errorf("blob: %s %s", blob.Name, blob.MediaType)
		}
		for range 2 { // each Open starts at byte zero, even after the context that resolved is gone
			ctx, cancel := context.WithCancel(context.Background())
			body, err := blob.Open(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(body)
			body.Close()
			if string(got) != string(want) {
				t.Fatalf("got %q", got)
			}
		}
	}
	if f.metadata.Load() != before {
		t.Error("Resolve or Open queried Artifact Hub")
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	_, s := newFake(t)
	s.Charts = []string{"acme/widget"}
	good := collect(t, s)[0]
	for name, mutate := range map[string]func(*registry.Reference){
		"off-host URL":     func(r *registry.Reference) { r.Attributes["download_url"] = "http://169.254.169.254/latest/meta-data/" },
		"digest missing":   func(r *registry.Reference) { delete(r.Attributes, "sha256") },
		"bad digest":       func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:bad" },
		"uppercase digest": func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:" + strings.Repeat("A", 64) },
		"zero size":        func(r *registry.Reference) { r.Attributes["size"] = "0" },
		"bad package ID":   func(r *registry.Reference) { r.Attributes["package_id"] = "../x" },
		"other registry":   func(r *registry.Reference) { r.Registry = "other" },
		"path in name":     func(r *registry.Reference) { r.Name = "../acme/widget" },
		"no repository":    func(r *registry.Reference) { r.Name = "widget" },
		"slash in version": func(r *registry.Reference) { r.Version = "1/../2" },
		"empty version":    func(r *registry.Reference) { r.Version = "" },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatalf("untampered reference: %v", err)
	}
}

func TestDownloadHostRule(t *testing.T) {
	s := &Source{} // production rule: no DownloadHosts override
	const repo = "https://Charts.Acme.example/stable"
	for download, ok := range map[string]bool{
		"https://charts.acme.example/widget-1.0.0.tgz":                                     true,
		"https://CHARTS.acme.example:443/widget-1.0.0.tgz":                                 true,
		"https://github.com/acme/charts/releases/download/widget-1.0.0/widget-1.0.0.tgz":   true,
		"https://raw.githubusercontent.com/acme/charts/main/widget-1.0.0.tgz":              true,
		"https://evil.example/widget-1.0.0.tgz":                                            false,
		"https://charts.acme.example.evil.example/widget-1.0.0.tgz":                        false,
		"http://charts.acme.example/widget-1.0.0.tgz":                                      false,
		"https://charts.acme.example:8443/widget-1.0.0.tgz":                                false,
		"https://user:pw@charts.acme.example/widget-1.0.0.tgz":                             false,
		"https://169.254.169.254/latest/meta-data/":                                        false,
		"https://[::1]/widget.tgz":                                                         false,
		"https://localhost/widget.tgz":                                                     false,
		"https://intranet/widget.tgz":                                                      false,
		"https://vault.internal/widget.tgz":                                                false,
		"oci://charts.acme.example/widget":                                                 false,
		"":                                                                                 false,
		"https://objects.githubusercontent.com/github-production-release-asset/widget.tgz": false,
	} {
		if err := s.checkDownload(repo, download); (err == nil) != ok {
			t.Errorf("%q: got %v, want ok=%v", download, err, ok)
		}
	}
	// A repository hosted on a private name cannot widen the rule to itself.
	for _, repoURL := range []string{"https://169.254.169.254/charts", "http://localhost:8080/charts", "https://10.0.0.5/charts"} {
		u, _ := url.Parse(repoURL)
		if err := s.checkDownload(repoURL, "https://"+u.Host+"/widget.tgz"); err == nil {
			t.Errorf("repository %q granted itself as a download host", repoURL)
		}
	}
	if err := s.checkDownload("not a url", "https://github.com/a/b.tgz"); err == nil {
		t.Error("accepted an invalid repository URL")
	}
}

func TestCancellationStopsDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, s := range []*Source{
		{Charts: []string{"acme/widget"}, Attempts: 1},
		{Attempts: 1},
	} {
		if err := s.Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	}
}

func TestEmitErrorStopsDiscovery(t *testing.T) {
	_, s := newFake(t)
	s.Charts = []string{"acme/widget"}
	s.VersionDepth = "unlimited"
	stop := errors.New("limit reached")
	n := 0
	err := s.Discover(context.Background(), func(registry.Reference) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("got %v after %d emits", err, n)
	}
}

func TestSearchRequestsAreSpaced(t *testing.T) {
	f, s := newFake(t)
	s.SearchInterval = 40 * time.Millisecond
	f.total = 3 // one chart "moved": the walk repeats, three passes in all
	start := time.Now()
	collect(t, s)
	if elapsed := time.Since(start); len(f.searches) != 3 || elapsed < 70*time.Millisecond {
		t.Errorf("%d search requests took %s, want 3 spaced at least 40ms apart", len(f.searches), elapsed)
	}
}
