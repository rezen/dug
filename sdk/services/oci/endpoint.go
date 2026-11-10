package oci

import (
	"fmt"
	"net/url"
	"strings"
)

// Catalog kinds name the vendor API that can enumerate an endpoint.
const (
	CatalogNone      = ""
	CatalogDockerHub = "dockerhub"
	CatalogQuay      = "quay"
)

// Endpoint describes one registry.
type Endpoint struct {
	Name       string   // short name used in references: docker.io
	API        string   // distribution API origin: https://registry-1.docker.io
	AuthHosts  []string // hosts besides API that may issue tokens
	Catalog    string   // vendor catalog API kind, or CatalogNone
	CatalogAPI string   // vendor catalog API origin
}

// Default endpoints. GHCR and ECR Public have no repository catalog.
var (
	DockerHub = Endpoint{Name: "docker.io", API: "https://registry-1.docker.io", AuthHosts: []string{"auth.docker.io"}, Catalog: CatalogDockerHub, CatalogAPI: "https://hub.docker.com"}
	Quay      = Endpoint{Name: "quay.io", API: "https://quay.io", Catalog: CatalogQuay, CatalogAPI: "https://quay.io"}
	GHCR      = Endpoint{Name: "ghcr.io", API: "https://ghcr.io"}
	ECRPublic = Endpoint{Name: "public.ecr.aws", API: "https://public.ecr.aws"}
)

// Endpoints returns the default endpoints by name.
func Endpoints() map[string]Endpoint {
	m := map[string]Endpoint{}
	for _, e := range []Endpoint{DockerHub, Quay, GHCR, ECRPublic} {
		m[e.Name] = e
	}
	return m
}

// Hosts lists the hostnames the endpoint's API and token service use, for the
// source's documentation of where bytes are fetched.
func (e Endpoint) Hosts() []string {
	u, _ := url.Parse(e.API)
	hosts := []string{u.Host}
	return append(hosts, e.AuthHosts...)
}

// allowsRealm reports whether a token realm may be contacted: the endpoint's
// own API or auth hosts, on the API's scheme.
func (e Endpoint) allowsRealm(realm *url.URL) bool {
	api, err := url.Parse(e.API)
	if err != nil || realm.Scheme != api.Scheme {
		return false
	}
	if strings.EqualFold(realm.Host, api.Host) {
		return true
	}
	for _, h := range e.AuthHosts {
		if strings.EqualFold(realm.Host, h) {
			return true
		}
	}
	return false
}

// sameOrigin validates a registry-supplied pagination link and returns it as a
// path and query relative to the API origin.
func (e Endpoint) sameOrigin(raw, prefix string) (string, error) {
	api, err := url.Parse(e.API)
	if err != nil {
		return "", err
	}
	u, err := api.Parse(raw)
	if err != nil || u.Scheme != api.Scheme || !strings.EqualFold(u.Host, api.Host) || !strings.HasPrefix(u.Path, prefix) {
		return "", fmt.Errorf("oci %s pagination left the API origin", e.Name)
	}
	return u.RequestURI(), nil
}

// ParseReference splits "docker.io/library/alpine" into its endpoint name and
// repository. A Docker Hub repository without a namespace gets library/.
func ParseReference(ref string) (host, repo string, err error) {
	host, repo, ok := strings.Cut(ref, "/")
	if !ok || host == "" || repo == "" {
		return "", "", fmt.Errorf("oci repository %q must look like HOST/REPOSITORY, for example docker.io/library/alpine", ref)
	}
	if host == "docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if !ValidRepository(repo) {
		return "", "", fmt.Errorf("oci invalid repository %q", repo)
	}
	return host, repo, nil
}
