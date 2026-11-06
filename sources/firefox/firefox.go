// Package firefox archives public Firefox extensions from Mozilla Add-ons.
package firefox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
)

type Source struct {
	Client       *http.Client
	Endpoint     string // API base override for embedding/tests.
	Slugs        []string
	Search       string
	PageSize     int
	VersionDepth string
	Attempts     int
	// DownloadHosts restricts XPI download URLs; nil means DefaultDownloadHosts.
	DownloadHosts []string
	searchWindow  int // AMO search caps each result window at 30,000.
}

// DefaultDownloadHosts is where Mozilla serves XPI files. Persisted download
// URLs must stay on it.
var DefaultDownloadHosts = []string{"addons.mozilla.org"}

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

var _ registry.Registry = (*Source)(nil)

type version struct {
	ID      int64  `json:"id"`
	Version string `json:"version"`
	File    struct {
		ID      int64     `json:"id"`
		Created time.Time `json:"created"`
		Hash    string    `json:"hash"`
		Size    int64     `json:"size"`
		Status  string    `json:"status"`
		URL     string    `json:"url"`
	} `json:"file"`
}
type addon struct {
	ID              int64     `json:"id"`
	Slug            string    `json:"slug"`
	Type            string    `json:"type"`
	LastUpdated     time.Time `json:"last_updated"`
	WeeklyDownloads int64     `json:"weekly_downloads"`
	Users           int64     `json:"average_daily_users"`
	Ratings         struct {
		Average float64 `json:"average"`
		Count   int64   `json:"count"`
	} `json:"ratings"`
	Tags       []string   `json:"tags"`
	Categories categories `json:"categories"`
	Authors    []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Username string `json:"username"`
		URL      string `json:"url"`
	} `json:"authors"`
	Current version `json:"current_version"`
	raw     json.RawMessage
}

// UnmarshalJSON keeps the verbatim listing record next to the decoded fields.
func (a *addon) UnmarshalJSON(data []byte) error {
	type plain addon
	if err := json.Unmarshal(data, (*plain)(a)); err != nil {
		return err
	}
	a.raw = slices.Clone(data)
	return nil
}

// categories decodes AMO category slugs, which the API has published both as
// a flat list and as lists keyed by application.
type categories []string

func (c *categories) UnmarshalJSON(data []byte) error {
	var flat []string
	if err := json.Unmarshal(data, &flat); err == nil {
		*c = flat
		return nil
	}
	var byApp map[string][]string
	if err := json.Unmarshal(data, &byApp); err != nil {
		return fmt.Errorf("unexpected categories field: %w", err)
	}
	*c = nil
	for _, app := range slices.Sorted(maps.Keys(byApp)) {
		*c = append(*c, byApp[app]...)
	}
	return nil
}

// authorOf is the first listed author, which AMO treats as the owner. Its
// numeric account ID outlives username changes.
func (a addon) authorOf() *registry.Author {
	if len(a.Authors) == 0 {
		return nil
	}
	first := a.Authors[0]
	author := registry.Author{Name: first.Name, Username: first.Username, URL: first.URL}
	if first.ID > 0 {
		author.ID = strconv.FormatInt(first.ID, 10)
	}
	if author.Validate() != nil {
		return nil
	}
	return &author
}

type page[T any] struct {
	Count   int    `json:"count"`
	Next    string `json:"next"`
	Results []T    `json:"results"`
}

func (s *Source) base() string {
	if s.Endpoint != "" {
		return strings.TrimRight(s.Endpoint, "/") + "/"
	}
	return "https://addons.mozilla.org/api/v5/"
}
func (s *Source) pageSize() int {
	if s.PageSize == 0 {
		return 50
	}
	return s.PageSize
}

func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	if s.pageSize() < 1 || s.pageSize() > 50 {
		return fmt.Errorf("firefox page_size must be 1–50")
	}
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	seen := make(map[int64]bool)
	add := func(a addon) error {
		if a.ID < 1 || a.Slug == "" || a.Type != "extension" {
			return fmt.Errorf("invalid Firefox extension metadata")
		}
		if seen[a.ID] {
			return nil
		}
		seen[a.ID] = true
		return s.emitVersions(ctx, a, depth, emit)
	}
	if len(s.Slugs) > 0 {
		for _, slug := range s.Slugs {
			if err := validIdentifier(slug); err != nil {
				return err
			}
			var a addon
			if err := s.query(ctx, s.base()+"addons/addon/"+url.PathEscape(slug)+"/", &a); err != nil {
				return err
			}
			if err := add(a); err != nil {
				return err
			}
		}
		return nil
	}
	// Search windows are split by immutable creation time, avoiding silent
	// truncation at AMO's search result cap. The catalog remains a live view.
	return s.search(ctx, time.Unix(0, 0).UTC(), time.Now().UTC().Add(24*time.Hour).Truncate(time.Second), add)
}

func (s *Source) search(ctx context.Context, from, to time.Time, emit func(addon) error) error {
	q := url.Values{"app": {"firefox"}, "type": {"extension"}, "sort": {"created"}, "page_size": {strconv.Itoa(s.pageSize())}, "created__gte": {from.Format(time.RFC3339)}, "created__lt": {to.Format(time.RFC3339)}}
	if s.Search != "" {
		q.Set("q", s.Search)
	}
	raw := s.base() + "addons/search/?" + q.Encode()
	var first page[addon]
	if err := s.query(ctx, raw, &first); err != nil {
		return err
	}
	window := s.searchWindow
	if window == 0 {
		window = 30000
	}
	if first.Count > window {
		if to.Sub(from) <= time.Second {
			return fmt.Errorf("firefox search window still exceeds %d entries at one-second resolution", window)
		}
		mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		if err := s.search(ctx, mid, to, emit); err != nil {
			return err
		}
		return s.search(ctx, from, mid, emit)
	}
	seenPages := map[string]bool{raw: true}
	p := first
	count := 0
	for {
		for _, a := range p.Results {
			if err := emit(a); err != nil {
				return err
			}
			count++
		}
		if p.Next == "" {
			if count < first.Count {
				return fmt.Errorf("firefox search ended early: got %d of %d entries", count, first.Count)
			}
			return nil
		}
		next, err := s.next(p.Next, seenPages)
		if err != nil {
			return err
		}
		p = page[addon]{}
		if err := s.query(ctx, next, &p); err != nil {
			return err
		}
	}
}

func (s *Source) emitVersions(ctx context.Context, a addon, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	metadata := map[string]string{"rating": strconv.FormatFloat(a.Ratings.Average*20, 'f', -1, 64), "rating_average": strconv.FormatFloat(a.Ratings.Average, 'f', -1, 64), "num_ratings": strconv.FormatInt(a.Ratings.Count, 10)}
	// AMO publishes a trailing seven-day download count and average daily
	// users per add-on, never a lifetime total.
	facts := &registry.Facts{Downloads: &registry.DownloadStats{Week: registry.Int64(a.WeeklyDownloads)}, Installs: registry.Int64(a.Users)}
	if !a.LastUpdated.IsZero() {
		facts.UpdatedAt = registry.Time(a.LastUpdated)
	}
	// AMO's fixed category taxonomy and free-form tags are both keywords for
	// corpus selection, so they share the reference's tag set.
	tags := registry.NormalizeTags(slices.Concat(a.Tags, a.Categories))
	author := a.authorOf()
	seen := make(map[int64]bool)
	count := 0
	done := errors.New("version depth reached")
	add := func(v version) error {
		if seen[v.File.ID] {
			return nil
		}
		seen[v.File.ID] = true
		if v.File.Created.IsZero() {
			return fmt.Errorf("%s: missing version creation date", a.Slug)
		}
		if !depth.Since.IsZero() && v.File.Created.Before(depth.Since) {
			return nil
		}
		if v.File.Status != "public" || v.ID < 1 || v.File.ID < 1 || v.Version == "" {
			return fmt.Errorf("%s: invalid public version", a.Slug)
		}
		versionMetadata := maps.Clone(metadata)
		versionMetadata["release_date_source"] = "amo-file-created"
		versionMetadata["version_rank"] = strconv.Itoa(count + 1)
		versionFacts := facts.Clone()
		versionFacts.ReleasedAt = registry.Time(v.File.Created)
		ref := registry.Reference{Registry: "firefox", Namespace: "addons.mozilla.org", Name: a.Slug, Version: v.Version, Metadata: versionMetadata, Facts: versionFacts, Tags: slices.Clone(tags), Author: author.Clone(), Raw: slices.Clone(a.raw), Attributes: map[string]string{"download_url": v.File.URL, "sha256": v.File.Hash, "size": strconv.FormatInt(v.File.Size, 10), "addon_id": strconv.FormatInt(a.ID, 10), "version_id": strconv.FormatInt(v.ID, 10), "file_id": strconv.FormatInt(v.File.ID, 10)}}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return err
		}
		if err := emit(ref); err != nil {
			return err
		}
		count++
		if depth.Max > 0 && count >= depth.Max {
			return done
		}
		return nil
	}
	if err := add(a.Current); err != nil {
		if errors.Is(err, done) {
			return nil
		}
		return err
	}
	raw := s.base() + "addons/addon/" + strconv.FormatInt(a.ID, 10) + "/versions/?page_size=" + strconv.Itoa(s.pageSize())
	seenPages := map[string]bool{raw: true}
	for {
		var p page[version]
		if err := s.query(ctx, raw, &p); err != nil {
			return err
		}
		for _, v := range p.Results {
			if err := add(v); err != nil {
				if errors.Is(err, done) {
					return nil
				}
				return err
			}
		}
		if p.Next == "" {
			return nil
		}
		var err error
		raw, err = s.next(p.Next, seenPages)
		if err != nil {
			return err
		}
	}
}

func (s *Source) next(raw string, seen map[string]bool) (string, error) {
	base, err := url.Parse(s.base())
	if err != nil {
		return "", err
	}
	u, err := base.Parse(raw)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, base.Path) {
		return "", fmt.Errorf("firefox pagination left API origin/path")
	}
	if seen[u.String()] {
		return "", fmt.Errorf("firefox pagination cycle")
	}
	seen[u.String()] = true
	return u.String(), nil
}

func (s *Source) query(ctx context.Context, raw string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("firefox metadata exceeds 32 MiB")
	}
	return json.Unmarshal(data, out)
}

func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != "firefox" || ref.Version == "" || ref.Name == "" {
		return registry.Artifact{}, fmt.Errorf("invalid Firefox reference")
	}
	raw := ref.Attributes["download_url"]
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	digest := ref.Attributes["sha256"]
	hash, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(hash) != 32 || digest != "sha256:"+hex.EncodeToString(hash) {
		return registry.Artifact{}, fmt.Errorf("invalid Firefox SHA-256")
	}
	size, err := strconv.ParseInt(ref.Attributes["size"], 10, 64)
	if err != nil || size < 1 {
		return registry.Artifact{}, fmt.Errorf("invalid Firefox XPI size")
	}
	fileID, err := strconv.ParseInt(ref.Attributes["file_id"], 10, 64)
	if err != nil || fileID < 1 {
		return registry.Artifact{}, fmt.Errorf("invalid Firefox file ID")
	}
	return registry.Artifact{Reference: ref, Revision: digest, Blobs: []registry.Blob{{Name: ref.Name + "." + strconv.FormatInt(fileID, 10) + ".xpi", MediaType: "application/x-xpinstall", Digest: digest, Size: size, Open: func(ctx context.Context) (io.ReadCloser, error) {
		response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}}}}, nil
}
