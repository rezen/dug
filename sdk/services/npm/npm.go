// Package npm is a client for the public npm registry, shared by sources that
// archive packages published there (n8n community nodes today; Node-RED,
// Homebridge and a general npm source share the same API).
//
// It covers the three upstream APIs those sources need: package documents
// (packuments) with version history and integrity hashes, the search endpoint,
// and download counts. It owns the registry's quirks so each source does not
// rediscover them: the search endpoint serves only its first 5,000 results,
// bulk download counts reject scoped names, and integrity hashes are SHA-512
// (or SHA-1 for old versions) rather than SHA-256.
package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/retry"
)

const (
	// DefaultRegistry serves package documents, search and tarballs.
	DefaultRegistry = "https://registry.npmjs.org/"
	// DefaultDownloads serves download counts.
	DefaultDownloads = "https://api.npmjs.org/"
	// DefaultHost is the only host npm serves tarballs from.
	DefaultHost = "registry.npmjs.org"

	// SearchWindow is how many results the search endpoint will page through.
	// Past it, offsets return the top of the ranking again instead of an error,
	// so a walk must stop here and report that the catalog is larger.
	SearchWindow = 5000

	searchPageSize = 250
	maxMetadata    = 32 << 20
	// BulkDownloadNames is the most names one bulk download-count request takes.
	BulkDownloadNames = 128
)

// ErrNotFound reports a package, version or count the registry does not have.
var ErrNotFound = errors.New("npm: not found")

// Client talks to the npm registry. The zero value is not usable without
// HTTP; empty endpoints mean the public registry. It is safe for concurrent
// use.
type Client struct {
	HTTP      *http.Client
	Registry  string // base URL with trailing slash; tests point it at httptest
	Downloads string // download-count API base
	Attempts  int    // HTTP attempts per request; 0 means 4
}

func (c *Client) registry() string {
	if c.Registry != "" {
		return strings.TrimRight(c.Registry, "/") + "/"
	}
	return DefaultRegistry
}

func (c *Client) downloads() string {
	if c.Downloads != "" {
		return strings.TrimRight(c.Downloads, "/") + "/"
	}
	return DefaultDownloads
}

// ValidName accepts npm package names, scoped or not. Legacy packages may use
// uppercase letters, so the check is about safety in URLs and file names, not
// the current publishing rules.
func ValidName(name string) error {
	invalid := fmt.Errorf("invalid npm package name %q", name)
	if name == "" || len(name) > 214 {
		return invalid
	}
	base := name
	if strings.HasPrefix(name, "@") {
		scope, rest, ok := strings.Cut(name[1:], "/")
		if !ok || !validSegment(scope) {
			return invalid
		}
		base = rest
	}
	if !validSegment(base) || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
		return invalid
	}
	return nil
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_', r == '~':
		default:
			return false
		}
	}
	return true
}

// ValidVersion accepts semver-shaped strings: letters, digits, dots, plus and
// hyphen. npm rejects anything else at publish time.
func ValidVersion(version string) error {
	if version == "" || len(version) > 256 {
		return fmt.Errorf("invalid npm version %q", version)
	}
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '+':
		default:
			return fmt.Errorf("invalid npm version %q", version)
		}
	}
	return nil
}

// TarballURL is where the registry serves a version's tarball:
// BASE/NAME/-/BASENAME-VERSION.tgz, where BASENAME drops the scope. Sources
// derive the URL from a validated name and version instead of trusting one
// persisted in an inventory.
func TarballURL(registryBase, name, version string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	if err := ValidVersion(version); err != nil {
		return "", err
	}
	if registryBase == "" {
		registryBase = DefaultRegistry
	}
	return strings.TrimRight(registryBase, "/") + "/" + name + "/-/" + TarballName(name, version), nil
}

// TarballName is the tarball's file name, unique per package version and free
// of path separators: BASENAME-VERSION.tgz.
func TarballName(name, version string) string {
	if _, base, scoped := strings.Cut(name, "/"); scoped {
		name = base
	}
	return name + "-" + version + ".tgz"
}

// Person is a maintainer or publisher as npm describes them.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// UnmarshalJSON tolerates the string form "Name <email>" old packages use.
func (p *Person) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		name, email, _ := strings.Cut(s, "<")
		p.Name, p.Email = strings.TrimSpace(name), strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(email), ">"))
		return nil
	}
	type plain Person
	var v plain
	if json.Unmarshal(data, &v) != nil {
		*p = Person{}
		return nil
	}
	*p = Person(v)
	return nil
}

// Repository is the repository field, which npm allows as a string or object.
// Malformed values decode to the zero value: the field is the publisher's
// unverified claim and never worth failing a listing over.
type Repository struct {
	URL       string
	Directory string
}

func (r *Repository) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*r = Repository{URL: s}
		return nil
	}
	var v struct {
		URL       string `json:"url"`
		Directory string `json:"directory"`
	}
	if json.Unmarshal(data, &v) == nil {
		*r = Repository{URL: v.URL, Directory: v.Directory}
		return nil
	}
	*r = Repository{}
	return nil
}

// Text decodes a field that is a string in practice but has been published as
// other JSON types (license objects, deprecated: false). Non-strings are empty.
type Text string

func (t *Text) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*t = Text(s)
		return nil
	}
	var v struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &v) == nil {
		*t = Text(v.Type)
		return nil
	}
	*t = ""
	return nil
}

// Dist is a version's distribution record.
type Dist struct {
	Tarball      string `json:"tarball"`
	Shasum       string `json:"shasum"`
	Integrity    string `json:"integrity"`
	Attestations *struct {
		Provenance *struct {
			PredicateType string `json:"predicateType"`
		} `json:"provenance"`
	} `json:"attestations"`
}

// HasProvenance reports whether the version carries a build provenance
// attestation.
func (d Dist) HasProvenance() bool {
	return d.Attestations != nil && d.Attestations.Provenance != nil
}

// Hash returns the version's content hash: the integrity field, or for old
// versions the SHA-1 shasum.
func (d Dist) Hash() (Integrity, error) {
	if d.Integrity != "" {
		return ParseIntegrity(d.Integrity)
	}
	return FromShasum(d.Shasum)
}

// Version is one published version of a package.
type Version struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Dist       Dist              `json:"dist"`
	NpmUser    *Person           `json:"_npmUser"`
	Scripts    map[string]string `json:"scripts"`
	Deprecated Text              `json:"deprecated"`
	License    Text              `json:"license"`
	Repository Repository        `json:"repository"`
	// Raw is the verbatim version document from the packument.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the verbatim version document next to the decoded fields.
func (v *Version) UnmarshalJSON(data []byte) error {
	type plain Version
	if err := json.Unmarshal(data, (*plain)(v)); err != nil {
		return err
	}
	v.Raw = slices.Clone(data)
	return nil
}

// HasInstallScript reports whether npm runs code at install time: a
// preinstall, install or postinstall script.
func (v Version) HasInstallScript() bool {
	for _, name := range []string{"preinstall", "install", "postinstall"} {
		if v.Scripts[name] != "" {
			return true
		}
	}
	return false
}

// Times maps "created", "modified" and each version to its RFC 3339 time.
// Entries that are not strings, like the "unpublished" record of a removed
// package, are dropped.
type Times map[string]string

func (t *Times) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = make(Times, len(raw))
	for key, value := range raw {
		var s string
		if json.Unmarshal(value, &s) == nil {
			(*t)[key] = s
		}
	}
	return nil
}

// Packument is a package document: every version plus publish times.
type Packument struct {
	Name        string             `json:"name"`
	DistTags    map[string]string  `json:"dist-tags"`
	Versions    map[string]Version `json:"versions"`
	Time        Times              `json:"time"`
	Keywords    []string           `json:"keywords"`
	Maintainers []Person           `json:"maintainers"`
	License     Text               `json:"license"`
	Repository  Repository         `json:"repository"`
}

// Modified is when the package document last changed.
func (p *Packument) Modified() (time.Time, bool) { return p.timeOf("modified") }

// Published is when a version was published.
func (p *Packument) Published(version string) (time.Time, bool) {
	if version == "created" || version == "modified" || version == "unpublished" {
		return time.Time{}, false
	}
	return p.timeOf(version)
}

func (p *Packument) timeOf(key string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, p.Time[key])
	return t, err == nil
}

// Current is the version the registry serves by default: the latest dist-tag.
// A package without one falls back to its most recently published version.
func (p *Packument) Current() (string, bool) {
	if v := p.DistTags["latest"]; v != "" {
		if _, ok := p.Versions[v]; ok {
			return v, true
		}
	}
	ordered := p.OrderedVersions()
	if len(ordered) == 0 {
		return "", false
	}
	return ordered[0], true
}

// OrderedVersions lists every version newest first by publish time, ties
// broken by version string so the order is deterministic. Versions without a
// publish time sort last. The current version is not moved; callers that want
// it first use Current.
func (p *Packument) OrderedVersions() []string {
	versions := make([]string, 0, len(p.Versions))
	for v := range p.Versions {
		versions = append(versions, v)
	}
	sortVersions(versions, func(v string) time.Time { t, _ := p.Published(v); return t })
	return versions
}

// Packument fetches a package document. A missing or unpublished package
// reports ErrNotFound.
func (c *Client) Packument(ctx context.Context, name string) (*Packument, error) {
	if err := ValidName(name); err != nil {
		return nil, retry.Permanent(err)
	}
	var doc Packument
	if err := c.query(ctx, c.registry()+escapeName(name), &doc); err != nil {
		return nil, fmt.Errorf("npm package %s: %w", name, err)
	}
	if doc.Name != name {
		return nil, retry.Permanent(fmt.Errorf("npm package %s: unexpected document for %q", name, doc.Name))
	}
	// An unpublished package keeps its document, with publish times only.
	if len(doc.Versions) == 0 {
		return nil, fmt.Errorf("npm package %s has no published versions: %w", name, ErrNotFound)
	}
	return &doc, nil
}

// escapeName encodes a package name as one path segment; the registry accepts
// the scope separator as %2F.
func escapeName(name string) string { return url.PathEscape(name) }

// SearchObject is one search result.
type SearchObject struct {
	Package struct {
		Name     string    `json:"name"`
		Version  string    `json:"version"`
		Keywords []string  `json:"keywords"`
		Date     time.Time `json:"date"`
	} `json:"package"`
	Downloads struct {
		Weekly  int64 `json:"weekly"`
		Monthly int64 `json:"monthly"`
	} `json:"downloads"`
	Updated time.Time `json:"updated"`
}

// Search streams the first SearchWindow results for text, such as
// "keywords:n8n-community-node-package". It returns the registry's reported
// total; a total above SearchWindow means the walk saw only part of the
// catalog and the caller must decide what that means. Results repeat across
// pages when the index shifts mid-walk, so callers dedupe by name.
func (c *Client) Search(ctx context.Context, text string, emit func(SearchObject) error) (total int, err error) {
	for from := 0; ; from += searchPageSize {
		if from > 0 {
			// Ten rapid pages trip the registry's rate limit; pace the walk.
			if err := retry.Wait(ctx, 500*time.Millisecond); err != nil {
				return total, err
			}
		}
		q := url.Values{"text": {text}, "size": {strconv.Itoa(searchPageSize)}, "from": {strconv.Itoa(from)}}
		var page struct {
			Total   int            `json:"total"`
			Objects []SearchObject `json:"objects"`
		}
		if err := c.query(ctx, c.registry()+"-/v1/search?"+q.Encode(), &page); err != nil {
			return 0, fmt.Errorf("npm search: %w", err)
		}
		if page.Total < 0 {
			return 0, retry.Permanent(fmt.Errorf("npm search: negative total"))
		}
		total = page.Total
		reachable := min(total, SearchWindow)
		if len(page.Objects) == 0 && from < reachable {
			return total, retry.Permanent(fmt.Errorf("npm search ended early: %d of %d results", from, reachable))
		}
		for _, object := range page.Objects {
			if err := emit(object); err != nil {
				return total, err
			}
		}
		if from+searchPageSize >= reachable {
			return total, nil
		}
	}
}

// DownloadCounts returns each name's downloads over a trailing window:
// "last-day", "last-week" or "last-month". Names the registry has no count for
// are absent from the result. Unscoped names go through the bulk endpoint in
// chunks; scoped names are not supported in bulk and cost one request each.
func (c *Client) DownloadCounts(ctx context.Context, period string, names []string) (map[string]int64, error) {
	switch period {
	case "last-day", "last-week", "last-month":
	default:
		return nil, fmt.Errorf("npm download period %q is not last-day, last-week or last-month", period)
	}
	counts := make(map[string]int64, len(names))
	var bulk []string
	flush := func() error {
		if len(bulk) == 0 {
			return nil
		}
		defer func() { bulk = bulk[:0] }()
		if len(bulk) == 1 {
			return c.downloadOne(ctx, period, bulk[0], counts)
		}
		var doc map[string]*struct {
			Downloads int64 `json:"downloads"`
		}
		if err := c.query(ctx, c.downloads()+"downloads/point/"+period+"/"+strings.Join(bulk, ","), &doc); err != nil {
			return fmt.Errorf("npm downloads: %w", err)
		}
		for _, name := range bulk {
			if entry := doc[name]; entry != nil {
				if entry.Downloads < 0 {
					return retry.Permanent(fmt.Errorf("npm downloads: negative count for %s", name))
				}
				counts[name] = entry.Downloads
			}
		}
		return nil
	}
	for _, name := range names {
		if err := ValidName(name); err != nil {
			return nil, retry.Permanent(err)
		}
		if strings.HasPrefix(name, "@") {
			if err := c.downloadOne(ctx, period, name, counts); err != nil {
				return nil, err
			}
			continue
		}
		bulk = append(bulk, name)
		if len(bulk) == BulkDownloadNames {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (c *Client) downloadOne(ctx context.Context, period, name string, counts map[string]int64) error {
	var doc struct {
		Downloads int64 `json:"downloads"`
	}
	err := c.query(ctx, c.downloads()+"downloads/point/"+period+"/"+escapeName(name), &doc)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("npm downloads %s: %w", name, err)
	}
	if doc.Downloads < 0 {
		return retry.Permanent(fmt.Errorf("npm downloads: negative count for %s", name))
	}
	counts[name] = doc.Downloads
	return nil
}

// query fetches one bounded JSON document. A 404 is ErrNotFound; decode
// failures are permanent because retrying the same bytes cannot help.
func (c *Client) query(ctx context.Context, raw string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, c.HTTP, raw, c.Attempts)
	if err != nil {
		var status *httpx.StatusError
		if errors.As(err, &status) && status.Code == http.StatusNotFound {
			return ErrNotFound
		}
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadata+1))
	if err != nil {
		return err
	}
	if len(data) > maxMetadata {
		return retry.Permanent(fmt.Errorf("metadata exceeds 32 MiB"))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return retry.Permanent(fmt.Errorf("decode %s: %w", raw, err))
	}
	return nil
}
