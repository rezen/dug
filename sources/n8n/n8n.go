// Package n8n archives n8n community node packages from the npm registry.
//
// A community node is an npm package carrying the n8n-community-node-package
// keyword. n8n loads it into its own process, where it can read every
// credential the instance holds, and self-hosted instances install any
// matching package without review. npm has no listing API for a keyword that
// can be walked to the end, so enumeration combines two indexes; see
// enumerate.go. Everything else (package documents, version history,
// tarballs, integrity hashes) comes from npm through sdk/services/npm.
package n8n

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/pool"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services/npm"
)

// Identity is the registry name the host selects: `dug list n8n`.
const Identity = "n8n"

// Namespace records where artifacts came from; manifests persist it.
const Namespace = npm.DefaultHost

// DefaultKeyword is the keyword n8n requires of community node packages.
const DefaultKeyword = "n8n-community-node-package"

// DefaultDownloadHosts is where npm serves tarballs. Resolve refuses persisted
// download URLs on any other host, so a tampered inventory cannot steer this
// process at arbitrary servers.
var DefaultDownloadHosts = []string{npm.DefaultHost}

// Enumeration modes for Source.Enumerate.
const (
	EnumerateAll        = "all"        // ecosyste.ms keyword index plus npm search
	EnumerateSearch     = "search"     // npm search only: the top 5,000 results
	EnumerateEcosystems = "ecosystems" // ecosyste.ms keyword index only
)

type Source struct {
	Client    *http.Client
	Registry  string // npm registry base override for tests; empty means npm.DefaultRegistry
	Downloads string // npm download-count API base override
	Index     string // ecosyste.ms API base override; empty means DefaultIndex
	Attempts  int    // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts overrides DefaultDownloadHosts; tests point it at httptest.
	DownloadHosts []string

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Packages     []string // explicit npm package names; empty walks the keyword
	Keyword      string   // empty means DefaultKeyword
	Enumerate    string   // empty means EnumerateAll
	VersionDepth string   // empty means the current release only
	Concurrency  int      // package lookups in flight; 0 means 4
}

var _ registry.Registry = (*Source)(nil)

func (s *Source) npm() *npm.Client {
	return &npm.Client{HTTP: s.Client, Registry: s.Registry, Downloads: s.Downloads, Attempts: s.Attempts}
}

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

func (s *Source) keyword() string {
	if s.Keyword != "" {
		return s.Keyword
	}
	return DefaultKeyword
}

const (
	defaultWorkers = 4
	maxConcurrency = 16
)

func (s *Source) workers() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return defaultWorkers
}

// chunk is how many packages get their download counts in one lookup.
const chunk = npm.BulkDownloadNames

// job is one package to fetch and the chunk whose download counts it needs.
type job struct {
	name  string
	chunk int
}

// fetched is a worker's result for one job.
type fetched struct {
	job
	doc *npm.Packument
	err error
}

// monthly looks up one chunk's download counts, once, on first call.
type monthly func() (map[string]int64, error)

// downloadCounts returns a lazy lookup per chunk of names, one request each.
func downloadCounts(ctx context.Context, client *npm.Client, names []string) []monthly {
	counts := make([]monthly, (len(names)+chunk-1)/chunk)
	for c := range counts {
		batch := names[c*chunk : min((c+1)*chunk, len(names))]
		// One window, last-month: scoped names cost a request each, and a
		// month is steadier for ordering than a week.
		counts[c] = sync.OnceValues(func() (map[string]int64, error) {
			return client.DownloadCounts(ctx, "last-month", batch)
		})
	}
	return counts
}

// fetch runs on a worker. It starts this chunk's count lookup and the next
// one's, so the counts are ready by the time the previous chunk has finished
// emitting.
func (s *Source) fetch(ctx context.Context, client *npm.Client, counts []monthly, j job) fetched {
	for _, c := range []int{j.chunk, j.chunk + 1} {
		if c < len(counts) {
			go counts[c]()
		}
	}
	doc, err := client.Packument(ctx, j.name)
	return fetched{j, doc, err}
}

// Discover emits one reference per selected version of each package, current
// version first. Every reference is self-resolved before emission so a listing
// never persists something stage two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	explicit := len(s.Packages) > 0
	var names []string
	var searchTotal int
	if explicit {
		for _, name := range s.Packages {
			if err := npm.ValidName(name); err != nil {
				return err
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	} else if names, searchTotal, err = s.enumerate(ctx); err != nil {
		return err
	}
	if s.Concurrency < 0 || s.Concurrency > maxConcurrency {
		return fmt.Errorf("n8n listing_concurrency must be 1–%d", maxConcurrency)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := s.npm()
	counts := downloadCounts(ctx, client, names)
	jobs := make([]job, len(names))
	for i, name := range names {
		jobs[i] = job{name, i / chunk}
	}
	found := 0
	// Packument fetches run concurrently; everything that emits stays on this
	// goroutine, in package order.
	err = pool.Ordered(ctx, s.workers(), jobs, func(ctx context.Context, j job) fetched {
		return s.fetch(ctx, client, counts, j)
	}, func(f fetched) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		by, err := counts[f.chunk]()
		if err != nil {
			return err
		}
		emitted, err := s.emitFetched(ctx, f, by, explicit, depth, emit)
		if emitted {
			found++
		}
		return err
	})
	if err != nil {
		return err
	}
	if !explicit && searchTotal > found {
		// Neither index is exhaustive, so a gap is expected, never silent.
		slog.Warn("n8n walk saw fewer packages than npm search reports; the corpus is a subset of the catalog",
			"found", found, "npm_search_total", searchTotal, "keyword", s.keyword(), "enumerate", s.mode())
	}
	return nil
}

// emitFetched emits one fetched package and reports whether it was a package
// the walk counts as found.
func (s *Source) emitFetched(ctx context.Context, f fetched, monthly map[string]int64, explicit bool, depth registry.VersionDepth, emit func(registry.Reference) error) (bool, error) {
	name := f.name
	if errors.Is(f.err, npm.ErrNotFound) {
		if explicit {
			return false, fmt.Errorf("n8n package %s: %w", name, f.err)
		}
		// The indexes lag npm: a package they list may be gone.
		slog.Warn("n8n skipping package that npm no longer serves", "package", name, "error", f.err)
		return false, nil
	}
	if f.err != nil {
		return false, f.err
	}
	if !explicit && !slices.Contains(registry.NormalizeTags(f.doc.Keywords), s.keyword()) {
		slog.Warn("n8n skipping package that no longer carries the keyword", "package", name, "keyword", s.keyword())
		return false, nil
	}
	var month *int64
	if n, ok := monthly[name]; ok {
		month = registry.Int64(n)
	}
	return true, s.emitVersions(ctx, name, f.doc, month, depth, emit)
}

// emitVersions emits the current version first, then older versions newest
// first while the depth allows.
func (s *Source) emitVersions(ctx context.Context, name string, doc *npm.Packument, month *int64, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	current, ok := doc.Current()
	if !ok {
		return fmt.Errorf("n8n package %s has no versions", name)
	}
	// Package-wide facts are attached to every version, including old ones;
	// they are a listing-time snapshot, not per-version history. npm publishes
	// no lifetime total, so only the trailing month is set.
	facts := &registry.Facts{}
	if month != nil {
		facts.Downloads = &registry.DownloadStats{Month: month}
	}
	if modified, ok := doc.Modified(); ok {
		facts.UpdatedAt = registry.Time(modified)
	}
	tags := registry.NormalizeTags(doc.Keywords)
	ordered := append([]string{current}, slices.DeleteFunc(doc.OrderedVersions(), func(v string) bool { return v == current })...)
	count := 0
	for _, version := range ordered {
		v := doc.Versions[version]
		published, hasTime := doc.Published(version)
		if !depth.Since.IsZero() {
			if !hasTime {
				return fmt.Errorf("%s@%s: missing publish time for a date cutoff", name, version)
			}
			if published.Before(depth.Since) {
				// Newest first, so only the current version can be older than
				// a later one: skip it and keep looking.
				if version == current {
					continue
				}
				return nil
			}
		}
		if v.Version != version || (v.Name != "" && v.Name != name) {
			return fmt.Errorf("%s@%s: package document describes %s@%s", name, version, v.Name, v.Version)
		}
		hash, err := v.Dist.Hash()
		if err != nil {
			return fmt.Errorf("%s@%s: %w", name, version, err)
		}
		download, err := npm.TarballURL(s.registryBase(), name, version)
		if err != nil {
			return err
		}
		metadata := map[string]string{"version_rank": strconv.Itoa(count + 1)}
		versionFacts := facts.Clone()
		if hasTime {
			versionFacts.ReleasedAt = registry.Time(published)
			metadata["release_date_source"] = "npm-time"
		}
		if repository := firstRepository(v.Repository, doc.Repository); repository.URL != "" {
			if normalized, ok := registry.NormalizeRepositoryURL(repository.URL); ok {
				versionFacts.Repository = &registry.Repository{URL: normalized, Directory: repository.Directory, Source: registry.RepositoryDeclared}
			}
		}
		if v.HasInstallScript() {
			metadata["install_script"] = "true"
		}
		if v.Deprecated != "" {
			metadata["deprecated"] = "true"
		}
		if license := firstText(v.License, doc.License); license != "" {
			metadata["license"] = string(license)
		}
		if v.Dist.HasProvenance() {
			metadata["provenance"] = "true"
		}
		ref := registry.Reference{
			Registry:   Identity,
			Namespace:  Namespace,
			Name:       name,
			Version:    version,
			Attributes: map[string]string{"download_url": download, "integrity": hash.String()},
			Metadata:   metadata,
			Facts:      versionFacts,
			Tags:       slices.Clone(tags),
			Author:     publisher(v),
			Raw:        slices.Clone(v.Raw),
		}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return fmt.Errorf("%s@%s: %w", name, version, err)
		}
		if err := emit(ref); err != nil {
			return err
		}
		count++
		if depth.Max > 0 && count >= depth.Max {
			return nil
		}
	}
	return nil
}

// publisher is the account that published this version: the registry's
// _npmUser, which differs from the package maintainers whenever a release is
// made by someone else, the case an account takeover produces.
func publisher(v npm.Version) *registry.Author {
	if v.NpmUser == nil || (v.NpmUser.Name == "" && v.NpmUser.Email == "") {
		return nil
	}
	return &registry.Author{Username: v.NpmUser.Name, Email: v.NpmUser.Email}
}

func firstRepository(candidates ...npm.Repository) npm.Repository {
	for _, c := range candidates {
		if c.URL != "" {
			return c
		}
	}
	return npm.Repository{}
}

func firstText(candidates ...npm.Text) npm.Text {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

func (s *Source) registryBase() string {
	if s.Registry != "" {
		return s.Registry
	}
	return npm.DefaultRegistry
}

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// hash. It makes no network requests; the pinned descriptor is final, so a
// version unpublished upstream fails instead of being replaced by a newer one.
//
// npm publishes SHA-512 (SHA-1 for old versions), not SHA-256, so the blob
// carries no digest for the host to check. The plugin enforces the published
// hash itself while the tarball streams, and the host computes SHA-256.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	if err := npm.ValidName(ref.Name); err != nil {
		return registry.Artifact{}, err
	}
	if err := npm.ValidVersion(ref.Version); err != nil {
		return registry.Artifact{}, err
	}
	raw := ref.Attributes["download_url"]
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	// The host check admits any path on npm; only the one tarball URL this
	// name and version map to is acceptable.
	want, err := npm.TarballURL(s.registryBase(), ref.Name, ref.Version)
	if err != nil {
		return registry.Artifact{}, err
	}
	if raw != want {
		return registry.Artifact{}, fmt.Errorf("n8n download URL %q is not the tarball of %s@%s", raw, ref.Name, ref.Version)
	}
	integrity, err := npm.ParseIntegrity(ref.Attributes["integrity"])
	if err != nil {
		return registry.Artifact{}, err
	}
	return registry.Artifact{Reference: ref, Revision: integrity.String(), Blobs: []registry.Blob{{
		Name:      npm.TarballName(ref.Name, ref.Version),
		MediaType: "application/gzip",
		Size:      -1,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
			if err != nil {
				return nil, err
			}
			return integrity.Verify(response.Body), nil
		},
	}}}, nil
}
