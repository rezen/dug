// Package joomla archives Joomla extensions listed in the Joomla Extensions
// Directory's Install from Web catalog. The directory is an index, not a host:
// the packages live on each developer's own server. An install URL that is an
// update-server manifest yields every stable release it advertises, pinned with
// the digest the manifest publishes. An install URL that is a zip yields one
// release, pinned by streaming the archive through SHA-256 at listing time.
// Anything else (landing pages, plain-HTTP-only hosts, archives over the size
// limit) is skipped.
package joomla

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"html"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/pool"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services"
)

// Identity is the registry name the host selects: `dug list joomla`.
const Identity = "joomla"

// Namespace records where artifacts came from; manifests persist it.
const Namespace = "extensions.joomla.org"

// DefaultEndpoint is the Install from Web catalog the Joomla installer plugin
// reads (`com_apps`). It answers JSON whose data.html is an HTML fragment.
const DefaultEndpoint = "https://appscdn.joomla.org/webapps/index.php"

// ReleaseHosts are accepted as download hosts for every extension, in
// addition to the host of its manifest. Many developers keep the manifest on
// their own site and attach the packages to GitHub releases.
var ReleaseHosts = []string{"github.com", "codeload.github.com", "raw.githubusercontent.com"}

const (
	maxMetadataBytes  = 32 << 20
	defaultMaxPackage = 512 << 20
	pageSize          = 100
	maxPages          = 1000
	defaultWorkers    = 4
)

type Source struct {
	Client   *http.Client
	Endpoint string // catalog URL override for tests; empty means DefaultEndpoint
	Attempts int    // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts replaces the manifest-host rule with an exact allowlist;
	// tests point it at httptest.
	DownloadHosts []string

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Slugs        []string // JED slugs; found by walking the catalog
	IDs          []string // Install from Web extension IDs; no walk needed
	Categories   []string // catalog category IDs to walk; empty means all
	VersionDepth string   // empty means the current release only
	Concurrency  int      // extension and manifest lookups in flight; 0 means 4
	// MaxPackageBytes is the largest directly linked archive the listing will
	// pin; 0 means 512 MiB. Larger archives are skipped.
	MaxPackageBytes int64
}

var _ registry.Registry = (*Source)(nil)

var (
	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,199}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,127}$`)
	idPattern      = regexp.MustCompile(`^[0-9]{1,10}$`)

	itemSplit    = regexp.MustCompile(`<li class="item span3">`)
	extensionID  = regexp.MustCompile(`view=extension&id=([0-9]+)`)
	extensionURL = regexp.MustCompile(`https://extensions\.joomla\.org/extension/([a-z0-9][a-z0-9._-]*)/`)
	categoryLink = regexp.MustCompile(`view=category&id=([0-9]+)"`)
	ratingText   = regexp.MustCompile(`([0-9]+) reviews? with a score of ([0-9]+)/100`)
	versionLine  = regexp.MustCompile(`<dt>Version</dt><dd>([^<]*)</dd>`)
	updatedText  = regexp.MustCompile(`last updated on ([0-9]{1,2} [A-Za-z]+ [0-9]{4})`)
	licenseLine  = regexp.MustCompile(`<dt>License</dt><dd>([^<]*)</dd>`)
	typeBadge    = regexp.MustCompile(`<span title="(Component|Module|Plugin|Package|Template|Language|Library|File)" class="badge jbadge`)
	authorText   = regexp.MustCompile(`</strong> \(By ([^)<]+)\)`)
	installCall  = regexp.MustCompile(`Joomla\.installfromweb\('([^']*)'`)
)

func (s *Source) endpoint() string {
	if s.Endpoint != "" {
		return s.Endpoint
	}
	return DefaultEndpoint
}

func (s *Source) attempts() int {
	if s.Attempts > 0 {
		return s.Attempts
	}
	return 4
}

func (s *Source) workers() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return defaultWorkers
}

func validSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("invalid %s extension slug %q", Identity, slug)
	}
	return nil
}

func validVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid %s version %q", Identity, version)
	}
	return nil
}

// cleanVersion turns the directory's version text into a version string, or
// "" when nothing usable is left: "3.0 beta" becomes 3.0-beta.
func cleanVersion(text string) string {
	version := strings.Join(strings.Fields(text), "-")
	if validVersion(version) != nil {
		return ""
	}
	return version
}

func validID(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid %s %s %q", Identity, kind, id)
	}
	return nil
}

// extension is what the catalog's view of one extension tells us.
type extension struct {
	ID      string
	Slug    string
	Install string // the install URL the directory links; empty when none
	// Manifest is Install once it is known to be an update manifest; empty for
	// an extension linked straight to its archive.
	Manifest string
	// Version is the version the directory shows, normalized; it names the one
	// release of an extension that has no manifest.
	Version string
	License string
	Author  string
	Types   []string
	Rating  int64 // 0–100; -1 when unrated
	Reviews int64
	Updated time.Time
}

// release is one installable version from an extension's update manifest.
type release struct {
	Version string
	URL     string
	Digests map[string]string // algorithm -> lowercase hex
	Size    int64             // 0 when the manifest gives none
}

// listed is one catalog entry as a category page shows it. The slug is empty
// when the listing carries no review link.
type listed struct{ ID, Slug string }

// Discover finds the extensions to archive, looks each one up, and emits one
// reference per release. Every reference is self-resolved before emission so a
// listing never persists something stage two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	if !depth.Since.IsZero() {
		return errDateDepth
	}
	targets, err := s.targets(ctx)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(s.Slugs))
	for _, slug := range s.Slugs {
		if err := validSlug(slug); err != nil {
			return err
		}
		wanted[slug] = true
	}
	explicit := len(s.IDs) > 0 || len(s.Slugs) > 0
	var emitted, outOfScope int
	found := map[string]bool{}
	err = pool.Ordered(ctx, s.workers(), targets, s.lookup, func(r outcome) error {
		if r.err != nil {
			return r.err
		}
		if len(wanted) > 0 && !wanted[r.ext.Slug] {
			return nil
		}
		found[r.ext.Slug] = true
		if r.skip != "" {
			if explicit {
				return fmt.Errorf("%s %s (id %s) cannot be archived: %s", Identity, r.ext.Slug, r.ext.ID, r.skip)
			}
			outOfScope++
			slog.Debug("joomla extension skipped", "slug", r.ext.Slug, "reason", r.skip)
			return nil
		}
		emitted++
		return s.emitVersions(ctx, r.ext, r.releases, depth, emit)
	})
	if err != nil {
		return err
	}
	for _, slug := range s.Slugs {
		if !found[slug] {
			return fmt.Errorf("%s extension %q is not in the Install from Web catalog", Identity, slug)
		}
	}
	slog.Info("joomla discovery finished", "extensions", emitted, "skipped", outOfScope, "listed", len(targets))
	return nil
}

var errDateDepth = fmt.Errorf("%s update manifests carry no release dates; version_depth must be a count or unlimited", Identity)

// targets lists the extension IDs to look up, in a stable order.
func (s *Source) targets(ctx context.Context) ([]listed, error) {
	if len(s.IDs) > 0 {
		out := make([]listed, 0, len(s.IDs))
		seen := map[string]bool{}
		for _, id := range s.IDs {
			if err := validID("extension ID", id); err != nil {
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, listed{ID: id})
			}
		}
		return out, nil
	}
	categories := s.Categories
	if len(categories) == 0 {
		var err error
		if categories, err = s.categoryIDs(ctx); err != nil {
			return nil, err
		}
	}
	wanted := map[string]bool{}
	for _, slug := range s.Slugs {
		wanted[slug] = true
	}
	var out []listed
	seen := map[string]bool{}
	for _, category := range categories {
		if err := validID("category ID", category); err != nil {
			return nil, err
		}
		err := s.walkCategory(ctx, category, func(item listed) {
			if seen[item.ID] || (len(wanted) > 0 && item.Slug != "" && !wanted[item.Slug]) {
				return
			}
			seen[item.ID] = true
			out = append(out, item)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// categoryIDs reads the category list from the catalog's front page.
func (s *Source) categoryIDs(ctx context.Context) ([]string, error) {
	page, err := s.page(ctx, url.Values{"view": {"dashboard"}})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range categoryLink.FindAllStringSubmatch(page, -1) {
		if !slices.Contains(ids, m[1]) {
			ids = append(ids, m[1])
		}
	}
	if len(ids) == 0 {
		return nil, retry.Permanent(fmt.Errorf("%s catalog front page lists no categories", Identity))
	}
	return ids, nil
}

// walkCategory pages through one category, newest first by creation time, which
// never changes. Joomla restarts from the first page when the offset passes the
// end, so the walk ends on a short page or a page with nothing new. Every page
// holds each item twice in its markup; the first sighting counts.
func (s *Source) walkCategory(ctx context.Context, category string, visit func(listed)) error {
	seen := map[string]bool{}
	for start, pages := 0, 0; ; start, pages = start+pageSize, pages+1 {
		if pages >= maxPages {
			return retry.Permanent(fmt.Errorf("%s category %s exceeds %d pages", Identity, category, maxPages))
		}
		page, err := s.page(ctx, url.Values{
			"view": {"category"}, "id": {category}, "ordering": {"core_created_time"},
			"limit": {strconv.Itoa(pageSize)}, "limitstart": {strconv.Itoa(start)},
		})
		if err != nil {
			return err
		}
		items, err := parseListing(page)
		if err != nil {
			return fmt.Errorf("%s category %s: %w", Identity, category, err)
		}
		fresh := 0
		for _, item := range items {
			if !seen[item.ID] {
				seen[item.ID] = true
				fresh++
				visit(item)
			}
		}
		if fresh == 0 || len(items) < pageSize {
			return nil
		}
	}
}

func parseListing(page string) ([]listed, error) {
	var items []listed
	seen := map[string]bool{}
	for _, block := range itemSplit.Split(page, -1)[1:] {
		m := extensionID.FindStringSubmatch(block)
		if m == nil {
			return nil, retry.Permanent(errors.New("listing entry without an extension ID"))
		}
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		item := listed{ID: m[1]}
		if m := extensionURL.FindStringSubmatch(block); m != nil {
			item.Slug = m[1]
		}
		items = append(items, item)
	}
	return items, nil
}

// outcome is the result for one target: the extension's page and, when it is in
// scope, its releases. skip carries the reason an extension is out of scope.
type outcome struct {
	ext      extension
	releases []release
	skip     string
	err      error
}

func (s *Source) lookup(ctx context.Context, target listed) outcome {
	ext, err := s.extension(ctx, target.ID)
	if err != nil {
		return outcome{err: err}
	}
	if s.DownloadHosts == nil {
		ext.Install = normalizeInstallURL(ext.Install)
	}
	out := outcome{ext: ext}
	switch {
	case ext.Install == "":
		out.skip = "no install URL"
	case strings.HasSuffix(strings.ToLower(mustPath(ext.Install)), ".xml"):
		s.manifestReleases(ctx, &out)
	default:
		s.directRelease(ctx, &out)
	}
	return out
}

// manifestReleases fills out from the extension's update manifest. A URL that
// ends in .xml but serves a zip, as some download servers do, is handled as a
// direct link.
func (s *Source) manifestReleases(ctx context.Context, out *outcome) {
	ext := out.ext
	ext.Manifest = ext.Install
	if err := s.checkManifest(ext.Manifest); err != nil {
		out.skip = err.Error()
		return
	}
	releases, skipped, err := s.releases(ctx, ext)
	switch {
	case ctx.Err() != nil:
		out.err = ctx.Err()
	case errors.Is(err, errNotManifest):
		s.directRelease(ctx, out)
	case err != nil:
		// A developer's server being down or its manifest being malformed
		// is that extension's problem, not the walk's.
		slog.Warn("joomla manifest unavailable", "slug", ext.Slug, "manifest", ext.Manifest, "err", err)
		out.skip = "manifest unavailable"
	case len(releases) == 0:
		out.skip = fmt.Sprintf("no release with a download and a digest (%d skipped)", skipped)
	default:
		out.ext, out.releases = ext, releases
	}
}

// directRelease fills out for an extension the directory links straight to its
// archive. The directory publishes no digest, size or history for it, so the
// listing streams the archive once through SHA-256 and pins what it saw; stage
// two then fails if the developer has since replaced the file.
func (s *Source) directRelease(ctx context.Context, out *outcome) {
	ext := out.ext
	if ext.Version == "" {
		out.skip = "the directory shows no usable version"
		return
	}
	if err := s.checkDirect(ext.Install); err != nil {
		out.skip = err.Error()
		return
	}
	digest, size, err := s.pinPackage(ctx, ext.Install)
	switch {
	case ctx.Err() != nil:
		out.err = ctx.Err()
	case errors.Is(err, errNotZip):
		out.skip = "install URL is not a zip archive"
	case err != nil:
		slog.Warn("joomla package unavailable", "slug", ext.Slug, "url", ext.Install, "err", err)
		out.skip = "package unavailable"
	default:
		out.releases = []release{{Version: ext.Version, URL: ext.Install, Digests: map[string]string{"sha256": digest}, Size: size}}
	}
}

var (
	errNotZip      = errors.New("not a zip archive")
	errNotManifest = errors.New("not an update manifest")
	zipMagic       = []byte("PK\x03\x04")
)

func (s *Source) maxPackage() int64 {
	if s.MaxPackageBytes > 0 {
		return s.MaxPackageBytes
	}
	return defaultMaxPackage
}

// pinPackage downloads one archive, checks it starts like a zip, and returns
// its SHA-256 and size. It never keeps the bytes.
func (s *Source) pinPackage(ctx context.Context, raw string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.external(), raw, s.attempts())
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	limit := s.maxPackage()
	if response.ContentLength > limit {
		return "", 0, fmt.Errorf("package is %d bytes, over the %d byte limit", response.ContentLength, limit)
	}
	head := make([]byte, len(zipMagic))
	if _, err := io.ReadFull(response.Body, head); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", 0, errNotZip
		}
		return "", 0, err
	}
	if !bytes.Equal(head, zipMagic) {
		return "", 0, errNotZip
	}
	h := sha256.New()
	h.Write(head)
	n, err := io.Copy(h, io.LimitReader(response.Body, limit-int64(len(head))+1))
	if err != nil {
		return "", 0, err
	}
	if size := n + int64(len(head)); size <= limit {
		return hex.EncodeToString(h.Sum(nil)), size, nil
	}
	return "", 0, fmt.Errorf("package exceeds the %d byte limit", limit)
}

// external returns the client for developers' servers. Their URLs come from
// catalog entries anyone can submit, so every redirect hop must stay on HTTPS
// to a public DNS name, and a link cannot bounce the plugin to an internal
// address. Tests that supply an allowlist keep the shared client unchanged.
func (s *Source) external() *http.Client {
	client := s.Client
	if client == nil {
		client = services.DefaultDugHttpClient()
	}
	if s.DownloadHosts != nil {
		return client
	}
	guarded := *client
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return publicHTTPS(req.URL.String())
	}
	return &guarded
}

// normalizeInstallURL repairs two forms the directory links that are not the
// file itself. An http URL becomes https, because some entries still link
// the plain form of a server that answers on both and nothing is
// fetched over a channel anyone on the path could edit. A GitHub blob URL, which
// is the HTML viewer, becomes the raw file it shows.
func normalizeInstallURL(raw string) string {
	if rest, ok := strings.CutPrefix(raw, "http://"); ok {
		raw = "https://" + rest
	}
	if rest, ok := strings.CutPrefix(raw, "https://github.com/"); ok {
		if parts := strings.SplitN(rest, "/", 5); len(parts) == 5 && parts[2] == "blob" {
			return "https://raw.githubusercontent.com/" + strings.Join([]string{parts[0], parts[1], parts[3], parts[4]}, "/")
		}
	}
	return raw
}

func mustPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// extension fetches and parses the catalog's page for one extension.
func (s *Source) extension(ctx context.Context, id string) (extension, error) {
	page, err := s.page(ctx, url.Values{"view": {"extension"}, "id": {id}})
	if err != nil {
		return extension{}, fmt.Errorf("%s extension %s: %w", Identity, id, err)
	}
	ext := extension{ID: id, Rating: -1}
	m := extensionURL.FindStringSubmatch(page)
	if m == nil {
		return extension{}, retry.Permanent(fmt.Errorf("%s extension %s: page has no directory link", Identity, id))
	}
	ext.Slug = m[1]
	if m := ratingText.FindStringSubmatch(page); m != nil {
		ext.Reviews, _ = strconv.ParseInt(m[1], 10, 64)
		ext.Rating, _ = strconv.ParseInt(m[2], 10, 64)
	}
	if m := versionLine.FindStringSubmatch(page); m != nil {
		text := html.UnescapeString(m[1])
		before, _, _ := strings.Cut(text, "(last updated on")
		ext.Version = cleanVersion(before)
		if d := updatedText.FindStringSubmatch(text); d != nil {
			if t, err := time.Parse("2 January 2006", d[1]); err == nil {
				ext.Updated = t
			}
		}
	}
	if m := licenseLine.FindStringSubmatch(page); m != nil {
		ext.License = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	if m := authorText.FindStringSubmatch(page); m != nil {
		ext.Author = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	for _, m := range typeBadge.FindAllStringSubmatch(page, -1) {
		ext.Types = append(ext.Types, m[1])
	}
	if m := installCall.FindStringSubmatch(page); m != nil {
		ext.Install = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return ext, nil
}

// page asks the catalog for a view and returns its HTML fragment.
func (s *Source) page(ctx context.Context, q url.Values) (string, error) {
	q = maps.Clone(q)
	q.Set("format", "json")
	q.Set("option", "com_apps")
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			HTML string `json:"html"`
		} `json:"data"`
	}
	data, err := s.fetch(ctx, s.Client, s.endpoint()+"?"+q.Encode())
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", retry.Permanent(fmt.Errorf("%s catalog: %w", Identity, err))
	}
	if !envelope.Success || envelope.Data.HTML == "" {
		return "", retry.Permanent(fmt.Errorf("%s catalog %s view reported failure", Identity, q.Get("view")))
	}
	return envelope.Data.HTML, nil
}

// fetch reads one bounded metadata document. Failures that no retry can fix are
// permanent so the host stops retrying too.
func (s *Source) fetch(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, client, raw, s.attempts())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes {
		return nil, retry.Permanent(fmt.Errorf("%s metadata exceeds 32 MiB", Identity))
	}
	return data, nil
}

// updateSet mirrors the Joomla update server format.
type updateSet struct {
	Updates []struct {
		Version   string `xml:"version"`
		Downloads []struct {
			Type   string `xml:"type,attr"`
			Format string `xml:"format,attr"`
			URL    string `xml:",chardata"`
		} `xml:"downloads>downloadurl"`
		Tags   []string `xml:"tags>tag"`
		SHA256 string   `xml:"sha256"`
		SHA384 string   `xml:"sha384"`
		SHA512 string   `xml:"sha512"`
		Length string   `xml:"length"`
	} `xml:"update"`
}

var digestSizes = map[string]int{"sha256": sha256.Size, "sha384": sha512.Size384, "sha512": sha512.Size}

// releases reads the extension's manifest and returns its installable releases
// newest first, plus the count of entries it had to leave out. An entry is left
// out when it is not a stable full zip, has no digest, or its download URL
// breaks the host rule. A version listed twice with different downloads is
// ambiguous and is dropped.
func (s *Source) releases(ctx context.Context, ext extension) ([]release, int, error) {
	data, err := s.fetch(ctx, s.external(), ext.Manifest)
	if err != nil {
		return nil, 0, err
	}
	if bytes.HasPrefix(data, zipMagic) {
		return nil, 0, errNotManifest
	}
	var set updateSet
	if err := xml.Unmarshal(data, &set); err != nil {
		return nil, 0, retry.Permanent(fmt.Errorf("manifest %s: %w", ext.Manifest, err))
	}
	if len(set.Updates) == 0 {
		return nil, 0, retry.Permanent(fmt.Errorf("manifest %s lists no updates", ext.Manifest))
	}
	byVersion := map[string]release{}
	conflicted := map[string]bool{}
	skipped := 0
	for _, u := range set.Updates {
		version := strings.TrimSpace(u.Version)
		if validVersion(version) != nil {
			skipped++
			continue
		}
		if len(u.Tags) > 0 && !slices.ContainsFunc(u.Tags, func(t string) bool { return strings.EqualFold(strings.TrimSpace(t), "stable") }) {
			skipped++
			continue
		}
		rel := release{Version: version, Digests: map[string]string{}}
		for _, d := range u.Downloads {
			if (d.Type == "" || d.Type == "full") && (d.Format == "" || d.Format == "zip") {
				rel.URL = strings.TrimSpace(d.URL)
				break
			}
		}
		for algorithm, value := range map[string]string{"sha256": u.SHA256, "sha384": u.SHA384, "sha512": u.SHA512} {
			if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
				rel.Digests[algorithm] = value
			}
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(u.Length), 10, 64); err == nil && n > 0 {
			rel.Size = n
		}
		if rel.URL == "" || len(rel.Digests) == 0 {
			skipped++
			continue
		}
		if err := s.checkDownload(ext.Manifest, rel.URL); err != nil {
			slog.Warn("joomla release skipped", "slug", ext.Slug, "version", version, "err", err)
			skipped++
			continue
		}
		if _, err := parseDigests(rel.Digests); err != nil {
			slog.Warn("joomla release skipped", "slug", ext.Slug, "version", version, "err", err)
			skipped++
			continue
		}
		if prior, ok := byVersion[version]; ok {
			if prior.URL != rel.URL || !maps.Equal(prior.Digests, rel.Digests) || prior.Size != rel.Size {
				conflicted[version] = true
			}
			continue
		}
		byVersion[version] = rel
	}
	var out []release
	for version, rel := range byVersion {
		if conflicted[version] {
			slog.Warn("joomla release skipped", "slug", ext.Slug, "version", version, "err", "manifest lists the version with different downloads")
			skipped++
			continue
		}
		out = append(out, rel)
	}
	slices.SortFunc(out, func(a, b release) int { return compareVersions(b.Version, a.Version) })
	return out, skipped, nil
}

// compareVersions orders dotted versions: numeric parts numerically, and a
// pre-release suffix such as -beta1 before the bare version.
func compareVersions(a, b string) int {
	ta, tb := versionTokens(a), versionTokens(b)
	for i := 0; i < max(len(ta), len(tb)); i++ {
		switch {
		case i >= len(ta):
			if isNumber(tb[i]) {
				return -1
			}
			return 1
		case i >= len(tb):
			if isNumber(ta[i]) {
				return 1
			}
			return -1
		}
		x, y := ta[i], tb[i]
		switch {
		case isNumber(x) && isNumber(y):
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				return len(x) - len(y)
			}
		case isNumber(x):
			return 1
		case isNumber(y):
			return -1
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return strings.Compare(a, b)
}

func versionTokens(v string) []string {
	return strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' || r == '~' })
}

func isNumber(token string) bool {
	return token != "" && strings.Trim(token, "0123456789") == ""
}

// emitVersions emits the newest release first, then older ones while the depth
// allows. Item-wide facts are attached to every version, including old ones;
// they are a listing-time snapshot, not history.
func (s *Source) emitVersions(ctx context.Context, ext extension, releases []release, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	facts := &registry.Facts{}
	if !ext.Updated.IsZero() {
		facts.UpdatedAt = registry.Time(ext.Updated)
	}
	metadata := map[string]string{}
	if ext.Rating >= 0 {
		metadata["rating"] = strconv.FormatInt(ext.Rating, 10)
		metadata["num_ratings"] = strconv.FormatInt(ext.Reviews, 10)
	}
	if ext.License != "" {
		metadata["license"] = ext.License
	}
	tags := registry.NormalizeTags(ext.Types)
	var author *registry.Author
	if ext.Author != "" {
		author = &registry.Author{Name: ext.Author}
	}
	for i, rel := range releases {
		versionMetadata := maps.Clone(metadata)
		versionMetadata["version_rank"] = strconv.Itoa(i + 1)
		attributes := map[string]string{"extension_id": ext.ID, "download_url": rel.URL}
		versionMetadata["source"] = "directory-link"
		if ext.Manifest != "" {
			attributes["manifest_url"] = ext.Manifest
			versionMetadata["source"] = "update-manifest"
		}
		for algorithm, value := range rel.Digests {
			attributes[algorithm] = value
		}
		if rel.Size > 0 {
			attributes["size"] = strconv.FormatInt(rel.Size, 10)
		}
		ref := registry.Reference{
			Registry:   Identity,
			Namespace:  Namespace,
			Name:       ext.Slug,
			Version:    rel.Version,
			Attributes: attributes,
			Metadata:   versionMetadata,
			Facts:      facts.Clone(),
			Tags:       slices.Clone(tags),
			Author:     author.Clone(),
		}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return fmt.Errorf("%s@%s: %w", ext.Slug, rel.Version, err)
		}
		if err := emit(ref); err != nil {
			return err
		}
		if depth.Max > 0 && i+1 >= depth.Max {
			return nil
		}
	}
	return nil
}

// checkManifest applies the rule for the manifest URL: HTTPS to a public DNS
// name, because the digest it publishes is what the archive is verified
// against and plain HTTP lets anyone on the path replace both.
func (s *Source) checkManifest(raw string) error {
	if s.DownloadHosts != nil {
		return httpx.ValidateURLHost(raw, s.DownloadHosts)
	}
	if err := publicHTTPS(raw); err != nil {
		return fmt.Errorf("manifest %w", err)
	}
	return nil
}

// checkDirect applies the rule for an archive the directory links itself: HTTPS
// to a public DNS name. There is no manifest to tie the host to, so the pinned
// digest is what binds the bytes; the host rule only keeps the plugin off
// internal addresses. Run the plugin without access to internal networks.
func (s *Source) checkDirect(raw string) error {
	if s.DownloadHosts != nil {
		return httpx.ValidateURLHost(raw, s.DownloadHosts)
	}
	return publicHTTPS(raw)
}

// checkDownload applies the download host rule: a package may only be fetched
// from the host of the manifest that published it (with or without www.), or
// from a GitHub release host, over HTTPS to a public DNS name. Inventories live in S3 and could be
// edited, so this runs again in Resolve. The persisted manifest URL is part of
// the same descriptor, so the rule narrows what a tampered inventory can reach
// to named public hosts; it cannot make an arbitrary host impossible. Run the
// plugin without access to internal networks.
func (s *Source) checkDownload(manifestURL, downloadURL string) error {
	if s.DownloadHosts != nil {
		return httpx.ValidateURLHost(downloadURL, s.DownloadHosts)
	}
	if err := publicHTTPS(manifestURL); err != nil {
		return fmt.Errorf("manifest %w", err)
	}
	if err := publicHTTPS(downloadURL); err != nil {
		return err
	}
	manifest, _ := url.Parse(manifestURL)
	download, _ := url.Parse(downloadURL)
	host := strings.ToLower(download.Hostname())
	if sameSite(host, strings.ToLower(manifest.Hostname())) || slices.Contains(ReleaseHosts, host) {
		return nil
	}
	return fmt.Errorf("download host %q is neither the manifest host %q nor a GitHub release host", host, manifest.Hostname())
}

// sameSite reports whether two lowercase hosts are equal once a leading www. is
// ignored, since sites commonly serve the same files from both.
func sameSite(a, b string) bool {
	return strings.TrimPrefix(a, "www.") == strings.TrimPrefix(b, "www.")
}

// publicHTTPS accepts only an HTTPS URL on the default port whose host is a
// dotted DNS name, not an IP literal, localhost, or an internal suffix.
func publicHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return fmt.Errorf("URL must be https without credentials")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("URL must use the default https port")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("host %q is not a public DNS name", host)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".corp", ".arpa"} {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("host %q is not a public DNS name", host)
		}
	}
	return nil
}

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// digest. It makes no network requests; the pinned descriptor is final, so a
// version removed from the manifest fails instead of being replaced.
//
// Manifests publish SHA-256, SHA-384 or SHA-512, any subset. A SHA-256 goes in
// the blob for the host to check; the plugin checks the strongest of the others
// itself while the package streams.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity || validSlug(ref.Name) != nil || validVersion(ref.Version) != nil {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	if err := validID("extension ID", ref.Attributes["extension_id"]); err != nil {
		return registry.Artifact{}, err
	}
	manifest, hasManifest := ref.Attributes["manifest_url"]
	raw := ref.Attributes["download_url"]
	if hasManifest {
		if !strings.HasSuffix(strings.ToLower(mustPath(manifest)), ".xml") {
			return registry.Artifact{}, fmt.Errorf("invalid %s manifest URL", Identity)
		}
		if s.DownloadHosts != nil {
			if err := httpx.ValidateURLHost(manifest, s.DownloadHosts); err != nil {
				return registry.Artifact{}, err
			}
		}
		if err := s.checkDownload(manifest, raw); err != nil {
			return registry.Artifact{}, err
		}
	} else if err := s.checkDirect(raw); err != nil {
		return registry.Artifact{}, err
	}
	digests := map[string]string{}
	for algorithm := range digestSizes {
		if value, ok := ref.Attributes[algorithm]; ok {
			digests[algorithm] = value
		}
	}
	verify, err := parseDigests(digests)
	if err != nil {
		return registry.Artifact{}, err
	}
	if !hasManifest {
		// A directly linked archive is pinned by the listing itself, so its
		// descriptor is only complete with a SHA-256 and a size.
		if _, ok := digests["sha256"]; !ok || len(digests) != 1 || ref.Attributes["size"] == "" {
			return registry.Artifact{}, fmt.Errorf("%s directly linked archive needs a SHA-256 and a size", Identity)
		}
	}
	size := int64(-1)
	if value, ok := ref.Attributes["size"]; ok {
		if size, err = strconv.ParseInt(value, 10, 64); err != nil || size < 1 {
			return registry.Artifact{}, fmt.Errorf("invalid %s artifact size", Identity)
		}
	}
	blob := registry.Blob{
		Name:      blobName(ref.Name + "-" + ref.Version + ".zip"),
		MediaType: "application/zip",
		Size:      size,
	}
	revision := ""
	if value, ok := digests["sha256"]; ok {
		blob.Digest = "sha256:" + value
		revision = blob.Digest
	}
	for _, algorithm := range []string{"sha512", "sha384"} {
		if value, ok := digests[algorithm]; ok {
			if revision == "" {
				revision = algorithm + ":" + value
			}
			break
		}
	}
	blob.Open = func(ctx context.Context) (io.ReadCloser, error) {
		response, err := httpx.Get(ctx, s.external(), raw, s.attempts())
		if err != nil {
			return nil, err
		}
		if verify == nil {
			return response.Body, nil
		}
		return verify(response.Body), nil
	}
	return registry.Artifact{Reference: ref, Revision: revision, Blobs: []registry.Blob{blob}}, nil
}

// parseDigests checks every digest is exactly the lowercase hex of its
// algorithm and at least one is present. It returns a wrapper that enforces the
// strongest digest the host does not check itself (SHA-512, else SHA-384), or
// nil when SHA-256 is the only one.
func parseDigests(digests map[string]string) (func(io.ReadCloser) io.ReadCloser, error) {
	if len(digests) == 0 {
		return nil, fmt.Errorf("%s artifact has no digest", Identity)
	}
	for algorithm, value := range digests {
		want, ok := digestSizes[algorithm]
		sum, err := hex.DecodeString(value)
		if !ok || err != nil || len(sum) != want || value != hex.EncodeToString(sum) {
			return nil, fmt.Errorf("invalid %s %s digest", Identity, label(algorithm))
		}
	}
	for _, algorithm := range []string{"sha512", "sha384"} {
		if value, ok := digests[algorithm]; ok {
			sum, _ := hex.DecodeString(value)
			return func(body io.ReadCloser) io.ReadCloser {
				h := sha512.New()
				if algorithm == "sha384" {
					h = sha512.New384()
				}
				return &verifier{ReadCloser: body, algorithm: algorithm, want: sum, sum: h}
			}, nil
		}
	}
	return nil, nil
}

// verifier fails the read that reaches EOF unless the bytes hash to the
// published digest. A reader that stops early is not verified; the host always
// reads blobs to the end to compute its own digest.
type verifier struct {
	io.ReadCloser
	algorithm string
	want      []byte
	sum       hash.Hash
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.ReadCloser.Read(p)
	v.sum.Write(p[:n])
	if err == io.EOF && !bytes.Equal(v.sum.Sum(nil), v.want) {
		return n, fmt.Errorf("%s download does not match its published %s digest", Identity, label(v.algorithm))
	}
	return n, err
}

// label spells an algorithm key the way the registries do: sha512 as SHA-512.
func label(algorithm string) string {
	return "SHA-" + strings.TrimPrefix(algorithm, "sha")
}

// blobName keeps a manifest file name free of path separators, because
// consumers write blob names to disk.
func blobName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
}
