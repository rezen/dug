// Package ansiblegalaxy archives the collection tarballs published on Ansible Galaxy.
package ansiblegalaxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
)

// Identity is the registry name the host selects: `dug list ansible-galaxy`.
const Identity = "ansible-galaxy"

// Namespace records where artifacts came from; manifests persist it.
const Namespace = "galaxy.ansible.com"

// DefaultEndpoint is the published-collections API base, with a trailing
// slash. Galaxy also serves roles (API v1, GitHub hosted) and other pulp
// repositories; only the "published" repository is walked here.
const DefaultEndpoint = "https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/"

// DefaultDownloadHosts is where the registry serves artifact bytes. Resolve
// refuses persisted download URLs on any other host, so a tampered inventory
// cannot steer this process at arbitrary servers. The persisted URL is
// galaxy.ansible.com; it redirects to signed S3 URLs, which are never stored.
var DefaultDownloadHosts = []string{"galaxy.ansible.com"}

const (
	maxMetadataBytes = 32 << 20
	maxPageSize      = 100 // the API silently clamps larger limits
)

type Source struct {
	Client   *http.Client
	Endpoint string // metadata API base override for tests; empty means DefaultEndpoint
	Attempts int    // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts overrides DefaultDownloadHosts; tests point it at httptest.
	DownloadHosts []string

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Collections  []string // fully qualified names, namespace.name
	Namespaces   []string
	PageSize     int    // 0 means 100
	VersionDepth string // empty means the current release only
}

var _ registry.Registry = (*Source)(nil)

var errStop = errors.New("version depth reached")

var (
	partPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)
)

// collection is one catalog item as the index API describes it.
type collection struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Deprecated     bool   `json:"deprecated"`
	HighestVersion struct {
		Version string `json:"version"`
	} `json:"highest_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	DownloadCount *int64    `json:"download_count"`
}

type versionSummary struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// versionDetail is the per-version document that carries the digest and size.
type versionDetail struct {
	Version         string    `json:"version"`
	CreatedAt       time.Time `json:"created_at"`
	RequiresAnsible string    `json:"requires_ansible"`
	Marks           []string  `json:"marks"`
	DownloadURL     string    `json:"download_url"`
	GitCommitSHA    string    `json:"git_commit_sha"`
	Artifact        struct {
		Filename string `json:"filename"`
		SHA256   string `json:"sha256"`
		Size     int64  `json:"size"`
	} `json:"artifact"`
	Metadata struct {
		License    []string `json:"license"`
		Repository string   `json:"repository"`
		Tags       []string `json:"tags"`
	} `json:"metadata"`
	raw json.RawMessage
}

// UnmarshalJSON keeps the verbatim version document next to the decoded fields.
func (d *versionDetail) UnmarshalJSON(data []byte) error {
	type plain versionDetail
	if err := json.Unmarshal(data, (*plain)(d)); err != nil {
		return err
	}
	d.raw = slices.Clone(data)
	return nil
}

type page[T any] struct {
	Meta struct {
		Count int `json:"count"`
	} `json:"meta"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
	Data []T `json:"data"`
}

func (s *Source) base() string {
	if s.Endpoint != "" {
		return strings.TrimRight(s.Endpoint, "/") + "/"
	}
	return DefaultEndpoint
}

func (s *Source) attempts() int {
	if s.Attempts > 0 {
		return s.Attempts
	}
	return 4
}

func (s *Source) pageSize() int {
	if s.PageSize > 0 {
		return s.PageSize
	}
	return maxPageSize
}

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

// validPart rejects namespace and collection names that could escape a URL
// path or a file name, or that would make a fully qualified name ambiguous.
func validPart(part string) error {
	if !partPattern.MatchString(part) {
		return fmt.Errorf("invalid %s name %q", Identity, part)
	}
	return nil
}

func validVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid %s version %q", Identity, version)
	}
	return nil
}

// splitName parses a fully qualified collection name, namespace.name.
func splitName(fqcn string) (namespace, name string, err error) {
	namespace, name, ok := strings.Cut(fqcn, ".")
	if !ok || validPart(namespace) != nil || validPart(name) != nil {
		return "", "", fmt.Errorf("invalid %s collection %q (want namespace.name)", Identity, fqcn)
	}
	return namespace, name, nil
}

func (s *Source) collectionURL(namespace, name string) string {
	return s.base() + "index/" + namespace + "/" + name + "/"
}

// Discover walks the catalog, or only the selected collections or namespaces,
// and emits one reference per selected version. Every reference is
// self-resolved before emission so a listing never persists something stage
// two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	if len(s.Collections) > 0 && len(s.Namespaces) > 0 {
		return fmt.Errorf("%s collections and namespaces are mutually exclusive", Identity)
	}
	if s.PageSize < 0 || s.PageSize > maxPageSize {
		return fmt.Errorf("%s page_size must be 1–%d", Identity, maxPageSize)
	}
	seen := make(map[string]bool)
	// Live catalogs shift under offset pagination, so dedupe by name.
	once := func(c collection) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if validPart(c.Namespace) != nil || validPart(c.Name) != nil {
			return false, fmt.Errorf("invalid %s catalog entry %q.%q", Identity, c.Namespace, c.Name)
		}
		key := c.Namespace + "." + c.Name
		if seen[key] {
			return false, nil
		}
		seen[key] = true
		return true, nil
	}
	// A collection or version that vanishes between the index page and its own
	// request is skipped during a full walk; an explicit selection fails.
	walked := func(c collection) error {
		fresh, err := once(c)
		if err != nil || !fresh {
			return err
		}
		err = s.emitVersions(ctx, c, depth, emit)
		var status *httpx.StatusError
		if errors.As(err, &status) && status.Code == http.StatusNotFound {
			slog.Warn("ansible-galaxy collection disappeared during listing", "collection", c.Namespace+"."+c.Name, "url", status.URL)
			return nil
		}
		return err
	}
	switch {
	case len(s.Collections) > 0:
		for _, fqcn := range s.Collections {
			namespace, name, err := splitName(fqcn)
			if err != nil {
				return err
			}
			var c collection
			if err := s.query(ctx, s.collectionURL(namespace, name), &c); err != nil {
				return fmt.Errorf("%s: %w", fqcn, err)
			}
			if c.Namespace != namespace || c.Name != name {
				return retry.Permanent(fmt.Errorf("%s returned %q.%q for %s", Identity, c.Namespace, c.Name, fqcn))
			}
			fresh, err := once(c)
			if err != nil {
				return err
			}
			if fresh {
				if err := s.emitVersions(ctx, c, depth, emit); err != nil {
					return fmt.Errorf("%s: %w", fqcn, err)
				}
			}
		}
		return nil
	case len(s.Namespaces) > 0:
		for _, namespace := range s.Namespaces {
			if err := validPart(namespace); err != nil {
				return err
			}
			if err := s.walkCatalog(ctx, namespace, walked); err != nil {
				return err
			}
		}
		return nil
	}
	return s.walkCatalog(ctx, "", walked)
}

// walkCatalog enumerates the index page by page. The API orders it by
// creation time, newest first, and exposes no sort or date filter, so new
// collections shift later offsets (duplicates, which the caller drops) but
// never hide an existing one. Next-page URLs must stay on the API origin and
// must not repeat, and a walk that sees fewer collections than the API
// reports fails instead of returning a partial catalog.
func (s *Source) walkCatalog(ctx context.Context, namespace string, add func(collection) error) error {
	query := url.Values{"limit": {strconv.Itoa(s.pageSize())}}
	if namespace != "" {
		query.Set("namespace", namespace)
	}
	raw := s.base() + "index/?" + query.Encode()
	seenPages := map[string]bool{raw: true}
	seen := make(map[string]bool)
	first, last := -1, 0
	for {
		var p page[collection]
		if err := s.query(ctx, raw, &p); err != nil {
			return err
		}
		if first < 0 {
			first = p.Meta.Count
		}
		last = p.Meta.Count
		for _, c := range p.Data {
			seen[c.Namespace+"."+c.Name] = true
			if err := add(c); err != nil {
				return err
			}
		}
		if p.Links.Next == "" {
			// Deletions can only lower the count, so the smaller of the two
			// totals is what a complete walk must have seen.
			if want := min(first, last); len(seen) < want {
				return fmt.Errorf("%s catalog ended after %d of %d collections", Identity, len(seen), want)
			}
			return nil
		}
		next, err := s.next(p.Links.Next, seenPages)
		if err != nil {
			return err
		}
		raw = next
	}
}

// emitVersions emits the highest version first, then older versions in the
// order the API lists them while the depth allows.
func (s *Source) emitVersions(ctx context.Context, c collection, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	fqcn := c.Namespace + "." + c.Name
	if c.HighestVersion.Version == "" {
		return retry.Permanent(fmt.Errorf("%s: no published version", fqcn))
	}
	// Collection-wide facts are attached to every version, including old ones;
	// they are a listing-time snapshot, not per-version history. Galaxy
	// reports only a lifetime download total for the collection.
	facts := &registry.Facts{}
	if c.DownloadCount != nil {
		facts.Downloads = &registry.DownloadStats{Total: registry.Int64(*c.DownloadCount)}
	}
	if !c.UpdatedAt.IsZero() {
		facts.UpdatedAt = registry.Time(c.UpdatedAt)
	}
	metadata := map[string]string{"deprecated": strconv.FormatBool(c.Deprecated)}
	author := &registry.Author{Username: c.Namespace}
	seen := make(map[string]bool)
	count := 0
	// created is the listing's release time when known, so a version outside a
	// date cutoff costs no detail request.
	add := func(version string, created time.Time) error {
		if seen[version] {
			return nil
		}
		seen[version] = true
		if err := validVersion(version); err != nil {
			return fmt.Errorf("%s: %w", fqcn, err)
		}
		if !depth.Since.IsZero() && !created.IsZero() && created.Before(depth.Since) {
			return nil
		}
		var d versionDetail
		if err := s.query(ctx, s.collectionURL(c.Namespace, c.Name)+"versions/"+version+"/", &d); err != nil {
			return fmt.Errorf("%s@%s: %w", fqcn, version, err)
		}
		if d.Version != version || d.CreatedAt.IsZero() {
			return retry.Permanent(fmt.Errorf("%s@%s: invalid version document", fqcn, version))
		}
		if !depth.Since.IsZero() && d.CreatedAt.Before(depth.Since) {
			return nil
		}
		versionMetadata := maps.Clone(metadata)
		versionMetadata["version_rank"] = strconv.Itoa(count + 1)
		versionMetadata["release_date_source"] = "galaxy-version-created-at"
		if d.RequiresAnsible != "" {
			versionMetadata["requires_ansible"] = d.RequiresAnsible
		}
		if len(d.Metadata.License) > 0 {
			versionMetadata["license"] = strings.Join(d.Metadata.License, ",")
		}
		if len(d.Marks) > 0 {
			versionMetadata["marks"] = strings.Join(d.Marks, ",")
		}
		versionFacts := facts.Clone()
		versionFacts.ReleasedAt = registry.Time(d.CreatedAt)
		if repo, ok := registry.NormalizeRepositoryURL(d.Metadata.Repository); ok {
			versionFacts.Repository = &registry.Repository{URL: repo, Commit: d.GitCommitSHA, Source: registry.RepositoryDeclared}
		}
		ref := registry.Reference{
			Registry:  Identity,
			Namespace: Namespace,
			Name:      fqcn,
			Version:   version,
			Attributes: map[string]string{
				"download_url": d.DownloadURL,
				"sha256":       normalizeDigest(d.Artifact.SHA256),
				"size":         strconv.FormatInt(d.Artifact.Size, 10),
			},
			Metadata: versionMetadata,
			Facts:    versionFacts,
			Tags:     registry.NormalizeTags(d.Metadata.Tags),
			Author:   author.Clone(),
			Raw:      slices.Clone(d.raw),
		}
		if d.Artifact.Filename != archiveName(ref) {
			return retry.Permanent(fmt.Errorf("%s@%s: unexpected archive name %q", fqcn, version, d.Artifact.Filename))
		}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return fmt.Errorf("%s@%s: %w", fqcn, version, err)
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
	if err := add(c.HighestVersion.Version, time.Time{}); err != nil {
		if errors.Is(err, errStop) {
			return nil
		}
		return err
	}
	if depth.Max == 1 {
		return nil
	}
	query := url.Values{"limit": {strconv.Itoa(s.pageSize())}}
	raw := s.collectionURL(c.Namespace, c.Name) + "versions/?" + query.Encode()
	seenPages := map[string]bool{raw: true}
	for {
		var p page[versionSummary]
		if err := s.query(ctx, raw, &p); err != nil {
			return fmt.Errorf("%s: %w", fqcn, err)
		}
		for _, v := range p.Data {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := add(v.Version, v.CreatedAt); err != nil {
				if errors.Is(err, errStop) {
					return nil
				}
				return err
			}
		}
		if p.Links.Next == "" {
			return nil
		}
		next, err := s.next(p.Links.Next, seenPages)
		if err != nil {
			return fmt.Errorf("%s: %w", fqcn, err)
		}
		raw = next
	}
}

// normalizeDigest turns the registry's hex digest into sha256:<hex>.
func normalizeDigest(value string) string {
	return "sha256:" + strings.ToLower(strings.TrimPrefix(value, "sha256:"))
}

// next validates a pagination link: same origin and path prefix as the API
// base, and not seen before. Galaxy returns site-relative links.
func (s *Source) next(raw string, seen map[string]bool) (string, error) {
	base, err := url.Parse(s.base())
	if err != nil {
		return "", err
	}
	u, err := base.Parse(raw)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, base.Path) {
		return "", fmt.Errorf("%s pagination left the API origin", Identity)
	}
	if seen[u.String()] {
		return "", fmt.Errorf("%s pagination cycle", Identity)
	}
	seen[u.String()] = true
	return u.String(), nil
}

// query fetches one bounded metadata document. Decode failures are permanent:
// retrying the same bytes cannot help, and the host should not retry either.
func (s *Source) query(ctx context.Context, raw string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, raw, s.attempts())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxMetadataBytes {
		return retry.Permanent(fmt.Errorf("%s metadata exceeds 32 MiB", Identity))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return retry.Permanent(fmt.Errorf("%s %s: %w", Identity, raw, err))
	}
	return nil
}

// archiveName is the tarball name Galaxy gives a collection version.
func archiveName(ref registry.Reference) string {
	namespace, name, _ := strings.Cut(ref.Name, ".")
	return namespace + "-" + name + "-" + ref.Version + ".tar.gz"
}

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// digest. It makes no network requests; the pinned descriptor is final, so a
// version deleted upstream fails instead of being replaced by a newer one.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	if _, _, err := splitName(ref.Name); err != nil {
		return registry.Artifact{}, err
	}
	if err := validVersion(ref.Version); err != nil {
		return registry.Artifact{}, err
	}
	raw := ref.Attributes["download_url"]
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	// Pin the route as well as the host: only the artifact route for exactly
	// this version, never another path on the registry's host.
	name := archiveName(ref)
	if u, err := url.Parse(raw); err != nil || u.RawQuery != "" || path.Base(u.Path) != name || path.Base(path.Dir(u.Path)) != "artifacts" {
		return registry.Artifact{}, fmt.Errorf("invalid %s download URL", Identity)
	}
	digest, err := parseDigest(ref.Attributes["sha256"])
	if err != nil {
		return registry.Artifact{}, err
	}
	size, err := strconv.ParseInt(ref.Attributes["size"], 10, 64)
	if err != nil || size < 1 {
		return registry.Artifact{}, fmt.Errorf("invalid %s artifact size", Identity)
	}
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

// parseDigest accepts exactly sha256:<64 lowercase hex>.
func parseDigest(value string) (string, error) {
	raw := strings.TrimPrefix(value, "sha256:")
	hash, err := hex.DecodeString(raw)
	if err != nil || len(hash) != 32 || raw != hex.EncodeToString(hash) {
		return "", fmt.Errorf("invalid %s SHA-256", Identity)
	}
	return "sha256:" + raw, nil
}
