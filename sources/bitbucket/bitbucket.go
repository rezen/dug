// Package bitbucket archives Bitbucket Cloud repositories: tag archives and a
// main-branch snapshot, selected by workspace (the tenant) or by repository.
package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services"
)

// Identity is the registry name the host selects: `dug list bitbucket`.
const Identity = "bitbucket"

// Namespace records where artifacts came from; manifests persist it.
const Namespace = "bitbucket.org"

// DefaultEndpoint is the REST API base, with a trailing slash.
const DefaultEndpoint = "https://api.bitbucket.org/2.0/"

// DefaultDownloadBase is where archives are served. Resolve derives every
// download URL from validated IDs under this base; none is persisted.
const DefaultDownloadBase = "https://bitbucket.org/"

// DefaultDownloadHosts is where the registry serves artifact bytes.
var DefaultDownloadHosts = []string{"bitbucket.org"}

// Snapshot modes.
const (
	SnapshotFallback = "fallback" // main branch only for repositories without tags
	SnapshotAlways   = "always"
	SnapshotOnly     = "only"
	SnapshotNever    = "never"
)

const (
	maxMetadataBytes = 32 << 20
	pageLen          = 100 // Bitbucket rejects larger pagelen values with HTTP 400
)

// Source archives Bitbucket Cloud repositories.
type Source struct {
	Client   *http.Client
	Endpoint string // REST API base override for tests; empty means DefaultEndpoint
	// DownloadBase overrides DefaultDownloadBase; tests point it at httptest.
	DownloadBase string
	Attempts     int // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts overrides DefaultDownloadHosts; tests point it at httptest.
	DownloadHosts []string
	// Token is sent as a bearer token to the API and download origins only,
	// over HTTPS or loopback. It is never written to references.
	Token string

	// Query options. PluginFactory validates them; Discover re-checks.
	Workspaces   []string
	Repos        []string // workspace/slug
	Forks        bool
	Snapshot     string // empty means SnapshotFallback
	VersionDepth string // empty means the current version only
}

var _ registry.Registry = (*Source)(nil)

var errStop = errors.New("version depth reached")

var (
	slugPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// repo is one repository document from the API.
type repo struct {
	FullName    string    `json:"full_name"`
	Slug        string    `json:"slug"`
	SCM         string    `json:"scm"`
	UUID        string    `json:"uuid"`
	IsPrivate   bool      `json:"is_private"`
	Language    string    `json:"language"`
	Size        int64     `json:"size"`
	CreatedOn   time.Time `json:"created_on"`
	UpdatedOn   time.Time `json:"updated_on"`
	Description string    `json:"description"`
	MainBranch  *struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
	Parent *struct {
		FullName string `json:"full_name"`
	} `json:"parent"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Workspace struct {
		Slug  string `json:"slug"`
		Name  string `json:"name"`
		UUID  string `json:"uuid"`
		Links struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
	} `json:"workspace"`
}

// ref is a tag or branch with the commit it points at.
type ref struct {
	Name   string `json:"name"`
	Target struct {
		Hash string    `json:"hash"`
		Date time.Time `json:"date"`
	} `json:"target"`
}

type page[T any] struct {
	Size   int    `json:"size"`
	Next   string `json:"next"`
	Values []T    `json:"values"`
}

func (s *Source) base() string {
	if s.Endpoint != "" {
		return strings.TrimRight(s.Endpoint, "/") + "/"
	}
	return DefaultEndpoint
}

func (s *Source) downloadBase() string {
	if s.DownloadBase != "" {
		return strings.TrimRight(s.DownloadBase, "/") + "/"
	}
	return DefaultDownloadBase
}

func (s *Source) attempts() int {
	if s.Attempts > 0 {
		return s.Attempts
	}
	return 4
}

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

func (s *Source) snapshot() string {
	if s.Snapshot == "" {
		return SnapshotFallback
	}
	return s.Snapshot
}

// validSlug accepts a workspace or repository slug.
func validSlug(slug string) bool {
	return slugPattern.MatchString(slug) && slug != ".." && !strings.HasSuffix(slug, ".git")
}

// ParseRepo splits and validates workspace/slug.
func ParseRepo(full string) (workspace, slug string, err error) {
	workspace, slug, ok := strings.Cut(full, "/")
	if !ok || !validSlug(workspace) || !validSlug(slug) {
		return "", "", fmt.Errorf("invalid Bitbucket repository %q (want workspace/slug)", full)
	}
	return workspace, slug, nil
}

func (s *Source) validate() error {
	if len(s.Workspaces) == 0 && len(s.Repos) == 0 {
		return fmt.Errorf("bitbucket needs workspaces or repos; the public catalog cannot be enumerated")
	}
	for _, w := range s.Workspaces {
		if !validSlug(w) {
			return fmt.Errorf("invalid Bitbucket workspace %q", w)
		}
	}
	for _, r := range s.Repos {
		if _, _, err := ParseRepo(r); err != nil {
			return err
		}
	}
	switch s.snapshot() {
	case SnapshotFallback, SnapshotAlways, SnapshotOnly, SnapshotNever:
	default:
		return fmt.Errorf("bitbucket snapshot must be fallback, always, only, or never")
	}
	return nil
}

// Discover walks the selected repositories and emits one reference per
// version, current first. Every reference is self-resolved before emission.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	if err := s.validate(); err != nil {
		return err
	}
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	seen := make(map[string]bool) // repository uuid
	visit := func(r repo, explicit bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.UUID == "" || seen[r.UUID] {
			return nil
		}
		seen[r.UUID] = true
		err := s.emitRepo(ctx, r, depth, explicit, emit)
		if err != nil && !explicit && isSkippable(err) {
			slog.Warn("bitbucket: skipping repository", "repository", r.FullName, "error", err)
			return nil
		}
		return err
	}
	for _, full := range s.Repos {
		workspace, slug, _ := ParseRepo(full)
		var r repo
		if err := s.query(ctx, s.base()+"repositories/"+workspace+"/"+slug, &r); err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
		if err := visit(r, true); err != nil {
			return err
		}
	}
	for _, workspace := range s.Workspaces {
		if err := s.walkWorkspace(ctx, workspace, visit); err != nil {
			return err
		}
	}
	return nil
}

// walkWorkspace lists a workspace's repositories ordered by creation time, an
// immutable field, so paging is stable while repositories are pushed to.
func (s *Source) walkWorkspace(ctx context.Context, workspace string, visit func(repo, bool) error) error {
	raw := s.base() + "repositories/" + workspace + "?pagelen=" + strconv.Itoa(pageLen) + "&sort=created_on"
	total, count := -1, 0
	err := walk(ctx, s, raw, func(p page[repo]) error {
		if total < 0 {
			total = p.Size
		}
		for _, r := range p.Values {
			count++
			if r.Workspace.Slug != "" && !strings.EqualFold(r.Workspace.Slug, workspace) {
				return fmt.Errorf("bitbucket workspace %s listed a repository of %s", workspace, r.Workspace.Slug)
			}
			if r.SCM != "git" {
				slog.Warn("bitbucket: skipping non-git repository", "repository", r.FullName, "scm", r.SCM)
				continue
			}
			if r.Parent != nil && !s.Forks {
				continue
			}
			if err := visit(r, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if total >= 0 && count < total {
		slog.Warn("bitbucket: workspace listing ended early; repositories may have been deleted during the walk", "workspace", workspace, "listed", count, "reported", total)
	}
	return nil
}

// walk follows next links from raw, calling fn once per page.
func walk[T any](ctx context.Context, s *Source, raw string, fn func(page[T]) error) error {
	seenPages := map[string]bool{raw: true}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p page[T]
		if err := s.query(ctx, raw, &p); err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
		if p.Next == "" {
			return nil
		}
		next, err := s.next(p.Next, seenPages)
		if err != nil {
			return err
		}
		raw = next
	}
}

// isSkippable reports whether a repository-level failure may be skipped
// during a workspace walk: the repository vanished or is not accessible.
func isSkippable(err error) bool {
	var status *httpx.StatusError
	return errors.As(err, &status) && (status.Code == http.StatusNotFound || status.Code == http.StatusForbidden)
}

// emitRepo emits tag versions newest first, then a main-branch snapshot
// according to the snapshot mode.
func (s *Source) emitRepo(ctx context.Context, r repo, depth registry.VersionDepth, explicit bool, emit func(registry.Reference) error) error {
	workspace, slug, err := ParseRepo(r.FullName)
	if err != nil {
		return err
	}
	if r.SCM != "git" {
		return fmt.Errorf("%s: unsupported scm %q", r.FullName, r.SCM)
	}
	if r.UpdatedOn.IsZero() || r.UUID == "" {
		return fmt.Errorf("%s: incomplete repository metadata", r.FullName)
	}
	facts := &registry.Facts{UpdatedAt: registry.Time(r.UpdatedOn)}
	metadata := map[string]string{
		"private":    strconv.FormatBool(r.IsPrivate),
		"fork":       strconv.FormatBool(r.Parent != nil),
		"repo_size":  strconv.FormatInt(r.Size, 10),
		"created_at": r.CreatedOn.UTC().Format(time.RFC3339),
		"repo_uuid":  r.UUID,
	}
	if r.Language != "" {
		metadata["language"] = r.Language
	}
	if r.MainBranch != nil {
		metadata["default_branch"] = r.MainBranch.Name
	}
	author := registry.Author{Username: workspace, Name: r.Workspace.Name, ID: r.Workspace.UUID, URL: r.Workspace.Links.HTML.Href}
	repoURL, _ := registry.NormalizeRepositoryURL(r.Links.HTML.Href)

	versions := map[string]bool{}
	count := 0
	add := func(kind string, v ref, source string) error {
		if versions[v.Name] {
			return nil
		}
		// Depth bounds tags; a snapshot is the current head and is never cut off.
		if kind == "tag" && !depth.Since.IsZero() && v.Target.Date.Before(depth.Since) {
			return errStop
		}
		versions[v.Name] = true
		count++
		versionMetadata := maps.Clone(metadata)
		versionMetadata["kind"] = kind
		versionMetadata["version_rank"] = strconv.Itoa(count)
		versionMetadata["release_date_source"] = source
		versionFacts := facts.Clone()
		versionFacts.ReleasedAt = registry.Time(v.Target.Date)
		if repoURL != "" {
			versionFacts.Repository = &registry.Repository{URL: repoURL, Source: registry.RepositoryRegistry, Commit: v.Target.Hash}
		}
		reference := registry.Reference{
			Registry:   Identity,
			Namespace:  Namespace,
			Name:       workspace + "/" + slug,
			Version:    v.Name,
			Attributes: map[string]string{"commit": v.Target.Hash, "kind": kind},
			Metadata:   versionMetadata,
			Facts:      versionFacts,
			Author:     author.Clone(),
		}
		if _, err := s.Resolve(ctx, reference); err != nil {
			return fmt.Errorf("%s@%s: %w", r.FullName, v.Name, err)
		}
		if err := emit(reference); err != nil {
			return err
		}
		if depth.Max > 0 && count >= depth.Max && kind == "tag" {
			return errStop
		}
		return nil
	}

	mode := s.snapshot()
	tags := 0
	if mode != SnapshotOnly {
		tagsURL := s.base() + "repositories/" + workspace + "/" + slug + "/refs/tags?pagelen=" + strconv.Itoa(pageLen) + "&sort=-target.date"
		err := walk(ctx, s, tagsURL, func(p page[ref]) error {
			for _, tag := range p.Values {
				if tag.Name == "" || !commitPattern.MatchString(tag.Target.Hash) || tag.Target.Date.IsZero() {
					return fmt.Errorf("%s: invalid tag %q", r.FullName, tag.Name)
				}
				tags++
				if err := add("tag", tag, "git-commit-date"); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return err
		}
	}
	if mode == SnapshotNever || (mode == SnapshotFallback && tags > 0) {
		if tags == 0 && mode == SnapshotNever {
			if explicit {
				return fmt.Errorf("%s has no tags and snapshot=never", r.FullName)
			}
			slog.Warn("bitbucket: repository has no tags and snapshot=never", "repository", r.FullName)
		}
		return nil
	}
	if r.MainBranch == nil || r.MainBranch.Name == "" {
		if explicit {
			return fmt.Errorf("%s has no main branch (empty repository)", r.FullName)
		}
		slog.Warn("bitbucket: skipping empty repository", "repository", r.FullName)
		return nil
	}
	var head ref
	if err := s.query(ctx, s.base()+"repositories/"+workspace+"/"+slug+"/refs/branches/"+url.PathEscape(r.MainBranch.Name), &head); err != nil {
		return fmt.Errorf("%s: main branch: %w", r.FullName, err)
	}
	if head.Name == "" {
		head.Name = r.MainBranch.Name
	}
	if !commitPattern.MatchString(head.Target.Hash) || head.Target.Date.IsZero() {
		return fmt.Errorf("%s: invalid main branch head", r.FullName)
	}
	return add("snapshot", head, "git-commit-date")
}

// next validates a pagination link: same origin and path prefix as the API
// base, and not seen before.
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

// bearer adds the token to requests for the API and download origins only.
// Redirect targets are separate round trips, so the token never follows a
// redirect to another host.
type bearer struct {
	base   http.RoundTripper
	token  string
	hosts  map[string]bool
	secure func(*url.URL) bool
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	if b.hosts[req.URL.Host] && b.secure(req.URL) {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(req)
}

// client returns the HTTP client for upstream requests: the shared client, or
// a token-bearing wrapper around its transport.
func (s *Source) client() *http.Client {
	client := s.Client
	if client == nil {
		client = services.DefaultDugHttpClient()
	}
	if s.Token == "" {
		return client
	}
	hosts := map[string]bool{}
	for _, raw := range []string{s.base(), s.downloadBase()} {
		if u, err := url.Parse(raw); err == nil {
			hosts[u.Host] = true
		}
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	wrapped := *client
	wrapped.Transport = bearer{base: transport, token: s.Token, hosts: hosts, secure: func(u *url.URL) bool {
		if u.Scheme == "https" {
			return true
		}
		host := u.Hostname()
		return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
	}}
	return &wrapped
}

// query fetches one bounded metadata document. Decode failures are permanent.
func (s *Source) query(ctx context.Context, raw string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.client(), raw, s.attempts())
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

// Resolve trusts nothing in the reference: the download URL is derived from the
// validated workspace, slug and commit, so a tampered inventory cannot point
// the token-bearing client elsewhere. It makes no network requests.
func (s *Source) Resolve(ctx context.Context, reference registry.Reference) (registry.Artifact, error) {
	if reference.Registry != Identity || reference.Version == "" {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	workspace, slug, err := ParseRepo(reference.Name)
	if err != nil {
		return registry.Artifact{}, err
	}
	commit := reference.Attributes["commit"]
	if !commitPattern.MatchString(commit) {
		return registry.Artifact{}, fmt.Errorf("invalid %s commit %q", Identity, commit)
	}
	if kind := reference.Attributes["kind"]; kind != "tag" && kind != "snapshot" {
		return registry.Artifact{}, fmt.Errorf("invalid %s kind %q", Identity, kind)
	}
	raw := s.downloadBase() + workspace + "/" + slug + "/get/" + commit + ".tar.gz"
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	// Bitbucket publishes no digest or size for archives; the host hashes them.
	return registry.Artifact{Reference: reference, Revision: commit, Blobs: []registry.Blob{{
		Name:      slug + "-" + commit[:12] + ".tar.gz",
		MediaType: "application/gzip",
		Size:      -1,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			response, err := httpx.Get(ctx, s.client(), raw, s.attempts())
			if err != nil {
				return nil, err
			}
			return response.Body, nil
		},
	}}}, nil
}
