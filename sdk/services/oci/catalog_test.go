package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHubRepositoriesAndTags(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/namespaces/library/repositories":
			if r.URL.Query().Get("page") == "2" {
				io.WriteString(w, `{"count":2,"next":null,"results":[{"name":"busybox","namespace":"library","description":"Busybox","star_count":3525,"pull_count":13002128980,"last_updated":"2026-10-30T10:58:14.081369Z"}]}`)
				return
			}
			fmt.Fprintf(w, `{"count":2,"next":"%s/v2/namespaces/library/repositories?page=2&page_size=100","results":[{"name":"centos","namespace":"library","star_count":7780,"pull_count":1186962232,"last_updated":"2022-12-09T19:13:54.287062Z"}]}`, server.URL)
		case "/v2/namespaces/library/repositories/alpine/tags":
			io.WriteString(w, `{"count":2,"next":null,"results":[{"name":"latest","digest":"`+sum("a")+`","last_updated":"2026-11-08T14:24:47Z"},{"name":"3.20","digest":"`+sum("b")+`","last_updated":"2026-04-16T23:53:23Z"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ep := Endpoint{Name: "docker.io", API: "http://unused.invalid", Catalog: CatalogDockerHub, CatalogAPI: server.URL}
	c := &Client{HTTP: server.Client(), Attempts: 1}

	var repos []Repository
	if err := c.Repositories(context.Background(), ep, "library", func(r Repository) error { repos = append(repos, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "library/centos" || *repos[0].Pulls != 1186962232 || *repos[1].Stars != 3525 || repos[1].UpdatedAt.Year() != 2026 {
		t.Fatalf("repos = %+v", repos)
	}
	var tags []Tag
	if err := c.CatalogTags(context.Background(), ep, "library/alpine", func(tag Tag) error { tags = append(tags, tag); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Name != "latest" || tags[0].Digest != sum("a") || !tags[1].Updated.Before(tags[0].Updated) {
		t.Fatalf("tags = %+v", tags)
	}
}

func TestHubRepositoriesRejectShortWalkAndCrossOriginNext(t *testing.T) {
	handler := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	}
	short := handler(`{"count":5,"next":null,"results":[{"name":"a","namespace":"library"}]}`)
	defer short.Close()
	c := &Client{Attempts: 1}
	err := c.Repositories(context.Background(), Endpoint{Name: "docker.io", Catalog: CatalogDockerHub, CatalogAPI: short.URL}, "library", func(Repository) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ended after 1 of 5") {
		t.Fatalf("short walk err = %v", err)
	}
	away := handler(`{"count":2,"next":"https://evil.example/v2/namespaces/library/repositories?page=2","results":[{"name":"a","namespace":"library"}]}`)
	defer away.Close()
	err = c.Repositories(context.Background(), Endpoint{Name: "docker.io", Catalog: CatalogDockerHub, CatalogAPI: away.URL}, "library", func(Repository) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "left the API origin") {
		t.Fatalf("cross-origin err = %v", err)
	}
	wrong := handler(`{"count":1,"next":null,"results":[{"name":"a","namespace":"someone-else"}]}`)
	defer wrong.Close()
	err = c.Repositories(context.Background(), Endpoint{Name: "docker.io", Catalog: CatalogDockerHub, CatalogAPI: wrong.URL}, "library", func(Repository) error { return nil })
	if err == nil {
		t.Fatal("repository outside the requested namespace accepted")
	}
}

func TestQuayRepositoriesAndTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/v1/repository":
			if q.Get("next_page") == "tok" {
				io.WriteString(w, `{"repositories":[{"namespace":"coreos","name":"etcd","description":null}]}`)
				return
			}
			if q.Get("namespace") != "coreos" || q.Get("public") != "true" {
				t.Errorf("query = %v", q)
			}
			io.WriteString(w, `{"repositories":[{"namespace":"coreos","name":"flannel","description":"fabric"}],"next_page":"tok"}`)
		case "/api/v1/repository/coreos/etcd/tag/":
			if q.Get("page") == "2" {
				io.WriteString(w, `{"tags":[{"name":"v3.7.2","manifest_digest":"`+sum("old")+`","start_ts":1790000000}],"page":2,"has_additional":false}`)
				return
			}
			io.WriteString(w, `{"tags":[{"name":"v3.7.3","manifest_digest":"`+sum("new")+`","start_ts":1792710403}],"page":1,"has_additional":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ep := Endpoint{Name: "quay.io", API: server.URL, Catalog: CatalogQuay, CatalogAPI: server.URL}
	c := &Client{HTTP: server.Client(), Attempts: 1}
	var names []string
	if err := c.Repositories(context.Background(), ep, "coreos", func(r Repository) error { names = append(names, r.Name); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "coreos/flannel,coreos/etcd" {
		t.Fatalf("names = %v", names)
	}
	var tags []Tag
	if err := c.CatalogTags(context.Background(), ep, "coreos/etcd", func(tag Tag) error { tags = append(tags, tag); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Name != "v3.7.3" || tags[0].Updated != time.Unix(1792710403, 0).UTC() {
		t.Fatalf("tags = %+v", tags)
	}
}

func TestNoCatalog(t *testing.T) {
	c := &Client{}
	for _, ep := range []Endpoint{GHCR, ECRPublic} {
		if err := c.Repositories(context.Background(), ep, "x", nil); !errors.Is(err, ErrNoCatalog) {
			t.Errorf("%s err = %v", ep.Name, err)
		}
		if err := c.CatalogTags(context.Background(), ep, "x/y", nil); !errors.Is(err, ErrNoCatalog) {
			t.Errorf("%s tags err = %v", ep.Name, err)
		}
	}
}

func TestCreatedFallsBackToConfig(t *testing.T) {
	f, ep := newFake(t)
	cfg := `{"created":"2026-04-16T23:53:23Z"}`
	child := `{"schemaVersion":2,"mediaType":"` + MediaOCIManifest + `","config":{"mediaType":"c","digest":"` + sum(cfg) + `","size":` + fmt.Sprint(len(cfg)) + `},"layers":[]}`
	f.manifests[sum(child)] = child
	c := &Client{HTTP: f.Client(), Attempts: 1}
	index := ManifestDoc{Manifest: Manifest{MediaType: MediaOCIIndex, Manifests: []Descriptor{
		{Digest: sum("att"), Annotations: map[string]string{annotationRefType: "attestation-manifest"}},
		{Digest: sum(child), Size: int64(len(child))},
	}}}
	// With the annotation present no request is needed.
	annotated := ManifestDoc{Manifest: Manifest{Annotations: map[string]string{annotationCreated: "2026-01-02T03:04:05Z"}}}
	if got, err := c.Created(context.Background(), ep, "acme/app", annotated); err != nil || got.Year() != 2026 || got.Month() != 1 {
		t.Fatalf("annotated = %v, %v", got, err)
	}
	if got, err := c.Created(context.Background(), ep, "acme/app", ManifestDoc{}); err != nil || !got.IsZero() {
		t.Fatalf("empty = %v, %v", got, err)
	}
	// An index whose first usable child has no config blob route fails
	// loudly rather than guessing.
	if _, err := c.Created(context.Background(), ep, "acme/app", index); err == nil {
		t.Fatal("expected the missing config blob to surface as an error")
	}
}
