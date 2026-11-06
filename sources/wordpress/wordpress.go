// Package wordpress enumerates current public WordPress.org plugin and theme
// releases. The two directories share one API shape, download host and SVN
// history model, so one adapter serves either, each as its own registry.
package wordpress

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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
)

// Kind selects the WordPress.org directory a Source enumerates. Plugins and
// themes have separate slug namespaces, so each is served under its own
// registry identity and a plugin can never be confused with a theme.
type Kind string

const (
	Plugins Kind = "plugins" // registry identity "wordpress"
	Themes  Kind = "themes"  // registry identity "wordpress-themes"
)

// directory holds the upstream constants that differ between the two
// WordPress.org catalogs.
type directory struct {
	kind        Kind
	registry    string   // identity served to the host
	item        string   // singular noun for messages
	endpoint    string   // info API base URL
	svn         string   // SVN base URL
	svnVersions string   // path between a slug and its version directories
	query       string   // catalog listing action
	information string   // single-item action
	download    string   // request field that includes download_link
	browse      []string // accepted browse values; the first is the default
}

var directories = map[Kind]directory{
	Plugins: {
		kind: Plugins, registry: "wordpress", item: "plugin",
		endpoint: "https://api.wordpress.org/plugins/info/1.2/",
		// Plugin releases are SVN tags under slug/tags/.
		svn: "https://plugins.svn.wordpress.org", svnVersions: "tags/",
		query: "query_plugins", information: "plugin_information", download: "download_link",
		browse: []string{"updated", "popular", "new"},
	},
	Themes: {
		kind: Themes, registry: "wordpress-themes", item: "theme",
		endpoint: "https://api.wordpress.org/themes/info/1.2/",
		// Theme releases are committed straight under the slug as version
		// directories; there is no trunk or tags level.
		svn: "https://themes.svn.wordpress.org", svnVersions: "",
		// The themes API spells this field without an underscore, and listings
		// omit download_link unless it is requested.
		query: "query_themes", information: "theme_information", download: "downloadlink",
		// popular lists only themes with active installs; updated and new walk
		// the whole catalog, and featured is a small curated set.
		browse: []string{"updated", "popular", "new", "featured"},
	},
}

func (k Kind) directory() (directory, error) {
	if k == "" {
		k = Plugins
	}
	d, ok := directories[k]
	if !ok {
		return directory{}, fmt.Errorf("unknown WordPress directory %q (supported: plugins, themes)", k)
	}
	return d, nil
}

// Registry is the identity the host names to reach this directory.
func (k Kind) Registry() string {
	d, _ := k.directory()
	return d.registry
}

type Source struct {
	Client *http.Client
	// Kind is the directory to enumerate; empty means Plugins.
	Kind         Kind
	Endpoint     string
	PageSize     int
	Search       string
	Slugs        []string
	Attempts     int
	VersionDepth string
	SVNEndpoint  string
	Browse       string
	// DownloadHosts restricts ZIP download URLs; nil means DefaultDownloadHosts.
	DownloadHosts []string
}

// DefaultDownloadHosts is where WordPress.org serves plugin and theme ZIPs.
// Persisted download URLs must stay on it so a tampered inventory or catalog
// entry cannot point this plugin process at arbitrary servers.
var DefaultDownloadHosts = []string{"downloads.wordpress.org"}

func (s *Source) validateDownloadURL(raw string) error {
	hosts := s.DownloadHosts
	if hosts == nil {
		hosts = DefaultDownloadHosts
	}
	return httpx.ValidateURLHost(raw, hosts)
}

// Verify the WordPress adapter satisfies the registry contract at compile time.
var _ registry.Registry = (*Source)(nil)

// text accepts a JSON string or a bare number. The themes API encodes purely
// numeric versions such as 1.0 as numbers; the digits are kept verbatim so the
// version still matches its download URL and SVN directory.
type text string

func (t *text) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*t = text(s)
		return nil
	}
	if string(data) == "null" {
		return nil
	}
	if _, err := strconv.ParseFloat(string(data), 64); err != nil {
		return fmt.Errorf("expected a string or number, got %s", data)
	}
	*t = text(data)
	return nil
}

// item is one plugin or theme as the info API describes it.
type item struct {
	Slug         string `json:"slug"`
	Version      text   `json:"version"`
	DownloadLink string `json:"download_link"`
	Error        string `json:"error"`
	LastUpdated  string `json:"last_updated"`
	// LastUpdatedTime is the theme directory's second-resolution GMT update
	// time; plugins carry a zoned timestamp in LastUpdated instead.
	LastUpdatedTime string `json:"last_updated_time"`
	// Template is the parent theme slug when a theme is a child theme.
	Template       string            `json:"template"`
	ActiveInstalls *int64            `json:"active_installs"`
	Downloaded     *int64            `json:"downloaded"`
	Rating         *float64          `json:"rating"`
	NumRatings     *int64            `json:"num_ratings"`
	Ratings        map[string]int64  `json:"ratings"`
	Versions       map[string]string `json:"versions"`
	Tags           tagSet            `json:"tags"`
	Author         author            `json:"author"`
	AuthorProfile  string            `json:"author_profile"`
	raw            json.RawMessage
}

// UnmarshalJSON keeps the verbatim directory record next to the decoded fields.
func (p *item) UnmarshalJSON(data []byte) error {
	type plain item
	if err := json.Unmarshal(data, (*plain)(p)); err != nil {
		return err
	}
	p.raw = slices.Clone(data)
	return nil
}

// tagSet decodes the directory's tags: an object of slug to display name,
// which PHP serializes as an empty array when a plugin or theme has none.
type tagSet []string

func (t *tagSet) UnmarshalJSON(data []byte) error {
	var named map[string]string
	if err := json.Unmarshal(data, &named); err == nil {
		*t = slices.Sorted(maps.Keys(named))
		return nil
	}
	var listed []string
	if err := json.Unmarshal(data, &listed); err != nil {
		return fmt.Errorf("unexpected tags field: %w", err)
	}
	*t = listed
	return nil
}

// author decodes the directory's author field. The plugin directory sends a
// display name, usually wrapped in a profile link; the theme directory sends
// the account nicename, or with extended_author an object naming both.
type author struct {
	Name, Username, Profile string
}

var authorLink = regexp.MustCompile(`(?is)<a\b[^>]*\bhref="([^"]*)"[^>]*>(.*?)</a>`)
var anyTag = regexp.MustCompile(`(?s)<[^>]*>`)

func (a *author) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		if m := authorLink.FindStringSubmatch(text); m != nil {
			a.Profile = html.UnescapeString(m[1])
			text = m[2]
		}
		a.Name = strings.TrimSpace(html.UnescapeString(anyTag.ReplaceAllString(text, "")))
		return nil
	}
	var object struct {
		DisplayName string `json:"display_name"`
		Nicename    string `json:"user_nicename"`
		Profile     string `json:"profile"`
		Author      string `json:"author"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("unexpected author field: %w", err)
	}
	a.Name = strings.TrimSpace(html.UnescapeString(object.DisplayName))
	if a.Name == "" {
		a.Name = strings.TrimSpace(html.UnescapeString(object.Author))
	}
	a.Username, a.Profile = object.Nicename, object.Profile
	return nil
}

// authorOf builds the registry author from the item's author fields. The
// profile URL's last path segment is the account username, which is the
// stable handle across display-name changes.
func (p item) authorOf(kind Kind) *registry.Author {
	a := registry.Author{Name: p.Author.Name, Username: p.Author.Username, URL: p.Author.Profile}
	if a.URL == "" {
		a.URL = p.AuthorProfile
	}
	if kind == Themes && a.Username == "" && a.URL == "" && a.Name != "" {
		// A bare theme author string is the nicename, not a display name.
		a.Username, a.Name = a.Name, ""
	}
	if a.Username == "" && a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			a.Username = strings.ToLower(strings.TrimSpace(strings.Trim(u.Path, "/")))
			if strings.Contains(a.Username, "/") {
				a.Username = ""
			}
		}
	}
	if a.Validate() != nil {
		return nil
	}
	return &a
}

func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	dir, err := s.Kind.directory()
	if err != nil {
		return err
	}
	if s.PageSize < 1 || s.PageSize > 250 {
		return fmt.Errorf("WordPress page size must be between 1 and 250")
	}
	emitItem := func(p item) error {
		if p.Error != "" {
			return fmt.Errorf("WordPress API: %s", p.Error)
		}
		if p.Slug == "" || p.Version == "" || p.DownloadLink == "" {
			return fmt.Errorf("incomplete WordPress %s metadata for %q", dir.item, p.Slug)
		}
		if err := s.validateDownloadURL(p.DownloadLink); err != nil {
			return err
		}
		// Typed facts carry the cross-registry metrics; metadata keeps the
		// WordPress-specific ones. WordPress.org hosts every plugin and theme
		// in its own SVN, which is the canonical source repository.
		metadata := make(map[string]string)
		facts := &registry.Facts{Repository: &registry.Repository{URL: dir.svn + "/" + p.Slug, Source: registry.RepositoryRegistry}}
	updated:
		for _, raw := range []string{p.LastUpdatedTime, p.LastUpdated} {
			if raw == "" {
				continue
			}
			for _, layout := range []string{time.RFC3339, "2006-01-02 3:04pm MST", "2006-01-02 15:04:05", "2006-01-02"} {
				if updated, err := time.Parse(layout, raw); err == nil {
					facts.UpdatedAt = registry.Time(updated)
					break updated
				}
			}
		}
		if p.Template != "" {
			metadata["template"] = p.Template
		}
		if p.ActiveInstalls != nil {
			facts.Installs = registry.Int64(*p.ActiveInstalls)
		}
		if p.Downloaded != nil {
			// The directory publishes one all-time count per plugin or theme.
			facts.Downloads = &registry.DownloadStats{Total: registry.Int64(*p.Downloaded)}
		}
		if p.Rating != nil {
			metadata["rating"] = strconv.FormatFloat(*p.Rating, 'f', -1, 64)
		}
		if p.NumRatings != nil {
			metadata["num_ratings"] = strconv.FormatInt(*p.NumRatings, 10)
		}
		if p.Ratings != nil {
			ratings, err := json.Marshal(p.Ratings)
			if err != nil {
				return err
			}
			metadata["ratings"] = string(ratings)
			for stars, count := range p.Ratings {
				metadata["ratings_"+stars] = strconv.FormatInt(count, 10)
			}
		}
		tags := registry.NormalizeTags(p.Tags)
		author := p.authorOf(dir.kind)
		versions, err := s.selectVersions(ctx, p)
		if err != nil {
			return err
		}
		for i, v := range versions {
			if err := s.validateDownloadURL(v.URL); err != nil {
				return err
			}
			versionMetadata := maps.Clone(metadata)
			versionMetadata["version_rank"] = strconv.Itoa(i + 1)
			versionFacts := facts.Clone()
			if !v.Released.IsZero() {
				versionFacts.ReleasedAt = registry.Time(v.Released)
				versionMetadata["release_date_source"] = "svn-tag-creation"
			}
			ref := registry.Reference{Registry: dir.registry, Namespace: "wordpress.org", Name: p.Slug, Version: v.Version, Attributes: map[string]string{"download_url": v.URL}, Metadata: versionMetadata, Facts: versionFacts, Tags: slices.Clone(tags), Author: author.Clone(), Raw: slices.Clone(p.raw)}
			if err := emit(ref); err != nil {
				return err
			}
		}
		return nil
	}
	if len(s.Slugs) > 0 {
		for _, slug := range s.Slugs {
			var result item
			if err := s.query(ctx, url.Values{"action": {dir.information}, "request[slug]": {slug}}, &result); err != nil {
				return err
			}
			if err := emitItem(result); err != nil {
				return err
			}
		}
		return nil
	}
	browse := s.Browse
	if browse == "" {
		browse = dir.browse[0]
	}
	for page := 1; ; page++ {
		params := url.Values{"action": {dir.query}, "request[browse]": {browse}, "request[page]": {strconv.Itoa(page)}, "request[per_page]": {strconv.Itoa(s.PageSize)}}
		if s.Search != "" {
			params.Set("request[search]", s.Search)
		}
		var result struct {
			Info struct {
				Pages int `json:"pages"`
			} `json:"info"`
			Plugins []item `json:"plugins"`
			Themes  []item `json:"themes"`
			Error   string `json:"error"`
		}
		if err := s.query(ctx, params, &result); err != nil {
			return err
		}
		if result.Error != "" {
			return fmt.Errorf("WordPress API: %s", result.Error)
		}
		items := result.Plugins
		if dir.kind == Themes {
			items = result.Themes
		}
		for _, p := range items {
			if err := emitItem(p); err != nil {
				return err
			}
		}
		if page >= result.Info.Pages || len(items) == 0 {
			return nil
		}
	}
}

func (s *Source) query(ctx context.Context, params url.Values, output any) error {
	dir, err := s.Kind.directory()
	if err != nil {
		return err
	}
	// extended_author makes the theme directory send the author object instead
	// of a bare nicename; the plugin directory ignores it.
	for _, field := range []string{dir.download, "last_updated", "active_installs", "downloaded", "rating", "ratings", "num_ratings", "tags", "author", "author_profile", "extended_author"} {
		params.Set("request[fields]["+field+"]", "1")
	}
	// Long-form fields are never read; dropping them keeps responses small.
	// Each API ignores field names it does not know.
	for _, field := range []string{"description", "short_description", "sections", "icons", "banners", "screenshots", "reviews"} {
		params.Set("request[fields]["+field+"]", "0")
	}
	if s.VersionDepth != "" && s.VersionDepth != "1" {
		params.Set("request[fields][versions]", "1")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = dir.endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	u.RawQuery = params.Encode()
	response, err := httpx.Get(ctx, s.Client, u.String(), s.Attempts)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output)
}

func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	raw := ref.Attributes["download_url"]
	if err := s.validateDownloadURL(raw); err != nil {
		return registry.Artifact{}, err
	}
	return registry.Artifact{Reference: ref, Revision: ref.Version, Blobs: []registry.Blob{{
		Name: ref.Name + "." + ref.Version + ".zip", MediaType: "application/zip", Size: -1,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
			if err != nil {
				return nil, err
			}
			return response.Body, nil
		},
	}}}, nil
}
