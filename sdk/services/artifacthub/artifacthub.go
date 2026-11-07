// Package artifacthub is a client for the Artifact Hub catalog API, shared by
// sources that archive packages Artifact Hub indexes (Helm charts today;
// operators, Falco rules, Gatekeeper policies and the other kinds share the
// same API).
//
// Artifact Hub does not host bytes. It indexes repositories, so a package's
// content URL points at whatever host the repository uses: a chart repo's own
// site, GitHub releases, or an oci:// registry. The client reports that URL
// and the SHA-256 digest Artifact Hub recorded; deciding which hosts are
// acceptable is the source's job.
package artifacthub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/retry"
)

const (
	// DefaultBase is the public API.
	DefaultBase = "https://artifacthub.io/api/v1/"

	// KindHelm is the package kind for Helm charts.
	KindHelm           = 0
	KindContainerImage = 12

	// PageSize is the largest page the search endpoint accepts.
	PageSize    = 60
	maxMetadata = 32 << 20
)

// ErrNotFound reports a package or version Artifact Hub does not have.
var ErrNotFound = errors.New("artifacthub: not found")

// Client talks to Artifact Hub. Empty Base means the public API. It is safe
// for concurrent use.
type Client struct {
	HTTP     *http.Client
	Base     string // API base URL; tests point it at httptest
	Attempts int    // HTTP attempts per request; 0 means 4

	// Interval and SearchInterval are the minimum delays between package
	// requests and between search requests. Zero means no spacing. Artifact
	// Hub's CDN answers a sustained search rate above about one request per
	// second with a bare 429 and no Retry-After, so a bulk walk should space
	// searches at least two seconds apart.
	Interval       time.Duration
	SearchInterval time.Duration

	packages, searches spacer
}

// spacer enforces a minimum delay between the requests that share it.
type spacer struct {
	mu   sync.Mutex
	next time.Time
}

func (p *spacer) wait(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		return ctx.Err()
	}
	p.mu.Lock()
	at := p.next
	if now := time.Now(); at.Before(now) {
		at = now
	}
	p.next = at.Add(every)
	p.mu.Unlock()
	return retry.Wait(ctx, time.Until(at))
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/") + "/"
	}
	return DefaultBase
}

// ValidSegment accepts repository and package names as they appear in URL
// paths: lowercase letters, digits, dot, underscore and hyphen. It exists so
// a name from an inventory cannot add path segments or query parameters.
func ValidSegment(s string) error {
	if s == "" || len(s) > 253 || s == "." || s == ".." {
		return fmt.Errorf("invalid artifacthub name %q", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return fmt.Errorf("invalid artifacthub name %q", s)
		}
	}
	return nil
}

// ValidVersion accepts semver-shaped chart versions.
func ValidVersion(v string) error {
	if v == "" || len(v) > 256 {
		return fmt.Errorf("invalid artifacthub version %q", v)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '+', r == '_':
		default:
			return fmt.Errorf("invalid artifacthub version %q", v)
		}
	}
	return nil
}

// Repository is the repository a package is published from.
type Repository struct {
	ID                      string `json:"repository_id"`
	Name                    string `json:"name"`
	URL                     string `json:"url"`
	Kind                    int    `json:"kind"`
	Official                bool   `json:"official"`
	VerifiedPublisher       bool   `json:"verified_publisher"`
	CNCF                    bool   `json:"cncf"`
	Organization            string `json:"organization_name"`
	OrganizationDisplayName string `json:"organization_display_name"`
	User                    string `json:"user_alias"`
}

// Link is a labelled URL a package lists, such as its chart source.
type Link struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

// Stats are the counters Artifact Hub reports for a package.
type Stats struct {
	Subscriptions int64 `json:"subscriptions"`
	Webhooks      int64 `json:"webhooks"`
}

// Package is a search result or a package document. Fields only the full
// document carries (ContentURL, Digest, AvailableVersions, Stats) are empty
// in search results.
type Package struct {
	ID                string       `json:"package_id"`
	Name              string       `json:"name"`
	NormalizedName    string       `json:"normalized_name"`
	Description       string       `json:"description"`
	Version           string       `json:"version"`
	AppVersion        string       `json:"app_version"`
	License           string       `json:"license"`
	Stars             int64        `json:"stars"`
	Deprecated        bool         `json:"deprecated"`
	Signed            bool         `json:"signed"`
	Prerelease        bool         `json:"prerelease"`
	Timestamp         int64        `json:"ts"`
	ContentURL        string       `json:"content_url"`
	Digest            string       `json:"digest"`
	HomeURL           string       `json:"home_url"`
	Category          string       `json:"category_display_name"`
	Keywords          []string     `json:"keywords"`
	Links             []Link       `json:"links"`
	Stats             Stats        `json:"stats"`
	AvailableVersions []VersionRef `json:"available_versions"`
	Repository        Repository   `json:"repository"`
}

// Updated is when the indexed version was published, from the repository's
// own timestamp.
func (p Package) Updated() time.Time {
	if p.Timestamp <= 0 {
		return time.Time{}
	}
	return time.Unix(p.Timestamp, 0).UTC()
}

// VersionRef is one entry of a package's version list.
type VersionRef struct {
	Version    string `json:"version"`
	AppVersion string `json:"app_version"`
	Prerelease bool   `json:"prerelease"`
	Timestamp  int64  `json:"ts"`
}

// Published is the version's release time, zero when unknown.
func (v VersionRef) Published() time.Time {
	if v.Timestamp <= 0 {
		return time.Time{}
	}
	return time.Unix(v.Timestamp, 0).UTC()
}

// Filter narrows a search. The zero value is Artifact Hub's default view,
// which hides deprecated packages.
type Filter struct {
	Repos      []string // repository names; a package matches any of them
	Text       string   // full-text query
	Deprecated bool     // include deprecated packages as well
}

// Search streams every package of one kind and returns Artifact Hub's reported
// total. Results can repeat or be missed when the index shifts mid-walk, so
// callers dedupe by Package.ID. A walk that ends before the reported total is
// an error rather than a silent truncation.
func (c *Client) Search(ctx context.Context, kind int, emit func(Package) error) (total int, err error) {
	return c.SearchFiltered(ctx, kind, Filter{}, emit)
}

// SearchFiltered is Search restricted by a Filter. Repository names are
// checked, because they become query parameters.
func (c *Client) SearchFiltered(ctx context.Context, kind int, filter Filter, emit func(Package) error) (total int, err error) {
	for _, repo := range filter.Repos {
		if err := ValidSegment(repo); err != nil {
			return 0, retry.Permanent(err)
		}
	}
	for offset := 0; ; offset += PageSize {
		q := url.Values{
			"kind":   {strconv.Itoa(kind)},
			"limit":  {strconv.Itoa(PageSize)},
			"offset": {strconv.Itoa(offset)},
			"sort":   {"last_updated"},
		}
		if len(filter.Repos) > 0 {
			q["repo"] = filter.Repos
		}
		if filter.Text != "" {
			q.Set("ts_query_web", filter.Text)
		}
		if filter.Deprecated {
			q.Set("deprecated", "true")
		}
		var page struct {
			Packages []Package `json:"packages"`
		}
		header, err := c.query(ctx, "packages/search?"+q.Encode(), &page)
		if err != nil {
			return 0, fmt.Errorf("artifacthub search: %w", err)
		}
		total, err = strconv.Atoi(header.Get("Pagination-Total-Count"))
		if err != nil || total < 0 {
			return 0, retry.Permanent(fmt.Errorf("artifacthub search: missing or invalid total count"))
		}
		if len(page.Packages) == 0 && offset < total {
			return total, retry.Permanent(fmt.Errorf("artifacthub search ended early: %d of %d results", offset, total))
		}
		for _, p := range page.Packages {
			if err := emit(p); err != nil {
				return total, err
			}
		}
		if offset+PageSize >= total {
			return total, nil
		}
	}
}

// Package fetches the current document of a Helm package: its version list,
// content URL and digest.
func (c *Client) Package(ctx context.Context, repo, name string) (*Package, error) {
	return c.helm(ctx, repo, name, "")
}

// PackageVersion fetches one version's document, which is the only place the
// content URL and digest of an older version appear.
func (c *Client) PackageVersion(ctx context.Context, repo, name, version string) (*Package, error) {
	if err := ValidVersion(version); err != nil {
		return nil, retry.Permanent(err)
	}
	return c.helm(ctx, repo, name, version)
}

func (c *Client) helm(ctx context.Context, repo, name, version string) (*Package, error) {
	for _, s := range []string{repo, name} {
		if err := ValidSegment(s); err != nil {
			return nil, retry.Permanent(err)
		}
	}
	path := "packages/helm/" + repo + "/" + name
	if version != "" {
		path += "/" + url.PathEscape(version)
	}
	var doc Package
	if _, err := c.query(ctx, path, &doc); err != nil {
		return nil, fmt.Errorf("artifacthub package %s/%s: %w", repo, name, err)
	}
	if doc.NormalizedName != name || doc.Repository.Name != repo {
		return nil, retry.Permanent(fmt.Errorf("artifacthub package %s/%s: unexpected document for %s/%s", repo, name, doc.Repository.Name, doc.NormalizedName))
	}
	return &doc, nil
}

// query fetches one bounded JSON document. A 404 is ErrNotFound; decode
// failures are permanent because retrying the same bytes cannot help.
func (c *Client) query(ctx context.Context, path string, out any) (http.Header, error) {
	var err error
	if strings.HasPrefix(path, "packages/search?") {
		err = c.searches.wait(ctx, c.SearchInterval)
	} else {
		err = c.packages.wait(ctx, c.Interval)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, c.HTTP, c.base()+path, c.Attempts)
	if err != nil {
		var status *httpx.StatusError
		if errors.As(err, &status) && status.Code == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadata+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadata {
		return nil, retry.Permanent(fmt.Errorf("metadata exceeds 32 MiB"))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, retry.Permanent(fmt.Errorf("decode %s: %w", path, err))
	}
	return response.Header, nil
}
