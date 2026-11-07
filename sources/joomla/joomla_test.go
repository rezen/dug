package joomla

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func sum512(data []byte) string {
	h := sha512.Sum512(data)
	return hex.EncodeToString(h[:])
}

func sum256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

type fakeExt struct {
	slug, install, license, author, updated string
	reviews, score                          int
	types                                   []string
}

// fake mimics the Install from Web catalog: JSON envelopes around HTML
// fragments, category pages that repeat every item and restart from the first
// page when the offset passes the end, and developer-hosted manifests.
type fake struct {
	categories map[string][]int
	extensions map[int]fakeExt
	files      map[string]string // path -> body for manifests and packages
	views      atomic.Int32
	downloads  atomic.Int32
	server     *httptest.Server
}

func newFake(t *testing.T) *fake {
	f := &fake{categories: map[string][]int{}, extensions: map[int]fakeExt{}, files: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) source() *Source {
	return &Source{
		Client:        f.server.Client(),
		Endpoint:      f.server.URL + "/webapps/index.php",
		Attempts:      1,
		DownloadHosts: []string{strings.TrimPrefix(f.server.URL, "http://")},
	}
}

func envelope(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": nil, "messages": nil, "data": map[string]any{"html": html}})
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/webapps/index.php" {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			f.downloads.Add(1)
		}
		body, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
		return
	}
	q := r.URL.Query()
	switch q.Get("view") {
	case "dashboard":
		ids := slices.Sorted(maps.Keys(f.categories))
		var html strings.Builder
		html.WriteString(`<ul class="nav nav-list"><li class="active"><a class="transcode selected" href="index.php?option=com_apps&format=json&view=dashboard">Home</a></li>`)
		for _, id := range ids {
			fmt.Fprintf(&html, `<li><a class="transcode" href="index.php?option=com_apps&format=json&view=category&id=%s">Category %s</a></li>`, id, id)
		}
		envelope(w, html.String())
	case "category":
		items := f.categories[q.Get("id")]
		limit, _ := strconv.Atoi(q.Get("limit"))
		start, _ := strconv.Atoi(q.Get("limitstart"))
		if q.Get("ordering") != "core_created_time" || limit != pageSize {
			http.Error(w, "unexpected paging", http.StatusBadRequest)
			return
		}
		if start >= len(items) { // Joomla restarts from the first page
			start = 0
		}
		page := items[start:min(start+limit, len(items))]
		var html strings.Builder
		html.WriteString(`<ul class="thumbnails">`)
		for pass := 0; pass < 2; pass++ { // the real markup lists each item twice
			for _, id := range page {
				e := f.extensions[id]
				fmt.Fprintf(&html, `<li class="item span3"><div class="thumbnail"><p class="rating center"><a target="_blank" href="https://extensions.joomla.org/extension/%s/#reviews">%d reviews with a score of %d/100</a></p><div onclick="Joomla.loadweb(apps_base_url+'index.php?option=com_apps&format=json&view=extension&id=%d');"><h4 class="center muted"><a class="transcode ajaxloaded" href="index.php?option=com_apps&format=json&view=extension&id=%d">%s</a></h4></div></div></li>`, e.slug, e.reviews, e.score, id, id, e.slug)
			}
		}
		html.WriteString(`</ul>`)
		envelope(w, html.String())
	case "extension":
		f.views.Add(1)
		id, _ := strconv.Atoi(q.Get("id"))
		e, ok := f.extensions[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":true,"code":404,"message":"not found"}`)
			return
		}
		var badges strings.Builder
		for _, kind := range e.types {
			fmt.Fprintf(&badges, `<span title="%s" class="badge jbadge badge-j%s">%s</span>&nbsp;`, kind, strings.ToLower(kind), kind)
		}
		install := ""
		if e.install != "" {
			install = fmt.Sprintf(`<a class="install btn btn-success" href="#" onclick="return Joomla.installfromweb('%s', '%s')"><span class="icon-checkmark" aria-hidden="true"></span> Install</a>`, strings.ReplaceAll(e.install, "{base}", f.server.URL), e.slug)
		}
		envelope(w, fmt.Sprintf(`<div class="com-apps-container"><div class="rating"><a target="_blank" href="https://extensions.joomla.org/extension/%s/#reviews">%d reviews with a score of %d/100</a></div><dl class="dl-horizontal"><dt>Version</dt><dd>1.0 (last updated on %s)</dd><dt>License</dt><dd>%s</dd><dt>Download Type</dt><dd>Free download</dd></dl><div class="item-badge-container">%s</div><div class="item-buttons form-actions">%s</div><div class="item-desc"><p class="item-desc-title"><strong>%s</strong> (By %s)</p></div></div>`, e.slug, e.reviews, e.score, e.updated, e.license, badges.String(), install, e.slug, e.author))
	default:
		http.Error(w, "unknown view", http.StatusBadRequest)
	}
}

// manifest renders a Joomla update server document from release entries.
func manifest(releases ...string) string {
	return `<?xml version="1.0" encoding="utf-8"?><updates>` + strings.Join(releases, "") + `</updates>`
}

func update(version, downloadURL string, extra ...string) string {
	return `<update><name>Pkg</name><element>pkg_demo</element><type>package</type><version>` + version + `</version><downloads><downloadurl type="full" format="zip">` + downloadURL + `</downloadurl></downloads><tags><tag>stable</tag></tags><targetplatform name="joomla" version="5.*"/>` + strings.Join(extra, "") + `</update>`
}

func digestTags(body string, size bool) []string {
	tags := []string{"<sha512>" + sum512([]byte(body)) + "</sha512>"}
	if size {
		tags = append(tags, "<length>"+strconv.Itoa(len(body))+"</length>")
	}
	return tags
}

// catalog builds the standard fixture: one in-scope extension with a messy
// manifest, and one of each kind that must be left out.
func catalog(t *testing.T) (*fake, map[string]string) {
	f := newFake(t)
	bodies := map[string]string{"1.0.0": "zip one", "1.1.0": "zip two", "2.0.0": "zip three"}
	for version, body := range bodies {
		f.files["/dl/demo-"+version+".zip"] = body
	}
	f.files["/dl/demo-3.0.0-beta1.zip"] = "beta"
	f.files["/updates/demo.xml"] = manifest(
		update("1.0.0", "{base}/dl/demo-1.0.0.zip", digestTags(bodies["1.0.0"], false)...),
		update("2.0.0", "{base}/dl/demo-2.0.0.zip", digestTags(bodies["2.0.0"], true)...),
		// the same version repeated for another target platform
		update("2.0.0", "{base}/dl/demo-2.0.0.zip", digestTags(bodies["2.0.0"], true)...),
		update("1.1.0", "{base}/dl/demo-1.1.0.zip", append(digestTags(bodies["1.1.0"], true), "<sha256>"+sum256([]byte(bodies["1.1.0"]))+"</sha256>")...),
		// a pre-release, a release without a digest, and an ambiguous version
		strings.Replace(update("3.0.0-beta1", "{base}/dl/demo-3.0.0-beta1.zip", digestTags("beta", false)...), "<tag>stable</tag>", "<tag>beta</tag>", 1),
		update("0.9.0", "{base}/dl/demo-0.9.0.zip"),
		update("0.8.0", "{base}/dl/demo-0.8.0.zip", digestTags("a", false)...),
		update("0.8.0", "{base}/dl/demo-0.8.0-other.zip", digestTags("b", false)...),
	)
	for key, value := range f.files {
		f.files[key] = strings.ReplaceAll(value, "{base}", f.server.URL)
	}
	f.extensions[80] = fakeExt{slug: "demo-backup", install: "{base}/updates/demo.xml", license: "GPLv2 or later", author: "Demo Ltd", updated: "04 September 2026", reviews: 12, score: 98, types: []string{"Component", "Plugin"}}
	f.extensions[81] = fakeExt{slug: "direct-zip", install: "{base}/dl/direct.zip", reviews: 1, score: 100}
	f.extensions[82] = fakeExt{slug: "no-install", reviews: 3, score: 80}
	f.extensions[83] = fakeExt{slug: "dead-manifest", install: "{base}/updates/missing.xml", reviews: 2, score: 60}
	f.categories["9"] = []int{80, 81, 82}
	f.categories["10"] = []int{82, 83, 80} // extensions appear in several categories
	return f, bodies
}

func discover(t *testing.T, s *Source) ([]registry.Reference, error) {
	t.Helper()
	var refs []registry.Reference
	err := s.Discover(context.Background(), func(r registry.Reference) error {
		refs = append(refs, r)
		return nil
	})
	return refs, err
}

func versions(refs []registry.Reference) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name+"@"+r.Version)
	}
	return out
}

func TestOptionValidation(t *testing.T) {
	factory := PluginFactory(nil)
	for _, options := range []map[string]string{
		{"unknown": "x"},
		{"slugs": "../bad"},
		{"slugs": "Upper"},
		{"slugs": "a,,b"},
		{"ids": "abc"},
		{"ids": "1,"},
		{"categories": "-1"},
		{"ids": "80", "slugs": "demo"},
		{"ids": "80", "categories": "9"},
		{"version_depth": "0"},
		{"version_depth": "2025-01-01"}, // manifests carry no release dates
		{"listing_concurrency": "0"},
		{"listing_concurrency": "17"},
		{"listing_concurrency": "x"},
	} {
		if _, err := factory(options); err == nil {
			t.Errorf("accepted %v", options)
		}
	}
	for _, options := range []map[string]string{
		nil,
		{"slugs": "akeeba-backup, admin-tools"},
		{"slugs": "demo", "categories": "9,10"},
		{"ids": "80,1737"},
		{"version_depth": "unlimited"},
		{"version_depth": "3", "listing_concurrency": "16"},
	} {
		if _, err := factory(options); err != nil {
			t.Errorf("rejected %v: %v", options, err)
		}
	}
	_, err := factory(map[string]string{"nope": "1"})
	if err == nil || !strings.Contains(err.Error(), "slugs, ids, categories") {
		t.Errorf("unknown option error does not list the supported keys: %v", err)
	}
}

func TestDiscoverWalksCatalog(t *testing.T) {
	for name, tc := range map[string]struct {
		depth string
		want  []string
	}{
		"current":   {"", []string{"demo-backup@2.0.0"}},
		"two":       {"2", []string{"demo-backup@2.0.0", "demo-backup@1.1.0"}},
		"unlimited": {"unlimited", []string{"demo-backup@2.0.0", "demo-backup@1.1.0", "demo-backup@1.0.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, bodies := catalog(t)
			s := f.source()
			s.VersionDepth = tc.depth
			refs, err := discover(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if got := versions(refs); !slices.Equal(got, tc.want) {
				t.Fatalf("versions = %v, want %v", got, tc.want)
			}
			if got := f.views.Load(); got != 4 {
				t.Errorf("extension views = %d, want one per distinct extension (4)", got)
			}
			first := refs[0]
			if first.Registry != Identity || first.Namespace != Namespace {
				t.Errorf("identity %q namespace %q", first.Registry, first.Namespace)
			}
			wantAttrs := map[string]string{
				"extension_id": "80",
				"manifest_url": f.server.URL + "/updates/demo.xml",
				"download_url": f.server.URL + "/dl/demo-2.0.0.zip",
				"sha512":       sum512([]byte(bodies["2.0.0"])),
				"size":         strconv.Itoa(len(bodies["2.0.0"])),
			}
			if !maps.Equal(first.Attributes, wantAttrs) {
				t.Errorf("attributes = %v, want %v", first.Attributes, wantAttrs)
			}
			wantMeta := map[string]string{"rating": "98", "num_ratings": "12", "license": "GPLv2 or later", "version_rank": "1"}
			if !maps.Equal(first.Metadata, wantMeta) {
				t.Errorf("metadata = %v, want %v", first.Metadata, wantMeta)
			}
			if first.Facts == nil || first.Facts.UpdatedAt == nil || !first.Facts.UpdatedAt.Equal(time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("updated_at = %+v", first.Facts)
			}
			if first.Facts.ReleasedAt != nil || first.Facts.Downloads != nil || first.Facts.Installs != nil || first.Facts.Repository != nil {
				t.Errorf("facts the registry does not publish must stay nil: %+v", first.Facts)
			}
			if !slices.Equal(first.Tags, []string{"component", "plugin"}) {
				t.Errorf("tags = %v", first.Tags)
			}
			if first.Author == nil || first.Author.Name != "Demo Ltd" {
				t.Errorf("author = %+v", first.Author)
			}
			if len(refs) > 1 {
				if refs[1].Metadata["version_rank"] != "2" {
					t.Errorf("second version rank = %q", refs[1].Metadata["version_rank"])
				}
				if refs[1].Attributes["sha256"] != sum256([]byte(bodies["1.1.0"])) {
					t.Errorf("published sha256 not pinned: %v", refs[1].Attributes)
				}
				if refs[len(refs)-1].Attributes["size"] != "" && refs[len(refs)-1].Version == "1.0.0" {
					t.Errorf("size invented for a release without length")
				}
			}
			// Facts are cloned per version.
			if len(refs) > 1 && refs[0].Facts == refs[1].Facts {
				t.Error("references share one Facts value")
			}
		})
	}
}

func TestDateDepthIsRejected(t *testing.T) {
	f, _ := catalog(t)
	s := f.source()
	s.VersionDepth = "2025-01-01"
	if _, err := discover(t, s); err == nil || !strings.Contains(err.Error(), "no release dates") {
		t.Fatalf("date cutoff error = %v", err)
	}
}

func TestExplicitSelection(t *testing.T) {
	f, _ := catalog(t)
	s := f.source()
	s.IDs = []string{"80"}
	refs, err := discover(t, s)
	if err != nil || len(refs) != 1 || refs[0].Name != "demo-backup" {
		t.Fatalf("ids: %v, %v", versions(refs), err)
	}

	s = f.source()
	s.Slugs = []string{"demo-backup"}
	refs, err = discover(t, s)
	if err != nil || len(refs) != 1 {
		t.Fatalf("slugs: %v, %v", versions(refs), err)
	}

	// An explicitly requested item that cannot be archived is an error, not a
	// silent skip.
	for name, tc := range map[string]struct {
		ids, slugs []string
		want       string
	}{
		"direct zip":    {ids: []string{"81"}, want: "not an update manifest"},
		"no install":    {ids: []string{"82"}, want: "no install URL"},
		"dead manifest": {slugs: []string{"dead-manifest"}, want: "manifest unavailable"},
		"unknown id":    {ids: []string{"999"}, want: "999"},
		"unknown slug":  {slugs: []string{"nothing-here"}, want: "not in the Install from Web catalog"},
	} {
		s := f.source()
		s.IDs, s.Slugs = tc.ids, tc.slugs
		if _, err := discover(t, s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
	}
}

func TestCategoryOption(t *testing.T) {
	f, _ := catalog(t)
	s := f.source()
	s.Categories = []string{"10"}
	refs, err := discover(t, s)
	if err != nil || len(refs) != 1 {
		t.Fatalf("categories: %v, %v", versions(refs), err)
	}
	// 82, 83 and 80 were looked up; 81 is only in category 9.
	if got := f.views.Load(); got != 3 {
		t.Errorf("views = %d, want 3", got)
	}
}

func TestPagingIsCompleteAndOrdered(t *testing.T) {
	for _, total := range []int{pageSize - 1, pageSize, pageSize + 1, 2*pageSize + 5} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			f := newFake(t)
			for i := 0; i < total; i++ {
				id := 1000 + i
				f.categories["5"] = append(f.categories["5"], id)
				f.extensions[id] = fakeExt{slug: "ext-" + strconv.Itoa(id), reviews: 1, score: 50}
			}
			s := f.source()
			s.Concurrency = 8
			targets, err := s.targets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(targets) != total {
				t.Fatalf("listed %d of %d", len(targets), total)
			}
			for i, target := range targets {
				if target.ID != strconv.Itoa(1000+i) || target.Slug != "ext-"+target.ID {
					t.Fatalf("target %d = %+v", i, target)
				}
			}
		})
	}
}

func TestEmissionOrderIsDeterministicUnderConcurrency(t *testing.T) {
	f := newFake(t)
	for i := 0; i < 30; i++ {
		id := 100 + i
		slug := "ext-" + strconv.Itoa(id)
		body := "body " + slug
		f.files["/dl/"+slug+".zip"] = body
		f.files["/updates/"+slug+".xml"] = manifest(update("1.0", f.server.URL+"/dl/"+slug+".zip", digestTags(body, false)...))
		f.extensions[id] = fakeExt{slug: slug, install: "{base}/updates/" + slug + ".xml", reviews: 1, score: 1}
		f.categories["1"] = append(f.categories["1"], id)
	}
	s := f.source()
	s.Concurrency = 8
	refs, err := discover(t, s)
	if err != nil || len(refs) != 30 {
		t.Fatalf("%d refs, %v", len(refs), err)
	}
	for i, r := range refs {
		if r.Name != "ext-"+strconv.Itoa(100+i) {
			t.Fatalf("ref %d = %s", i, r.Name)
		}
	}
}

func TestPinnedDownloadMakesNoMetadataRequests(t *testing.T) {
	f, bodies := catalog(t)
	s := f.source()
	s.IDs = []string{"80"}
	refs, err := discover(t, s)
	if err != nil {
		t.Fatal(err)
	}
	views := f.views.Load()
	art, err := s.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if art.Revision != "sha512:"+sum512([]byte(bodies["2.0.0"])) {
		t.Errorf("revision = %q", art.Revision)
	}
	blob := art.Blobs[0]
	if blob.Digest != "" || blob.Size != int64(len(bodies["2.0.0"])) || blob.MediaType != "application/zip" || blob.Name != "demo-backup-2.0.0.zip" {
		t.Errorf("blob = %+v", blob)
	}
	for i := 0; i < 2; i++ { // each Open starts again at byte zero
		// Open takes its own context: the one Resolve ran under is long gone.
		body, err := blob.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(data) != bodies["2.0.0"] {
			t.Fatalf("download %d = %q, %v", i, data, err)
		}
	}
	if f.views.Load() != views {
		t.Error("Open consulted the catalog")
	}

	// A SHA-256, when published, is handed to the host.
	s2 := f.source()
	s2.IDs = []string{"80"}
	s2.VersionDepth = "2"
	refs, _ = discover(t, s2)
	art, err = s2.Resolve(context.Background(), refs[1])
	if err != nil {
		t.Fatal(err)
	}
	if art.Blobs[0].Digest != "sha256:"+sum256([]byte(bodies["1.1.0"])) || art.Revision != art.Blobs[0].Digest {
		t.Errorf("sha256 not preferred: %+v revision %q", art.Blobs[0], art.Revision)
	}
}

func TestDownloadMustMatchPublishedDigest(t *testing.T) {
	f, bodies := catalog(t)
	s := f.source()
	s.IDs = []string{"80"}
	refs, _ := discover(t, s)
	f.files["/dl/demo-2.0.0.zip"] = bodies["2.0.0"] + " tampered"
	art, err := s.Resolve(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	body, err := art.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "SHA-512") {
		t.Fatalf("tampered download read cleanly: %v", err)
	}
}

func TestSHA384IsVerified(t *testing.T) {
	body := "sha384 body"
	h := sha512.Sum384([]byte(body))
	f := newFake(t)
	f.files["/dl/x.zip"] = body
	f.files["/updates/x.xml"] = manifest(update("1.0", f.server.URL+"/dl/x.zip", "<sha384>"+strings.ToUpper(hex.EncodeToString(h[:]))+"</sha384>"))
	f.extensions[1] = fakeExt{slug: "x", install: "{base}/updates/x.xml"}
	s := f.source()
	s.IDs = []string{"1"}
	refs, err := discover(t, s)
	if err != nil || len(refs) != 1 {
		t.Fatalf("%v %v", refs, err)
	}
	if refs[0].Attributes["sha384"] != hex.EncodeToString(h[:]) {
		t.Errorf("digest not normalized to lowercase: %v", refs[0].Attributes)
	}
	art, err := s.Resolve(context.Background(), refs[0])
	if err != nil || art.Revision != "sha384:"+hex.EncodeToString(h[:]) {
		t.Fatalf("%+v %v", art, err)
	}
	f.files["/dl/x.zip"] = "different"
	data, err := art.Blobs[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(data); err == nil {
		t.Error("SHA-384 mismatch went unnoticed")
	}
}

func TestResolveRejectsTamperedReferences(t *testing.T) {
	f, _ := catalog(t)
	s := f.source()
	s.IDs = []string{"80"}
	refs, _ := discover(t, s)
	good := refs[0]
	if _, err := s.Resolve(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*registry.Reference){
		"off-host URL":        func(r *registry.Reference) { r.Attributes["download_url"] = "http://169.254.169.254/latest/meta-data/" },
		"loopback URL":        func(r *registry.Reference) { r.Attributes["download_url"] = "http://127.0.0.1:1/dl/x.zip" },
		"off-host manifest":   func(r *registry.Reference) { r.Attributes["manifest_url"] = "http://169.254.169.254/x.xml" },
		"manifest not xml":    func(r *registry.Reference) { r.Attributes["manifest_url"] = f.server.URL + "/updates/demo.zip" },
		"short digest":        func(r *registry.Reference) { r.Attributes["sha512"] = "abcd" },
		"uppercase digest":    func(r *registry.Reference) { r.Attributes["sha512"] = strings.ToUpper(r.Attributes["sha512"]) },
		"prefixed digest":     func(r *registry.Reference) { r.Attributes["sha512"] = "sha512:" + r.Attributes["sha512"] },
		"no digest":           func(r *registry.Reference) { delete(r.Attributes, "sha512") },
		"zero size":           func(r *registry.Reference) { r.Attributes["size"] = "0" },
		"bad size":            func(r *registry.Reference) { r.Attributes["size"] = "ten" },
		"bad extension id":    func(r *registry.Reference) { r.Attributes["extension_id"] = "80/../1" },
		"other registry":      func(r *registry.Reference) { r.Registry = "other" },
		"path in name":        func(r *registry.Reference) { r.Name = "../demo" },
		"uppercase slug":      func(r *registry.Reference) { r.Name = "Demo" },
		"empty version":       func(r *registry.Reference) { r.Version = "" },
		"path in version":     func(r *registry.Reference) { r.Version = "1/../2" },
		"missing attributes":  func(r *registry.Reference) { r.Attributes = nil },
		"wrong digest length": func(r *registry.Reference) { r.Attributes["sha256"] = strings.Repeat("0", 63) },
	} {
		ref := good
		ref.Attributes = maps.Clone(good.Attributes)
		mutate(&ref)
		if _, err := s.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%s: resolved a tampered reference", name)
		}
	}
}

func TestDownloadHostRule(t *testing.T) {
	s := &Source{} // production rule: no allowlist override
	manifestURL := "https://updates.example.com/demo/update.xml"
	for download, ok := range map[string]bool{
		"https://updates.example.com/files/demo.zip":                    true,
		"https://UPDATES.example.com/files/demo.zip":                    true,
		"https://github.com/acme/demo/releases/download/1.0/demo.zip":   true,
		"https://codeload.github.com/acme/demo/zip/refs/tags/1.0":       true,
		"https://raw.githubusercontent.com/acme/demo/master/demo.zip":   true,
		"https://cdn.example.com/demo.zip":                              false, // a different host, even a sibling
		"https://evil.example.net/demo.zip":                             false,
		"http://updates.example.com/files/demo.zip":                     false,
		"https://updates.example.com:8443/files/demo.zip":               false,
		"https://user:pw@updates.example.com/demo.zip":                  false,
		"https://169.254.169.254/latest/meta-data/":                     false,
		"https://[::1]/demo.zip":                                        false,
		"https://localhost/demo.zip":                                    false,
		"https://intranet/demo.zip":                                     false,
		"https://box.internal/demo.zip":                                 false,
		"https://updates.example.com.evil.net/demo.zip":                 false,
		"https://github.com.evil.net/acme/demo/releases/download/x.zip": false,
		"ftp://updates.example.com/demo.zip":                            false,
		"":                                                              false,
		"https://api.github.com/repos/acme/demo/zipball/1.0":            false,
	} {
		err := s.checkDownload(manifestURL, download)
		if (err == nil) != ok {
			t.Errorf("checkDownload(%q) = %v, want ok=%v", download, err, ok)
		}
	}
	for _, bad := range []string{"http://updates.example.com/update.xml", "https://10.0.0.1/update.xml", "https://localhost/update.xml"} {
		if err := s.checkDownload(bad, "https://github.com/acme/demo/releases/download/1.0/demo.zip"); err == nil {
			t.Errorf("accepted manifest %q", bad)
		}
		if err := s.checkManifest(bad); err == nil {
			t.Errorf("checkManifest accepted %q", bad)
		}
	}
	if err := s.checkManifest(manifestURL); err != nil {
		t.Error(err)
	}
}

func TestCatalogSurvivesBrokenManifests(t *testing.T) {
	f, _ := catalog(t)
	f.files["/updates/garbled.xml"] = `<updates><update><version>1.0`
	f.files["/updates/set.xml"] = `<extensionset name="x"><extension name="y"/></extensionset>`
	f.extensions[84] = fakeExt{slug: "garbled", install: "{base}/updates/garbled.xml"}
	f.extensions[85] = fakeExt{slug: "ext-set", install: "{base}/updates/set.xml"}
	f.categories["9"] = append(f.categories["9"], 84, 85)
	refs, err := discover(t, f.source())
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(refs); !slices.Equal(got, []string{"demo-backup@2.0.0"}) {
		t.Errorf("versions = %v", got)
	}
}

func TestInsecureManifestsAreSkippedWithoutAllowlist(t *testing.T) {
	f, _ := catalog(t)
	s := f.source()
	s.DownloadHosts = nil // production rule: the fake's http URLs are refused
	refs, err := discover(t, s)
	if err != nil || len(refs) != 0 {
		t.Fatalf("%v, %v", versions(refs), err)
	}
	s.IDs = []string{"80"}
	if _, err := discover(t, s); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("explicit selection of a manifest the production rule refuses: %v", err)
	}
}

func TestCatalogFailuresAreErrors(t *testing.T) {
	f, _ := catalog(t)
	// Joomla's own endpoint failing is not an extension being unavailable.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("view") == "extension" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		f.serve(w, r)
	}))
	defer broken.Close()
	s := f.source()
	s.Endpoint = broken.URL + "/webapps/index.php"
	if _, err := discover(t, s); err == nil {
		t.Fatal("catalog outage was swallowed")
	}

	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") }))
	defer notJSON.Close()
	s.Endpoint = notJSON.URL + "/webapps/index.php"
	if _, err := discover(t, s); err == nil {
		t.Fatal("non-JSON catalog accepted")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { envelope(w, "<p>nothing</p>") }))
	defer empty.Close()
	s.Endpoint = empty.URL + "/webapps/index.php"
	if _, err := discover(t, s); err == nil || !strings.Contains(err.Error(), "no categories") {
		t.Fatalf("categoryless front page: %v", err)
	}
}

func TestCancellation(t *testing.T) {
	f, _ := catalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.source().Discover(ctx, func(registry.Reference) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}

	// Cancelling from emit stops the walk promptly.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	stop := errors.New("enough")
	err = f.source().Discover(ctx, func(registry.Reference) error { cancel(); return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("emit error = %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"10.5.0", "10.0.2", "9.6.2", "2.0.0", "1.10", "1.9", "1.0.1", "1.0.0", "1.0.0-rc1", "1.0.0-beta2", "1.0.0-beta1", "0.9"}
	for i := range ordered {
		for j := range ordered {
			got := compareVersions(ordered[i], ordered[j])
			want := 0
			if i < j {
				want = 1
			} else if i > j {
				want = -1
			}
			if (got > 0) != (want > 0) || (got < 0) != (want < 0) {
				t.Errorf("compareVersions(%q, %q) = %d, want sign %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestParseListingRejectsUnrecognizedMarkup(t *testing.T) {
	if _, err := parseListing(`<li class="item span3"><p>no id here</p></li>`); err == nil {
		t.Error("accepted a listing entry without an extension ID")
	}
	items, err := parseListing(`<ul></ul>`)
	if err != nil || len(items) != 0 {
		t.Errorf("empty category: %v, %v", items, err)
	}
	items, _ = parseListing(`<li class="item span3"><a href="index.php?option=com_apps&format=json&view=extension&id=7">x</a></li>`)
	if len(items) != 1 || items[0].Slug != "" {
		t.Errorf("unrated entry = %+v", items)
	}
}

func TestManifestURLsAreNormalized(t *testing.T) {
	for in, want := range map[string]string{
		"http://cdn.example.com/updates/x.xml":  "https://cdn.example.com/updates/x.xml",
		"https://cdn.example.com/updates/x.xml": "https://cdn.example.com/updates/x.xml",
		"ftp://cdn.example.com/updates/x.xml":   "ftp://cdn.example.com/updates/x.xml",
		"":                                      "",
		// the GitHub viewer page becomes the raw file
		"https://github.com/acme/demo/blob/master/updates.xml":     "https://raw.githubusercontent.com/acme/demo/master/updates.xml",
		"http://github.com/acme/demo/blob/main/dist/update.xml":    "https://raw.githubusercontent.com/acme/demo/main/dist/update.xml",
		"https://github.com/acme/demo/releases/download/1/upd.xml": "https://github.com/acme/demo/releases/download/1/upd.xml",
		"https://github.com/acme/demo/blob/master":                 "https://github.com/acme/demo/blob/master",
		"https://example.com/acme/demo/blob/master/updates.xml":    "https://example.com/acme/demo/blob/master/updates.xml",
	} {
		if got := normalizeManifestURL(in); got != want {
			t.Errorf("normalizeManifestURL(%q) = %q, want %q", in, got, want)
		}
	}
}
