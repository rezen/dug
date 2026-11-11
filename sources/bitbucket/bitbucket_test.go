package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rezen/dug/sdk/registry"
)

const (
	shaA = "a872aeb9c7d66021f78539a28be854f3f0f050b6"
	shaB = "1111111111111111111111111111111111111111"
	shaC = "2222222222222222222222222222222222222222"
	shaH = "64b7afbea3f78955bbbeb2cf4054d8aa0441d00e"
)

func repoJSON(slug, uuid string, extra string) string {
	return fmt.Sprintf(`{"type":"repository","full_name":"acme/%s","slug":"%s","scm":"git","uuid":"%s","is_private":false,
"language":"nodejs","size":5915235,"created_on":"2013-07-24T23:40:13.276392+00:00","updated_on":"2026-11-11T22:40:19.810569+00:00",
"mainbranch":{"name":"master","type":"branch"},"links":{"html":{"href":"https://bitbucket.org/acme/%s"}},
"workspace":{"slug":"acme","name":"Acme","uuid":"{ws}","links":{"html":{"href":"https://bitbucket.org/acme/"}}}%s}`, slug, slug, uuid, slug, extra)
}

func tagJSON(name, sha, date string) string {
	return fmt.Sprintf(`{"name":"%s","type":"tag","target":{"type":"commit","hash":"%s","date":"%s"}}`, name, sha, date)
}

type fake struct {
	server   *httptest.Server
	requests atomic.Int32
	auth     []string
}

// newFake serves workspace acme: aui (three tags, over two pages), notags (no
// tags), a fork, and an empty repository.
func newFake(t *testing.T) (*fake, *Source) {
	t.Helper()
	f := &fake{}
	mux := http.NewServeMux()
	var base string
	api := func(path string, body func(r *http.Request) string) {
		mux.HandleFunc("/2.0/"+path, func(w http.ResponseWriter, r *http.Request) {
			f.requests.Add(1)
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			writeString(w, body(r))
		})
	}
	api("repositories/acme", func(r *http.Request) string {
		repos := []string{
			repoJSON("aui", "{1}", ""),
			repoJSON("notags", "{2}", ""),
			repoJSON("fork", "{3}", `,"parent":{"full_name":"other/aui"}`),
			strings.Replace(repoJSON("empty", "{4}", ""), `"mainbranch":{"name":"master","type":"branch"},`, `"mainbranch":null,`, 1),
		}
		if r.URL.Query().Get("page") == "2" {
			return `{"pagelen":2,"size":4,"page":2,"values":[` + repos[2] + `,` + repos[3] + `]}`
		}
		return `{"pagelen":2,"size":4,"page":1,"next":"` + base + `/2.0/repositories/acme?pagelen=100&sort=created_on&page=2","values":[` + repos[0] + `,` + repos[1] + `]}`
	})
	api("repositories/acme/aui", func(*http.Request) string { return repoJSON("aui", "{1}", "") })
	api("repositories/acme/aui/refs/tags", func(r *http.Request) string {
		if r.URL.Query().Get("page") == "2" {
			return `{"size":3,"values":[` + tagJSON("1.0.0", shaC, "2024-01-01T00:00:00+00:00") + `]}`
		}
		return `{"size":3,"next":"` + base + `/2.0/repositories/acme/aui/refs/tags?pagelen=100&sort=-target.date&page=2","values":[` +
			tagJSON("9.12.17", shaA, "2026-11-04T09:03:36+00:00") + `,` + tagJSON("2.0.0", shaB, "2025-06-01T00:00:00+00:00") + `]}`
	})
	api("repositories/acme/notags", func(*http.Request) string { return repoJSON("notags", "{2}", "") })
	api("repositories/acme/notags/refs/tags", func(*http.Request) string { return `{"size":0,"values":[]}` })
	api("repositories/acme/notags/refs/branches/master", func(*http.Request) string {
		return `{"name":"master","type":"branch","target":{"type":"commit","hash":"` + shaH + `","date":"2026-10-01T00:00:00+00:00"}}`
	})
	api("repositories/acme/aui/refs/branches/master", func(*http.Request) string {
		return `{"name":"master","type":"branch","target":{"type":"commit","hash":"` + shaH + `","date":"2026-11-10T00:00:00+00:00"}}`
	})
	api("repositories/acme/fork/refs/tags", func(*http.Request) string {
		return `{"size":1,"values":[` + tagJSON("fork-1", shaC, "2025-01-01T00:00:00+00:00") + `]}`
	})
	api("repositories/acme/empty/refs/tags", func(*http.Request) string { return `{"size":0,"values":[]}` })
	mux.HandleFunc("/acme/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		writeString(w, "archive:"+r.URL.Path)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	base = f.server.URL
	host := strings.TrimPrefix(base, "http://")
	return f, &Source{Client: f.server.Client(), Endpoint: base + "/2.0/", DownloadBase: base + "/", DownloadHosts: []string{host}, Attempts: 1, Workspaces: []string{"acme"}}
}

func writeString(w http.ResponseWriter, s string) { _, _ = io.WriteString(w, s) }

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return refs
}

func versions(refs []registry.Reference) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name+"@"+r.Version)
	}
	return out
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil, "")
	for _, options := range []map[string]string{
		nil, {"unknown": "x"}, {"workspaces": "../bad"}, {"workspaces": ""}, {"repos": "noslash"}, {"repos": "a/b/c"},
		{"workspaces": "acme", "snapshot": "sometimes"}, {"workspaces": "acme", "forks": "maybe"}, {"workspaces": "acme", "version_depth": "0"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{
		{"workspaces": "acme"}, {"workspaces": "acme, other", "snapshot": "only"}, {"repos": "acme/aui", "version_depth": "unlimited"},
		{"workspaces": "acme", "forks": "true", "version_depth": "2025-01-01"},
	} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
}

func TestDiscoverByDepth(t *testing.T) {
	for depth, want := range map[string][]string{
		"":           {"acme/aui@9.12.17", "acme/notags@master"},
		"2":          {"acme/aui@9.12.17", "acme/aui@2.0.0", "acme/notags@master"},
		"unlimited":  {"acme/aui@9.12.17", "acme/aui@2.0.0", "acme/aui@1.0.0", "acme/notags@master"},
		"2025-01-01": {"acme/aui@9.12.17", "acme/aui@2.0.0", "acme/notags@master"},
		"2099-01-01": {"acme/notags@master"},
	} {
		t.Run("depth="+depth, func(t *testing.T) {
			_, s := newFake(t)
			s.VersionDepth = depth
			if got := versions(discover(t, s)); !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestDiscoverFactsAndMetadata(t *testing.T) {
	_, s := newFake(t)
	s.VersionDepth = "2"
	refs := discover(t, s)
	r := refs[1] // aui 2.0.0
	if r.Namespace != Namespace || r.Attributes["commit"] != shaB || r.Attributes["kind"] != "tag" || len(r.Attributes) != 2 {
		t.Fatalf("attributes %+v", r)
	}
	if err := r.Facts.Validate(); err != nil {
		t.Fatal(err)
	}
	f := r.Facts
	if f.UpdatedAt == nil || f.UpdatedAt.Year() != 2026 || f.ReleasedAt == nil || f.ReleasedAt.Year() != 2025 {
		t.Fatalf("times %+v", f)
	}
	if f.Repository == nil || f.Repository.URL != "https://bitbucket.org/acme/aui" || f.Repository.Source != registry.RepositoryRegistry || f.Repository.Commit != shaB {
		t.Fatalf("repository %+v", f.Repository)
	}
	if f.Downloads != nil || f.VersionDownloads != nil || f.Installs != nil {
		t.Fatalf("published nothing about downloads, got %+v", f)
	}
	want := map[string]string{"kind": "tag", "version_rank": "2", "language": "nodejs", "repo_size": "5915235", "private": "false", "fork": "false",
		"default_branch": "master", "release_date_source": "git-commit-date", "repo_uuid": "{1}", "created_at": "2013-07-24T23:40:13Z"}
	if !maps.Equal(r.Metadata, want) {
		t.Fatalf("metadata %v, want %v", r.Metadata, want)
	}
	if r.Author == nil || r.Author.Username != "acme" || r.Author.Name != "Acme" || r.Author.ID != "{ws}" {
		t.Fatalf("author %+v", r.Author)
	}
	if refs[2].Attributes["kind"] != "snapshot" || refs[2].Metadata["kind"] != "snapshot" {
		t.Fatalf("snapshot %+v", refs[2])
	}
}

func TestSnapshotModesAndForks(t *testing.T) {
	_, s := newFake(t)
	s.Snapshot = SnapshotAlways
	if got := versions(discover(t, s)); !slices.Equal(got, []string{"acme/aui@9.12.17", "acme/aui@master", "acme/notags@master"}) {
		t.Fatalf("always: %v", got)
	}
	s.Snapshot = SnapshotOnly
	if got := versions(discover(t, s)); !slices.Equal(got, []string{"acme/aui@master", "acme/notags@master"}) {
		t.Fatalf("only: %v", got)
	}
	s.Snapshot = SnapshotNever
	if got := versions(discover(t, s)); !slices.Equal(got, []string{"acme/aui@9.12.17"}) {
		t.Fatalf("never: %v", got)
	}
	s.Snapshot, s.Forks = SnapshotNever, true
	if got := versions(discover(t, s)); !slices.Equal(got, []string{"acme/aui@9.12.17", "acme/fork@fork-1"}) {
		t.Fatalf("forks: %v", got)
	}
}

func TestExplicitRepoErrors(t *testing.T) {
	_, s := newFake(t)
	s.Workspaces = nil
	s.Repos = []string{"acme/missing"}
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("a missing explicit repository must fail")
	}
	s.Repos = []string{"acme/aui"}
	if got := versions(discover(t, s)); !slices.Equal(got, []string{"acme/aui@9.12.17"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPinnedDownloadAndToken(t *testing.T) {
	f, s := newFake(t)
	s.Token = "secret"
	refs := discover(t, s)
	artifact, err := s.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Revision != shaA || artifact.Blobs[0].Size != -1 || artifact.Blobs[0].Digest != "" || artifact.Blobs[0].Name != "aui-"+shaA[:12]+".tar.gz" {
		t.Fatalf("artifact %+v", artifact)
	}
	before := f.requests.Load()
	body, err := artifact.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "archive:/acme/aui/get/"+shaA+".tar.gz" || f.requests.Load() != before+1 {
		t.Fatalf("body %q, requests %d -> %d", data, before, f.requests.Load())
	}
	for i, auth := range f.auth {
		if auth != "Bearer secret" {
			t.Fatalf("request %d sent %q", i, auth)
		}
	}
}

func TestTokenNotSentToOtherHosts(t *testing.T) {
	var seen atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Store(r.Header.Get("Authorization")) }))
	defer other.Close()
	s := &Source{Token: "secret", Endpoint: "https://api.bitbucket.org/2.0/"}
	response, err := s.client().Get(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if v, _ := seen.Load().(string); v != "" {
		t.Fatalf("token sent to %s: %q", other.URL, v)
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	s := &Source{}
	good := registry.Reference{Registry: Identity, Name: "acme/aui", Version: "1", Attributes: map[string]string{"commit": shaA, "kind": "tag"}}
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*registry.Reference){
		"short commit":     func(r *registry.Reference) { r.Attributes["commit"] = "abc" },
		"uppercase commit": func(r *registry.Reference) { r.Attributes["commit"] = strings.ToUpper(shaA) },
		"path commit":      func(r *registry.Reference) { r.Attributes["commit"] = "../../" + shaA[:34] },
		"bad kind":         func(r *registry.Reference) { r.Attributes["kind"] = "release" },
		"other registry":   func(r *registry.Reference) { r.Registry = "other" },
		"path in name":     func(r *registry.Reference) { r.Name = "acme/../x" },
		"no workspace":     func(r *registry.Reference) { r.Name = "aui" },
		"empty version":    func(r *registry.Reference) { r.Version = "" },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
	// A download base off the allowlist (the metadata-address case) is refused.
	off := &Source{DownloadBase: "http://169.254.169.254/"}
	if _, err := off.Resolve(context.Background(), good); err == nil {
		t.Error("resolved an off-host download base")
	}
}

func TestPaginationStaysOnOrigin(t *testing.T) {
	s := &Source{Endpoint: "https://api.example/2.0/"}
	seen := map[string]bool{}
	if _, err := s.next("https://api.example/2.0/repositories/acme?page=2", seen); err != nil {
		t.Fatal(err)
	}
	if _, err := s.next("https://api.example/2.0/repositories/acme?page=2", seen); err == nil {
		t.Fatal("accepted a pagination cycle")
	}
	for _, raw := range []string{"https://other.example/2.0/x", "https://api.example/other/", "http://api.example/2.0/x", "/other"} {
		if _, err := s.next(raw, map[string]bool{}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestCancellationStopsDiscovery(t *testing.T) {
	_, s := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestSelectorRequired(t *testing.T) {
	if err := (&Source{}).Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("discovered without a selector")
	}
}
