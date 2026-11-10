package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/retry"
)

// ErrNoCatalog reports an endpoint that cannot enumerate repositories or order
// tags by recency. The caller must supply repository names.
var ErrNoCatalog = errors.New("oci: registry has no catalog API")

// Repository is one repository a vendor catalog lists. Pulls, Stars and
// UpdatedAt are nil where the vendor does not publish them.
type Repository struct {
	Name        string // namespace/repository
	Description string
	Pulls       *int64
	Stars       *int64
	UpdatedAt   *time.Time
}

// Tag is one tag from a vendor catalog, newest first. Digest is the manifest
// or index the tag pointed at when listed.
type Tag struct {
	Name    string
	Digest  string
	Updated time.Time
}

// Repositories enumerates a namespace through the vendor API (Docker Hub
// namespaces, Quay namespaces). Quay also accepts an empty namespace, which
// walks every public repository.
func (c *Client) Repositories(ctx context.Context, ep Endpoint, namespace string, fn func(Repository) error) error {
	switch ep.Catalog {
	case CatalogDockerHub:
		return c.hubRepositories(ctx, ep, namespace, fn)
	case CatalogQuay:
		return c.quayRepositories(ctx, ep, namespace, fn)
	}
	return fmt.Errorf("%w: %s", ErrNoCatalog, ep.Name)
}

// CatalogTags lists a repository's tags newest first through the vendor API.
func (c *Client) CatalogTags(ctx context.Context, ep Endpoint, repo string, fn func(Tag) error) error {
	switch ep.Catalog {
	case CatalogDockerHub:
		return c.hubTags(ctx, ep, repo, fn)
	case CatalogQuay:
		return c.quayTags(ctx, ep, repo, fn)
	}
	return fmt.Errorf("%w: %s", ErrNoCatalog, ep.Name)
}

// catalogGet is a bounded JSON GET on the vendor API origin. Vendor APIs are
// public and unauthenticated here, so no token exchange is involved.
func (c *Client) catalogGet(ctx context.Context, ep Endpoint, rawPath string, out any) error {
	ep.API = ep.CatalogAPI
	body, _, err := c.read(ctx, ep, "", rawPath, "application/json", maxMetadataBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return retry.Permanent(fmt.Errorf("oci %s %s: %w", ep.Name, rawPath, err))
	}
	return nil
}

func validNamespace(ns string) bool {
	return ns != "" && ValidRepository(ns) && !strings.Contains(ns, "/")
}

func (c *Client) hubRepositories(ctx context.Context, ep Endpoint, namespace string, fn func(Repository) error) error {
	if !validNamespace(namespace) {
		return fmt.Errorf("oci invalid %s namespace %q", ep.Name, namespace)
	}
	api := ep
	api.API = ep.CatalogAPI
	next := "/v2/namespaces/" + namespace + "/repositories?page_size=100"
	seen := map[string]bool{next: true}
	total := -1
	got := 0
	for {
		var page struct {
			Count   int    `json:"count"`
			Next    string `json:"next"`
			Results []struct {
				Name        string     `json:"name"`
				Namespace   string     `json:"namespace"`
				Description string     `json:"description"`
				Stars       *int64     `json:"star_count"`
				Pulls       *int64     `json:"pull_count"`
				LastUpdated *time.Time `json:"last_updated"`
			} `json:"results"`
		}
		if err := c.catalogGet(ctx, ep, next, &page); err != nil {
			return err
		}
		total = page.Count
		for _, r := range page.Results {
			name := r.Namespace + "/" + r.Name
			if r.Namespace != namespace || !ValidRepository(name) {
				return retry.Permanent(fmt.Errorf("oci %s namespace %s listed invalid repository %q", ep.Name, namespace, name))
			}
			got++
			if err := fn(Repository{Name: name, Description: r.Description, Pulls: r.Pulls, Stars: r.Stars, UpdatedAt: r.LastUpdated}); err != nil {
				return err
			}
		}
		if page.Next == "" {
			break
		}
		path, err := api.sameOrigin(page.Next, "/v2/")
		if err != nil {
			return err
		}
		if seen[path] {
			return retry.Permanent(fmt.Errorf("oci %s repository pagination cycle", ep.Name))
		}
		seen[path] = true
		next = path
	}
	if total >= 0 && got < total {
		return retry.Permanent(fmt.Errorf("oci %s namespace %s ended after %d of %d repositories", ep.Name, namespace, got, total))
	}
	return nil
}

func (c *Client) hubTags(ctx context.Context, ep Endpoint, repo string, fn func(Tag) error) error {
	namespace, name, ok := strings.Cut(repo, "/")
	if !ok || !validNamespace(namespace) || !ValidRepository(name) {
		return fmt.Errorf("oci invalid %s repository %q", ep.Name, repo)
	}
	api := ep
	api.API = ep.CatalogAPI
	next := "/v2/namespaces/" + namespace + "/repositories/" + name + "/tags?page_size=100"
	seen := map[string]bool{next: true}
	for {
		var page struct {
			Next    string `json:"next"`
			Results []struct {
				Name        string    `json:"name"`
				Digest      string    `json:"digest"`
				LastUpdated time.Time `json:"last_updated"`
			} `json:"results"`
		}
		if err := c.catalogGet(ctx, ep, next, &page); err != nil {
			return err
		}
		for _, t := range page.Results {
			if err := fn(Tag{Name: t.Name, Digest: t.Digest, Updated: t.LastUpdated}); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		path, err := api.sameOrigin(page.Next, "/v2/")
		if err != nil {
			return err
		}
		if seen[path] {
			return retry.Permanent(fmt.Errorf("oci %s tag pagination cycle", ep.Name))
		}
		seen[path] = true
		next = path
	}
}

func (c *Client) quayRepositories(ctx context.Context, ep Endpoint, namespace string, fn func(Repository) error) error {
	if namespace != "" && !validNamespace(namespace) {
		return fmt.Errorf("oci invalid %s namespace %q", ep.Name, namespace)
	}
	base := "/api/v1/repository?public=true&limit=100"
	if namespace != "" {
		base += "&namespace=" + url.QueryEscape(namespace)
	}
	next := base
	seen := map[string]bool{}
	for {
		var page struct {
			Repositories []struct {
				Namespace   string  `json:"namespace"`
				Name        string  `json:"name"`
				Description *string `json:"description"`
			} `json:"repositories"`
			NextPage string `json:"next_page"`
		}
		if err := c.catalogGet(ctx, ep, next, &page); err != nil {
			return err
		}
		for _, r := range page.Repositories {
			name := r.Namespace + "/" + r.Name
			if !ValidRepository(name) || (namespace != "" && r.Namespace != namespace) {
				return retry.Permanent(fmt.Errorf("oci %s listed invalid repository %q", ep.Name, name))
			}
			repo := Repository{Name: name}
			if r.Description != nil {
				repo.Description = *r.Description
			}
			if err := fn(repo); err != nil {
				return err
			}
		}
		if page.NextPage == "" {
			return nil
		}
		if seen[page.NextPage] {
			return retry.Permanent(fmt.Errorf("oci %s repository pagination cycle", ep.Name))
		}
		seen[page.NextPage] = true
		next = base + "&next_page=" + url.QueryEscape(page.NextPage)
	}
}

func (c *Client) quayTags(ctx context.Context, ep Endpoint, repo string, fn func(Tag) error) error {
	namespace, name, ok := strings.Cut(repo, "/")
	if !ok || !validNamespace(namespace) || !ValidRepository(name) {
		return fmt.Errorf("oci invalid %s repository %q", ep.Name, repo)
	}
	for page := 1; ; page++ {
		var body struct {
			Tags []struct {
				Name           string `json:"name"`
				ManifestDigest string `json:"manifest_digest"`
				StartTS        int64  `json:"start_ts"`
			} `json:"tags"`
			HasAdditional bool `json:"has_additional"`
		}
		path := fmt.Sprintf("/api/v1/repository/%s/%s/tag/?limit=100&page=%d&onlyActiveTags=true", namespace, name, page)
		if err := c.catalogGet(ctx, ep, path, &body); err != nil {
			return err
		}
		for _, t := range body.Tags {
			tag := Tag{Name: t.Name, Digest: t.ManifestDigest}
			if t.StartTS > 0 {
				tag.Updated = time.Unix(t.StartTS, 0).UTC()
			}
			if err := fn(tag); err != nil {
				return err
			}
		}
		if !body.HasAdditional {
			return nil
		}
		if len(body.Tags) == 0 {
			return retry.Permanent(fmt.Errorf("oci %s tag pagination returned an empty page with more pages pending", ep.Name))
		}
	}
}

// Describe returns what the vendor catalog publishes about one repository.
// Endpoints without a per-repository API (Quay's omits pull counts, GHCR and
// ECR have no catalog) return only the name, so explicit selections still list.
func (c *Client) Describe(ctx context.Context, ep Endpoint, repo string) (Repository, error) {
	if !ValidRepository(repo) {
		return Repository{}, fmt.Errorf("oci invalid repository %q", repo)
	}
	if ep.Catalog != CatalogDockerHub {
		return Repository{Name: repo}, nil
	}
	namespace, name, ok := strings.Cut(repo, "/")
	if !ok || !validNamespace(namespace) {
		return Repository{}, fmt.Errorf("oci invalid %s repository %q", ep.Name, repo)
	}
	var r struct {
		Description string     `json:"description"`
		Stars       *int64     `json:"star_count"`
		Pulls       *int64     `json:"pull_count"`
		LastUpdated *time.Time `json:"last_updated"`
	}
	if err := c.catalogGet(ctx, ep, "/v2/namespaces/"+namespace+"/repositories/"+name, &r); err != nil {
		return Repository{}, err
	}
	return Repository{Name: repo, Description: r.Description, Pulls: r.Pulls, Stars: r.Stars, UpdatedAt: r.LastUpdated}, nil
}
