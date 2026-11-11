// Package jenkins archives Jenkins plugins (.hpi) published through the
// Jenkins update center.
package jenkins

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
)

// Identity is the registry name the host selects: `dug list jenkins`.
const Identity = "jenkins"

// Namespace records where artifacts came from; manifests persist it.
const Namespace = "updates.jenkins.io"

// DefaultEndpoint is the update center directory, with a trailing slash. It
// holds update-center.actual.json (the current version of every plugin) and
// plugin-versions.json (every published version).
const DefaultEndpoint = "https://updates.jenkins.io/current/"

// DefaultDownloadHosts is where the update center serves artifact bytes. The
// persisted URL is always on updates.jenkins.io; it redirects to get.jenkins.io
// and then to a mirror, which Resolve never sees. Resolve refuses persisted
// download URLs on any other host, so a tampered inventory cannot steer this
// process at arbitrary servers.
var DefaultDownloadHosts = []string{"updates.jenkins.io"}

const (
	maxCatalogBytes  = 32 << 20
	maxVersionsBytes = 128 << 20 // plugin-versions.json was 24 MiB in 2026-11 and grows
)

type Source struct {
	Client   *http.Client
	Endpoint string // update center directory override for tests; empty means DefaultEndpoint
	Attempts int    // upstream HTTP attempts per request; 0 means 4
	// DownloadHosts overrides DefaultDownloadHosts; tests point it at httptest.
	DownloadHosts []string
	// Roots overrides the embedded Jenkins update center root certificates
	// that signed documents must chain to; tests supply their own.
	Roots *x509.CertPool
	// Now overrides the clock used for certificate validity; nil means time.Now.
	Now func() time.Time

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Plugins      []string
	VersionDepth string // empty means the current release only
}

var _ registry.Registry = (*Source)(nil)

var errStop = errors.New("version depth reached")

// nameRE is the plugin short name: Jenkins artifact IDs.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// release is one published plugin version. plugin-versions.json carries every
// field except Size; update-center.actual.json adds Size for the current one.
type release struct {
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	URL          string    `json:"url"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	Released     time.Time `json:"releaseTimestamp"`
	RequiredCore string    `json:"requiredCore"`
}

// entry is a catalog entry: the current release plus package-wide facts.
type entry struct {
	release
	Popularity *int64   `json:"popularity"`
	Health     *int64   `json:"health"`
	SCM        string   `json:"scm"`
	Labels     []string `json:"labels"`
	Developers []struct {
		ID   string `json:"developerId"`
		Name string `json:"name"`
	} `json:"developers"`
}

type catalog struct {
	Plugins      map[string]entry `json:"plugins"`
	Warnings     []warning        `json:"warnings"`
	Deprecations map[string]struct {
		URL string `json:"url"`
	} `json:"deprecations"`
}

// warning is a security advisory. Its patterns match whole version strings
// (Java Matcher.matches semantics) and cover releases anywhere in history.
type warning struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Versions []struct {
		Pattern string `json:"pattern"`
	} `json:"versions"`
}

// advisories is what the update center says about one plugin: the advisories
// with a compiled pattern each, and its deprecation notice if any.
type advisories struct {
	warnings      []compiledWarning
	deprecated    bool
	deprecatedURL string
}

type compiledWarning struct {
	id       string
	patterns []versionPattern
}

// versionPattern is a Java whole-string version regex. Go's regexp has no
// lookahead, but the update center uses exactly one form of it, a leading
// negative lookahead `(?!X)REST` (six patterns in 2026-11): the version must
// match REST entirely and X must not match at its start. Anything else
// Go cannot parse is an error rather than a guess.
type versionPattern struct {
	deny  *regexp.Regexp // prefix match that excludes the version; nil when absent
	allow *regexp.Regexp
}

func compilePattern(pattern string) (versionPattern, error) {
	var p versionPattern
	if rest, ok := strings.CutPrefix(pattern, "(?!"); ok {
		end := closingParen(rest)
		if end < 0 {
			return p, fmt.Errorf("unbalanced lookahead in %q", pattern)
		}
		deny, err := regexp.Compile("^(?:" + rest[:end] + ")")
		if err != nil {
			return p, err
		}
		p.deny, pattern = deny, rest[end+1:]
	}
	allow, err := regexp.Compile("^(?:" + pattern + ")$")
	p.allow = allow
	return p, err
}

// closingParen returns the index of the ")" that closes a group whose "(" was
// just consumed, skipping escapes and character classes, or -1.
func closingParen(s string) int {
	depth, inClass := 1, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p versionPattern) matches(version string) bool {
	return p.allow.MatchString(version) && (p.deny == nil || !p.deny.MatchString(version))
}

// advisoriesFor compiles only the patterns of the plugin being listed, so an
// unparsable pattern for an unrelated plugin cannot fail a whole walk.
func (c catalog) advisoriesFor(name string) (advisories, error) {
	var a advisories
	if d, ok := c.Deprecations[name]; ok {
		a.deprecated, a.deprecatedURL = true, d.URL
	}
	for _, w := range c.Warnings {
		if w.Type != "plugin" || w.Name != name {
			continue
		}
		cw := compiledWarning{id: w.ID}
		for _, v := range w.Versions {
			p, err := compilePattern(v.Pattern)
			if err != nil {
				return a, retry.Permanent(fmt.Errorf("%s warning %s: %w", Identity, w.ID, err))
			}
			cw.patterns = append(cw.patterns, p)
		}
		a.warnings = append(a.warnings, cw)
	}
	return a, nil
}

// affecting returns the sorted advisory IDs whose patterns match the version.
func (a advisories) affecting(version string) []string {
	var ids []string
	for _, w := range a.warnings {
		for _, p := range w.patterns {
			if p.matches(version) {
				ids = append(ids, w.id)
				break
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

type history struct {
	Plugins map[string]map[string]release `json:"plugins"`
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

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

// validName rejects values that could escape a URL path or a file name.
func validName(name string) error {
	if !nameRE.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid %s plugin name %q", Identity, name)
	}
	return nil
}

func validVersion(version string) error {
	if version == "" || strings.ContainsAny(version, "/\\?# \x00\t\n\r") {
		return fmt.Errorf("invalid %s version %q", Identity, version)
	}
	return nil
}

// Discover walks the whole update center, or only Plugins when set, and emits
// one reference per selected version, current first. Every reference is
// self-resolved before emission so a listing never persists something stage
// two would reject.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	var cat catalog
	if err := s.query(ctx, s.base()+"update-center.actual.json", maxCatalogBytes, &cat); err != nil {
		return err
	}
	if len(cat.Plugins) == 0 {
		return retry.Permanent(fmt.Errorf("%s update center lists no plugins", Identity))
	}
	names := slices.Clone(s.Plugins)
	if len(names) == 0 {
		names = slices.Sorted(maps.Keys(cat.Plugins))
	}
	for _, name := range names {
		if err := validName(name); err != nil {
			return err
		}
		if _, ok := cat.Plugins[name]; !ok {
			return fmt.Errorf("%s plugin %q is not in the update center", Identity, name)
		}
	}
	// plugin-versions.json is 24 MiB; skip it when only current releases are
	// wanted, which is the default.
	var past history
	if depth.Max != 1 {
		if err := s.query(ctx, s.base()+"plugin-versions.json", maxVersionsBytes, &past); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		p := cat.Plugins[name]
		if p.Name != name {
			return fmt.Errorf("%s update center entry %q is named %q", Identity, name, p.Name)
		}
		adv, err := cat.advisoriesFor(name)
		if err != nil {
			return err
		}
		if err := s.emitVersions(ctx, p, adv, past.Plugins[name], depth, emit); err != nil {
			return err
		}
	}
	return nil
}

// emitVersions emits the current release first, then older ones newest first
// while the depth allows. The update center orders nothing by version, and
// version strings are not semver, so release time is the order.
func (s *Source) emitVersions(ctx context.Context, p entry, adv advisories, past map[string]release, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	// Package-wide facts are a listing-time snapshot attached to every version.
	// popularity is the install count Jenkins controllers report, so it maps to
	// Installs; the update center publishes no download counts.
	facts := &registry.Facts{Installs: p.Popularity}
	if !p.Released.IsZero() {
		facts.UpdatedAt = registry.Time(p.Released)
	}
	if repo, ok := registry.NormalizeRepositoryURL(p.SCM); ok {
		facts.Repository = &registry.Repository{URL: repo, Source: registry.RepositoryDeclared}
	}
	// The update center's deprecation list is exhaustive, so false is a fact.
	metadata := map[string]string{"deprecated": strconv.FormatBool(adv.deprecated)}
	if adv.deprecatedURL != "" {
		metadata["deprecation_url"] = adv.deprecatedURL
	}
	if p.Health != nil {
		metadata["health_score"] = strconv.FormatInt(*p.Health, 10)
	}
	tags := registry.NormalizeTags(p.Labels)
	var author *registry.Author
	if len(p.Developers) > 0 {
		a := registry.Author{Name: p.Developers[0].Name, Username: p.Developers[0].ID}
		if a.Validate() == nil {
			author = &a
		}
	}
	count := 0
	add := func(v release) error {
		if v.Released.IsZero() {
			return fmt.Errorf("%s@%s: missing release timestamp", p.Name, v.Version)
		}
		if !depth.Since.IsZero() && v.Released.Before(depth.Since) {
			return nil
		}
		digest, err := base64Digest(v.SHA256)
		if err != nil {
			return fmt.Errorf("%s@%s: %w", p.Name, v.Version, err)
		}
		attributes := map[string]string{"download_url": v.URL, "sha256": digest}
		// Only the current release carries a size in the update center.
		if v.Size > 0 {
			attributes["size"] = strconv.FormatInt(v.Size, 10)
		}
		versionMetadata := maps.Clone(metadata)
		versionMetadata["version_rank"] = strconv.Itoa(count + 1)
		versionMetadata["release_date_source"] = "jenkins-release-timestamp"
		// Advisories are a listing-time snapshot of what the update center
		// knows now, matched against this version's string.
		if ids := adv.affecting(v.Version); len(ids) > 0 {
			versionMetadata["security_warnings"] = strings.Join(ids, ",")
		}
		if v.RequiredCore != "" {
			versionMetadata["required_core"] = v.RequiredCore
		}
		versionFacts := facts.Clone()
		versionFacts.ReleasedAt = registry.Time(v.Released)
		ref := registry.Reference{
			Registry:   Identity,
			Namespace:  Namespace,
			Name:       p.Name,
			Version:    v.Version,
			Attributes: attributes,
			Metadata:   versionMetadata,
			Facts:      versionFacts,
			Tags:       slices.Clone(tags),
			Author:     author.Clone(),
		}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return fmt.Errorf("%s@%s: %w", p.Name, v.Version, err)
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
	if err := add(p.release); err != nil {
		if errors.Is(err, errStop) {
			return nil
		}
		return err
	}
	if depth.Max == 1 {
		return nil
	}
	older := make([]release, 0, len(past))
	for version, v := range past {
		if version == p.Version || v.Version == p.Version {
			continue
		}
		// The map key is normalized (lucene-search "5.1"); the entry's own
		// version is what the artifact and its download URL carry ("5.01").
		if v.Name != p.Name {
			return fmt.Errorf("%s@%s: history entry is named %q", p.Name, version, v.Name)
		}
		older = append(older, v)
	}
	slices.SortFunc(older, func(a, b release) int {
		if c := b.Released.Compare(a.Released); c != 0 {
			return c
		}
		return strings.Compare(b.Version, a.Version)
	})
	for _, v := range older {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := add(v); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// base64Digest turns the update center's base64 SHA-256 into sha256:<hex>.
func base64Digest(value string) (string, error) {
	hash, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(hash) != 32 {
		return "", retry.Permanent(fmt.Errorf("invalid %s SHA-256", Identity))
	}
	return "sha256:" + hex.EncodeToString(hash), nil
}

// query fetches one bounded metadata document and verifies the update center's
// signature before decoding any of it. A decode failure is permanent: retrying
// the same bytes cannot help, and the host should not retry either. A
// signature failure is not, since a mirror may briefly serve a stale or
// partial copy.
func (s *Source) query(ctx context.Context, raw string, limit int, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, raw, s.attempts())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return retry.Permanent(fmt.Errorf("%s metadata exceeds %d MiB", Identity, limit>>20))
	}
	roots := s.Roots
	if roots == nil {
		if roots, err = defaultRoots(); err != nil {
			return retry.Permanent(err)
		}
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if err := verifyDocument(data, roots, now); err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return retry.Permanent(fmt.Errorf("%s %s: %w", Identity, raw, err))
	}
	return nil
}

func isInvalidDownloadPath(path, name, version string) bool {
	if !strings.HasPrefix(path, "/download/plugins/") || !strings.HasSuffix(path, ".hpi") {
		return true
	}
	// The update center's download URL is always /download/plugins/<name>/<version>/<name>.hpi.
	// Two historical releases are served under a different version spelling than their version string (lucene-search 5.1 is 5.01), so only the plugin directory and the .hpi suffix are checked.
	parts := strings.Split(path, "/")
	if len(parts) < 5 || parts[3] != name || parts[len(parts)-1] != name+".hpi" {
		return true
	}
	return false
}

// Resolve trusts nothing in the reference: every persisted field is validated
// again so a tampered inventory cannot redirect downloads or smuggle a bad
// digest. It makes no network requests; the pinned descriptor is final, so a
// version deleted upstream fails instead of being replaced by a newer one.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != Identity || validName(ref.Name) != nil || validVersion(ref.Version) != nil {
		return registry.Artifact{}, fmt.Errorf("invalid %s reference", Identity)
	}
	raw := ref.Attributes["download_url"]
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	// Two historical releases are served under a different version spelling
	// than their version string (lucene-search 5.1 is 5.01), so only the
	// plugin directory and the .hpi suffix are checked.
	u, err := url.Parse(raw)
	// isInvalidDownloadPath looks at the path alone, so the rest of the URL is
	// checked here: no query, fragment, credentials or dot segments.
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil || strings.Contains(u.Path, "..") ||
		isInvalidDownloadPath(u.Path, ref.Name, ref.Version) {
		return registry.Artifact{}, fmt.Errorf("invalid %s download URL", Identity)
	}
	digest, err := parseDigest(ref.Attributes["sha256"])
	if err != nil {
		return registry.Artifact{}, err
	}
	// The update center publishes a size for current releases only; older
	// ones stay -1 and the host verifies the digest alone.
	size := int64(-1)
	if value, ok := ref.Attributes["size"]; ok {
		size, err = strconv.ParseInt(value, 10, 64)
		if err != nil || size < 1 {
			return registry.Artifact{}, fmt.Errorf("invalid %s artifact size", Identity)
		}
	}
	return registry.Artifact{Reference: ref, Revision: digest, Blobs: []registry.Blob{{
		Name:      blobName(ref.Name + "-" + ref.Version + ".hpi"),
		MediaType: "application/java-archive",
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

// blobName keeps a manifest file name free of path separators, because
// consumers write blob names to disk.
func blobName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
}
