package n8n

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services/npm"
)

// DefaultIndex is the ecosyste.ms API, a third-party index of package
// registries that lists packages by keyword and pages through all of them.
const DefaultIndex = "https://packages.ecosyste.ms/api/v1/"

const (
	indexPageSize    = 1000
	maxMetadataBytes = 32 << 20
)

func (s *Source) mode() string {
	if s.Enumerate != "" {
		return s.Enumerate
	}
	return EnumerateAll
}

func (s *Source) indexBase() string {
	if s.Index != "" {
		return strings.TrimRight(s.Index, "/") + "/"
	}
	return DefaultIndex
}

// enumerate lists the names of packages that carry the keyword.
//
// npm offers no way to walk a keyword to the end. Its search reports the full
// total but serves only the first 5,000 results (later offsets silently repeat
// the top of the ranking), and keyword queries can be narrowed only with AND,
// never split into exact complements. The ecosyste.ms index pages cleanly
// through every package it knows, but its sync lags npm, so it misses newer
// packages and still lists removed ones. The default mode takes the union:
// the index gives the bulk, search adds what the index has not seen, and
// Discover verifies each name against npm. searchTotal is npm's own count,
// which Discover compares with what it found to report any remaining gap.
func (s *Source) enumerate(ctx context.Context) (names []string, searchTotal int, err error) {
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if mode := s.mode(); mode != EnumerateAll && mode != EnumerateSearch && mode != EnumerateEcosystems {
		return nil, 0, fmt.Errorf("n8n enumerate must be %s, %s or %s", EnumerateAll, EnumerateSearch, EnumerateEcosystems)
	}
	if s.mode() != EnumerateSearch {
		if err := s.walkIndex(ctx, add); err != nil {
			return nil, 0, err
		}
	}
	if s.mode() != EnumerateEcosystems {
		total, err := s.npm().Search(ctx, "keywords:"+s.keyword(), func(o npm.SearchObject) error {
			if err := npm.ValidName(o.Package.Name); err != nil {
				slog.Warn("n8n skipping search result with an invalid name", "name", o.Package.Name)
				return nil
			}
			add(o.Package.Name)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		searchTotal = total
		if total > npm.SearchWindow && s.mode() == EnumerateSearch {
			slog.Warn("n8n search enumeration is partial: npm serves only its first results",
				"served", npm.SearchWindow, "npm_search_total", total, "keyword", s.keyword())
		}
	}
	return names, searchTotal, nil
}

// walkIndex pages through the ecosyste.ms keyword listing. Next links must
// stay on the API origin and must not repeat.
func (s *Source) walkIndex(ctx context.Context, add func(string)) error {
	base, err := url.Parse(s.indexBase())
	if err != nil {
		return err
	}
	raw := fmt.Sprintf("%skeywords/%s?per_page=%d&page=1", s.indexBase(), url.PathEscape(s.keyword()), indexPageSize)
	seenPages := map[string]bool{raw: true}
	for {
		var page struct {
			Packages []struct {
				Name      string `json:"name"`
				Ecosystem string `json:"ecosystem"`
			} `json:"packages"`
		}
		header, err := s.getJSON(ctx, raw, &page)
		if err != nil {
			return fmt.Errorf("n8n keyword index: %w", err)
		}
		for _, p := range page.Packages {
			// The keyword index spans ecosystems; only npm packages are nodes.
			if p.Ecosystem != "npm" {
				continue
			}
			if err := npm.ValidName(p.Name); err != nil {
				slog.Warn("n8n skipping index entry with an invalid name", "name", p.Name)
				continue
			}
			add(p.Name)
		}
		next := nextLink(header.Values("Link"))
		if next == "" {
			return nil
		}
		u, err := base.Parse(next)
		if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, base.Path) {
			return fmt.Errorf("n8n keyword index pagination left the API origin")
		}
		if seenPages[u.String()] {
			return fmt.Errorf("n8n keyword index pagination cycle")
		}
		seenPages[u.String()] = true
		raw = u.String()
	}
}

// nextLink extracts the rel="next" target from Link headers.
func nextLink(values []string) string {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			target, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			target = strings.TrimSpace(target)
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, param := range strings.Split(params, ";") {
				if strings.ReplaceAll(strings.TrimSpace(param), " ", "") == `rel="next"` {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
}

// getJSON fetches one bounded metadata document and returns its headers.
// Decode failures are permanent: retrying the same bytes cannot help.
func (s *Source) getJSON(ctx context.Context, raw string, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, raw, s.Attempts)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes {
		return nil, retry.Permanent(fmt.Errorf("metadata exceeds 32 MiB"))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, retry.Permanent(fmt.Errorf("decode %s: %w", raw, err))
	}
	return response.Header, nil
}
