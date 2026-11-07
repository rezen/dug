// Package obsidian archives Obsidian community plugins: the catalog in
// obsidianmd/obsidian-releases, with every version taken from the plugin's
// GitHub releases (main.js, manifest.json and optional styles.css).
package obsidian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	ghsvc "github.com/rezen/dug/sdk/services/github"
)

// Identity is the registry name the host selects: `dug list obsidian`.
const Identity = "obsidian"

// Namespace records where the catalog lives; manifests persist it.
const Namespace = "obsidian.md"

// DefaultCatalogEndpoint serves community-plugins.json and
// community-plugin-stats.json, with a trailing slash.
const DefaultCatalogEndpoint = "https://raw.githubusercontent.com/obsidianmd/obsidian-releases/master/"

// DefaultReleaseEndpoint serves release assets at
// /OWNER/NAME/releases/download/TAG/FILE, without API quota or a token.
const DefaultReleaseEndpoint = "https://github.com"

const maxMetadataBytes = 32 << 20

// pluginFiles are the only assets Obsidian loads from a release.
var pluginFiles = map[string]string{
	"main.js":       "text/javascript",
	"manifest.json": "application/json",
	"styles.css":    "text/css",
}

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Source struct {
	Client   *http.Client
	Attempts int // upstream HTTP attempts per request; 0 means 4
	// GitHubEndpoint is the REST API base used for release listings; empty
	// means the public API. Token is sent to that origin only and is never
	// written to references.
	GitHubEndpoint string
	Token          string
	// CatalogEndpoint and ReleaseEndpoint override the public hosts for tests.
	CatalogEndpoint string
	ReleaseEndpoint string

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	IDs          []string // plugin ids; empty walks the whole catalog
	VersionDepth string   // empty means the current release only
	Concurrency  int      // plugins whose releases are fetched in flight; 0 means 4
}

const defaultWorkers = 4

func (s *Source) workers() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return defaultWorkers
}

var _ registry.Registry = (*Source)(nil)

var errStop = errors.New("version depth reached")

// entry is one catalog entry.
type entry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Author string `json:"author"`
	Repo   string `json:"repo"`
}

// stats are per-plugin download counters: "downloads", "updated" (Unix
// milliseconds) and one counter per version.
type stats map[string]float64

// asset is the pinned descriptor of one release file. Download URLs are
// derived from the repository, tag and name at resolution, never persisted.
type asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
}

func (s *Source) attempts() int {
	if s.Attempts > 0 {
		return s.Attempts
	}
	return 4
}

func (s *Source) catalogURL(file string) string {
	base := s.CatalogEndpoint
	if base == "" {
		base = DefaultCatalogEndpoint
	}
	return strings.TrimRight(base, "/") + "/" + file
}

func (s *Source) releaseHost() string {
	if s.ReleaseEndpoint != "" {
		return strings.TrimRight(s.ReleaseEndpoint, "/")
	}
	return DefaultReleaseEndpoint
}

func (s *Source) gh() *ghsvc.Client {
	return &ghsvc.Client{HTTP: s.Client, Endpoint: s.GitHubEndpoint, Token: s.Token, Attempts: s.Attempts}
}

func validID(id string) error {
	if !idPattern.MatchString(id) || id == ".." {
		return fmt.Errorf("invalid %s plugin id %q", Identity, id)
	}
	return nil
}

// Discover walks the catalog, or only the selected ids, and emits one
// reference per release, current first. Every reference is self-resolved before
// emission so a listing never persists something stage two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	for _, id := range s.IDs {
		if err := validID(id); err != nil {
			return err
		}
	}
	if _, err := s.gh().API(); err != nil {
		return err
	}
	var catalog []entry
	if err := s.fetch(ctx, s.catalogURL("community-plugins.json"), &catalog); err != nil {
		return err
	}
	var all map[string]stats
	if err := s.fetch(ctx, s.catalogURL("community-plugin-stats.json"), &all); err != nil {
		return err
	}
	byID := make(map[string]entry, len(catalog))
	order := make([]string, 0, len(catalog))
	for _, p := range catalog {
		if _, dup := byID[p.ID]; dup {
			continue
		}
		byID[p.ID] = p
		order = append(order, p.ID)
	}
	explicit := len(s.IDs) > 0
	if explicit {
		order = slices.Clone(s.IDs)
		for _, id := range order {
			if _, ok := byID[id]; !ok {
				return fmt.Errorf("%s plugin %q is not in the community catalog", Identity, id)
			}
		}
	}
	// Each plugin's releases are fetched by a pool worker, which only collects
	// references; they are emitted here, in catalog order, from this goroutine.
	var listings []listing
	done := make(map[string]bool, len(order))
	for _, id := range order {
		if done[id] {
			continue
		}
		done[id] = true
		listings = append(listings, listing{entry: byID[id], invalid: validID(id) != nil})
	}
	return pool.Ordered(ctx, s.workers(), listings,
		func(ctx context.Context, p listing) fetched {
			if p.invalid || ctx.Err() != nil {
				return fetched{p, pool.Collected[registry.Reference]{Err: ctx.Err()}}
			}
			return fetched{p, pool.Collect(func(add func(registry.Reference) error) error {
				return s.emitVersions(ctx, p.entry, all[p.entry.ID], depth, add)
			})}
		},
		func(f fetched) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if f.invalid {
				slog.Warn("skipping obsidian catalog entry with invalid id", "id", f.entry.ID)
				return nil
			}
			err := f.Emit(emit)
			if !explicit && errors.Is(err, errUnavailable) {
				slog.Warn("skipping unavailable obsidian plugin", "id", f.entry.ID, "repo", f.entry.Repo, "error", err)
				return nil
			}
			return err
		})
}

// listing is a catalog entry to fetch; invalid ids are warned about at
// emission time, in order, instead of fetched.
type listing struct {
	entry   entry
	invalid bool
}

// fetched is one plugin's references and the error that ended its lookups.
// References before the error are still emitted.
type fetched struct {
	listing
	pool.Collected[registry.Reference]
}

var errUnavailable = errors.New("nothing to archive")

// emitVersions emits the plugin's releases newest first while the depth allows.
func (s *Source) emitVersions(ctx context.Context, p entry, st stats, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	owner, name, err := ghsvc.ParseRepo(p.Repo)
	if err != nil {
		return fmt.Errorf("%s: %w: %v", p.ID, errUnavailable, err)
	}
	facts := &registry.Facts{}
	if v, ok := st["downloads"]; ok && v >= 0 {
		facts.Downloads = &registry.DownloadStats{Total: registry.Int64(int64(v))}
	}
	if v, ok := st["updated"]; ok && v > 0 {
		facts.UpdatedAt = registry.Time(time.UnixMilli(int64(v)))
	}
	if u, ok := registry.NormalizeRepositoryURL("https://github.com/" + owner + "/" + name); ok {
		facts.Repository = &registry.Repository{URL: u, Source: registry.RepositoryDeclared}
	}
	var author *registry.Author
	if a := strings.TrimSpace(p.Author); a != "" {
		author = &registry.Author{Name: a}
	}
	count := 0
	seen := make(map[string]bool)
	err = ghsvc.Pages(ctx, s.gh(), s.gh().URL("repos", owner, name, "releases")+"?per_page=100", func(items []ghsvc.Release) error {
		for _, rel := range items {
			if rel.Draft {
				continue
			}
			if rel.ID < 1 || !tagPattern.MatchString(rel.TagName) {
				slog.Warn("skipping obsidian release with unusable tag", "repository", p.Repo, "tag", rel.TagName)
				continue
			}
			if seen[rel.TagName] {
				continue
			}
			seen[rel.TagName] = true
			published := rel.PublishedAt
			if published.IsZero() {
				return fmt.Errorf("%s@%s: missing release publication date", p.ID, rel.TagName)
			}
			if !depth.Since.IsZero() && published.Before(depth.Since) {
				continue
			}
			var assets []asset
			for _, a := range rel.Assets {
				if _, ok := pluginFiles[a.Name]; !ok || a.State != "uploaded" {
					continue
				}
				if a.Digest != "" && !digestPattern.MatchString(a.Digest) {
					a.Digest = "" // an algorithm other than SHA-256: the host hashes the bytes
				}
				assets = append(assets, asset{ID: a.ID, Name: a.Name, Size: a.Size, Digest: a.Digest})
			}
			if !hasFile(assets, "main.js") || !hasFile(assets, "manifest.json") {
				slog.Warn("skipping obsidian release without main.js and manifest.json", "repository", p.Repo, "tag", rel.TagName)
				continue
			}
			slices.SortFunc(assets, func(a, b asset) int { return strings.Compare(a.Name, b.Name) })
			encoded, err := json.Marshal(assets)
			if err != nil {
				return err
			}
			metadata := map[string]string{
				"plugin_name":         p.Name,
				"repo":                p.Repo,
				"prerelease":          strconv.FormatBool(rel.Prerelease),
				"asset_count":         strconv.Itoa(len(assets)),
				"version_rank":        strconv.Itoa(count + 1),
				"release_date_source": "github-release-published",
			}
			versionFacts := facts.Clone()
			versionFacts.ReleasedAt = registry.Time(published)
			if v, ok := st[rel.TagName]; ok && v >= 0 {
				versionFacts.VersionDownloads = &registry.DownloadStats{Total: registry.Int64(int64(v))}
			} else if v, ok := st[strings.TrimPrefix(rel.TagName, "v")]; ok && v >= 0 {
				versionFacts.VersionDownloads = &registry.DownloadStats{Total: registry.Int64(int64(v))}
			}
			ref := registry.Reference{
				Registry:  Identity,
				Namespace: Namespace,
				Name:      p.ID,
				Version:   rel.TagName,
				Attributes: map[string]string{
					"repo":       p.Repo,
					"release_id": strconv.FormatInt(rel.ID, 10),
					"assets":     string(encoded),
				},
				Metadata: metadata,
				Facts:    versionFacts,
				Author:   author.Clone(),
			}
			if _, err := s.Resolve(ctx, ref); err != nil {
				return fmt.Errorf("%s@%s: %w", p.ID, rel.TagName, err)
			}
			if err := emit(ref); err != nil {
				return err
			}
			count++
			if depth.Max > 0 && count >= depth.Max {
				return errStop
			}
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	if ghsvc.Status(err) == http.StatusNotFound || ghsvc.Status(err) == http.StatusGone || ghsvc.Status(err) == http.StatusUnavailableForLegalReasons {
		return fmt.Errorf("%s: %w: repository %s: %v", p.ID, errUnavailable, p.Repo, err)
	}
	if err == nil && count == 0 {
		return fmt.Errorf("%s: %w: no usable releases", p.ID, errUnavailable)
	}
	return err
}

func hasFile(assets []asset, name string) bool {
	return slices.ContainsFunc(assets, func(a asset) bool { return a.Name == name })
}

// fetch downloads one bounded catalog document. Decode failures are
// permanent: retrying the same bytes cannot help, and the host should not
// retry either.
func (s *Source) fetch(ctx context.Context, raw string, out any) error {
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

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// digest. It makes no network requests; the pinned descriptor is final, so a
// release deleted upstream fails instead of being replaced by a newer one.
// GitHub release IDs do not change when a maintainer replaces an asset, so
// the Revision is only as immutable as the assets' own digests.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity || validID(ref.Name) != nil || !tagPattern.MatchString(ref.Version) {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	owner, repoName, err := ghsvc.ParseRepo(ref.Attributes["repo"])
	if err != nil {
		return registry.Artifact{}, err
	}
	releaseID, err := strconv.ParseInt(ref.Attributes["release_id"], 10, 64)
	if err != nil || releaseID < 1 {
		return registry.Artifact{}, fmt.Errorf("invalid %s release id", Identity)
	}
	var assets []asset
	if err := json.Unmarshal([]byte(ref.Attributes["assets"]), &assets); err != nil {
		return registry.Artifact{}, fmt.Errorf("invalid %s asset list: %w", Identity, err)
	}
	seen := make(map[string]bool, len(assets))
	blobs := make([]registry.Blob, 0, len(assets))
	for _, a := range assets {
		mediaType, ok := pluginFiles[a.Name]
		if !ok || seen[a.Name] || a.ID < 1 || a.Size < 0 || (a.Digest != "" && !digestPattern.MatchString(a.Digest)) {
			return registry.Artifact{}, fmt.Errorf("invalid %s release asset %q", Identity, a.Name)
		}
		seen[a.Name] = true
		raw := s.releaseHost() + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/releases/download/" + url.PathEscape(ref.Version) + "/" + a.Name
		blobs = append(blobs, registry.Blob{
			Name:      a.Name,
			MediaType: mediaType,
			Digest:    a.Digest,
			Size:      a.Size,
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				response, err := httpx.Get(ctx, s.Client, raw, s.attempts())
				if err != nil {
					return nil, err
				}
				return response.Body, nil
			},
		})
	}
	if !seen["main.js"] || !seen["manifest.json"] {
		return registry.Artifact{}, fmt.Errorf("%s release lacks main.js or manifest.json", Identity)
	}
	return registry.Artifact{Reference: ref, Revision: "release:" + strconv.FormatInt(releaseID, 10), Blobs: blobs}, nil
}
