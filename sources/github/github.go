// Package github archives GitHub repository releases, release assets, tags and
// branch snapshots through the REST API.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"io"
	"log/slog"
	"maps"
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
)

// Snapshot modes decide when a repository's default branch head is archived.
const (
	SnapshotFallback = "fallback" // only repositories without releases (default)
	SnapshotAlways   = "always"   // every repository, alongside its releases
	SnapshotOnly     = "only"     // every repository, instead of releases
	SnapshotNever    = "never"    // releases only
)

// Download modes are recorded in the download attribute. Public github.com
// repositories use the quota-free codeload and release download hosts without
// a token; private and Enterprise repositories use API routes with the token.
const (
	downloadDirect = "direct"
	downloadAPI    = "api"
)

type Source struct {
	Client *http.Client
	// Endpoint is the REST API base; empty means ghsvc.DefaultEndpoint.
	Endpoint string
	// Token is sent as a bearer token to the API origin only. It is never
	// written to references.
	Token string
	// Exactly one of Repos, Owner or Search selects repositories.
	Repos  []string // owner/name
	Owner  string   // user or organization login
	Search string   // repository search query, partitioned by creation date
	// Forks includes forked repositories when enumerating an owner.
	Forks bool
	// Assets archives release assets alongside the source archive.
	Assets bool
	// Snapshot is one of the Snapshot* modes; empty means SnapshotFallback.
	Snapshot string
	// Refs names additional branches or tags to snapshot for every repository.
	Refs []string
	// Tags archives every tag not already listed as a release or ref.
	Tags         bool
	VersionDepth string
	Attempts     int
	// Concurrency is how many repositories are looked up (releases, tags,
	// commits) at once; 0 means DefaultConcurrency. Search and owner paging
	// stay serial.
	Concurrency int
	// CodeloadEndpoint and ReleaseEndpoint override the public download hosts
	// (codeload.github.com and github.com) for local fixtures or a deployment
	// that serves archives itself; they are not RPC options. Without them,
	// only github.com repositories download directly.
	CodeloadEndpoint string
	ReleaseEndpoint  string
	searchWindow     int // GitHub returns at most 1,000 results per search query.
}

var _ registry.Registry = (*Source)(nil)

var (
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// DefaultConcurrency and MaxConcurrency bound the repository lookups in flight.
// The default is conservative because the REST quota is shared by every lookup.
const (
	DefaultConcurrency = 4
	MaxConcurrency     = 16
)

var errStop = errors.New("version depth reached")
var errMissing = errors.New("repository has nothing to archive")

// validRef applies git's ref-name rules, which also keep names URL-safe once
// escaped per path segment.
func validRef(name string) error {
	if name == "" || name == "." || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") ||
		strings.ContainsAny(name, " \t\n\r\x7f\\~^:?*[") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }) {
		return fmt.Errorf("invalid git ref %q", name)
	}
	return nil
}

// validBlobName mirrors the SDK rule: manifests record blob names and consumers
// write them to disk.
func validBlobName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

type repo = ghsvc.Repo
type asset = ghsvc.Asset
type release = ghsvc.Release

type tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (c commit) date() time.Time {
	if !c.Commit.Committer.Date.IsZero() {
		return c.Commit.Committer.Date
	}
	return c.Commit.Author.Date
}

func (s *Source) snapshot() string {
	if s.Snapshot == "" {
		return SnapshotFallback
	}
	return s.Snapshot
}

// namespace is the GitHub host a reference belongs to: github.com for the
// public API, otherwise the Enterprise Server host.
func (s *Source) namespace() string {
	u, err := s.gh().API()
	if err != nil || u.Hostname() == "api.github.com" {
		return "github.com"
	}
	return u.Hostname()
}

func (s *Source) downloadMode(r repo) string {
	if !r.Private && (s.CodeloadEndpoint != "" || s.namespace() == "github.com") {
		return downloadDirect
	}
	return downloadAPI
}

func (s *Source) validate() error {
	if _, err := s.gh().API(); err != nil {
		return err
	}
	switch s.snapshot() {
	case SnapshotFallback, SnapshotAlways, SnapshotOnly, SnapshotNever:
	default:
		return fmt.Errorf("github snapshot must be fallback, always, only, or never")
	}
	if s.Concurrency < 0 || s.Concurrency > MaxConcurrency {
		return fmt.Errorf("github listing_concurrency must be 1–%d", MaxConcurrency)
	}
	modes := 0
	for _, set := range []bool{len(s.Repos) > 0, s.Owner != "", s.Search != ""} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return fmt.Errorf("github requires exactly one of repos, owner, or search")
	}
	if s.Owner != "" && !ghsvc.ValidOwner(s.Owner) {
		return fmt.Errorf("invalid GitHub owner %q", s.Owner)
	}
	if strings.Contains(strings.ToLower(s.Search), "created:") {
		return fmt.Errorf("github search must not contain created:; discovery partitions creation dates itself")
	}
	for _, ref := range s.Refs {
		if err := validRef(ref); err != nil {
			return err
		}
	}
	for _, raw := range []string{s.CodeloadEndpoint, s.ReleaseEndpoint} {
		if raw != "" {
			if err := httpx.ValidateURL(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	if err := s.validate(); err != nil {
		return err
	}
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	// The walk stays serial and feeds the pool; each repository's lookups
	// collect references, which are emitted here in walk order.
	return pool.Walk(ctx, s.workers(), s.uniqueRepos,
		func(ctx context.Context, r repo) fetched {
			return fetched{r, pool.Collect(func(add func(registry.Reference) error) error {
				return s.emitRepo(ctx, r, depth, add)
			})}
		},
		func(f fetched) error {
			err := f.Emit(emit)
			if errors.Is(err, errMissing) && len(s.Repos) == 0 {
				slog.Warn("skipping GitHub repository", "repository", f.repo.FullName, "error", err)
				return nil
			}
			return err
		})
}

// fetched is one repository's references and the error that ended its lookups.
// References before the error are still emitted.
type fetched struct {
	repo repo
	pool.Collected[registry.Reference]
}

// uniqueRepos walks the selected repositories, pushing each valid one once.
func (s *Source) uniqueRepos(ctx context.Context, push func(repo) error) error {
	seen := make(map[int64]bool)
	return s.walk(ctx, func(r repo) error {
		if r.ID < 1 || r.DefaultBranch == "" {
			return fmt.Errorf("invalid GitHub repository metadata for %q", r.FullName)
		}
		if _, _, err := ghsvc.ParseRepo(r.FullName); err != nil {
			return err
		}
		if seen[r.ID] {
			return nil
		}
		seen[r.ID] = true
		return push(r)
	})
}

func (s *Source) workers() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return DefaultConcurrency
}

// walk visits the selected repositories serially.
func (s *Source) walk(ctx context.Context, add func(repo) error) error {
	switch {
	case len(s.Repos) > 0:
		for _, full := range s.Repos {
			owner, name, err := ghsvc.ParseRepo(full)
			if err != nil {
				return err
			}
			var r repo
			if _, err := s.gh().GetJSON(ctx, s.gh().URL("repos", owner, name), &r); err != nil {
				return err
			}
			if err := add(r); err != nil {
				return err
			}
		}
		return nil
	case s.Owner != "":
		return s.ownerRepos(ctx, add)
	default:
		// Search windows are split by immutable creation time, avoiding silent
		// truncation at GitHub's 1,000-result cap. The catalog remains a live view.
		return s.search(ctx, time.Date(2007, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC().Add(24*time.Hour).Truncate(time.Second), add)
	}
}

func (s *Source) ownerRepos(ctx context.Context, add func(repo) error) error {
	var account struct {
		Type string `json:"type"`
	}
	if _, err := s.gh().GetJSON(ctx, s.gh().URL("users", s.Owner), &account); err != nil {
		return err
	}
	var raw string
	switch account.Type {
	case "Organization":
		kind := "sources"
		if s.Forks {
			kind = "all"
		}
		raw = s.gh().URL("orgs", s.Owner, "repos") + "?per_page=100&type=" + kind
	case "User":
		raw = s.gh().URL("users", s.Owner, "repos") + "?per_page=100&type=owner"
	default:
		return fmt.Errorf("GitHub account %q has unexpected type %q", s.Owner, account.Type)
	}
	return ghsvc.Pages(ctx, s.gh(), raw, func(items []repo) error {
		for _, r := range items {
			if r.Fork && !s.Forks {
				continue
			}
			if err := add(r); err != nil {
				return err
			}
		}
		return nil
	})
}

type searchResult struct {
	Total      int    `json:"total_count"`
	Incomplete bool   `json:"incomplete_results"`
	Items      []repo `json:"items"`
}

const searchTime = "2006-01-02T15:04:05+00:00"

func (s *Source) search(ctx context.Context, from, to time.Time, add func(repo) error) error {
	q := strings.TrimSpace(s.Search) + " created:" + from.UTC().Format(searchTime) + ".." + to.UTC().Format(searchTime)
	raw := s.gh().URL("search", "repositories") + "?" + url.Values{"q": {q}, "per_page": {"100"}}.Encode()
	first, header, err := s.searchPage(ctx, raw)
	if err != nil {
		return err
	}
	window := s.searchWindow
	if window == 0 {
		window = 1000
	}
	if first.Total > window {
		if to.Sub(from) < 2*time.Second {
			return fmt.Errorf("github search window still exceeds %d repositories at one-second resolution", window)
		}
		// created:A..B is inclusive at both ends, so the halves meet one second apart.
		mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		if err := s.search(ctx, mid, to, add); err != nil {
			return err
		}
		return s.search(ctx, from, mid.Add(-time.Second), add)
	}
	seen := map[string]bool{raw: true}
	page := first
	for {
		for _, r := range page.Items {
			if err := add(r); err != nil {
				return err
			}
		}
		raw = ghsvc.NextLink(header)
		if raw == "" {
			return nil
		}
		if seen[raw] {
			return fmt.Errorf("github pagination cycle at %s", raw)
		}
		seen[raw] = true
		if page, header, err = s.searchPage(ctx, raw); err != nil {
			return err
		}
	}
}

// searchPage retries pages GitHub flags as incomplete (a search timeout)
// instead of silently accepting a partial window.
func (s *Source) searchPage(ctx context.Context, raw string) (searchResult, http.Header, error) {
	attempts := max(s.Attempts, 1)
	for i := 0; ; i++ {
		var result searchResult
		header, err := s.gh().GetJSON(ctx, raw, &result)
		if err != nil {
			return searchResult{}, nil, err
		}
		if !result.Incomplete {
			return result, header, nil
		}
		if i+1 >= attempts {
			return searchResult{}, nil, fmt.Errorf("github search returned incomplete results for %s", raw)
		}
		if err := retry.Wait(ctx, time.Duration(i+1)*2*time.Second); err != nil {
			return searchResult{}, nil, err
		}
	}
}

func repoFacts(r repo) map[string]string {
	facts := map[string]string{
		"repo_id":        strconv.FormatInt(r.ID, 10),
		"stars":          strconv.FormatInt(r.Stars, 10),
		"forks":          strconv.FormatInt(r.Forks, 10),
		"open_issues":    strconv.FormatInt(r.OpenIssues, 10),
		"fork":           strconv.FormatBool(r.Fork),
		"archived":       strconv.FormatBool(r.Archived),
		"private":        strconv.FormatBool(r.Private),
		"default_branch": r.DefaultBranch,
	}
	if r.Language != "" {
		facts["language"] = r.Language
	}
	if r.License != nil && r.License.SPDX != "" && r.License.SPDX != "NOASSERTION" {
		facts["license"] = r.License.SPDX
	}
	if !r.CreatedAt.IsZero() {
		facts["created_at"] = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return facts
}

// repoTypedFacts fills the cross-registry facts: the repository is the
// archived resource itself, so its URL is authoritative rather than a
// publisher's claim, and the last push is the package update time.
func repoTypedFacts(r repo) *registry.Facts {
	facts := &registry.Facts{}
	if url, ok := registry.NormalizeRepositoryURL(r.HTMLURL); ok {
		facts.Repository = &registry.Repository{URL: url, Source: registry.RepositoryRegistry}
	}
	if !r.PushedAt.IsZero() {
		facts.UpdatedAt = registry.Time(r.PushedAt)
	}
	return facts
}

// repoEmitter emits one repository's versions, de-duplicating by version name
// across releases, snapshots, refs and tags.
type repoEmitter struct {
	s        *Source
	r        repo
	owner    string
	name     string
	facts    map[string]string
	typed    *registry.Facts
	topics   []string
	author   *registry.Author
	versions map[string]bool
	emit     func(registry.Reference) error
}

func (s *Source) emitRepo(ctx context.Context, r repo, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	owner, name, _ := ghsvc.ParseRepo(r.FullName)
	e := &repoEmitter{s: s, r: r, owner: owner, name: name, facts: repoFacts(r), typed: repoTypedFacts(r), topics: registry.NormalizeTags(r.Topics), author: r.AuthorOf(), versions: make(map[string]bool), emit: emit}
	releases := 0
	if s.snapshot() != SnapshotOnly {
		n, err := e.releases(ctx, depth)
		if err != nil {
			return err
		}
		releases = n
	}
	mode := s.snapshot()
	if mode == SnapshotAlways || mode == SnapshotOnly || (mode == SnapshotFallback && releases == 0) {
		if err := e.snapshot(ctx, r.DefaultBranch, true); err != nil {
			return err
		}
	} else if releases == 0 && !s.Tags && len(s.Refs) == 0 {
		return fmt.Errorf("%s: %w: no releases", r.FullName, errMissing)
	}
	for _, ref := range s.Refs {
		err := e.snapshot(ctx, ref, false)
		if ghsvc.Status(err) == http.StatusNotFound && len(s.Repos) == 0 {
			slog.Warn("skipping missing GitHub ref", "repository", r.FullName, "ref", ref)
			continue
		}
		if err != nil {
			return err
		}
	}
	if s.Tags {
		return e.tags(ctx, depth)
	}
	return nil
}

// send emits one version. facts is cloned, so callers may pass shared facts;
// the archived commit is recorded on the repository fact as well as in the
// identity-bearing commit attribute.
func (e *repoEmitter) send(ctx context.Context, version, sha, kind string, metadata map[string]string, facts *registry.Facts) error {
	facts = facts.Clone()
	if facts.Repository != nil {
		facts.Repository.Commit = sha
	}
	ref := registry.Reference{Registry: "github", Namespace: e.s.namespace(), Name: e.r.FullName, Version: version, Attributes: map[string]string{"commit": sha, "download": e.s.downloadMode(e.r)}, Metadata: metadata, Facts: facts, Tags: slices.Clone(e.topics), Author: e.author.Clone(), Raw: slices.Clone(e.r.Raw)}
	ref.Metadata["kind"] = kind
	if e.r.Owner.Type != "" {
		ref.Metadata["owner_type"] = e.r.Owner.Type
	}
	if assets := metadata["assets"]; assets != "" {
		delete(ref.Metadata, "assets")
		ref.Attributes["assets"] = assets
	}
	if id := metadata["release_id"]; id != "" {
		delete(ref.Metadata, "release_id")
		ref.Attributes["release_id"] = id
	}
	if _, err := e.s.Resolve(ctx, ref); err != nil {
		return fmt.Errorf("%s@%s: %w", e.r.FullName, version, err)
	}
	if err := e.emit(ref); err != nil {
		return err
	}
	e.versions[version] = true
	return nil
}

// releases emits the selected releases and reports how many published
// releases the repository has, which decides the snapshot fallback.
func (e *repoEmitter) releases(ctx context.Context, depth registry.VersionDepth) (int, error) {
	s := e.s
	count := 0
	seen := make(map[int64]bool)
	err := ghsvc.Pages(ctx, s.gh(), s.gh().URL("repos", e.owner, e.name, "releases")+"?per_page=100", func(items []release) error {
		for _, rel := range items {
			if rel.Draft || seen[rel.ID] {
				continue
			}
			seen[rel.ID] = true
			if rel.ID < 1 || validRef(rel.TagName) != nil {
				return fmt.Errorf("%s: invalid release metadata", e.r.FullName)
			}
			published := rel.PublishedAt
			if published.IsZero() {
				published = rel.CreatedAt
			}
			if published.IsZero() {
				return fmt.Errorf("%s@%s: missing release publication date", e.r.FullName, rel.TagName)
			}
			if !depth.Since.IsZero() && published.Before(depth.Since) {
				continue
			}
			sha, err := s.tagCommit(ctx, e.owner, e.name, rel.TagName)
			if err != nil {
				return err
			}
			var assets []asset
			var downloads int64
			for _, a := range rel.Assets {
				downloads += a.Downloads
				if a.State != "uploaded" {
					slog.Warn("skipping incomplete GitHub release asset", "repository", e.r.FullName, "tag", rel.TagName, "asset", a.Name, "state", a.State)
					continue
				}
				descriptor := a.Asset
				if !digestPattern.MatchString(descriptor.Digest) {
					descriptor.Digest = ""
				}
				assets = append(assets, descriptor)
			}
			metadata := maps.Clone(e.facts)
			metadata["release_id"] = strconv.FormatInt(rel.ID, 10)
			if s.Assets && len(assets) > 0 {
				encoded, err := json.Marshal(assets)
				if err != nil {
					return err
				}
				metadata["assets"] = string(encoded)
			}
			metadata["prerelease"] = strconv.FormatBool(rel.Prerelease)
			metadata["release_date_source"] = "github-release-published"
			metadata["asset_count"] = strconv.Itoa(len(assets))
			metadata["version_rank"] = strconv.Itoa(count + 1)
			// GitHub counts asset downloads per release, so this is a
			// per-version total, not a lifetime figure for the repository.
			facts := e.typed.Clone()
			facts.ReleasedAt = registry.Time(published)
			facts.VersionDownloads = &registry.DownloadStats{Total: registry.Int64(downloads)}
			if err := e.send(ctx, rel.TagName, sha, "release", metadata, facts); err != nil {
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
		err = nil
	}
	return len(seen), err
}

// tagCommit resolves a release tag to the commit it points at, peeling
// annotated tags, so archives are pinned even if the tag later moves.
func (s *Source) tagCommit(ctx context.Context, owner, name, tag string) (string, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if _, err := s.gh().GetJSON(ctx, s.gh().URL("repos", owner, name, "git", "ref", "tags")+"/"+ghsvc.RefPath(tag), &ref); err != nil {
		return "", err
	}
	for peeled := 0; ref.Object.Type == "tag"; peeled++ {
		if peeled == 3 || !shaPattern.MatchString(ref.Object.SHA) {
			return "", fmt.Errorf("%s/%s@%s: cannot peel annotated tag", owner, name, tag)
		}
		if _, err := s.gh().GetJSON(ctx, s.gh().URL("repos", owner, name, "git", "tags", ref.Object.SHA), &ref); err != nil {
			return "", err
		}
	}
	if ref.Object.Type != "commit" || !shaPattern.MatchString(ref.Object.SHA) {
		return "", fmt.Errorf("%s/%s@%s: tag does not point at a commit", owner, name, tag)
	}
	return ref.Object.SHA, nil
}

// snapshot archives the current head of a branch or tag. The default branch
// is looked up by its branches route, which cannot be confused with a tag of
// the same name; extra refs resolve the way git does.
func (e *repoEmitter) snapshot(ctx context.Context, name string, defaultBranch bool) error {
	if e.versions[name] {
		return nil
	}
	s := e.s
	var head commit
	if defaultBranch {
		var branch struct {
			Commit commit `json:"commit"`
		}
		_, err := s.gh().GetJSON(ctx, s.gh().URL("repos", e.owner, e.name, "branches")+"/"+ghsvc.RefPath(name), &branch)
		if ghsvc.Status(err) == http.StatusNotFound {
			return fmt.Errorf("%s: %w: default branch %q not found (empty repository?)", e.r.FullName, errMissing, name)
		}
		if err != nil {
			return err
		}
		head = branch.Commit
	} else if _, err := s.gh().GetJSON(ctx, s.gh().URL("repos", e.owner, e.name, "commits")+"/"+ghsvc.RefPath(name), &head); err != nil {
		return err
	}
	if !shaPattern.MatchString(head.SHA) || head.date().IsZero() {
		return fmt.Errorf("%s@%s: invalid commit metadata", e.r.FullName, name)
	}
	metadata := maps.Clone(e.facts)
	metadata["release_date_source"] = "git-commit-date"
	facts := e.typed.Clone()
	facts.ReleasedAt = registry.Time(head.date())
	return e.send(ctx, name, head.SHA, "snapshot", metadata, facts)
}

// tags archives every remaining tag in API order. The tags route already
// reports peeled commits; dates are only looked up for a history cutoff.
func (e *repoEmitter) tags(ctx context.Context, depth registry.VersionDepth) error {
	s := e.s
	count := 0
	err := ghsvc.Pages(ctx, s.gh(), s.gh().URL("repos", e.owner, e.name, "tags")+"?per_page=100", func(items []tag) error {
		for _, t := range items {
			if e.versions[t.Name] {
				continue
			}
			if validRef(t.Name) != nil || !shaPattern.MatchString(t.Commit.SHA) {
				return fmt.Errorf("%s: invalid tag metadata", e.r.FullName)
			}
			metadata := maps.Clone(e.facts)
			facts := e.typed.Clone()
			if !depth.Since.IsZero() {
				var c commit
				if _, err := s.gh().GetJSON(ctx, s.gh().URL("repos", e.owner, e.name, "commits", t.Commit.SHA), &c); err != nil {
					return err
				}
				if c.date().IsZero() {
					return fmt.Errorf("%s@%s: missing commit date", e.r.FullName, t.Name)
				}
				if c.date().Before(depth.Since) {
					continue
				}
				facts.ReleasedAt = registry.Time(c.date())
				metadata["release_date_source"] = "git-commit-date"
			}
			if err := e.send(ctx, t.Name, t.Commit.SHA, "tag", metadata, facts); err != nil {
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
		err = nil
	}
	return err
}

func (s *Source) codeload() string {
	if s.CodeloadEndpoint != "" {
		return strings.TrimRight(s.CodeloadEndpoint, "/")
	}
	return "https://codeload.github.com"
}

func (s *Source) releaseHost() string {
	if s.ReleaseEndpoint != "" {
		return strings.TrimRight(s.ReleaseEndpoint, "/")
	}
	return "https://github.com"
}

// Resolve derives every download URL from validated reference fields. A
// tampered inventory therefore cannot point the token-bearing client at
// arbitrary API routes or hosts.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	owner, name, err := ghsvc.ParseRepo(ref.Name)
	if ref.Registry != "github" || err != nil || validRef(ref.Version) != nil {
		return registry.Artifact{}, fmt.Errorf("invalid GitHub reference")
	}
	sha := ref.Attributes["commit"]
	if !shaPattern.MatchString(sha) {
		return registry.Artifact{}, fmt.Errorf("invalid GitHub commit SHA")
	}
	mode := ref.Attributes["download"]
	if mode != downloadDirect && mode != downloadAPI {
		return registry.Artifact{}, fmt.Errorf("invalid GitHub download mode %q", mode)
	}
	archive := registry.Blob{Name: name + "-" + sha + ".zip", MediaType: "application/zip", Size: -1}
	if mode == downloadAPI {
		archive.Open = s.apiOpener(s.gh().URL("repos", owner, name, "zipball", sha), "application/vnd.github+json")
	} else {
		archive.Open = s.directOpener(s.codeload() + "/" + owner + "/" + name + "/zip/" + sha)
	}
	blobs := []registry.Blob{archive}
	if raw := ref.Attributes["assets"]; raw != "" {
		var assets []asset
		if err := json.Unmarshal([]byte(raw), &assets); err != nil {
			return registry.Artifact{}, fmt.Errorf("invalid GitHub asset list: %w", err)
		}
		seen := map[string]bool{archive.Name: true}
		for _, a := range assets {
			if a.ID < 1 || !validBlobName(a.Name) || seen[a.Name] || a.Size < 0 || (a.Digest != "" && !digestPattern.MatchString(a.Digest)) {
				return registry.Artifact{}, fmt.Errorf("invalid GitHub release asset %q", a.Name)
			}
			seen[a.Name] = true
			blob := registry.Blob{Name: a.Name, MediaType: a.ContentType, Digest: a.Digest, Size: a.Size}
			if blob.MediaType == "" {
				blob.MediaType = "application/octet-stream"
			}
			if mode == downloadAPI {
				blob.Open = s.apiOpener(s.gh().URL("repos", owner, name, "releases", "assets", strconv.FormatInt(a.ID, 10)), "application/octet-stream")
			} else {
				blob.Open = s.directOpener(s.releaseHost() + "/" + owner + "/" + name + "/releases/download/" + ghsvc.RefPath(ref.Version) + "/" + url.PathEscape(a.Name))
			}
			blobs = append(blobs, blob)
		}
	}
	return registry.Artifact{Reference: ref, Revision: sha, Blobs: blobs}, nil
}

// apiOpener downloads through the API origin with the token; GitHub redirects
// to a download host and Go drops the Authorization header on the way.
func (s *Source) apiOpener(raw, accept string) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		response, err := s.gh().Do(ctx, raw, accept, 0)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}
}

// directOpener downloads from public hosts without a token or API quota.
func (s *Source) directOpener(raw string) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}
}

// gh is the shared API client configured from the source's fields.
func (s *Source) gh() *ghsvc.Client {
	return &ghsvc.Client{HTTP: s.Client, Endpoint: s.Endpoint, Token: s.Token, Attempts: s.Attempts}
}
