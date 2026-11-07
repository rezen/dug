package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

const (
	sha0 = "0000000000000000000000000000000000000000"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
	sha4 = "4444444444444444444444444444444444444444"
	shaM = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tagT = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

const assetBytes = "tgz bytes"

type fake struct {
	t         *testing.T
	api       *httptest.Server
	files     *httptest.Server
	metadata  atomic.Int32 // JSON metadata requests
	authed    atomic.Int32 // API requests carrying the bearer token
	limited   atomic.Bool  // answer the next repository lookup with a rate limit
	searches  atomic.Int32
	downloads atomic.Int32
}

func date(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func (f *fake) repos() map[string]map[string]any {
	repo := func(id int64, full string, fork bool, created string) map[string]any {
		owner, name, _ := strings.Cut(full, "/")
		return map[string]any{"id": id, "full_name": full, "name": name, "fork": fork, "private": false, "archived": false, "default_branch": "main", "html_url": "https://github.com/" + full, "language": "Go", "stargazers_count": 42, "forks_count": 3, "open_issues_count": 1, "created_at": created, "pushed_at": "2026-02-01T00:00:00Z", "license": map[string]any{"spdx_id": "MIT"}, "topics": []string{"Security", "go", "security"}, "owner": map[string]any{"login": owner, "id": 99, "type": "Organization", "html_url": "https://github.com/" + owner}}
	}
	private := func(r map[string]any) map[string]any { r["private"] = true; return r }
	return map[string]map[string]any{
		"acme/widget":      repo(1, "acme/widget", false, "2010-01-01T00:00:00Z"),
		"acme/tool":        private(repo(2, "acme/tool", false, "2011-01-01T00:00:00Z")),
		"acme/empty":       repo(3, "acme/empty", false, "2012-01-01T00:00:00Z"),
		"acme/widget-fork": repo(4, "acme/widget-fork", true, "2013-01-01T00:00:00Z"),
		"alice/blog":       repo(5, "alice/blog", false, "2015-01-01T00:00:00Z"),
		"alice/notes":      repo(6, "alice/notes", true, "2020-01-01T00:00:00Z"),
	}
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(assetBytes)))
	f.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token forwarded to download host for %s", r.URL)
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case strings.HasPrefix(r.URL.Path, "/zip/"): // API zipball redirect target
			io.WriteString(w, "zip:"+strings.TrimPrefix(r.URL.Path, "/zip/"))
		case strings.HasPrefix(r.URL.Path, "/asset/"): // API asset redirect target
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset download without octet-stream accept: %q", r.Header.Get("Accept"))
			}
			io.WriteString(w, assetBytes)
		case len(parts) == 4 && parts[2] == "zip": // codeload.github.com/OWNER/NAME/zip/SHA
			io.WriteString(w, "zip:"+parts[3])
		case len(parts) == 6 && parts[2] == "releases" && parts[3] == "download": // github.com/OWNER/NAME/releases/download/TAG/NAME
			if parts[4] != "v3" || parts[5] != "tool.tgz" {
				t.Errorf("unexpected direct asset %s", r.URL.Path)
			}
			io.WriteString(w, assetBytes)
		default:
			t.Errorf("unexpected download request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	// A different host name than the API server, so Go drops Authorization on redirect.
	_, port, _ := net.SplitHostPort(f.files.Listener.Addr().String())
	filesBase := "http://localhost:" + port
	created := regexp.MustCompile(`created:(\S+)\.\.(\S+)`)
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer secret" {
			f.authed.Add(1)
		}
		if r.Header.Get("X-GitHub-Api-Version") == "" || !strings.HasPrefix(r.Header.Get("User-Agent"), "dug/") {
			t.Errorf("missing API headers on %s", r.URL)
		}
		base := "http://" + r.Host
		write := func(v any) {
			f.metadata.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
		link := func(next string) {
			w.Header().Set("Link", `<`+base+next+`>; rel="next", <`+base+r.URL.Path+`?page=1>; rel="first"`)
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		repos := f.repos()
		switch {
		case r.URL.Path == "/users/acme":
			write(map[string]any{"type": "Organization"})
		case r.URL.Path == "/users/alice":
			write(map[string]any{"type": "User"})
		case r.URL.Path == "/orgs/acme/repos":
			if r.URL.Query().Get("page") == "2" {
				write([]any{repos["acme/empty"]})
				return
			}
			items := []any{repos["acme/widget"]}
			if r.URL.Query().Get("type") == "all" {
				items = append(items, repos["acme/widget-fork"])
			}
			link("/orgs/acme/repos?page=2&type=" + r.URL.Query().Get("type"))
			write(append(items, repos["acme/tool"]))
		case r.URL.Path == "/users/alice/repos":
			write([]any{repos["alice/notes"], repos["alice/blog"]})
		case r.URL.Path == "/search/repositories":
			f.searches.Add(1)
			q := r.URL.Query().Get("q")
			m := created.FindStringSubmatch(q)
			if !strings.HasPrefix(q, "topic:dug ") || m == nil {
				t.Errorf("unexpected search query %q", q)
			}
			from, to := date(strings.Replace(m[1], "+00:00", "Z", 1)), date(strings.Replace(m[2], "+00:00", "Z", 1))
			var items []any
			for _, full := range []string{"acme/widget", "alice/blog", "alice/notes"} {
				if c := date(repos[full]["created_at"].(string)); !c.Before(from) && !c.After(to) {
					items = append(items, repos[full])
				}
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			page = max(page, 1)
			if page < len(items) {
				link("/search/repositories?" + url.Values{"page": {strconv.Itoa(page + 1)}, "q": {q}}.Encode())
			}
			var shown []any
			if page <= len(items) {
				shown = items[page-1 : page]
			}
			write(map[string]any{"total_count": len(items), "incomplete_results": false, "items": shown})
		case len(parts) == 3 && parts[0] == "repos":
			if f.limited.CompareAndSwap(true, false) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"API rate limit exceeded"}`)
				return
			}
			repo, ok := repos[parts[1]+"/"+parts[2]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			write(repo)
		case len(parts) == 4 && parts[0] == "repos" && parts[3] == "releases":
			if parts[1]+"/"+parts[2] != "acme/widget" {
				write([]any{})
				return
			}
			asset := func(id int64, name, state string, downloads int64) map[string]any {
				return map[string]any{"id": id, "name": name, "size": len(assetBytes), "state": state, "download_count": downloads, "digest": digest, "content_type": "application/gzip"}
			}
			if r.URL.Query().Get("page") == "2" {
				write([]any{
					map[string]any{"id": 1, "tag_name": "v1", "draft": true, "created_at": "2024-01-01T00:00:00Z"},
					map[string]any{"id": 10, "tag_name": "v0", "published_at": "2020-01-01T00:00:00Z"},
				})
				return
			}
			link("/repos/acme/widget/releases?page=2")
			write([]any{
				map[string]any{"id": 3, "tag_name": "v3", "published_at": "2026-01-01T00:00:00Z", "assets": []any{asset(31, "tool.tgz", "uploaded", 5), asset(32, "broken.zip", "open", 2)}},
				map[string]any{"id": 2, "tag_name": "v2", "prerelease": true, "published_at": "2025-06-01T00:00:00Z", "assets": []any{asset(21, "bad digest", "uploaded", 0)}},
			})
		case len(parts) == 6 && parts[3] == "git" && parts[4] == "ref" && parts[5] == "tags":
			http.NotFound(w, r)
		case len(parts) == 7 && parts[3] == "git" && parts[4] == "ref" && parts[5] == "tags":
			switch parts[6] {
			case "v3": // annotated tag, peeled through git/tags
				write(map[string]any{"object": map[string]any{"type": "tag", "sha": tagT}})
			case "v2":
				write(map[string]any{"object": map[string]any{"type": "commit", "sha": sha2}})
			case "v0":
				write(map[string]any{"object": map[string]any{"type": "commit", "sha": sha0}})
			default:
				http.NotFound(w, r)
			}
		case len(parts) == 6 && parts[3] == "git" && parts[4] == "tags" && parts[5] == tagT:
			write(map[string]any{"object": map[string]any{"type": "commit", "sha": sha3}})
		case len(parts) == 5 && parts[3] == "branches" && parts[4] == "main":
			if parts[1]+"/"+parts[2] == "acme/empty" {
				http.NotFound(w, r)
				return
			}
			write(map[string]any{"name": "main", "commit": map[string]any{"sha": shaM, "commit": map[string]any{"committer": map[string]any{"date": "2026-02-01T00:00:00Z"}}}})
		case len(parts) == 4 && parts[3] == "tags":
			if parts[1]+"/"+parts[2] != "acme/widget" {
				write([]any{})
				return
			}
			tag := func(name, sha string) map[string]any {
				return map[string]any{"name": name, "commit": map[string]any{"sha": sha}}
			}
			if r.URL.Query().Get("page") == "2" {
				write([]any{tag("v2", sha2), tag("v0", sha0)})
				return
			}
			link("/repos/acme/widget/tags?page=2")
			write([]any{tag("v4", sha4), tag("v3", sha3)})
		case len(parts) == 5 && parts[3] == "commits":
			dates := map[string]string{"v4": "2026-03-01T00:00:00Z", sha4: "2026-03-01T00:00:00Z", sha3: "2026-01-01T00:00:00Z", sha2: "2025-06-01T00:00:00Z", sha0: "2020-01-01T00:00:00Z"}
			when, ok := dates[parts[4]]
			if parts[1]+"/"+parts[2] != "acme/widget" || !ok {
				http.NotFound(w, r)
				return
			}
			sha := parts[4]
			if sha == "v4" {
				sha = sha4
			}
			write(map[string]any{"sha": sha, "commit": map[string]any{"author": map[string]any{"date": when}}})
		case len(parts) == 5 && parts[3] == "zipball":
			http.Redirect(w, r, filesBase+"/zip/"+parts[4], http.StatusFound)
		case len(parts) == 6 && parts[3] == "releases" && parts[4] == "assets":
			if r.Header.Get("Accept") != "application/octet-stream" {
				write(map[string]any{"id": parts[5]})
				return
			}
			http.Redirect(w, r, filesBase+"/asset/"+parts[5], http.StatusFound)
		default:
			t.Errorf("unexpected API request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.files.Close)
	return f
}

func (f *fake) source() *Source {
	return &Source{Client: f.api.Client(), Endpoint: f.api.URL, Token: "secret", Attempts: 2, Assets: true, CodeloadEndpoint: f.files.URL, ReleaseEndpoint: f.files.URL}
}

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
		t.Fatal(err)
	}
	return refs
}

func versions(refs []registry.Reference) []string {
	got := []string{}
	for _, ref := range refs {
		got = append(got, ref.Name+"@"+ref.Version)
	}
	return got
}

func TestReleasesVersionDepthAssetsAndDownload(t *testing.T) {
	f := newFake(t)
	for depth, want := range map[string][]string{"1": {"v3"}, "2": {"v3", "v2"}, "unlimited": {"v3", "v2", "v0"}, "2021-01-01": {"v3", "v2"}, "2027-01-01": {}} {
		t.Run(depth, func(t *testing.T) {
			s := f.source()
			s.Repos = []string{"acme/widget"}
			s.VersionDepth = depth
			refs := discover(t, s)
			got := []string{}
			for _, ref := range refs {
				got = append(got, ref.Version)
				if ref.Registry != "github" || ref.Namespace != "127.0.0.1" || ref.Name != "acme/widget" || ref.Metadata["kind"] != "release" || ref.Metadata["stars"] != "42" || ref.Metadata["license"] != "MIT" || ref.Metadata["updated_at"] != "" || ref.Metadata["html_url"] != "" {
					t.Fatal(ref)
				}
				var raw map[string]any
				if err := json.Unmarshal(ref.Raw, &raw); err != nil || raw["full_name"] != "acme/widget" {
					t.Fatalf("raw = %s (%v)", ref.Raw, err)
				}
				f := ref.Facts
				if f == nil || f.UpdatedAt == nil || f.UpdatedAt.Format(time.RFC3339) != "2026-02-01T00:00:00Z" || f.Repository == nil || f.Repository.URL != "https://github.com/acme/widget" || f.Repository.Source != registry.RepositoryRegistry || f.Repository.Commit != ref.Attributes["commit"] || f.Downloads != nil || f.Installs != nil {
					t.Fatalf("facts: %+v", f)
				}
				if !reflect.DeepEqual(ref.Tags, []string{"go", "security"}) || !reflect.DeepEqual(ref.Author, &registry.Author{Username: "acme", ID: "99", URL: "https://github.com/acme"}) || ref.Metadata["owner_type"] != "Organization" {
					t.Fatalf("tags=%v author=%+v metadata=%v", ref.Tags, ref.Author, ref.Metadata)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
	s := f.source()
	s.Repos = []string{"acme/widget"}
	s.VersionDepth = "2"
	refs := discover(t, s)
	v3, v2 := refs[0], refs[1]
	if v3.Attributes["commit"] != sha3 || v3.Attributes["release_id"] != "3" || v3.Attributes["download"] != "direct" || v3.Metadata["prerelease"] != "false" || v3.Metadata["downloads"] != "" || v3.Metadata["asset_count"] != "1" || v3.Metadata["version_rank"] != "1" || v3.Metadata["released_at"] != "" {
		t.Fatal(v3)
	}
	if v3.Facts == nil || v3.Facts.VersionDownloads == nil || v3.Facts.VersionDownloads.Total == nil || *v3.Facts.VersionDownloads.Total != 7 || v3.Facts.ReleasedAt == nil || v3.Facts.ReleasedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" || v3.Facts.Repository.Commit != sha3 {
		t.Fatalf("facts: %+v", v3.Facts)
	}
	var assets []asset
	if err := json.Unmarshal([]byte(v3.Attributes["assets"]), &assets); err != nil || len(assets) != 1 || assets[0].ID != 31 || assets[0].Name != "tool.tgz" || assets[0].Size != 9 || !strings.HasPrefix(assets[0].Digest, "sha256:") {
		t.Fatalf("%v %v", assets, err)
	}
	if v2.Attributes["commit"] != sha2 || v2.Metadata["prerelease"] != "true" || v2.Metadata["version_rank"] != "2" {
		t.Fatal(v2)
	}
	if strings.Contains(v3.Attributes["assets"]+v2.Attributes["assets"], "secret") || strings.Contains(fmt.Sprint(v3), "secret") {
		t.Fatal("token leaked into a reference")
	}
	if f.authed.Load() == 0 {
		t.Fatal("bearer token was not sent to the API")
	}

	before := f.metadata.Load()
	artifact, err := s.Resolve(context.Background(), v3)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Revision != sha3 || len(artifact.Blobs) != 2 || artifact.Blobs[0].Name != "widget-"+sha3+".zip" || artifact.Blobs[0].Size != -1 || artifact.Blobs[1].Name != "tool.tgz" || artifact.Blobs[1].Size != 9 || artifact.Blobs[1].Digest == "" || artifact.Blobs[1].MediaType != "application/gzip" {
		t.Fatalf("%+v", artifact)
	}
	for i, want := range []string{"zip:" + sha3, assetBytes} {
		body, err := artifact.Blobs[i].Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(data) != want {
			t.Fatalf("blob %d: %q %v", i, data, err)
		}
	}
	if f.metadata.Load() != before {
		t.Fatal("download re-queried metadata")
	}
	if f.downloads.Load() != 2 {
		t.Fatal(f.downloads.Load())
	}
	// Private repositories download through the API with the token; the
	// redirect target still sees no Authorization header.
	s.Repos = []string{"acme/tool"}
	refs = discover(t, s)
	if refs[0].Attributes["download"] != "api" || refs[0].Metadata["private"] != "true" {
		t.Fatal(refs[0])
	}
	authed := f.authed.Load()
	artifact, err = s.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifact.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(data) != "zip:"+shaM || f.authed.Load() != authed+1 {
		t.Fatalf("%q %v authed=%d", data, err, f.authed.Load()-authed)
	}

	// Assets can be left out of the inventory entirely.
	s.Assets = false
	if refs = discover(t, s); refs[0].Attributes["assets"] != "" {
		t.Fatal(refs[0].Attributes)
	}
	if artifact, err = s.Resolve(context.Background(), refs[0]); err != nil || len(artifact.Blobs) != 1 {
		t.Fatal(artifact, err)
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	s := &Source{}
	good := func() registry.Reference {
		return registry.Reference{Registry: "github", Name: "acme/widget", Version: "v3", Attributes: map[string]string{"commit": sha3, "download": "direct", "assets": `[{"id":31,"name":"tool.tgz","size":9}]`}}
	}
	if _, err := s.Resolve(context.Background(), good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*registry.Reference){
		"registry":       func(r *registry.Reference) { r.Registry = "gitlab" },
		"traversal name": func(r *registry.Reference) { r.Name = "acme/.." },
		"nested name":    func(r *registry.Reference) { r.Name = "acme/widget/x" },
		"short commit":   func(r *registry.Reference) { r.Attributes["commit"] = "abc" },
		"ref commit":     func(r *registry.Reference) { r.Attributes["commit"] = "main" },
		"download mode":  func(r *registry.Reference) { r.Attributes["download"] = "https://evil.example" },
		"no mode":        func(r *registry.Reference) { delete(r.Attributes, "download") },
		"bad version":    func(r *registry.Reference) { r.Version = "a..b" },
		"asset path":     func(r *registry.Reference) { r.Attributes["assets"] = `[{"id":31,"name":"../x","size":9}]` },
		"asset id":       func(r *registry.Reference) { r.Attributes["assets"] = `[{"id":0,"name":"x","size":9}]` },
		"asset digest": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":31,"name":"x","size":9,"digest":"sha256:bad"}]`
		},
		"duplicate name": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":31,"name":"x","size":9},{"id":32,"name":"x","size":9}]`
		},
		"source clash": func(r *registry.Reference) {
			r.Attributes["assets"] = `[{"id":31,"name":"widget-` + sha3 + `.zip","size":9}]`
		},
		"assets json": func(r *registry.Reference) { r.Attributes["assets"] = `{` },
	} {
		ref := good()
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestResolveDerivesURLsPerDownloadMode(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization")+" "+r.URL.String())
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	s := &Source{Client: server.Client(), Endpoint: server.URL + "/api/v3", Token: "secret", CodeloadEndpoint: server.URL + "/codeload", ReleaseEndpoint: server.URL + "/gh", Attempts: 1}
	for _, mode := range []string{"api", "direct"} {
		ref := registry.Reference{Registry: "github", Name: "acme/widget", Version: "release/1.0", Attributes: map[string]string{"commit": sha3, "download": mode, "assets": `[{"id":31,"name":"tool v1.tgz","size":2}]`}}
		artifact, err := s.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, blob := range artifact.Blobs {
			body, err := blob.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			body.Close()
		}
	}
	want := []string{
		"Bearer secret /api/v3/repos/acme/widget/zipball/" + sha3,
		"Bearer secret /api/v3/repos/acme/widget/releases/assets/31",
		" /codeload/acme/widget/zip/" + sha3,
		" /gh/acme/widget/releases/download/release/1.0/tool%20v1.tgz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestTagsFollowReleasesAndRefs(t *testing.T) {
	f := newFake(t)
	for _, tc := range []struct {
		depth string
		refs  []string
		want  []string
		kinds []string
	}{
		{depth: "1", want: []string{"v3", "v4"}, kinds: []string{"release", "tag"}},
		{depth: "unlimited", want: []string{"v3", "v2", "v0", "v4"}, kinds: []string{"release", "release", "release", "tag"}},
		{depth: "2026-02-15", want: []string{"v4"}, kinds: []string{"tag"}},
		{depth: "1", refs: []string{"v4"}, want: []string{"v3", "v4", "v2"}, kinds: []string{"release", "snapshot", "tag"}},
	} {
		s := f.source()
		s.Repos, s.Snapshot, s.Tags, s.Refs, s.VersionDepth = []string{"acme/widget"}, SnapshotNever, true, tc.refs, tc.depth
		refs := discover(t, s)
		got, kinds := []string{}, []string{}
		for _, ref := range refs {
			got = append(got, ref.Version)
			kinds = append(kinds, ref.Metadata["kind"])
			if ref.Metadata["kind"] == "tag" && ref.Version == "v4" && ref.Attributes["commit"] != sha4 {
				t.Fatal(ref)
			}
			if ref.Metadata["kind"] == "tag" && (ref.Facts != nil && ref.Facts.ReleasedAt != nil) != (tc.depth == "2026-02-15") {
				t.Fatalf("tag dates are only looked up for a history cutoff: %+v", ref.Facts)
			}
		}
		if !reflect.DeepEqual(got, tc.want) || !reflect.DeepEqual(kinds, tc.kinds) {
			t.Fatalf("%s %v: got %v %v want %v %v", tc.depth, tc.refs, got, kinds, tc.want, tc.kinds)
		}
	}
	// A repository with tags but no releases is not "missing" when tags are requested.
	s := f.source()
	s.Repos, s.Snapshot, s.Tags = []string{"acme/tool"}, SnapshotNever, true
	if got := versions(discover(t, s)); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestSnapshotModesAndRefs(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		repos    string
		snapshot string
		refs     []string
		want     []string
		wantErr  string
	}{
		{repos: "acme/tool", want: []string{"acme/tool@main"}},
		{repos: "acme/tool", snapshot: SnapshotNever, wantErr: "no releases"},
		{repos: "acme/empty", wantErr: "empty repository"},
		{repos: "acme/widget", snapshot: SnapshotNever, want: []string{"acme/widget@v3"}},
		{repos: "acme/widget", snapshot: SnapshotAlways, want: []string{"acme/widget@v3", "acme/widget@main"}},
		{repos: "acme/widget", snapshot: SnapshotOnly, want: []string{"acme/widget@main"}},
		{repos: "acme/widget,acme/tool", snapshot: SnapshotOnly, refs: []string{"v4", "main"}, want: []string{"acme/widget@main", "acme/widget@v4"}, wantErr: "404"},
		{repos: "acme/widget", snapshot: SnapshotOnly, refs: []string{"v4", "main"}, want: []string{"acme/widget@main", "acme/widget@v4"}},
	}
	for _, tc := range cases {
		t.Run(tc.repos+"/"+tc.snapshot+"/"+strings.Join(tc.refs, ","), func(t *testing.T) {
			s := f.source()
			s.Repos = strings.Split(tc.repos, ",")
			s.Snapshot = tc.snapshot
			s.Refs = tc.refs
			s.VersionDepth = "1"
			var refs []registry.Reference
			err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := versions(refs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for _, ref := range refs {
				if ref.Metadata["kind"] == "snapshot" {
					if ref.Attributes["assets"] != "" || ref.Metadata["version_rank"] != "" || ref.Facts == nil || ref.Facts.ReleasedAt == nil || ref.Facts.VersionDownloads != nil {
						t.Fatal(ref)
					}
					a, err := s.Resolve(context.Background(), ref)
					if err != nil || len(a.Blobs) != 1 || a.Revision != ref.Attributes["commit"] {
						t.Fatal(a, err)
					}
				}
			}
		})
	}
}

func TestOwnerEnumeration(t *testing.T) {
	f := newFake(t)
	for _, tc := range []struct {
		owner string
		forks bool
		want  []string
	}{
		{owner: "acme", want: []string{"acme/widget@main", "acme/tool@main"}}, // acme/empty is skipped with a warning
		{owner: "acme", forks: true, want: []string{"acme/widget@main", "acme/widget-fork@main", "acme/tool@main"}},
		{owner: "alice", want: []string{"alice/blog@main"}},
		{owner: "alice", forks: true, want: []string{"alice/notes@main", "alice/blog@main"}},
	} {
		s := f.source()
		s.Owner, s.Forks, s.Snapshot = tc.owner, tc.forks, SnapshotOnly
		if got := versions(discover(t, s)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s forks=%v: got %v want %v", tc.owner, tc.forks, got, tc.want)
		}
	}
	s := f.source()
	s.Owner, s.Snapshot, s.Refs = "alice", SnapshotOnly, []string{"v4"} // missing refs are skipped during enumeration
	if got := versions(discover(t, s)); !reflect.DeepEqual(got, []string{"alice/blog@main"}) {
		t.Fatal(got)
	}
}

func TestSearchPartitionsCappedResultsAndPaginates(t *testing.T) {
	f := newFake(t)
	s := f.source()
	s.Search, s.searchWindow = "topic:dug", 2
	var got []string
	err := s.search(context.Background(), date("2000-01-01T00:00:00Z"), date("2024-01-01T00:00:00Z"), func(r repo) error { got = append(got, r.FullName); return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Whole window (3 > 2) splits at 2012: the later half pages twice, the earlier half once.
	if !reflect.DeepEqual(got, []string{"alice/blog", "alice/notes", "acme/widget"}) || f.searches.Load() != 4 {
		t.Fatalf("got %v searches %d", got, f.searches.Load())
	}
	s.Snapshot = SnapshotOnly
	if got := versions(discover(t, s)); len(got) != 3 {
		t.Fatal(got)
	}
}

func TestRateLimitsAreWaitedOut(t *testing.T) {
	f := newFake(t)
	f.limited.Store(true)
	s := f.source()
	s.Repos, s.Snapshot = []string{"acme/tool"}, SnapshotOnly
	if got := versions(discover(t, s)); !reflect.DeepEqual(got, []string{"acme/tool@main"}) {
		t.Fatal(got)
	}
	now := time.Unix(1_700_000_000, 0)
	header := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	for _, tc := range []struct {
		status  int
		header  http.Header
		delay   time.Duration
		limited bool
	}{
		{403, header("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", "1700000010"), 11 * time.Second, true},
		{403, header("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", "1600000000"), time.Second, true},
		{403, header("Retry-After", "5"), 5 * time.Second, true},
		{429, header(), time.Minute, true},
		{403, header("X-RateLimit-Remaining", "10"), 0, false},
		{404, header("Retry-After", "5"), 0, false},
	} {
		delay, limited := ghsvc.RateLimitDelay(tc.status, tc.header, now)
		if delay != tc.delay || limited != tc.limited {
			t.Fatalf("%d %v: got %v %v", tc.status, tc.header, delay, limited)
		}
	}
	// Real denials are permanent: no retries against a 404.
	s.Repos = []string{"acme/missing"}
	before := f.metadata.Load()
	if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal(err)
	}
	if f.metadata.Load() != before {
		t.Fatal("404 was retried")
	}
}

func TestValidationAndCancellation(t *testing.T) {
	factory := PluginFactory(nil, "", "")
	for _, opts := range []map[string]string{
		{}, {"repos": "acme/widget", "owner": "acme"}, {"repos": "widget"}, {"repos": "acme/.."}, {"repos": "acme/widget,"},
		{"owner": "-acme"}, {"search": " "}, {"search": "x created:>2020"}, {"forks": "maybe", "owner": "acme"}, {"assets": "", "owner": "acme"},
		{"snapshot": "sometimes", "owner": "acme"}, {"refs": "a..b", "owner": "acme"}, {"refs": "v4 beta", "owner": "acme"},
		{"version_depth": "0", "owner": "acme"}, {"unknown": "x", "owner": "acme"}, {"tags": "yes", "owner": "acme"},
	} {
		if _, err := factory(opts); err == nil {
			t.Fatal(opts)
		}
	}
	source, err := factory(map[string]string{"repos": "acme/widget, Acme/Tool", "snapshot": "always", "refs": "release/1.x,v4", "assets": "false", "tags": "true", "version_depth": "2025-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if s := source.(*Source); !reflect.DeepEqual(s.Repos, []string{"acme/widget", "Acme/Tool"}) || s.Assets || !s.Tags || s.Snapshot != SnapshotAlways || len(s.Refs) != 2 {
		t.Fatalf("%+v", s)
	}
	s := &Source{Endpoint: "http://api.example.com", Token: "secret", Repos: []string{"acme/widget"}}
	if err := s.validate(); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("token over plaintext accepted: %v", err)
	}
	s.Endpoint = "http://127.0.0.1:1"
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.gh().Do(context.Background(), "http://127.0.0.1:2/repos/a/b", "", 0); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("off-origin request accepted: %v", err)
	}
	if ns := (&Source{Endpoint: "https://ghe.example.com/api/v3"}).namespace(); ns != "ghe.example.com" {
		t.Fatal(ns)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Discover(ctx, func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestNextLink(t *testing.T) {
	h := http.Header{}
	h.Set("Link", `<https://api.github.com/repositories/1/releases?page=2>; rel="next", <https://api.github.com/repositories/1/releases?page=5>; rel="last"`)
	if got := ghsvc.NextLink(h); got != "https://api.github.com/repositories/1/releases?page=2" {
		t.Fatal(got)
	}
	h.Set("Link", `<https://api.github.com/x?page=1>; rel="prev"`)
	if got := ghsvc.NextLink(h); got != "" {
		t.Fatal(got)
	}
}

// A repository's lookups run concurrently, bounded by listing_concurrency, while
// references still come out in walk order even when later repositories answer first.
func TestListingConcurrencyOverlapsLookupsAndKeepsOrder(t *testing.T) {
	const repos = 12
	var inflight, peak atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users/bulk", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"type":"User"}`) })
	mux.HandleFunc("/users/bulk/repos", func(w http.ResponseWriter, r *http.Request) {
		var list []map[string]any
		for i := 0; i < repos; i++ {
			list = append(list, map[string]any{"id": i + 1, "full_name": fmt.Sprintf("bulk/r%02d", i), "default_branch": "main", "owner": map[string]any{"login": "bulk", "type": "User"}})
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/repos/bulk/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		n, _ := strconv.Atoi(strings.TrimPrefix(parts[2], "r"))
		switch parts[3] {
		case "releases":
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(time.Duration(repos-n) * 5 * time.Millisecond) // later repositories finish first
			inflight.Add(-1)
			io.WriteString(w, `[]`)
		case "branches":
			fmt.Fprintf(w, `{"commit":{"sha":"%040x","commit":{"committer":{"date":"2024-01-01T00:00:00Z"}}}}`, n+1)
		default:
			http.NotFound(w, r)
		}
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	run := func(concurrency int) ([]string, int32) {
		peak.Store(0)
		s := &Source{Client: api.Client(), Endpoint: api.URL, Attempts: 1, Owner: "bulk", Concurrency: concurrency, CodeloadEndpoint: api.URL, ReleaseEndpoint: api.URL}
		return versions(discover(t, s)), peak.Load()
	}
	want := make([]string, repos)
	for i := range want {
		want[i] = fmt.Sprintf("bulk/r%02d@main", i)
	}
	got, peakSerial := run(1)
	if !reflect.DeepEqual(got, want) || peakSerial > 2 { // pool.Stream admits about one lookup beyond its workers
		t.Fatalf("serial: got %v peak %d", got, peakSerial)
	}
	got, peakParallel := run(4)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel order: got %v want %v", got, want)
	}
	if peakParallel <= peakSerial || peakParallel > 6 {
		t.Fatalf("peak in flight %d, want overlap bounded near 4", peakParallel)
	}
}

func TestListingConcurrencyOption(t *testing.T) {
	factory := PluginFactory(http.DefaultClient, "", "")
	for value, ok := range map[string]bool{"1": true, "16": true, "0": false, "17": false, "x": false} {
		_, err := factory(map[string]string{"repos": "acme/widget", "listing_concurrency": value})
		if (err == nil) != ok {
			t.Errorf("listing_concurrency=%s: err %v", value, err)
		}
	}
}
