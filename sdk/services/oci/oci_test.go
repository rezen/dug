package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(data string) string {
	h := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(h[:])
}

// fakeRegistry serves a token realm, manifests, tags and one blob. Every /v2/
// request without the bearer token is challenged.
type fakeRegistry struct {
	*httptest.Server
	tokens    atomic.Int32
	manifests map[string]string // ref -> body
	other     *httptest.Server
}

func newFake(t *testing.T) (*fakeRegistry, Endpoint) {
	f := &fakeRegistry{manifests: map[string]string{}}
	f.other = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "other") }))
	t.Cleanup(f.other.Close)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			f.tokens.Add(1)
			if r.URL.Query().Get("scope") != "repository:acme/app:pull" {
				http.Error(w, "bad scope", 400)
				return
			}
			io.WriteString(w, `{"token":"t0k","expires_in":300}`)
			return
		case !strings.HasPrefix(r.URL.Path, "/v2/"):
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:evil/other:push"`, f.URL))
			w.WriteHeader(401)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/acme/app/manifests/"):
			body, ok := f.manifests[strings.TrimPrefix(r.URL.Path, "/v2/acme/app/manifests/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		case r.URL.Path == "/v2/acme/app/tags/list":
			if r.URL.Query().Get("last") == "" {
				w.Header().Set("Link", `</v2/acme/app/tags/list?n=1000&last=b>; rel="next"`)
				io.WriteString(w, `{"tags":["a","b"]}`)
				return
			}
			io.WriteString(w, `{"tags":["c"]}`)
		case r.URL.Path == "/v2/acme/app/blobs/"+sum("layer"):
			io.WriteString(w, "layer")
		case r.URL.Path == "/v2/acme/app/blobs/"+sum("hop"):
			http.Redirect(w, r, "/v2/acme/app/blobs/"+sum("layer"), http.StatusTemporaryRedirect)
		case r.URL.Path == "/v2/acme/app/blobs/"+sum("away"):
			http.Redirect(w, r, f.other.URL+"/x", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f, Endpoint{Name: "fake", API: f.URL}
}

func TestManifestTokenExchangeAndDigest(t *testing.T) {
	f, ep := newFake(t)
	manifest := `{"schemaVersion":2,"mediaType":"` + MediaOCIManifest + `","config":{"mediaType":"c","digest":"` + sum("cfg") + `","size":3},"layers":[{"mediaType":"l","digest":"` + sum("layer") + `","size":5}]}`
	f.manifests["1.0"] = manifest
	f.manifests[sum(manifest)] = manifest
	f.manifests[sum("other")] = manifest
	c := &Client{HTTP: f.Client(), Attempts: 1}

	doc, err := c.Manifest(context.Background(), ep, "acme/app", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Digest != sum(manifest) || len(doc.Manifest.Layers) != 1 || doc.Manifest.IsIndex() {
		t.Fatalf("doc = %+v", doc)
	}
	// The challenge asked for another repository's push scope; the client
	// requested pull for the repository it needed.
	if _, err := c.Manifest(context.Background(), ep, "acme/app", sum(manifest)); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1 (cached)", got)
	}
	if _, err := c.Manifest(context.Background(), ep, "acme/app", sum("other")); err == nil || !strings.Contains(err.Error(), "hashing to") {
		t.Fatalf("served-content mismatch err = %v", err)
	}
	if _, err := c.Manifest(context.Background(), ep, "acme/app", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	f.manifests["v1"] = `{"schemaVersion":1}`
	if _, err := c.Manifest(context.Background(), ep, "acme/app", "v1"); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("schema1 err = %v", err)
	}
	for _, bad := range []string{"", "a b", "sha256:ABC", "../x"} {
		if _, err := c.Manifest(context.Background(), ep, "acme/app", bad); err == nil {
			t.Fatalf("reference %q accepted", bad)
		}
	}
	if _, err := c.Manifest(context.Background(), ep, "Acme/App", "1.0"); err == nil {
		t.Fatal("uppercase repository accepted")
	}
}

func TestTokenRealmMustStayOnEndpointHosts(t *testing.T) {
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("token request reached the hostile host")
	}))
	defer hostile.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+hostile.URL+`/token"`)
		w.WriteHeader(401)
	}))
	defer registry.Close()
	c := &Client{HTTP: registry.Client(), Attempts: 1, Credentials: map[string]Credential{"fake": {Username: "u", Secret: "s"}}}
	_, err := c.Manifest(context.Background(), Endpoint{Name: "fake", API: registry.URL}, "acme/app", "1.0")
	if err == nil || !strings.Contains(err.Error(), "not an allowed token host") {
		t.Fatalf("err = %v", err)
	}
}

func TestTagsFollowLinkWithinOrigin(t *testing.T) {
	f, ep := newFake(t)
	c := &Client{HTTP: f.Client(), Attempts: 1}
	var tags []string
	if err := c.Tags(context.Background(), ep, "acme/app", func(tag string) error { tags = append(tags, tag); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "a,b,c" {
		t.Fatalf("tags = %v", tags)
	}
	for _, link := range []string{"https://evil.example/v2/acme/app/tags/list", "/elsewhere"} {
		if _, err := ep.sameOrigin(link, "/v2/"); err == nil {
			t.Fatalf("link %q accepted", link)
		}
	}
}

func TestBlobRedirects(t *testing.T) {
	f, ep := newFake(t)
	c := &Client{HTTP: f.Client(), Attempts: 1}
	read := func(content string) (string, error) {
		body, err := c.OpenBlob(context.Background(), ep, "acme/app", sum(content))
		if err != nil {
			return "", err
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		return string(data), err
	}
	if got, err := read("layer"); err != nil || got != "layer" {
		t.Fatalf("layer = %q, %v", got, err)
	}
	if got, err := read("hop"); err != nil || got != "layer" {
		t.Fatalf("same-host redirect = %q, %v", got, err)
	}
	if _, err := read("away"); err == nil || !strings.Contains(err.Error(), "not a public host") {
		t.Fatalf("IP-literal redirect err = %v", err)
	}
	if _, err := c.OpenBlob(context.Background(), ep, "acme/app", "sha256:zz"); err == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestRedirectPolicy(t *testing.T) {
	origin, _ := url.Parse("https://registry-1.docker.io")
	policy := redirectPolicy(origin)
	for target, ok := range map[string]bool{
		"https://production.cloudfront.docker.com/x": true,
		"https://registry-1.docker.io/other":         true,
		"http://production.cloudfront.docker.com/x":  false,
		"https://169.254.169.254/latest/meta-data/":  false,
		"https://[::1]/x":                            false,
		"https://localhost/x":                        false,
		"https://intranet/x":                         false,
		"https://db.internal/x":                      false,
		"https://user:pw@cdn.example.com/x":          false,
	} {
		req, _ := http.NewRequest("GET", target, nil)
		if err := policy(req, []*http.Request{{}}); (err == nil) != ok {
			t.Errorf("redirect to %s: err = %v, want ok=%v", target, err, ok)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull,push"`)
	if got["realm"] != "https://auth.docker.io/token" || got["service"] != "registry.docker.io" || got["scope"] != "repository:library/alpine:pull,push" {
		t.Fatalf("parsed = %v", got)
	}
}

func TestParseReference(t *testing.T) {
	for ref, want := range map[string]string{
		"docker.io/alpine":          "docker.io library/alpine",
		"docker.io/grafana/grafana": "docker.io grafana/grafana",
		"ghcr.io/actions/runner":    "ghcr.io actions/runner",
	} {
		host, repo, err := ParseReference(ref)
		if err != nil || host+" "+repo != want {
			t.Errorf("%s = %q %q %v", ref, host, repo, err)
		}
	}
	for _, ref := range []string{"alpine", "docker.io/", "/x", "ghcr.io/Upper/Case", "ghcr.io/a//b"} {
		if _, _, err := ParseReference(ref); err == nil {
			t.Errorf("%q accepted", ref)
		}
	}
}
