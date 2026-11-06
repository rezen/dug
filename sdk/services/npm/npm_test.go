package npm

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidNameAndVersion(t *testing.T) {
	for _, name := range []string{"left-pad", "@scope/pkg", "n8n-nodes-x.y", "Legacy_Name", "a~b"} {
		if err := ValidName(name); err != nil {
			t.Errorf("rejected name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "..", "../x", "@scope", "@/pkg", "@scope/", "@scope/../x", "a/b", "a b", "a?b", ".hidden", "_private", "@scope/.x", "a\\b", strings.Repeat("a", 215)} {
		if err := ValidName(name); err == nil {
			t.Errorf("accepted name %q", name)
		}
	}
	for _, version := range []string{"1.0.0", "2.0.0-beta.1+build.5", "0.0.0-nightly-20260101"} {
		if err := ValidVersion(version); err != nil {
			t.Errorf("rejected version %q: %v", version, err)
		}
	}
	for _, version := range []string{"", "1/0", "1.0.0 ", "../1", "1?x", "1#x"} {
		if err := ValidVersion(version); err == nil {
			t.Errorf("accepted version %q", version)
		}
	}
}

func TestTarballURL(t *testing.T) {
	for _, tc := range []struct{ name, version, want string }{
		{"left-pad", "1.3.0", "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"},
		{"@scope/pkg", "2.0.0-beta.1", "https://registry.npmjs.org/@scope/pkg/-/pkg-2.0.0-beta.1.tgz"},
	} {
		got, err := TarballURL("", tc.name, tc.version)
		if err != nil || got != tc.want {
			t.Errorf("TarballURL(%q, %q) = %q, %v; want %q", tc.name, tc.version, got, err, tc.want)
		}
	}
	if got := TarballName("@scope/pkg", "1.0.0"); got != "pkg-1.0.0.tgz" {
		t.Errorf("TarballName = %q", got)
	}
	if _, err := TarballURL("", "../x", "1.0.0"); err == nil {
		t.Error("built a URL for an invalid name")
	}
	if _, err := TarballURL("", "pkg", "1/../../x"); err == nil {
		t.Error("built a URL for an invalid version")
	}
}

func sri(data string) string {
	sum := sha512.Sum512([]byte(data))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestIntegrity(t *testing.T) {
	good := sri("tarball bytes")
	parsed, err := ParseIntegrity(good)
	if err != nil || parsed.String() != good {
		t.Fatalf("ParseIntegrity = %v, %v", parsed, err)
	}
	sha1Hex := strings.Repeat("ab", 20)
	legacy, err := FromShasum(sha1Hex)
	if err != nil || legacy.Algorithm != "sha1" {
		t.Fatalf("FromShasum = %v, %v", legacy, err)
	}
	if round, err := ParseIntegrity(legacy.String()); err != nil || round.String() != legacy.String() {
		t.Fatalf("sha1 round trip = %v, %v", round, err)
	}
	for name, value := range map[string]string{
		"empty":           "",
		"unknown algo":    "md5-" + base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"sha256":          "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"short":           "sha512-AAAA",
		"list":            good + " " + good,
		"unpadded":        strings.TrimRight(good, "="),
		"url-safe base64": "sha512-" + base64.URLEncoding.EncodeToString(make([]byte, 64)) + "x",
	} {
		if _, err := ParseIntegrity(value); err == nil {
			t.Errorf("%s: accepted %q", name, value)
		}
	}
	for _, shasum := range []string{"", "xyz", strings.ToUpper(sha1Hex), sha1Hex[:38]} {
		if _, err := FromShasum(shasum); err == nil {
			t.Errorf("accepted shasum %q", shasum)
		}
	}
	if _, err := (Dist{Shasum: sha1Hex}).Hash(); err != nil {
		t.Errorf("shasum fallback: %v", err)
	}
	if _, err := (Dist{}).Hash(); err == nil {
		t.Error("accepted a dist with no hash")
	}
}

func TestVerify(t *testing.T) {
	want, _ := ParseIntegrity(sri("payload"))
	body, err := io.ReadAll(want.Verify(io.NopCloser(strings.NewReader("payload"))))
	if err != nil || string(body) != "payload" {
		t.Fatalf("verified read = %q, %v", body, err)
	}
	if _, err := io.ReadAll(want.Verify(io.NopCloser(strings.NewReader("tampered")))); err == nil {
		t.Fatal("accepted bytes that do not match the integrity")
	}
	if _, err := io.ReadAll(want.Verify(io.NopCloser(strings.NewReader("")))); err == nil {
		t.Fatal("accepted an empty body")
	}
}

const packumentJSON = `{
  "name": "n8n-nodes-demo",
  "dist-tags": {"latest": "1.1.0", "next": "2.0.0-rc.1"},
  "keywords": ["n8n-community-node-package", "demo"],
  "license": {"type": "MIT"},
  "repository": "github:acme/demo",
  "maintainers": [{"name": "acme", "email": "a@example.com"}, "Legacy Person <old@example.com>"],
  "time": {
    "created": "2025-01-01T00:00:00.000Z",
    "modified": "2025-06-01T12:00:00.500Z",
    "1.0.0": "2025-01-01T00:00:01.000Z",
    "1.1.0": "2025-03-01T00:00:00.000Z",
    "2.0.0-rc.1": "2025-06-01T12:00:00.000Z",
    "0.9.0": "not a time"
  },
  "versions": {
    "0.9.0": {"name": "n8n-nodes-demo", "version": "0.9.0", "deprecated": false, "dist": {"shasum": "` + "abababababababababababababababababababab" + `", "tarball": "x"}},
    "1.0.0": {"name": "n8n-nodes-demo", "version": "1.0.0", "scripts": {"postinstall": "node x.js"}, "dist": {"integrity": "%s", "tarball": "x"}},
    "1.1.0": {"name": "n8n-nodes-demo", "version": "1.1.0", "deprecated": "use 2.x", "license": {"type": "MIT"}, "repository": {"url": "git+https://github.com/acme/demo.git", "directory": "pkg"}, "_npmUser": {"name": "acme", "email": "a@example.com"}, "dist": {"integrity": "%s", "tarball": "x", "attestations": {"provenance": {"predicateType": "https://slsa.dev/provenance/v1"}}}},
    "2.0.0-rc.1": {"name": "n8n-nodes-demo", "version": "2.0.0-rc.1", "dist": {"integrity": "%s", "tarball": "x"}}
  }
}`

func newServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, &Client{HTTP: server.Client(), Registry: server.URL, Downloads: server.URL, Attempts: 1}
}

func TestPackument(t *testing.T) {
	doc := fmt.Sprintf(packumentJSON, sri("a"), sri("b"), sri("c"))
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/n8n-nodes-demo":
			io.WriteString(w, doc)
		case "/@acme/demo":
			io.WriteString(w, strings.Replace(doc, `"n8n-nodes-demo"`, `"@acme/demo"`, 1))
		case "/swapped":
			io.WriteString(w, doc)
		case "/unpublished":
			io.WriteString(w, `{"name": "unpublished", "time": {"created": "2025-01-01T00:00:00.000Z", "unpublished": {"time": "2025-05-18T01:39:59.066Z", "versions": ["0.5.1"]}}}`)
		default:
			http.NotFound(w, r)
		}
	})
	p, err := client.Packument(context.Background(), "n8n-nodes-demo")
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := p.Current(); !ok || current != "1.1.0" {
		t.Fatalf("Current = %q, %v", current, ok)
	}
	// Newest first by publish time; the unparseable time sorts last.
	if got, want := p.OrderedVersions(), []string{"2.0.0-rc.1", "1.1.0", "1.0.0", "0.9.0"}; !slices.Equal(got, want) {
		t.Fatalf("OrderedVersions = %v, want %v", got, want)
	}
	if modified, ok := p.Modified(); !ok || !modified.Equal(time.Date(2025, 6, 1, 12, 0, 0, 500_000_000, time.UTC)) {
		t.Fatalf("Modified = %v, %v", modified, ok)
	}
	if _, ok := p.Published("0.9.0"); ok {
		t.Error("parsed an invalid publish time")
	}
	if _, ok := p.Published("modified"); ok {
		t.Error("treated the modified entry as a version")
	}
	if p.License != "MIT" || p.Repository.URL != "github:acme/demo" {
		t.Errorf("tolerant fields: license=%q repository=%+v", p.License, p.Repository)
	}
	if len(p.Maintainers) != 2 || p.Maintainers[1].Name != "Legacy Person" || p.Maintainers[1].Email != "old@example.com" {
		t.Errorf("maintainers = %+v", p.Maintainers)
	}
	v := p.Versions["1.1.0"]
	if v.Deprecated != "use 2.x" || v.NpmUser == nil || v.NpmUser.Name != "acme" || !v.Dist.HasProvenance() || v.Repository.Directory != "pkg" || v.HasInstallScript() {
		t.Errorf("1.1.0 = %+v", v)
	}
	if !p.Versions["1.0.0"].HasInstallScript() || p.Versions["0.9.0"].Deprecated != "" {
		t.Error("install script or deprecated:false not decoded")
	}
	if _, err := client.Packument(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing package error = %v", err)
	}
	if _, err := client.Packument(context.Background(), "unpublished"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpublished package error = %v", err)
	}
	if _, err := client.Packument(context.Background(), "swapped"); err == nil {
		t.Fatal("accepted a document for a different package")
	}
	if _, err := client.Packument(context.Background(), "../x"); err == nil {
		t.Fatal("fetched an invalid name")
	}
	if _, err := client.Packument(context.Background(), "@acme/demo"); err != nil {
		t.Fatalf("scoped package: %v", err)
	}
}

func TestCurrentFallsBackToNewest(t *testing.T) {
	p := &Packument{
		DistTags: map[string]string{"latest": "9.9.9"}, // tag points at a version that does not exist
		Versions: map[string]Version{"1.0.0": {}, "1.1.0": {}},
		Time:     Times{"1.0.0": "2025-01-01T00:00:00Z", "1.1.0": "2025-02-01T00:00:00Z"},
	}
	if current, ok := p.Current(); !ok || current != "1.1.0" {
		t.Fatalf("Current = %q, %v", current, ok)
	}
	if _, ok := (&Packument{}).Current(); ok {
		t.Fatal("empty package has a current version")
	}
}

func TestSearchStopsAtWindow(t *testing.T) {
	var mu sync.Mutex
	var offsets []int
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/v1/search" || r.URL.Query().Get("text") != "keywords:demo" || r.URL.Query().Get("size") != "250" {
			http.Error(w, "bad request "+r.URL.String(), 400)
			return
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		mu.Lock()
		offsets = append(offsets, from)
		mu.Unlock()
		// Like the real endpoint, a window past the cap would repeat the top
		// of the ranking; the client must never ask for it.
		objects := make([]map[string]any, 250)
		for i := range objects {
			objects[i] = map[string]any{
				"package":   map[string]any{"name": fmt.Sprintf("pkg-%d", from+i), "version": "1.0.0", "date": "2025-01-01T00:00:00Z"},
				"downloads": map[string]any{"weekly": 7, "monthly": 30},
				"updated":   "2025-02-01T00:00:00Z",
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"total": 14244, "objects": objects})
	})
	var names []string
	total, err := client.Search(context.Background(), "keywords:demo", func(o SearchObject) error {
		names = append(names, o.Package.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 14244 || len(names) != SearchWindow || names[0] != "pkg-0" || names[len(names)-1] != "pkg-4999" {
		t.Fatalf("total=%d names=%d first=%s last=%s", total, len(names), names[0], names[len(names)-1])
	}
	if len(offsets) != 20 || offsets[len(offsets)-1] != 4750 {
		t.Fatalf("offsets = %v", offsets)
	}
}

func TestSearchSmallCatalogAndEarlyEnd(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		total, count := 3, 0
		if r.URL.Query().Get("text") == "short" {
			total = 600 // promises more than it serves
		}
		if from == 0 {
			count = 3
			if total > 3 {
				count = 250
			}
		}
		objects := make([]map[string]any, count)
		for i := range objects {
			objects[i] = map[string]any{"package": map[string]any{"name": fmt.Sprintf("p%d", i)}}
		}
		json.NewEncoder(w).Encode(map[string]any{"total": total, "objects": objects})
	})
	var n int
	total, err := client.Search(context.Background(), "small", func(SearchObject) error { n++; return nil })
	if err != nil || total != 3 || n != 3 {
		t.Fatalf("small catalog: total=%d n=%d err=%v", total, n, err)
	}
	if _, err := client.Search(context.Background(), "short", func(SearchObject) error { return nil }); err == nil || !strings.Contains(err.Error(), "ended early") {
		t.Fatalf("early end error = %v", err)
	}
	stop := errors.New("stop")
	if _, err := client.Search(context.Background(), "small", func(SearchObject) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("emit error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Search(ctx, "small", func(SearchObject) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search error = %v", err)
	}
}

func TestDownloadCounts(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.EscapedPath())
		mu.Unlock()
		const prefix = "/downloads/point/last-week/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		spec := strings.TrimPrefix(r.URL.Path, prefix)
		if strings.HasPrefix(spec, "@") {
			if strings.Contains(spec, ",") {
				http.Error(w, `{"error":"scoped packages are not currently supported in bulk lookups"}`, 400)
				return
			}
			fmt.Fprintf(w, `{"downloads": 500, "package": %q}`, spec)
			return
		}
		names := strings.Split(spec, ",")
		if len(names) > BulkDownloadNames {
			http.Error(w, "too many", 400)
			return
		}
		if len(names) == 1 { // the endpoint answers a single name with a flat document
			if names[0] == "gone" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"downloads": 11, "package": %q}`, names[0])
			return
		}
		doc := map[string]any{}
		for _, name := range names {
			if name == "gone" {
				doc[name] = nil
				continue
			}
			doc[name] = map[string]any{"downloads": len(name), "package": name}
		}
		json.NewEncoder(w).Encode(doc)
	})
	names := []string{"@acme/scoped", "gone"}
	for i := range 130 {
		names = append(names, fmt.Sprintf("pkg-%03d", i))
	}
	counts, err := client.DownloadCounts(context.Background(), "last-week", names)
	if err != nil {
		t.Fatal(err)
	}
	if counts["@acme/scoped"] != 500 || counts["pkg-007"] != int64(len("pkg-007")) || counts["pkg-129"] != int64(len("pkg-129")) {
		t.Fatalf("counts = scoped %d pkg-007 %d pkg-129 %d", counts["@acme/scoped"], counts["pkg-007"], counts["pkg-129"])
	}
	if _, ok := counts["gone"]; ok {
		t.Error("reported a count for a package the registry does not know")
	}
	// One scoped request, then 128 + 2 unscoped names in two bulk requests.
	if len(requests) != 3 {
		t.Fatalf("requests = %v", requests)
	}
	if requests[0] != "/downloads/point/last-week/@acme%2Fscoped" {
		t.Errorf("scoped request = %q", requests[0])
	}
	single, err := client.DownloadCounts(context.Background(), "last-week", []string{"solo"})
	if err != nil || single["solo"] != 11 {
		t.Fatalf("single = %v, %v", single, err)
	}
	if missing, err := client.DownloadCounts(context.Background(), "last-week", []string{"gone"}); err != nil || len(missing) != 0 {
		t.Fatalf("single missing = %v, %v", missing, err)
	}
	if _, err := client.DownloadCounts(context.Background(), "last-year", names); err == nil {
		t.Error("accepted an unsupported period")
	}
	if _, err := client.DownloadCounts(context.Background(), "last-week", []string{"../x"}); err == nil {
		t.Error("accepted an invalid name")
	}
}
