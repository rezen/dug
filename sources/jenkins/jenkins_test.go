package jenkins

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
)

const hpiBytes = "hpi bytes"

func b64(data string) string {
	sum := sha256.Sum256([]byte(data))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func hexDigest(data string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(data))) }

// fixture serves the two update-center documents, shaped like the real ones
// (base64 digests, ".00Z" timestamps, size only on the current release), plus
// the artifact. alpha has three releases; beta has one and no scm or developers.
func fixture(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var metadata, downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		hpi := func(name, version string) string {
			return fmt.Sprintf("%s/download/plugins/%s/%s/%s.hpi", base, name, version, name)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ".hpi"):
			downloads.Add(1)
			io.WriteString(w, hpiBytes)
		case r.URL.Path == "/current/update-center.actual.json":
			metadata.Add(1)
			writeSigned(t, w, `{"plugins":{
 "beta":{"name":"beta","version":"1.0","url":%q,"sha256":%q,"size":9,"releaseTimestamp":"2026-02-01T00:00:00.00Z","requiredCore":"2.400","popularity":7,"labels":[]},
 "alpha":{"name":"alpha","version":"5.11.0","url":%q,"sha256":%q,"size":9,"releaseTimestamp":"2026-10-11T11:34:36.00Z","requiredCore":"2.504.3",
  "popularity":224942,"health":95,"scm":"https://github.com/jenkinsci/alpha-plugin","labels":["scm","Git"],
  "developers":[{"developerId":"someone","name":"Some One"}]}},
 "warnings":[
  {"id":"SECURITY-2","type":"plugin","name":"alpha","versions":[{"pattern":"5[.]10[.]1|5[.]1"},{"lastVersion":"1","pattern":"4.*"}]},
  {"id":"SECURITY-1","type":"plugin","name":"alpha","versions":[{"pattern":"5[.]10[.]1"}]},
  {"id":"SECURITY-9","type":"core","name":"alpha","versions":[{"pattern":".*"}]},
  {"id":"SECURITY-8","type":"plugin","name":"beta","versions":[{"pattern":"2[.]0"}]},
  {"id":"SECURITY-7","type":"plugin","name":"other","versions":[{"pattern":"(unclosed"}]}],
 "deprecations":{"beta":{"url":"https://example.invalid/beta-deprecated"}}}`,
				hpi("beta", "1.0"), b64(hpiBytes), hpi("alpha", "5.11.0"), b64(hpiBytes))
		case r.URL.Path == "/current/plugin-versions.json":
			metadata.Add(1)
			// The 5.1 key with version 5.01 mirrors lucene-search in the real catalog.
			writeSigned(t, w, `{"plugins":{
 "beta":{"1.0":{"name":"beta","version":"1.0","url":%q,"sha256":%q,"releaseTimestamp":"2026-02-01T00:00:00.00Z","requiredCore":"2.400"}},
 "alpha":{
  "5.10.1":{"name":"alpha","version":"5.10.1","url":%q,"sha256":%q,"releaseTimestamp":"2026-03-25T20:14:00.00Z","requiredCore":"2.492.1"},
  "5.11.0":{"name":"alpha","version":"5.11.0","url":%q,"sha256":%q,"releaseTimestamp":"2026-10-11T11:34:36.00Z","requiredCore":"2.504.3"},
  "5.1":{"name":"alpha","version":"5.01","url":%q,"sha256":%q,"releaseTimestamp":"2011-02-15T02:53:18.00Z","requiredCore":"1.357"}}}}`,
				hpi("beta", "1.0"), b64(hpiBytes),
				hpi("alpha", "5.10.1"), b64(hpiBytes), hpi("alpha", "5.11.0"), b64(hpiBytes), hpi("alpha", "5.01"), b64(hpiBytes))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &metadata, &downloads
}

func source(server *httptest.Server) *Source {
	return &Source{Client: server.Client(), Endpoint: server.URL + "/current/", Attempts: 1,
		DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}, Roots: trusted().pool}
}

func discover(t *testing.T, s *Source) []registry.Reference {
	t.Helper()
	var refs []registry.Reference
	if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{{"unknown": "x"}, {"plugins": "../bad"}, {"plugins": ""}, {"plugins": "a,,b"}, {"version_depth": "0"}, {"version_depth": "soon"}} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{nil, {"plugins": "git, workflow-cps"}, {"version_depth": "unlimited"}, {"version_depth": "2025-01-01"}} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	_, err := factory(map[string]string{"unknown": "x"})
	if err == nil || !strings.Contains(err.Error(), "plugins, version_depth") {
		t.Errorf("unknown-option error does not list supported keys: %v", err)
	}
}

func TestVersionDepthFactsAndMetadata(t *testing.T) {
	server, metadata, _ := fixture(t)
	for depth, want := range map[string][]string{
		"1":          {"5.11.0"},
		"2":          {"5.11.0", "5.10.1"},
		"unlimited":  {"5.11.0", "5.10.1", "5.01"},
		"2026-01-01": {"5.11.0", "5.10.1"},
		"2099-01-01": {},
	} {
		t.Run(depth, func(t *testing.T) {
			before := metadata.Load()
			s := source(server)
			s.Plugins = []string{"alpha"}
			s.VersionDepth = depth
			refs := discover(t, s)
			got := []string{}
			for i, ref := range refs {
				got = append(got, ref.Version)
				if err := ref.Facts.Validate(); err != nil {
					t.Fatal(err)
				}
				f := ref.Facts
				if f.Installs == nil || *f.Installs != 224942 || f.Downloads != nil || f.VersionDownloads != nil ||
					f.UpdatedAt == nil || f.UpdatedAt.Format("2006-01-02") != "2026-10-11" || f.ReleasedAt == nil ||
					f.Repository == nil || f.Repository.URL != "https://github.com/jenkinsci/alpha-plugin" || f.Repository.Source != registry.RepositoryDeclared {
					t.Fatalf("facts: %+v", f)
				}
				if ref.Metadata["version_rank"] != fmt.Sprint(i+1) || ref.Metadata["health_score"] != "95" || ref.Metadata["release_date_source"] != "jenkins-release-timestamp" || ref.Metadata["installs"] != "" || ref.Metadata["deprecated"] != "false" || ref.Metadata["deprecation_url"] != "" {
					t.Fatalf("metadata: %v", ref.Metadata)
				}
				if !reflect.DeepEqual(ref.Tags, []string{"git", "scm"}) || !reflect.DeepEqual(ref.Author, &registry.Author{Name: "Some One", Username: "someone"}) {
					t.Fatalf("tags=%v author=%+v", ref.Tags, ref.Author)
				}
				if ref.Attributes["sha256"] != hexDigest(hpiBytes) {
					t.Fatalf("attributes: %v", ref.Attributes)
				}
				// Whole-string matching: 5.10.1 is hit by two advisories, 5.01 by
				// neither (the "5[.]1" pattern must not match "5.11.0" or "5.01"),
				// and core advisories never apply to a plugin.
				wantWarnings := map[string]string{"5.10.1": "SECURITY-1,SECURITY-2"}[ref.Version]
				if ref.Metadata["security_warnings"] != wantWarnings {
					t.Fatalf("%s security_warnings %q, want %q", ref.Version, ref.Metadata["security_warnings"], wantWarnings)
				}
				// Only the current release carries a size.
				if _, ok := ref.Attributes["size"]; ok != (i == 0) {
					t.Fatalf("version %s size attribute present=%v", ref.Version, ok)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			// version_depth 1 must not download the 24 MiB history document.
			wantRequests := int32(2)
			if depth == "1" {
				wantRequests = 1
			}
			if n := metadata.Load() - before; n != wantRequests {
				t.Fatalf("%d metadata requests, want %d", n, wantRequests)
			}
		})
	}
}

func TestFullWalkIsSortedAndAbsentFactsStayNil(t *testing.T) {
	server, _, _ := fixture(t)
	s := source(server)
	s.VersionDepth = "1"
	refs := discover(t, s)
	if len(refs) != 2 || refs[0].Name != "alpha" || refs[1].Name != "beta" {
		t.Fatalf("got %+v", refs)
	}
	beta := refs[1]
	if beta.Facts.Repository != nil || beta.Author != nil || beta.Tags != nil || beta.Metadata["health_score"] != "" {
		t.Fatalf("beta invented facts: %+v %+v %v", beta.Facts, beta.Author, beta.Metadata)
	}
	if beta.Metadata["deprecated"] != "true" || beta.Metadata["deprecation_url"] != "https://example.invalid/beta-deprecated" || beta.Metadata["security_warnings"] != "" {
		t.Fatalf("beta advisories: %v", beta.Metadata)
	}
	if *beta.Facts.Installs != 7 {
		t.Fatal("beta installs")
	}
}

func TestPinnedDownloadMakesNoMetadataRequest(t *testing.T) {
	server, metadata, downloads := fixture(t)
	s := source(server)
	s.Plugins = []string{"alpha"}
	s.VersionDepth = "unlimited"
	refs := discover(t, s)
	before := metadata.Load()
	for _, ref := range refs {
		artifact, err := s.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		blob := artifact.Blobs[0]
		if artifact.Revision != hexDigest(hpiBytes) || blob.Digest != artifact.Revision || blob.MediaType != "application/java-archive" || blob.Name != "alpha-"+ref.Version+".hpi" {
			t.Fatalf("artifact: %+v", artifact)
		}
		wantSize := int64(-1)
		if ref.Version == "5.11.0" {
			wantSize = 9
		}
		if blob.Size != wantSize {
			t.Fatalf("%s size %d, want %d", ref.Version, blob.Size, wantSize)
		}
		body, err := blob.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(data) != hpiBytes {
			t.Fatalf("%q %v", data, err)
		}
	}
	if metadata.Load() != before || downloads.Load() != 3 {
		t.Fatalf("metadata requests %d, downloads %d", metadata.Load()-before, downloads.Load())
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	s := &Source{}
	good := registry.Reference{Registry: Identity, Name: "git", Version: "5.11.0", Attributes: map[string]string{
		"download_url": "https://updates.jenkins.io/download/plugins/git/5.11.0/git.hpi",
		"sha256":       hexDigest("x"),
		"size":         "625223",
	}}
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	// A release published under a different directory spelling resolves.
	odd := good
	odd.Attributes = maps.Clone(good.Attributes)
	odd.Attributes["download_url"] = "https://updates.jenkins.io/download/plugins/git/5.01/git.hpi"
	if _, err := s.Resolve(context.Background(), odd); err != nil {
		t.Fatal(err)
	}
	// Size is optional for historical releases.
	delete(odd.Attributes, "size")
	if a, err := s.Resolve(context.Background(), odd); err != nil || a.Blobs[0].Size != -1 {
		t.Fatalf("%v %+v", err, a)
	}
	for name, mutate := range map[string]func(*registry.Reference){
		"off-host URL": func(r *registry.Reference) { r.Attributes["download_url"] = "http://169.254.169.254/latest/meta-data/" },
		"other host": func(r *registry.Reference) {
			r.Attributes["download_url"] = "https://get.jenkins.io/plugins/git/5.11.0/git.hpi"
		},
		"other plugin path": func(r *registry.Reference) {
			r.Attributes["download_url"] = "https://updates.jenkins.io/download/plugins/evil/1/evil.hpi"
		},
		"path traversal": func(r *registry.Reference) {
			r.Attributes["download_url"] = "https://updates.jenkins.io/download/plugins/git/../x/git.hpi"
		},
		"not an hpi": func(r *registry.Reference) {
			r.Attributes["download_url"] = "https://updates.jenkins.io/download/plugins/git/5.11.0/git.zip"
		},
		"query string":     func(r *registry.Reference) { r.Attributes["download_url"] += "?x=1" },
		"bad digest":       func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:bad" },
		"base64 digest":    func(r *registry.Reference) { r.Attributes["sha256"] = b64("x") },
		"uppercase digest": func(r *registry.Reference) { r.Attributes["sha256"] = "sha256:" + strings.Repeat("A", 64) },
		"missing digest":   func(r *registry.Reference) { delete(r.Attributes, "sha256") },
		"zero size":        func(r *registry.Reference) { r.Attributes["size"] = "0" },
		"nonnumeric size":  func(r *registry.Reference) { r.Attributes["size"] = "big" },
		"other registry":   func(r *registry.Reference) { r.Registry = "other" },
		"path in name":     func(r *registry.Reference) { r.Name = "../git" },
		"slash in version": func(r *registry.Reference) { r.Version = "1/2" },
		"empty version":    func(r *registry.Reference) { r.Version = "" },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
}

func TestDiscoverFailsLoudlyOnBadCatalogs(t *testing.T) {
	for name, tc := range map[string]struct {
		plugins []string
		body    string
	}{
		"unknown plugin":      {[]string{"nope"}, `{"plugins":{"a":{"name":"a"}}}`},
		"empty catalog":       {nil, `{"plugins":{}}`},
		"not json":            {nil, `<html>`},
		"key and name differ": {nil, `{"plugins":{"a":{"name":"b","version":"1"}}}`},
		"bad digest":          {nil, `{"plugins":{"a":{"name":"a","version":"1","url":"%s/download/plugins/a/1/a.hpi","sha256":"AAAA","releaseTimestamp":"2026-01-01T00:00:00.00Z"}}}`},
		"missing timestamp":   {nil, `{"plugins":{"a":{"name":"a","version":"1","url":"%s/download/plugins/a/1/a.hpi","sha256":"` + b64("x") + `"}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, sign(t, trusted(), strings.ReplaceAll(tc.body, "%s", "http://"+r.Host)))
			}))
			defer server.Close()
			s := source(server)
			s.Plugins = tc.plugins
			if err := s.Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
				t.Fatal("discovery succeeded")
			}
		})
	}
	// An undecodable document is permanent so the host stops retrying.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sign(t, trusted(), `{"plugins":"not a map"}`))
	}))
	defer server.Close()
	err := source(server).Discover(context.Background(), func(registry.Reference) error { return nil })
	if !retry.IsPermanent(err) {
		t.Fatalf("decode failure is retryable: %v", err)
	}
}

func TestCancellationStopsDiscovery(t *testing.T) {
	server, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := source(server).Discover(ctx, func(registry.Reference) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	// Cancelling from the emit callback stops the walk before beta.
	ctx, cancel = context.WithCancel(context.Background())
	var seen []string
	err := source(server).Discover(ctx, func(ref registry.Reference) error {
		seen = append(seen, ref.Name)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(seen, []string{"alpha"}) {
		t.Fatalf("got %v after %v", err, seen)
	}
}

func TestUnparsableAdvisoryPatternFailsOnlyItsPlugin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSigned(t, w, `{"plugins":{"a":{"name":"a","version":"1","url":"http://%s/download/plugins/a/1/a.hpi","sha256":%q,"releaseTimestamp":"2026-01-01T00:00:00.00Z"}},
 "warnings":[{"id":"SECURITY-1","type":"plugin","name":"%s","versions":[{"pattern":"(unclosed"}]}]}`, r.Host, b64("x"), "a")
	}))
	defer server.Close()
	err := source(server).Discover(context.Background(), func(registry.Reference) error { return nil })
	if err == nil || !retry.IsPermanent(err) {
		t.Fatalf("got %v, want a permanent error", err)
	}
}

func TestVersionPatterns(t *testing.T) {
	// Real update center patterns; the lookahead ones are SECURITY-577 and
	// SECURITY-1658. A leading (?!X) rejects versions where X matches at the start.
	for _, tc := range []struct {
		pattern, version string
		want             bool
	}{
		{"1[.](\\d|1[012345678])(|[.-].*)", "1.18", true},
		{"1[.](\\d|1[012345678])(|[.-].*)", "1.19", false},
		{"1[.](\\d|1[012345678])(|[.-].*)", "11.1", false}, // whole string, not a substring
		{"(?!2.0.1.2)(1|2[.]0)(|[.-].*)", "2.0", true},
		{"(?!2.0.1.2)(1|2[.]0)(|[.-].*)", "2.0.1.2", false},
		{"(?!(1[.](57[.][4-9]|66[.][1-9])))(1[.]([0-9]|[1-5][0-9]|6[0-7]))(|[-.].*)", "1.57.3", true},
		{"(?!(1[.](57[.][4-9]|66[.][1-9])))(1[.]([0-9]|[1-5][0-9]|6[0-7]))(|[-.].*)", "1.57.5", false},
		{"(?!(1[.](57[.][4-9]|66[.][1-9])))(1[.]([0-9]|[1-5][0-9]|6[0-7]))(|[-.].*)", "1.68", false},
		{"(?![)]x)a.*", "a", true}, // a ")" inside a class does not close the group
		{"(?![)]x)a.*", ")x", false},
	} {
		p, err := compilePattern(tc.pattern)
		if err != nil {
			t.Fatalf("%q: %v", tc.pattern, err)
		}
		if got := p.matches(tc.version); got != tc.want {
			t.Errorf("%q matches %q = %v, want %v", tc.pattern, tc.version, got, tc.want)
		}
	}
	for _, bad := range []string{"(unclosed", "(?!oops", "a(?=b)", "(?<=a)b"} {
		if _, err := compilePattern(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
