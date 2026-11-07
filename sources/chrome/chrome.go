// Package chrome archives public Chrome Web Store CRX releases.
package chrome

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/pool"
	"github.com/rezen/dug/sdk/registry"
)

const DefaultBrowserVersion = "150.0.0.0"

// DefaultDownloadHosts are the hosts Google's update service hands out CRX
// URLs on. Persisted download URLs must stay on them.
var DefaultDownloadHosts = []string{"clients2.googleusercontent.com", "clients2.google.com"}

var idPattern = regexp.MustCompile(`^[a-p]{32}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,3}$`)
var errUnavailable = errors.New("no public compatible CRX available")

type Source struct {
	Client             *http.Client
	IDs                []string
	BrowserVersion     string
	ListingConcurrency int
	Attempts           int
	// DownloadHosts restricts CRX download URLs; nil means DefaultDownloadHosts.
	DownloadHosts []string
	// SkipDetails stops the per-item store page fetch that supplies the user
	// count, rating, category and developer; the update service has none.
	SkipDetails bool
	// Endpoint overrides are for embedding and local fixtures, not RPC options.
	SitemapURL string
	UpdateURL  string
	DetailURL  string
}

var _ registry.Registry = (*Source)(nil)

func (s *Source) downloadHosts() []string {
	if s.DownloadHosts != nil {
		return s.DownloadHosts
	}
	return DefaultDownloadHosts
}

type lookupResult struct {
	id  string
	ref registry.Reference
	err error
}

// Discover walks the ID source while a bounded pool resolves metadata. Results
// are emitted in discovery order, so a slow lookup delays emission but never
// idles the other workers, and the sitemap walk overlaps with lookups instead
// of stalling on each batch.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	workers := s.ListingConcurrency
	if workers == 0 {
		workers = 8
	}
	if workers < 1 || workers > 128 {
		return fmt.Errorf("listing concurrency must be 1–128")
	}
	return pool.Walk(ctx, workers, s.walkIDs,
		func(ctx context.Context, id string) lookupResult {
			ref, err := s.lookup(ctx, id)
			if err != nil {
				err = fmt.Errorf("chrome %s: %w", id, err)
			}
			return lookupResult{id: id, ref: ref, err: err}
		},
		func(result lookupResult) error {
			switch {
			case errors.Is(result.err, errUnavailable) && len(s.IDs) == 0:
				slog.Warn("skipping unavailable Chrome entry", "id", result.id, "error", result.err)
				return nil
			case result.err != nil:
				return result.err
			}
			return emit(result.ref)
		})
}

// walkIDs pushes each valid, unseen extension ID: the selected IDs, or every ID
// the store's sitemap lists.
func (s *Source) walkIDs(ctx context.Context, push func(string) error) error {
	seen := make(map[string]bool)
	add := func(id string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !idPattern.MatchString(id) {
			return fmt.Errorf("invalid Chrome extension ID %q", id)
		}
		if seen[id] {
			return nil
		}
		seen[id] = true
		return push(id)
	}
	if len(s.IDs) != 0 {
		for _, id := range s.IDs {
			if err := add(id); err != nil {
				return err
			}
		}
		return nil
	}
	root := s.SitemapURL
	if root == "" {
		root = "https://chromewebstore.google.com/sitemap"
	}
	return s.walkSitemap(ctx, root, add)
}

func (s *Source) walkSitemap(ctx context.Context, root string, add func(string) error) error {
	base, err := url.Parse(root)
	if err != nil {
		return err
	}
	queue := []string{root}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		raw := queue[0]
		queue = queue[1:]
		if seen[raw] {
			continue
		}
		if len(seen) >= 10000 {
			return fmt.Errorf("sitemap index exceeds 10000 documents")
		}
		seen[raw] = true
		var doc struct {
			XMLName  xml.Name
			Sitemaps []struct {
				Loc string `xml:"loc"`
			} `xml:"sitemap"`
			URLs []struct {
				Loc string `xml:"loc"`
			} `xml:"url"`
		}
		if err := s.readXML(ctx, raw, 64<<20, &doc); err != nil {
			return fmt.Errorf("sitemap: %w", err)
		}
		switch doc.XMLName.Local {
		case "sitemapindex":
			for _, child := range doc.Sitemaps {
				u, err := base.Parse(strings.TrimSpace(child.Loc))
				if err != nil || u.Host != base.Host || u.Scheme != base.Scheme {
					return fmt.Errorf("sitemap child must share root origin")
				}
				queue = append(queue, u.String())
				if len(queue) > 10000 {
					return fmt.Errorf("sitemap queue exceeds 10000 documents")
				}
			}
		case "urlset":
			for _, entry := range doc.URLs {
				u, err := url.Parse(strings.TrimSpace(entry.Loc))
				if err != nil {
					return err
				}
				if !strings.HasPrefix(u.Path, "/detail/") {
					continue
				}
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				id := parts[len(parts)-1]
				if idPattern.MatchString(id) {
					if err := add(id); err != nil {
						return err
					}
				}
			}
		default:
			return fmt.Errorf("expected XML sitemap, got %q", doc.XMLName.Local)
		}
	}
	return nil
}

func (s *Source) readXML(ctx context.Context, raw string, limit int64, out any) error {
	_, err := s.readXMLRaw(ctx, raw, limit, out)
	return err
}

// readXMLRaw decodes the document into out and returns its verbatim bytes.
func (s *Source) readXMLRaw(ctx context.Context, raw string, limit int64, out any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var reader io.Reader = response.Body
	if strings.HasSuffix(response.Request.URL.Path, ".gz") || response.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		reader = z
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("XML exceeds %d bytes", limit)
	}
	return data, xml.Unmarshal(data, out)
}

func (s *Source) lookup(ctx context.Context, id string) (registry.Reference, error) {
	endpoint := s.UpdateURL
	if endpoint == "" {
		endpoint = "https://clients2.google.com/service/update2/crx"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return registry.Reference{}, err
	}
	version := s.BrowserVersion
	if version == "" {
		version = DefaultBrowserVersion
	}
	q := u.Query()
	q.Set("prodversion", version)
	q.Set("acceptformat", "crx3")
	q.Set("x", "id="+id+"&uc")
	u.RawQuery = q.Encode()
	var doc struct {
		XMLName xml.Name `xml:"gupdate"`
		Apps    []struct {
			ID     string `xml:"appid,attr"`
			Status string `xml:"status,attr"`
			Update struct {
				Status  string `xml:"status,attr"`
				Version string `xml:"version,attr"`
				URL     string `xml:"codebase,attr"`
				Hash    string `xml:"hash_sha256,attr"`
				Size    string `xml:"size,attr"`
			} `xml:"updatecheck"`
		} `xml:"app"`
	}
	update, err := s.readXMLRaw(ctx, u.String(), 2<<20, &doc)
	if err != nil {
		return registry.Reference{}, err
	}
	for _, app := range doc.Apps {
		if app.ID != id {
			continue
		}
		u := app.Update
		if app.Status == "error-unknownApplication" || u.Status == "noupdate" || u.Status == "error-unknownApplication" {
			return registry.Reference{}, errUnavailable
		}
		if (app.Status != "" && app.Status != "ok") || u.Status != "ok" {
			return registry.Reference{}, fmt.Errorf("update status app=%q update=%q", app.Status, u.Status)
		}
		ref := registry.Reference{Registry: "chrome", Namespace: "chromewebstore.google.com", Name: id, Version: u.Version, Attributes: map[string]string{"download_url": u.URL, "sha256": strings.ToLower(u.Hash), "size": u.Size}, Metadata: map[string]string{"store_url": "https://chromewebstore.google.com/detail/" + id}}
		if _, err := s.Resolve(ctx, ref); err != nil {
			return registry.Reference{}, err
		}
		if !s.SkipDetails {
			d, err := s.fetchDetails(ctx, id)
			if err != nil {
				return registry.Reference{}, err
			}
			d.apply(&ref)
			ref.Raw = rawRecord(update, d.record)
		}
		if ref.Raw == nil {
			ref.Raw = rawRecord(update, nil)
		}
		return ref, nil
	}
	return registry.Reference{}, fmt.Errorf("update response missing requested ID")
}

func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if ref.Registry != "chrome" || !idPattern.MatchString(ref.Name) || !versionPattern.MatchString(ref.Version) {
		return registry.Artifact{}, fmt.Errorf("invalid Chrome reference")
	}
	raw := ref.Attributes["download_url"]
	if err := httpx.ValidateURLHost(raw, s.downloadHosts()); err != nil {
		return registry.Artifact{}, err
	}
	hash := ref.Attributes["sha256"]
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 || hash != strings.ToLower(hash) {
		return registry.Artifact{}, fmt.Errorf("invalid Chrome SHA-256")
	}
	size, err := strconv.ParseInt(ref.Attributes["size"], 10, 64)
	if err != nil || size <= 0 {
		return registry.Artifact{}, fmt.Errorf("invalid Chrome CRX size")
	}
	return registry.Artifact{Reference: ref, Revision: ref.Version, Blobs: []registry.Blob{{Name: ref.Name + "." + ref.Version + ".crx", MediaType: "application/x-chrome-extension", Digest: "sha256:" + hash, Size: size, Open: func(ctx context.Context) (io.ReadCloser, error) {
		response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}}}}, nil
}
