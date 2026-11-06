package registry

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Facts are typed listing facts that mean the same thing in every registry.
// Like Metadata they are mutable observations excluded from artifact identity.
// A nil field means the registry does not publish that fact. Facts that only
// one registry has, such as star counts or rating histograms, stay in
// Reference.Metadata.
type Facts struct {
	// Downloads counts downloads of the whole package across all versions.
	Downloads *DownloadStats `json:"downloads,omitempty"`
	// VersionDownloads counts downloads of this version alone.
	VersionDownloads *DownloadStats `json:"version_downloads,omitempty"`
	// Installs is the package's current install base or active user count.
	Installs *int64 `json:"installs,omitempty"`
	// Repository is the package's source repository.
	Repository *Repository `json:"repository,omitempty"`
	// ReleasedAt is when this version was published.
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	// UpdatedAt is when the package last changed in the registry.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// DownloadStats are download counts exactly as the registry published them at
// listing time: Total since publication, and the others over trailing windows
// of 1, 7, 30, 90 and 365 days ending at listing time. A nil field means the
// registry does not publish that window; zero means it reported zero. Adapters
// must not derive one window from another.
type DownloadStats struct {
	Total   *int64 `json:"total,omitempty"`
	Day     *int64 `json:"day,omitempty"`
	Week    *int64 `json:"week,omitempty"`
	Month   *int64 `json:"month,omitempty"`
	Quarter *int64 `json:"quarter,omitempty"`
	Year    *int64 `json:"year,omitempty"`
}

// Repository locates a package's source repository. URL is the canonical form
// produced by NormalizeRepositoryURL, so the same repository compares equal
// across registries. Source says how the registry knows the repository, since
// in most registries it is an unverified claim by the publisher.
type Repository struct {
	URL string `json:"url"`
	// Directory is the package's subdirectory within a monorepo, if any.
	Directory string `json:"directory,omitempty"`
	// Commit is the commit the version was built from, when known.
	Commit string `json:"commit,omitempty"`
	Source string `json:"source"`
}

// Repository sources.
const (
	RepositoryDeclared = "declared" // self-declared by the publisher, unverified
	RepositoryAttested = "attested" // tied to the artifact by provenance attestations
	RepositoryRegistry = "registry" // the registry itself hosts the repository
)

// Int64 returns a pointer to v, for populating optional counts.
func Int64(v int64) *int64 { return &v }

// Time returns a pointer to t in UTC, for populating optional timestamps.
func Time(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

// Clone deep-copies the facts so emitted references share nothing mutable.
func (f *Facts) Clone() *Facts {
	if f == nil {
		return nil
	}
	c := Facts{Downloads: f.Downloads.clone(), VersionDownloads: f.VersionDownloads.clone(), Installs: cloneInt64(f.Installs), ReleasedAt: cloneTime(f.ReleasedAt), UpdatedAt: cloneTime(f.UpdatedAt)}
	if f.Repository != nil {
		r := *f.Repository
		c.Repository = &r
	}
	return &c
}

func (d *DownloadStats) clone() *DownloadStats {
	if d == nil {
		return nil
	}
	return &DownloadStats{Total: cloneInt64(d.Total), Day: cloneInt64(d.Day), Week: cloneInt64(d.Week), Month: cloneInt64(d.Month), Quarter: cloneInt64(d.Quarter), Year: cloneInt64(d.Year)}
}
func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	return Int64(*v)
}
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return Time(*t)
}

// Validate rejects facts no registry can truthfully publish: negative counts,
// zero timestamps, and repositories without a URL or with an unknown source.
func (f *Facts) Validate() error {
	if f == nil {
		return nil
	}
	for name, stats := range map[string]*DownloadStats{"downloads": f.Downloads, "version_downloads": f.VersionDownloads} {
		if stats == nil {
			continue
		}
		for _, count := range []*int64{stats.Total, stats.Day, stats.Week, stats.Month, stats.Quarter, stats.Year} {
			if count != nil && *count < 0 {
				return fmt.Errorf("%s: negative download count", name)
			}
		}
	}
	if f.Installs != nil && *f.Installs < 0 {
		return fmt.Errorf("installs: negative count")
	}
	for name, t := range map[string]*time.Time{"released_at": f.ReleasedAt, "updated_at": f.UpdatedAt} {
		if t != nil && t.IsZero() {
			return fmt.Errorf("%s: zero timestamp", name)
		}
	}
	if r := f.Repository; r != nil {
		if r.URL == "" {
			return fmt.Errorf("repository: missing URL")
		}
		switch r.Source {
		case RepositoryDeclared, RepositoryAttested, RepositoryRegistry:
		default:
			return fmt.Errorf("repository: unknown source %q", r.Source)
		}
	}
	return nil
}

// NormalizeRepositoryURL converts the forms registries accept for a source
// repository into one canonical https URL without a trailing slash or .git
// suffix, so repositories compare equal across registries. It accepts plain
// URLs, git+https and git+ssh URLs, scp-style git@host:path, the npm
// shorthands github:owner/name, gitlab:owner/name and bitbucket:owner/name,
// and bare owner/name (GitHub). It reports false for anything else.
func NormalizeRepositoryURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	for prefix, host := range map[string]string{"github:": "github.com", "gitlab:": "gitlab.com", "bitbucket:": "bitbucket.org"} {
		if rest, ok := strings.CutPrefix(raw, prefix); ok {
			raw = "https://" + host + "/" + rest
		}
	}
	raw = strings.TrimPrefix(raw, "git+")
	if at := strings.TrimPrefix(raw, "git@"); at != raw && !strings.Contains(at, "://") {
		// scp syntax: host:path
		host, path, ok := strings.Cut(at, ":")
		if !ok {
			return "", false
		}
		raw = "https://" + host + "/" + path
	}
	if !strings.Contains(raw, "://") {
		parts := strings.Split(raw, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(raw, ":@ ") {
			return "", false
		}
		raw = "https://github.com/" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
	default:
		return "", false
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	if path == "" {
		return "", false
	}
	return "https://" + strings.ToLower(u.Hostname()) + path, true
}

// FactKind says how a typed fact compares.
type FactKind int

const (
	FactNumber FactKind = iota + 1
	FactDate
	FactString
)

// Fact is one typed fact value, addressed by a dotted path such as
// downloads.total, released_at, repository.url or author.username.
type Fact struct {
	Kind   FactKind
	Number float64
	Date   time.Time
	String string
}

type factAccessor struct {
	kind FactKind
	get  func(Reference) (Fact, bool)
}

var factAccessors = func() map[string]factAccessor {
	number := func(pick func(*Facts) *int64) factAccessor {
		return factAccessor{FactNumber, func(r Reference) (Fact, bool) {
			if r.Facts == nil || pick(r.Facts) == nil {
				return Fact{}, false
			}
			return Fact{Kind: FactNumber, Number: float64(*pick(r.Facts))}, true
		}}
	}
	date := func(pick func(*Facts) *time.Time) factAccessor {
		return factAccessor{FactDate, func(r Reference) (Fact, bool) {
			if r.Facts == nil || pick(r.Facts) == nil {
				return Fact{}, false
			}
			return Fact{Kind: FactDate, Date: *pick(r.Facts)}, true
		}}
	}
	repo := func(pick func(*Repository) string) factAccessor {
		return factAccessor{FactString, func(r Reference) (Fact, bool) {
			if r.Facts == nil || r.Facts.Repository == nil || pick(r.Facts.Repository) == "" {
				return Fact{}, false
			}
			return Fact{Kind: FactString, String: pick(r.Facts.Repository)}, true
		}}
	}
	author := func(pick func(*Author) string) factAccessor {
		return factAccessor{FactString, func(r Reference) (Fact, bool) {
			if r.Author == nil || pick(r.Author) == "" {
				return Fact{}, false
			}
			return Fact{Kind: FactString, String: pick(r.Author)}, true
		}}
	}
	paths := map[string]factAccessor{
		"installs":             number(func(f *Facts) *int64 { return f.Installs }),
		"released_at":          date(func(f *Facts) *time.Time { return f.ReleasedAt }),
		"updated_at":           date(func(f *Facts) *time.Time { return f.UpdatedAt }),
		"repository.url":       repo(func(r *Repository) string { return r.URL }),
		"repository.directory": repo(func(r *Repository) string { return r.Directory }),
		"repository.commit":    repo(func(r *Repository) string { return r.Commit }),
		"repository.source":    repo(func(r *Repository) string { return r.Source }),
		"author.name":          author(func(a *Author) string { return a.Name }),
		"author.username":      author(func(a *Author) string { return a.Username }),
		"author.id":            author(func(a *Author) string { return a.ID }),
		"author.url":           author(func(a *Author) string { return a.URL }),
		"author.email":         author(func(a *Author) string { return a.Email }),
	}
	stats := map[string]func(*Facts) *DownloadStats{
		"downloads":         func(f *Facts) *DownloadStats { return f.Downloads },
		"version_downloads": func(f *Facts) *DownloadStats { return f.VersionDownloads },
	}
	windows := map[string]func(*DownloadStats) *int64{
		"total":   func(d *DownloadStats) *int64 { return d.Total },
		"day":     func(d *DownloadStats) *int64 { return d.Day },
		"week":    func(d *DownloadStats) *int64 { return d.Week },
		"month":   func(d *DownloadStats) *int64 { return d.Month },
		"quarter": func(d *DownloadStats) *int64 { return d.Quarter },
		"year":    func(d *DownloadStats) *int64 { return d.Year },
	}
	for scope, pickStats := range stats {
		for window, pickWindow := range windows {
			paths[scope+"."+window] = number(func(f *Facts) *int64 {
				d := pickStats(f)
				if d == nil {
					return nil
				}
				return pickWindow(d)
			})
		}
	}
	return paths
}()

// FactKindOf reports how the fact at path compares, or zero when path is not
// a typed fact. Hosts use it to validate order and filter specifications.
func FactKindOf(path string) FactKind {
	return factAccessors[path].kind
}

// FactPaths lists every typed fact path in sorted order.
func FactPaths() []string {
	paths := make([]string, 0, len(factAccessors))
	for path := range factAccessors {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Fact returns the typed fact at path, which addresses Facts fields such as
// downloads.total or released_at and Author fields such as author.username.
// It reports false when the reference does not carry that fact or the path is
// not a typed fact.
func (r Reference) Fact(path string) (Fact, bool) {
	accessor, ok := factAccessors[path]
	if !ok {
		return Fact{}, false
	}
	return accessor.get(r)
}
