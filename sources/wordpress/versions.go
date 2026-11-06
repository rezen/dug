package wordpress

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	version "github.com/hashicorp/go-version"
	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services"
	"golang.org/x/sync/errgroup"
)

type release struct {
	Version, URL string
	Released     time.Time
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (s *Source) selectVersions(ctx context.Context, p item) ([]release, error) {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return nil, err
	}
	current := release{Version: string(p.Version), URL: p.DownloadLink}
	if depth.Max == 1 {
		return []release{current}, nil
	}
	if p.Versions == nil {
		dir, err := s.Kind.directory()
		if err != nil {
			return nil, err
		}
		var info item
		if err := s.query(ctx, url.Values{"action": {dir.information}, "request[slug]": {p.Slug}}, &info); err != nil {
			return nil, err
		}
		if info.Error != "" {
			return nil, fmt.Errorf("WordPress versions: %s", info.Error)
		}
		p.Versions = info.Versions
	}
	if p.Versions == nil {
		return nil, fmt.Errorf("%s: version catalog missing", p.Slug)
	}
	var history []release
	for tag, raw := range p.Versions {
		// Plugins advertise moving trunk alongside tags; themes never do.
		if tag == "trunk" || tag == string(p.Version) {
			continue
		}
		if tag == "" || strings.ContainsAny(tag, "/\\") {
			return nil, fmt.Errorf("invalid WordPress version %q", tag)
		}
		history = append(history, release{Version: tag, URL: raw})
	}
	// The current published release is always first. Remaining available tags
	// use semantic version precedence, with lexical fallback for nonstandard tags.
	sort.Slice(history, func(i, j int) bool {
		a, ae := version.NewVersion(history[i].Version)
		b, be := version.NewVersion(history[j].Version)
		if ae == nil && be == nil {
			if !a.Equal(b) {
				return a.GreaterThan(b)
			}
		} else if (ae == nil) != (be == nil) {
			return ae == nil
		}
		return history[i].Version > history[j].Version
	})
	all := append([]release{current}, history...)
	if depth.Max > 0 {
		return all[:min(depth.Max, len(all))], nil
	}
	if depth.Since.IsZero() {
		return all, nil
	}
	// The API has version URLs but no release timestamps. SVN creation history
	// of each tag or version directory supplies dates; last-modified alone is
	// not a release date.
	tags, err := s.tagMetadata(ctx, p.Slug)
	if err != nil {
		return nil, err
	}
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for i := range all {
		group.Go(func() error {
			tag, ok := tags[all[i].Version]
			if !ok {
				return fmt.Errorf("%s@%s: no SVN tag date; cannot apply history cutoff", p.Slug, all[i].Version)
			}
			// A tag created before its last change cannot meet a later cutoff.
			if tag.Changed.Before(depth.Since) {
				return nil
			}
			date, err := s.tagCreated(gctx, p.Slug, all[i].Version, tag.Revision)
			if err != nil {
				return err
			}
			all[i].Released = date
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	selected := make([]release, 0, len(all))
	for _, v := range all {
		if !v.Released.IsZero() && !v.Released.Before(depth.Since) {
			selected = append(selected, v)
		}
	}
	return selected, nil
}

type tagMetadata struct {
	Changed  time.Time
	Revision int64
}

// svnURL is the SVN collection whose children are one plugin's tags or one
// theme's version directories.
func (s *Source) svnURL(slug string) (string, error) {
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("invalid WordPress slug %q", slug)
	}
	dir, err := s.Kind.directory()
	if err != nil {
		return "", err
	}
	base := s.SVNEndpoint
	if base == "" {
		base = dir.svn
	}
	return strings.TrimRight(base, "/") + "/" + slug + "/" + dir.svnVersions, nil
}
func (s *Source) tagMetadata(ctx context.Context, slug string) (map[string]tagMetadata, error) {
	raw, err := s.svnURL(slug)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Responses []struct {
			Href  string `xml:"href"`
			Props []struct {
				Status string `xml:"status"`
				Prop   struct {
					Date     string `xml:"creationdate"`
					Revision string `xml:"version-name"`
				} `xml:"prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	payload := `<D:propfind xmlns:D="DAV:"><D:prop><D:creationdate/><D:version-name/></D:prop></D:propfind>`
	if err := s.svnXML(ctx, "PROPFIND", raw, payload, http.StatusMultiStatus, &doc); err != nil {
		return nil, err
	}
	tags := make(map[string]tagMetadata)
	base, _ := url.Parse(raw)
	for _, response := range doc.Responses {
		u, err := url.Parse(response.Href)
		if err != nil {
			return nil, err
		}
		path := strings.TrimSuffix(u.Path, "/")
		if !strings.HasPrefix(path, base.Path) {
			continue
		}
		tag := strings.TrimPrefix(path, base.Path)
		if tag == "" || strings.Contains(tag, "/") {
			continue
		}
		for _, p := range response.Props {
			if p.Status != "HTTP/1.1 200 OK" {
				continue
			}
			date, err := time.Parse(time.RFC3339, p.Prop.Date)
			if err != nil {
				return nil, err
			}
			rev, err := strconv.ParseInt(p.Prop.Revision, 10, 64)
			if err != nil || rev < 1 {
				return nil, fmt.Errorf("invalid SVN revision")
			}
			tags[tag] = tagMetadata{Changed: date, Revision: rev}
		}
	}
	return tags, nil
}
func (s *Source) tagCreated(ctx context.Context, slug, tag string, revision int64) (time.Time, error) {
	base, err := s.svnURL(slug)
	if err != nil {
		return time.Time{}, err
	}
	var doc struct {
		Items []struct {
			Date string `xml:"date"`
		} `xml:"log-item"`
	}
	payload := fmt.Sprintf(`<S:log-report xmlns:S="svn:"><S:start-revision>0</S:start-revision><S:end-revision>%d</S:end-revision><S:limit>1</S:limit><S:strict-node-history/><S:path></S:path></S:log-report>`, revision)
	if err := s.svnXML(ctx, "REPORT", base+url.PathEscape(tag)+"/", payload, http.StatusOK, &doc); err != nil {
		return time.Time{}, err
	}
	if len(doc.Items) != 1 {
		return time.Time{}, fmt.Errorf("%s@%s: missing SVN creation history", slug, tag)
	}
	return time.Parse(time.RFC3339, doc.Items[0].Date)
}

func (s *Source) svnXML(ctx context.Context, method, raw, payload string, status int, out any) error {
	if err := httpx.ValidateURL(raw); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := s.Client
	if client == nil {
		client = services.DefaultDugHttpClient()
	}
	attempts := s.Attempts
	if attempts < 1 {
		attempts = 4
	}
	return retry.Do(ctx, attempts, func() error {
		req, err := http.NewRequestWithContext(ctx, method, raw, bytes.NewBufferString(payload))
		if err != nil {
			return retry.Permanent(err)
		}
		req.Header.Set("Content-Type", "text/xml")
		req.Header.Set("Depth", "1")
		req.Header.Set("User-Agent", "dug/0.1")
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			err := fmt.Errorf("SVN %s: HTTP %d", method, response.StatusCode)
			if response.StatusCode != 429 && response.StatusCode != 408 && response.StatusCode < 500 {
				return retry.Permanent(err)
			}
			if seconds, e := strconv.Atoi(response.Header.Get("Retry-After")); e == nil && seconds > 0 {
				if e := retry.Wait(ctx, time.Duration(min(seconds, 3600))*time.Second); e != nil {
					return e
				}
			}
			return err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 16<<20 {
			return retry.Permanent(fmt.Errorf("SVN metadata exceeds 16 MiB"))
		}
		if err := xml.Unmarshal(data, out); err != nil {
			return retry.Permanent(err)
		}
		return nil
	})
}
