// Package helm archives Helm chart packages indexed by Artifact Hub.
package helm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services/artifacthub"
)

// Identity is the registry name the host selects: `dug list helm`.
const Identity = "helm"

// Namespace records where artifacts came from; manifests persist it. Artifact
// Hub is the index; the chart bytes come from each repository's own host.
const Namespace = "artifacthub.io"

// ReleaseHosts are accepted as chart download hosts for every repository, in
// addition to the repository's own host. Many repositories publish an index on
// GitHub Pages and attach the chart archives to GitHub releases.
var ReleaseHosts = []string{"github.com", "codeload.github.com", "raw.githubusercontent.com"}

const (
	maxMetadataBytes = 32 << 20
	maxPageSize      = 60 // Artifact Hub rejects limit above 60.
	defaultInterval  = 400 * time.Millisecond
	// Artifact Hub's CDN answers more than about one search request per second
	// with a bare 429 and no Retry-After; two seconds ran 70 requests cleanly.
	defaultSearchInterval = 2 * time.Second
	maxCatalogPasses      = 3
)

type Source struct {
	Client   *http.Client
	Endpoint string // Artifact Hub API base override for tests; empty means the public API
	Attempts int    // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts, when non-nil, replaces the per-repository download host
	// rule with a fixed allowlist; tests point it at httptest.
	DownloadHosts []string
	// Interval is the minimum delay between package requests and
	// SearchInterval between search requests; 0 means 400ms and 2s.
	Interval       time.Duration
	SearchInterval time.Duration

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Charts       []string // REPOSITORY/CHART names; exclusive with Repos and Search
	Repos        []string // Artifact Hub repository names to restrict the walk to
	Search       string
	VersionDepth string // empty means the current release only
}

var _ registry.Registry = (*Source)(nil)

var (
	errStop = errors.New("version depth reached")
	// errUnarchivable marks a version whose descriptor cannot be pinned: no
	// digest, an OCI location, or a download host the repository does not own.
	errUnarchivable = errors.New("cannot be archived")
	// errOCIChart marks a chart whose archives live in an OCI registry, which
	// the OCI source covers. A walk skips the whole chart and counts it.
	errOCIChart = fmt.Errorf("%w: published to an OCI registry", errUnarchivable)
)

func (s *Source) attempts() int {
	if s.Attempts > 0 {
		return s.Attempts
	}
	return 4
}

// hub builds the Artifact Hub client for one discovery run. Its request
// spacing is per client, so a run builds it once.
func (s *Source) hub() *artifacthub.Client {
	c := &artifacthub.Client{HTTP: s.Client, Base: s.Endpoint, Attempts: s.Attempts, Interval: s.Interval, SearchInterval: s.SearchInterval}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.SearchInterval <= 0 {
		c.SearchInterval = defaultSearchInterval
	}
	return c
}

// validPart checks a repository or chart name, which become URL path
// segments and file names.
func validPart(kind, value string) error {
	if artifacthub.ValidSegment(value) != nil {
		return fmt.Errorf("invalid Helm %s name %q", kind, value)
	}
	return nil
}

// splitChart parses REPOSITORY/CHART.
func splitChart(name string) (repo, chart string, err error) {
	repo, chart, ok := strings.Cut(name, "/")
	if !ok {
		return "", "", fmt.Errorf("invalid Helm chart %q: expected REPOSITORY/CHART", name)
	}
	if err := validPart("repository", repo); err != nil {
		return "", "", err
	}
	if err := validPart("chart", chart); err != nil {
		return "", "", err
	}
	return repo, chart, nil
}

func validVersion(version string) error {
	if artifacthub.ValidVersion(version) != nil {
		return fmt.Errorf("invalid Helm chart version %q", version)
	}
	return nil
}

// Discover emits one reference per selected chart version: the explicit
// charts, or every chart Artifact Hub lists. Every reference is self-resolved
// before emission so a listing never persists something stage two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	if len(s.Charts) > 0 && (len(s.Repos) > 0 || s.Search != "") {
		return fmt.Errorf("use charts, or repos and search, not both")
	}
	hub := s.hub()
	seen := make(map[string]bool)
	if len(s.Charts) > 0 {
		for _, name := range s.Charts {
			repo, chart, err := splitChart(name)
			if err != nil {
				return err
			}
			doc, err := hub.Package(ctx, repo, chart)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if seen[doc.ID] {
				continue
			}
			seen[doc.ID] = true
			if err := s.emitVersions(ctx, hub, doc, 0, true, depth, emit); err != nil {
				return err
			}
		}
		return nil
	}
	entries, err := s.catalog(ctx, hub)
	if err != nil {
		return err
	}
	ociSkipped := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(strings.ToLower(e.Repository.URL), "oci:") {
			ociSkipped++
			continue
		}
		repo, chart := e.Repository.Name, e.NormalizedName
		if err := validPart("repository", repo); err != nil {
			return err
		}
		if err := validPart("chart", chart); err != nil {
			return err
		}
		doc, err := hub.Package(ctx, repo, chart)
		if errors.Is(err, artifacthub.ErrNotFound) {
			// Removed between the search and this lookup.
			slog.Warn("helm chart disappeared during the walk", "chart", repo+"/"+chart)
			continue
		}
		if err != nil {
			return err
		}
		if seen[doc.ID] {
			continue
		}
		seen[doc.ID] = true
		if err := s.emitVersions(ctx, hub, doc, e.Stars, false, depth, emit); err != nil {
			if errors.Is(err, errOCIChart) {
				ociSkipped++
				continue
			}
			return err
		}
	}
	if ociSkipped > 0 {
		slog.Warn("helm skipped charts published to OCI registries", "charts", ociSkipped)
	}
	return nil
}

// catalog collects every chart in the search, including deprecated ones, which
// Artifact Hub hides by default but which are still archives worth keeping.
// Search offsets shift while the catalog changes, so the whole index is
// gathered first (about 310 requests) and ordered by last update, which moves
// changed charts toward the front where a walk has already been; the walk is
// repeated when it comes up short of the reported total. A remaining gap is
// small churn, never a silent truncation: a large one is an error.
func (s *Source) catalog(ctx context.Context, hub *artifacthub.Client) ([]artifacthub.Package, error) {
	var entries []artifacthub.Package
	seen := make(map[string]bool)
	filter := artifacthub.Filter{Repos: s.Repos, Text: s.Search, Deprecated: true}
	total := 0
	for pass := 0; pass < maxCatalogPasses; pass++ {
		var err error
		total, err = hub.SearchFiltered(ctx, artifacthub.KindHelm, filter, func(p artifacthub.Package) error {
			if p.ID == "" {
				return retry.Permanent(fmt.Errorf("helm search result without a package ID"))
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				entries = append(entries, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(entries) >= total {
			return entries, nil
		}
	}
	if gap := total - len(entries); gap > max(5, total/200) {
		return nil, fmt.Errorf("helm search ended early: got %d of %d charts", len(entries), total)
	}
	slog.Warn("helm search missed charts that changed during the walk", "got", len(entries), "total", total)
	return entries, nil
}

// emitVersions emits the current version first, then older versions while the
// depth allows. Explicitly requested charts (strict) fail when their current
// version cannot be archived; during a catalog walk an unarchivable version is
// skipped with a warning. stars is the search result's count, 0 when unknown.
func (s *Source) emitVersions(ctx context.Context, hub *artifacthub.Client, doc *artifacthub.Package, stars int64, strict bool, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	repo, chart := doc.Repository.Name, doc.NormalizedName
	// Item-wide facts are attached to every version, including old ones; they
	// are a listing-time snapshot, not per-version history. Artifact Hub
	// publishes no download or install counts.
	facts := &registry.Facts{}
	if updated := doc.Updated(); !updated.IsZero() {
		facts.UpdatedAt = registry.Time(updated)
	}
	for _, l := range doc.Links {
		if !strings.Contains(strings.ToLower(l.Name), "source") {
			continue
		}
		if repoURL, ok := registry.NormalizeRepositoryURL(l.URL); ok {
			facts.Repository = &registry.Repository{URL: repoURL, Source: registry.RepositoryDeclared}
			break
		}
	}
	metadata := map[string]string{
		"repository":         repo,
		"official":           strconv.FormatBool(doc.Repository.Official),
		"verified_publisher": strconv.FormatBool(doc.Repository.VerifiedPublisher),
		"cncf":               strconv.FormatBool(doc.Repository.CNCF),
	}
	// Package documents carry no star count, and a search result without one
	// reads as zero, which sorts the same as missing.
	if stars > 0 {
		metadata["stars"] = strconv.FormatInt(stars, 10)
	}
	if doc.Category != "" {
		metadata["category"] = doc.Category
	}
	tags := slices.Clone(doc.Keywords)
	if doc.Category != "" {
		tags = append(tags, strings.ReplaceAll(strings.ToLower(doc.Category), " ", "-"))
	}
	tags = registry.NormalizeTags(tags)
	var author *registry.Author
	switch r := doc.Repository; {
	case r.Organization != "":
		author = &registry.Author{Name: r.OrganizationDisplayName, Username: r.Organization}
	case r.User != "":
		author = &registry.Author{Name: r.User, Username: r.User}
	}

	count := 0
	seenVersions := make(map[string]bool)
	add := func(d *artifacthub.Package, current bool) error {
		if err := validVersion(d.Version); err != nil {
			return err
		}
		if seenVersions[d.Version] {
			return nil
		}
		seenVersions[d.Version] = true
		released := d.Updated()
		if !depth.Since.IsZero() {
			if released.IsZero() {
				return fmt.Errorf("%s/%s@%s: missing release date for a date cutoff", repo, chart, d.Version)
			}
			if released.Before(depth.Since) {
				return nil
			}
		}
		attributes, err := s.attributes(doc.ID, d)
		if err != nil {
			if errors.Is(err, errOCIChart) && !strict {
				return err
			}
			if strict && current {
				return fmt.Errorf("%s/%s@%s: %w", repo, chart, d.Version, err)
			}
			slog.Warn("helm skipping version", "chart", repo+"/"+chart, "version", d.Version, "reason", err)
			return nil
		}
		versionMetadata := maps.Clone(metadata)
		versionMetadata["version_rank"] = strconv.Itoa(count + 1)
		versionMetadata["prerelease"] = strconv.FormatBool(d.Prerelease)
		versionMetadata["deprecated"] = strconv.FormatBool(d.Deprecated)
		versionMetadata["signed"] = strconv.FormatBool(d.Signed)
		if d.AppVersion != "" {
			versionMetadata["app_version"] = d.AppVersion
		}
		if d.License != "" {
			versionMetadata["license"] = d.License
		}
		versionFacts := facts.Clone()
		if !released.IsZero() {
			versionFacts.ReleasedAt = registry.Time(released)
			versionMetadata["release_date_source"] = "artifacthub-ts"
		}
		ref := registry.Reference{
			Registry:   Identity,
			Namespace:  Namespace,
			Name:       repo + "/" + chart,
			Version:    d.Version,
			Attributes: attributes,
			Metadata:   versionMetadata,
			Facts:      versionFacts,
			Tags:       slices.Clone(tags),
			Author:     author.Clone(),
		}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return fmt.Errorf("%s/%s@%s: %w", repo, chart, d.Version, err)
		}
		if err := emit(ref); err != nil {
			return err
		}
		count++
		if depth.Max > 0 && count >= depth.Max {
			return errStop
		}
		return nil
	}
	if err := add(doc, true); err != nil {
		if errors.Is(err, errStop) {
			return nil
		}
		return err
	}
	if depth.Max == 1 {
		return nil
	}
	for _, v := range doc.AvailableVersions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if v.Version == doc.Version || seenVersions[v.Version] {
			continue
		}
		if validVersion(v.Version) != nil {
			return retry.Permanent(fmt.Errorf("helm %s/%s: invalid version %q in version list", repo, chart, v.Version))
		}
		// The version list carries release times, so a date cutoff skips
		// old versions without a request.
		if published := v.Published(); !depth.Since.IsZero() && !published.IsZero() && published.Before(depth.Since) {
			continue
		}
		d, err := hub.PackageVersion(ctx, repo, chart, v.Version)
		if errors.Is(err, artifacthub.ErrNotFound) {
			slog.Warn("helm version disappeared", "chart", repo+"/"+chart, "version", v.Version)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s/%s@%s: %w", repo, chart, v.Version, err)
		}
		if d.Version != v.Version || d.ID != doc.ID {
			return retry.Permanent(fmt.Errorf("helm %s/%s@%s: unexpected version document", repo, chart, v.Version))
		}
		if err := add(d, false); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// attributes pins the descriptor Resolve needs. The digest is the SHA-256 the
// repository's index.yaml publishes for the archive; Artifact Hub passes it on.
func (s *Source) attributes(packageID string, d *artifacthub.Package) (map[string]string, error) {
	if strings.HasPrefix(strings.ToLower(d.ContentURL), "oci:") {
		return nil, errOCIChart
	}
	digest, err := parseDigest(d.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnarchivable, err)
	}
	if err := s.checkDownload(d.Repository.URL, d.ContentURL); err != nil {
		return nil, fmt.Errorf("%w: %v", errUnarchivable, err)
	}
	return map[string]string{
		"package_id":   packageID,
		"repo_url":     d.Repository.URL,
		"download_url": d.ContentURL,
		"sha256":       digest,
	}, nil
}

// checkDownload applies the download host rule. A chart archive may only be
// fetched from the host of the repository it was indexed under, or from a
// GitHub release host, over HTTPS to a public DNS name. Inventories live in S3
// and could be edited, so this runs again in Resolve. The persisted repository
// URL is part of the same descriptor, so the rule narrows what a tampered
// inventory can reach to named public hosts; it cannot make an arbitrary host
// impossible. Run the plugin without access to internal networks.
func (s *Source) checkDownload(repoURL, downloadURL string) error {
	if s.DownloadHosts != nil {
		return httpx.ValidateURLHost(downloadURL, s.DownloadHosts)
	}
	if err := httpx.ValidateURL(repoURL); err != nil {
		return fmt.Errorf("repository URL: %w", err)
	}
	if err := publicHTTPS(downloadURL); err != nil {
		return err
	}
	repo, _ := url.Parse(repoURL)
	download, _ := url.Parse(downloadURL)
	host := strings.ToLower(download.Hostname())
	if host == strings.ToLower(repo.Hostname()) || slices.Contains(ReleaseHosts, host) {
		return nil
	}
	return fmt.Errorf("download host %q is neither the repository host %q nor a GitHub release host", host, repo.Hostname())
}

// publicHTTPS accepts only an HTTPS URL on the default port whose host is a
// dotted DNS name, not an IP literal, localhost, or an internal suffix.
func publicHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return fmt.Errorf("download URL must be https without credentials")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("download URL must use the default https port")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("download host %q is not a public DNS name", host)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".corp", ".arpa"} {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("download host %q is not a public DNS name", host)
		}
	}
	return nil
}

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// digest. It makes no network requests; the pinned descriptor is final, so a
// version deleted upstream fails instead of being replaced by a newer one.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	repo, chart, err := splitChart(ref.Name)
	if err != nil {
		return registry.Artifact{}, err
	}
	if err := validVersion(ref.Version); err != nil {
		return registry.Artifact{}, err
	}
	if id := ref.Attributes["package_id"]; len(id) != 36 || strings.Trim(id, "0123456789abcdef-") != "" {
		return registry.Artifact{}, fmt.Errorf("invalid %s package ID", Identity)
	}
	raw := ref.Attributes["download_url"]
	if err := s.checkDownload(ref.Attributes["repo_url"], raw); err != nil {
		return registry.Artifact{}, err
	}
	digest, err := parseDigest(ref.Attributes["sha256"])
	if err != nil {
		return registry.Artifact{}, err
	}
	// Artifact Hub publishes no archive size.
	size := int64(-1)
	if value, ok := ref.Attributes["size"]; ok {
		if size, err = strconv.ParseInt(value, 10, 64); err != nil || size < 1 {
			return registry.Artifact{}, fmt.Errorf("invalid %s artifact size", Identity)
		}
	}
	name := blobName(repo + "-" + chart + "-" + ref.Version + ".tgz")
	return registry.Artifact{Reference: ref, Revision: digest, Blobs: []registry.Blob{{
		Name:      name,
		MediaType: "application/gzip",
		Digest:    digest,
		Size:      size,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			response, err := httpx.Get(ctx, s.Client, raw, s.attempts())
			if err != nil {
				return nil, err
			}
			return response.Body, nil
		},
	}}}, nil
}

// parseDigest accepts exactly sha256:<64 lowercase hex>, with or without the
// prefix, which is how Artifact Hub reports it.
func parseDigest(value string) (string, error) {
	raw := strings.TrimPrefix(value, "sha256:")
	hash, err := hex.DecodeString(raw)
	if err != nil || len(hash) != 32 || raw != hex.EncodeToString(hash) {
		return "", fmt.Errorf("invalid %s SHA-256", Identity)
	}
	return "sha256:" + raw, nil
}

// blobName keeps a manifest file name free of path separators, because
// consumers write blob names to disk.
func blobName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
}
