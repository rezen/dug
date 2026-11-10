package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services/oci"
)

func sum(data string) string {
	h := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(h[:])
}

const alpineName = "docker.io/library/alpine"

// fake serves Docker Hub's catalog API and a distribution API on one origin.
type fake struct {
	*httptest.Server
	manifests, blobs map[string]string
	tags             map[string][]string // distribution tag list by repository
	manifestGets     atomic.Int32
	tampered         map[string]string // manifest digest -> body served instead

	latest, v320, v319 string // tag digests
	amd64Layer         string
	sharedLayer        string
}

func newFake(t *testing.T) (*fake, *Source) {
	f := &fake{manifests: map[string]string{}, blobs: map[string]string{}, tags: map[string][]string{}, tampered: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)

	// A multi-arch index with an attestation, and two older single images.
	f.sharedLayer = "shared layer bytes"
	f.amd64Layer = "amd64-only layer bytes that are fairly long"
	amd64, _ := f.image("cfg-amd64", f.sharedLayer, f.amd64Layer)
	arm64, _ := f.image("cfg-arm64", f.sharedLayer, "arm64 layer")
	attest, _ := f.image("cfg-att", "in-toto sbom")
	idx := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[`+
		`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"amd64","os":"linux"}},`+
		`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"arm64","os":"linux","variant":"v8"}},`+
		`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}]}`,
		oci.MediaOCIIndex,
		oci.MediaOCIManifest, amd64, len(f.manifests[amd64]),
		oci.MediaOCIManifest, arm64, len(f.manifests[arm64]),
		oci.MediaOCIManifest, attest, len(f.manifests[attest]))
	f.latest = sum(idx)
	f.manifests[f.latest] = idx
	f.v320, _ = f.image("cfg-320", "layer 3.20")
	f.v319, _ = f.image("cfg-319", "layer 3.19")

	hub := oci.Endpoint{Name: "docker.io", API: f.URL, Catalog: oci.CatalogDockerHub, CatalogAPI: f.URL}
	ghcr := oci.Endpoint{Name: "ghcr.io", API: f.URL}
	return f, &Source{
		OCI:       &oci.Client{HTTP: f.Client(), Attempts: 1},
		Endpoints: map[string]oci.Endpoint{"docker.io": hub, "ghcr.io": ghcr},
	}
}

// image registers an image manifest and its blobs and returns its digest.
func (f *fake) image(config string, layers ...string) (digest, manifest string) {
	desc := func(mediaType, content string) string {
		f.blobs[sum(content)] = content
		return fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":%d}`, mediaType, sum(content), len(content))
	}
	var ls []string
	for _, l := range layers {
		ls = append(ls, desc("application/vnd.oci.image.layer.v1.tar+gzip", l))
	}
	manifest = fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":%s,"layers":[%s]}`,
		oci.MediaOCIManifest, desc("application/vnd.oci.image.config.v1+json", config), strings.Join(ls, ","))
	f.manifests[sum(manifest)] = manifest
	return sum(manifest), manifest
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/token":
		io.WriteString(w, `{"token":"t0k","expires_in":300}`)
		return
	case strings.HasPrefix(path, "/v2/namespaces/"):
		f.catalog(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer t0k" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.URL+`/token",service="fake"`)
		w.WriteHeader(401)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v2/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	kind, ref := parts[len(parts)-2], parts[len(parts)-1]
	repo := strings.Join(parts[:len(parts)-2], "/")
	switch kind {
	case "manifests":
		f.manifestGets.Add(1)
		body, ok := f.manifests[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if alt, ok := f.tampered[ref]; ok {
			body = alt
		}
		io.WriteString(w, body)
	case "blobs":
		body, ok := f.blobs[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	case "tags":
		tags, ok := f.tags[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"name":%q,"tags":["%s"]}`, repo, strings.Join(tags, `","`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fake) catalog(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/namespaces/library/repositories":
		io.WriteString(w, `{"count":1,"next":null,"results":[{"name":"alpine","namespace":"library","star_count":11583,"pull_count":12487502739,"last_updated":"2026-11-08T14:24:47Z"}]}`)
	case "/v2/namespaces/library/repositories/alpine":
		io.WriteString(w, `{"name":"alpine","namespace":"library","star_count":11583,"pull_count":12487502739,"last_updated":"2026-11-08T14:24:47Z"}`)
	case "/v2/namespaces/library/repositories/alpine/tags":
		fmt.Fprintf(w, `{"count":3,"next":null,"results":[`+
			`{"name":"latest","digest":%q,"last_updated":"2026-11-08T14:24:47Z"},`+
			`{"name":"3.20","digest":%q,"last_updated":"2026-04-16T23:53:23Z"},`+
			`{"name":"3.19","digest":%q,"last_updated":"2025-01-02T03:04:05Z"}]}`, f.latest, f.v320, f.v319)
	default:
		http.NotFound(w, r)
	}
}

func discover(t *testing.T, s *Source) ([]registry.Reference, error) {
	t.Helper()
	var refs []registry.Reference
	err := s.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); return nil })
	return refs, err
}

func versions(refs []registry.Reference) string {
	var v []string
	for _, r := range refs {
		v = append(v, r.Version)
	}
	return strings.Join(v, ",")
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil, nil)
	for _, options := range []map[string]string{
		{"unknown": "x"},
		{"repos": ""},
		{"repos": "alpine"},
		{"repos": "quay.io/Bad/Name"},
		{"repos": "example.com/library/alpine"},
		{"namespaces": "ghcr.io/actions"},
		{"namespaces": "quay.io"},
		{"namespaces": "quay.io/a/b"},
		{"registries": "ghcr.io"},
		{"registries": "quay.io/coreos"},
		{"registries": "quay.io", "repos": "docker.io/alpine"},
		{"platform": "amd64"},
		{"platform": "Linux/AMD64"},
		{"max_layer_size": "0"},
		{"max_layer_size": "big"},
		{"max_tags": "0"},
		{"max_tags": "100001"},
		{"version_depth": "0"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{
		nil,
		{"repos": "docker.io/alpine,ghcr.io/actions/runner,quay.io/coreos/etcd"},
		{"namespaces": "docker.io/library,quay.io/coreos"},
		{"registries": "docker.io,quay.io"},
		{"platform": "all", "max_layer_size": "1048576", "max_tags": "50", "version_depth": "2025-01-01"},
		{"platform": "linux/arm64/v8", "version_depth": "unlimited"},
	} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	_, err := factory(map[string]string{"nope": "x"})
	if err == nil || !strings.Contains(err.Error(), "supported: repos, namespaces") {
		t.Fatalf("unknown option error should list supported keys: %v", err)
	}
}

func TestDiscoverDepthFactsAndNoManifestRequests(t *testing.T) {
	f, s := newFake(t)
	s.Namespaces = []string{"docker.io/library"}
	for depth, want := range map[string]string{
		"":           "latest",
		"1":          "latest",
		"2":          "latest,3.20",
		"unlimited":  "latest,3.20,3.19",
		"2025-06-01": "latest,3.20",
		"2099-01-01": "",
	} {
		s.VersionDepth = depth
		refs, err := discover(t, s)
		if err != nil {
			t.Fatalf("depth %q: %v", depth, err)
		}
		if got := versions(refs); got != want {
			t.Errorf("depth %q = %q, want %q", depth, got, want)
		}
	}
	if got := f.manifestGets.Load(); got != 0 {
		t.Fatalf("discovery fetched %d manifests; it must pin the catalog's digest instead", got)
	}

	s.VersionDepth = "unlimited"
	refs, _ := discover(t, s)
	first := refs[0]
	if first.Registry != Identity || first.Namespace != "docker.io" || first.Name != alpineName || first.Version != "latest" {
		t.Fatalf("reference = %+v", first)
	}
	wantAttrs := map[string]string{"manifest_digest": f.latest, "platform": DefaultPlatform}
	if !maps.Equal(first.Attributes, wantAttrs) {
		t.Fatalf("attributes = %v", first.Attributes)
	}
	facts := first.Facts
	if facts.Downloads == nil || *facts.Downloads.Total != 12487502739 || facts.Downloads.Week != nil || facts.Installs != nil || facts.Repository != nil || facts.VersionDownloads != nil {
		t.Fatalf("facts = %+v", facts)
	}
	if !facts.UpdatedAt.Equal(time.Date(2026, 11, 8, 14, 24, 47, 0, time.UTC)) || !facts.ReleasedAt.Equal(*facts.UpdatedAt) {
		t.Fatalf("times = %v %v", facts.UpdatedAt, facts.ReleasedAt)
	}
	wantMeta := map[string]string{"stars": "11583", "version_rank": "1", "release_date_source": "dockerhub-tag-last-updated"}
	if !maps.Equal(first.Metadata, wantMeta) {
		t.Fatalf("metadata = %v", first.Metadata)
	}
	if refs[2].Metadata["version_rank"] != "3" || !refs[2].Facts.ReleasedAt.Equal(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("old version = %+v", refs[2])
	}
	// Facts are cloned per version.
	*refs[0].Facts.Downloads.Total = 1
	if *refs[1].Facts.Downloads.Total != 12487502739 {
		t.Fatal("references share download facts")
	}

	// An explicit repo gets the same facts through the repository lookup, and
	// pinning options travel in the attributes.
	s.Namespaces, s.Repos, s.VersionDepth, s.Platform, s.MaxLayerSize = nil, []string{"docker.io/alpine"}, "1", "all", 10
	refs, err := discover(t, s)
	if err != nil || len(refs) != 1 || *refs[0].Facts.Downloads.Total != 12487502739 {
		t.Fatalf("explicit repo = %+v, %v", refs, err)
	}
	if refs[0].Attributes["platform"] != "all" || refs[0].Attributes["max_layer_size"] != "10" {
		t.Fatalf("attributes = %v", refs[0].Attributes)
	}
}

func TestSameRepositoryOnTwoRegistriesGetsDistinctNames(t *testing.T) {
	f, s := newFake(t)
	f.tags["library/alpine"] = []string{"latest"}
	f.manifests["latest"] = f.manifests[f.v320]
	s.Repos = []string{"docker.io/alpine", "ghcr.io/library/alpine"}
	refs, err := discover(t, s)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs = %v, %v", refs, err)
	}
	if refs[0].Name != "docker.io/library/alpine" || refs[1].Name != "ghcr.io/library/alpine" || refs[0].Name == refs[1].Name {
		t.Fatalf("names = %q, %q: the storage layout ignores Namespace, so Name must differ", refs[0].Name, refs[1].Name)
	}
}

func TestDiscoverMissingExplicitRepoIsAnError(t *testing.T) {
	_, s := newFake(t)
	s.Repos = []string{"docker.io/library/ghost"}
	if _, err := discover(t, s); err == nil {
		t.Fatal("a missing explicit repository must fail")
	}
}

func TestDiscoverDefaultWalkNeedsCatalogRegistries(t *testing.T) {
	_, s := newFake(t)
	s.Registries = []string{"ghcr.io"}
	if _, err := discover(t, s); err == nil || !strings.Contains(err.Error(), "no catalog") {
		t.Fatalf("err = %v", err)
	}
	s.Registries = []string{"docker.io"}
	refs, err := discover(t, s)
	if err != nil || versions(refs) != "latest" {
		t.Fatalf("hub default walk = %v, %v", versions(refs), err)
	}
}

func TestDiscoverCancellation(t *testing.T) {
	_, s := newFake(t)
	s.Namespaces = []string{"docker.io/library"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Discover(ctx, func(registry.Reference) error { return nil }); err == nil || ctx.Err() == nil {
		t.Fatalf("err = %v", err)
	}
}

func ref(f *fake, digest, platform string, extra map[string]string) registry.Reference {
	attrs := map[string]string{"manifest_digest": digest, "platform": platform}
	maps.Copy(attrs, extra)
	return registry.Reference{Registry: Identity, Namespace: "docker.io", Name: alpineName, Version: "latest", Attributes: attrs}
}

func names(a registry.Artifact) []string {
	var n []string
	for _, b := range a.Blobs {
		n = append(n, b.Name)
	}
	return n
}

func read(t *testing.T, b registry.Blob) string {
	t.Helper()
	body, err := b.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResolveSelectsPlatformAndPinsBlobs(t *testing.T) {
	f, s := newFake(t)
	art, err := s.Resolve(context.Background(), ref(f, f.latest, "linux/amd64", nil))
	if err != nil {
		t.Fatal(err)
	}
	if art.Revision != f.latest {
		t.Fatalf("revision = %s, want the pinned index digest", art.Revision)
	}
	// index, amd64 manifest, config, shared layer, amd64 layer.
	if len(art.Blobs) != 5 {
		t.Fatalf("blobs = %v", names(art))
	}
	seen := map[string]bool{}
	for _, b := range art.Blobs {
		if seen[b.Name] || strings.ContainsAny(b.Name, `/\`) {
			t.Fatalf("bad blob name %q", b.Name)
		}
		seen[b.Name] = true
		content := read(t, b)
		if sum(content) != b.Digest || int64(len(content)) != b.Size {
			t.Errorf("%s: digest/size %s/%d do not match content", b.Name, b.Digest, b.Size)
		}
		// The index itself legitimately names every platform.
		if !strings.HasPrefix(b.Name, "index-") && (strings.Contains(content, "arm64") || strings.Contains(content, "sbom")) {
			t.Errorf("%s belongs to another platform", b.Name)
		}
	}
	for _, want := range []string{"index-", "manifest-", "config-", "layer-"} {
		if !slices.ContainsFunc(names(art), func(n string) bool { return strings.HasPrefix(n, want) }) {
			t.Errorf("no %s blob in %v", want, names(art))
		}
	}
	if !slices.ContainsFunc(art.Blobs, func(b registry.Blob) bool {
		return strings.HasSuffix(b.Name, ".tar.gz") && b.MediaType == "application/vnd.oci.image.layer.v1.tar+gzip"
	}) {
		t.Fatal("layers should be named .tar.gz")
	}

	// A variant-less selection matches arm64/v8; a variant pins exactly.
	if art, err = s.Resolve(context.Background(), ref(f, f.latest, "linux/arm64", nil)); err != nil || len(art.Blobs) != 5 {
		t.Fatalf("arm64 = %v, %v", names(art), err)
	}
	if _, err = s.Resolve(context.Background(), ref(f, f.latest, "linux/arm64/v7", nil)); err == nil || !strings.Contains(err.Error(), "linux/arm64/v8") {
		t.Fatalf("variant mismatch should list what is available: %v", err)
	}
	if _, err = s.Resolve(context.Background(), ref(f, f.latest, "linux/s390x", nil)); err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("missing platform err = %v", err)
	}
}

func TestResolveAllPlatformsIncludesAttestationsAndDedupesSharedLayers(t *testing.T) {
	f, s := newFake(t)
	art, err := s.Resolve(context.Background(), ref(f, f.latest, "all", nil))
	if err != nil {
		t.Fatal(err)
	}
	// index + 3 manifests + 3 configs + shared layer + amd64 + arm64 + sbom layers.
	if len(art.Blobs) != 11 {
		t.Fatalf("blobs = %d %v", len(art.Blobs), names(art))
	}
	found := false
	for _, b := range art.Blobs {
		found = found || strings.Contains(read(t, b), "in-toto sbom")
	}
	if !found {
		t.Fatal("attestation layer missing from platform=all")
	}
}

func TestResolvePlatformlessIndexArchivesEveryEntry(t *testing.T) {
	f, s := newFake(t)
	a, _ := f.image("cfg-sig-a", "signature a")
	b, _ := f.image("cfg-sig-b", "signature b")
	idx := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d},{"mediaType":%q,"digest":%q,"size":%d}]}`,
		oci.MediaOCIIndex, oci.MediaOCIManifest, a, len(f.manifests[a]), oci.MediaOCIManifest, b, len(f.manifests[b]))
	f.manifests[sum(idx)] = idx
	art, err := s.Resolve(context.Background(), ref(f, sum(idx), "linux/amd64", nil))
	if err != nil {
		t.Fatal(err)
	}
	// index + 2 manifests + 2 configs + 2 layers.
	if len(art.Blobs) != 7 {
		t.Fatalf("blobs = %v", names(art))
	}
}

func TestResolveSingleManifestIgnoresPlatform(t *testing.T) {
	f, s := newFake(t)
	art, err := s.Resolve(context.Background(), ref(f, f.v320, "linux/riscv64", nil))
	if err != nil || len(art.Blobs) != 3 || art.Revision != f.v320 {
		t.Fatalf("single = %v, %v", names(art), err)
	}
}

func TestResolveMaxLayerSizeOmitsLargeLayers(t *testing.T) {
	f, s := newFake(t)
	limit := fmt.Sprint(len(f.amd64Layer) - 1)
	art, err := s.Resolve(context.Background(), ref(f, f.latest, "linux/amd64", map[string]string{"max_layer_size": limit}))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range art.Blobs {
		if read(t, b) == f.amd64Layer {
			t.Fatal("oversized layer was kept")
		}
	}
	if len(art.Blobs) != 4 {
		t.Fatalf("blobs = %v", names(art))
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	f, s := newFake(t)
	good := ref(f, f.latest, "linux/amd64", nil)
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(*registry.Reference)) registry.Reference {
		r := good
		r.Attributes = maps.Clone(good.Attributes)
		fn(&r)
		return r
	}
	for name, bad := range map[string]registry.Reference{
		"registry":          mutate(func(r *registry.Reference) { r.Registry = "npm" }),
		"unknown namespace": mutate(func(r *registry.Reference) { r.Namespace = "169.254.169.254" }),
		"url-ish namespace": mutate(func(r *registry.Reference) { r.Namespace = "http://169.254.169.254/" }),
		"repo traversal":    mutate(func(r *registry.Reference) { r.Name = "docker.io/library/../../etc" }),
		"repo uppercase":    mutate(func(r *registry.Reference) { r.Name = "docker.io/Library/Alpine" }),
		"name lacks host":   mutate(func(r *registry.Reference) { r.Name = "library/alpine" }),
		"name other host":   mutate(func(r *registry.Reference) { r.Name = "quay.io/library/alpine" }),
		"empty version":     mutate(func(r *registry.Reference) { r.Version = "" }),
		"bad tag":           mutate(func(r *registry.Reference) { r.Version = "a b" }),
		"short digest":      mutate(func(r *registry.Reference) { r.Attributes["manifest_digest"] = "sha256:abc" }),
		"upper digest":      mutate(func(r *registry.Reference) { r.Attributes["manifest_digest"] = strings.ToUpper(f.latest) }),
		"missing digest":    mutate(func(r *registry.Reference) { delete(r.Attributes, "manifest_digest") }),
		"bad platform":      mutate(func(r *registry.Reference) { r.Attributes["platform"] = "../x" }),
		"zero layer cap":    mutate(func(r *registry.Reference) { r.Attributes["max_layer_size"] = "0" }),
		"padded layer cap":  mutate(func(r *registry.Reference) { r.Attributes["max_layer_size"] = "010" }),
	} {
		if _, err := s.Resolve(context.Background(), bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolveRejectsManifestBytesThatDoNotMatchTheDigest(t *testing.T) {
	f, s := newFake(t)
	f.tampered[f.v320] = f.manifests[f.v320] + " "
	if _, err := s.Resolve(context.Background(), ref(f, f.v320, "linux/amd64", nil)); err == nil || !strings.Contains(err.Error(), "hashing to") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveFailsWhenPinnedDigestIsGone(t *testing.T) {
	f, s := newFake(t)
	delete(f.manifests, f.v319)
	_, err := s.Resolve(context.Background(), ref(f, f.v319, "linux/amd64", nil))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a deleted version must fail, not be replaced: %v", err)
	}
}

func TestOpenSurvivesCancelledResolutionContextAndRestartsAtZero(t *testing.T) {
	f, s := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	art, err := s.Resolve(ctx, ref(f, f.v320, "linux/amd64", nil))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range art.Blobs {
		if a, c := read(t, b), read(t, b); a != c || sum(a) != b.Digest {
			t.Fatalf("%s: reads differ or mismatch digest", b.Name)
		}
	}
}

func TestGenericRegistryOrdersByCreationAndRefusesTruncation(t *testing.T) {
	f, s := newFake(t)
	cfg := func(created string) string { return fmt.Sprintf(`{"created":%q}`, created) }
	oldDigest, _ := f.image(cfg("2024-01-01T00:00:00Z"), "old")
	newDigest, _ := f.image(cfg("2026-06-01T00:00:00Z"), "new")
	noDate, _ := f.image(`{}`, "undated")
	f.manifests["a-old"], f.manifests["b-new"], f.manifests["c-nodate"] = f.manifests[oldDigest], f.manifests[newDigest], f.manifests[noDate]
	f.tags["acme/app"] = []string{"a-old", "b-new", "c-nodate"}
	s.Repos = []string{"ghcr.io/acme/app"}

	s.VersionDepth = "unlimited"
	refs, err := discover(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(refs); got != "b-new,a-old,c-nodate" {
		t.Fatalf("order = %s (lexical tag order must not leak through)", got)
	}
	if refs[0].Metadata["release_date_source"] != "image-created" || refs[2].Facts.ReleasedAt != nil || refs[2].Metadata["release_date_source"] != "" {
		t.Fatalf("metadata = %v / %v", refs[0].Metadata, refs[2].Metadata)
	}
	if refs[0].Attributes["manifest_digest"] != newDigest || refs[0].Facts.Downloads != nil {
		t.Fatalf("ref = %+v", refs[0])
	}

	s.VersionDepth = "1"
	if refs, err = discover(t, s); err != nil || versions(refs) != "b-new" {
		t.Fatalf("depth 1 = %v, %v", versions(refs), err)
	}
	s.VersionDepth = "2025-01-01"
	if _, err = discover(t, s); err == nil || !strings.Contains(err.Error(), "missing release date") {
		t.Fatalf("a date cutoff with an undated tag must fail: %v", err)
	}
	s.VersionDepth, s.MaxTags = "unlimited", 2
	if _, err = discover(t, s); err == nil || !strings.Contains(err.Error(), "max_tags") {
		t.Fatalf("a repository over max_tags must fail, not truncate: %v", err)
	}
}

func TestSelects(t *testing.T) {
	arm := oci.Descriptor{Platform: &oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}
	attest := oci.Descriptor{Platform: &oci.Platform{OS: "unknown", Architecture: "unknown"}, Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"}}
	for _, c := range []struct {
		platform string
		entry    oci.Descriptor
		want     bool
	}{
		{"all", attest, true},
		{"linux/arm64", arm, true},
		{"linux/arm64/v8", arm, true},
		{"linux/arm64/v7", arm, false},
		{"linux/amd64", arm, false},
		{"linux/amd64", attest, false},
		{"linux/amd64", oci.Descriptor{}, false},
	} {
		if got := selects(c.platform, c.entry); got != c.want {
			t.Errorf("selects(%s, %+v) = %v", c.platform, c.entry.Platform, got)
		}
	}
}
