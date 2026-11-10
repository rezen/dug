// Package oci is a client for OCI distribution registries (Docker Hub, Quay,
// GHCR, ECR Public and anything else that speaks the /v2/ API), shared by
// sources that archive container images and other OCI artifacts.
//
// The client owns the parts every registry agrees on: the anonymous or
// credentialed bearer token exchange, tag listing, manifest and index
// fetching with digest verification, and blob streaming. It never trusts a
// registry-supplied URL: token realms and Link headers must stay on the
// endpoint's own hosts, and blob redirects must stay on HTTPS to public names.
//
// Catalog enumeration is not part of the distribution API. GHCR and ECR have
// none, so catalog.go adds the vendor APIs for Docker Hub and Quay, and a
// source supplies repository names for the rest.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/retry"
)

// Media types the client understands.
const (
	MediaOCIIndex      = "application/vnd.oci.image.index.v1+json"
	MediaDockerList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaOCIManifest   = "application/vnd.oci.image.manifest.v1+json"
	MediaDockerV2      = "application/vnd.docker.distribution.manifest.v2+json"
	maxManifestBytes   = 4 << 20
	maxMetadataBytes   = 32 << 20
	maxTokenBytes      = 1 << 20
	maxConfigBytes     = 8 << 20
	annotationCreated  = "org.opencontainers.image.created"
	annotationRefType  = "vnd.docker.reference.type"
	defaultTokenExpiry = 60 * time.Second
)

const acceptManifests = MediaOCIIndex + "," + MediaDockerList + "," + MediaOCIManifest + "," + MediaDockerV2

var (
	repoPattern   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ErrNotFound reports a repository, tag or blob the registry does not have.
var ErrNotFound = errors.New("oci: not found")

// ValidRepository reports whether name is a legal distribution repository name
// of at most 255 characters.
func ValidRepository(name string) bool { return len(name) <= 255 && repoPattern.MatchString(name) }

// ValidTag reports whether tag is a legal distribution tag.
func ValidTag(tag string) bool { return tagPattern.MatchString(tag) }

// ValidDigest reports whether digest is exactly sha256:<64 lowercase hex>.
func ValidDigest(digest string) bool { return digestPattern.MatchString(digest) }

// Credential is a username and secret for one registry's token endpoint.
type Credential struct {
	Username, Secret string
}

// Client talks to distribution registries. The zero value works with the
// default HTTP client. It is safe for concurrent use.
type Client struct {
	HTTP     *http.Client
	Attempts int // HTTP attempts per request; 0 means 4
	// Credentials by Endpoint.Name. They are sent only to the endpoint's token
	// realm, over the endpoint's own scheme, never to blob redirect targets.
	Credentials map[string]Credential

	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	value   string
	expires time.Time
}

func (c *Client) attempts() int {
	if c.Attempts > 0 {
		return c.Attempts
	}
	return 4
}

// Descriptor points at content by digest.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     *Platform         `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Platform is the os/architecture an index entry targets.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// String formats the platform as os/arch[/variant].
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Manifest is an image manifest, artifact manifest or index.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// IsIndex reports whether the document lists other manifests.
func (m Manifest) IsIndex() bool {
	return m.MediaType == MediaOCIIndex || m.MediaType == MediaDockerList || (m.MediaType == "" && len(m.Manifests) > 0)
}

// IsAttestation reports whether an index entry is a BuildKit attestation
// (SBOM or provenance) attached to another entry.
func (d Descriptor) IsAttestation() bool {
	return d.Annotations[annotationRefType] == "attestation-manifest"
}

// ManifestDoc is a fetched manifest. Digest is computed from Raw, never taken
// from a response header.
type ManifestDoc struct {
	Digest   string
	Raw      []byte
	Manifest Manifest
}

// ValidateDescriptor checks the fields a source persists or fetches by.
func ValidateDescriptor(d Descriptor) error {
	if !ValidDigest(d.Digest) {
		return fmt.Errorf("oci descriptor digest %q is not sha256:<64 lowercase hex>", d.Digest)
	}
	if d.Size < 0 {
		return fmt.Errorf("oci descriptor %s has a negative size", d.Digest)
	}
	return nil
}

// Manifest fetches a manifest or index by tag or digest and decodes it. A
// digest reference is verified against the bytes served; a tag reference
// reports the digest of the bytes that came back.
func (c *Client) Manifest(ctx context.Context, ep Endpoint, repo, ref string) (ManifestDoc, error) {
	if !ValidRepository(repo) {
		return ManifestDoc{}, fmt.Errorf("oci invalid repository %q", repo)
	}
	if !ValidDigest(ref) && !ValidTag(ref) {
		return ManifestDoc{}, fmt.Errorf("oci invalid manifest reference %q", ref)
	}
	body, _, err := c.read(ctx, ep, repo, "/v2/"+repo+"/manifests/"+ref, acceptManifests, maxManifestBytes)
	if err != nil {
		return ManifestDoc{}, err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if ValidDigest(ref) && digest != ref {
		return ManifestDoc{}, retry.Permanent(fmt.Errorf("oci %s/%s manifest %s served content hashing to %s", ep.Name, repo, ref, digest))
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ManifestDoc{}, retry.Permanent(fmt.Errorf("oci %s/%s manifest %s: %w", ep.Name, repo, ref, err))
	}
	if m.SchemaVersion != 2 {
		return ManifestDoc{}, retry.Permanent(fmt.Errorf("oci %s/%s manifest %s has unsupported schema version %d", ep.Name, repo, ref, m.SchemaVersion))
	}
	return ManifestDoc{Digest: digest, Raw: body, Manifest: m}, nil
}

// Tags lists every tag in a repository in the registry's own order, which is
// lexical on all major registries. It follows Link headers within the
// endpoint's origin and fails on a cycle.
func (c *Client) Tags(ctx context.Context, ep Endpoint, repo string, fn func(tag string) error) error {
	if !ValidRepository(repo) {
		return fmt.Errorf("oci invalid repository %q", repo)
	}
	const page = 1000
	next := "/v2/" + repo + "/tags/list?n=" + fmt.Sprint(page)
	seen := map[string]bool{next: true}
	for {
		body, header, err := c.read(ctx, ep, repo, next, "application/json", maxMetadataBytes)
		if err != nil {
			return err
		}
		var list struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return retry.Permanent(fmt.Errorf("oci %s/%s tags: %w", ep.Name, repo, err))
		}
		for _, tag := range list.Tags {
			if !ValidTag(tag) {
				return retry.Permanent(fmt.Errorf("oci %s/%s lists invalid tag %q", ep.Name, repo, tag))
			}
			if err := fn(tag); err != nil {
				return err
			}
		}
		link := linkNext(header.Get("Link"))
		switch {
		case link != "":
			path, err := ep.sameOrigin(link, "/v2/")
			if err != nil {
				return err
			}
			next = path
		case len(list.Tags) >= page:
			next = "/v2/" + repo + "/tags/list?n=" + fmt.Sprint(page) + "&last=" + url.QueryEscape(list.Tags[len(list.Tags)-1])
		default:
			return nil
		}
		if seen[next] {
			return retry.Permanent(fmt.Errorf("oci %s/%s tag pagination cycle", ep.Name, repo))
		}
		seen[next] = true
	}
}

// OpenBlob streams a blob by digest. The context governs the whole transfer;
// callers verify the digest, as the dug host does for every blob.
func (c *Client) OpenBlob(ctx context.Context, ep Endpoint, repo, digest string) (io.ReadCloser, error) {
	if !ValidRepository(repo) || !ValidDigest(digest) {
		return nil, retry.Permanent(fmt.Errorf("oci invalid blob %s@%s", repo, digest))
	}
	resp, err := c.get(ctx, ep, repo, "/v2/"+repo+"/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Created returns when an image was built: the created annotation, else the
// config blob's created field, descending into the first non-attestation entry
// of an index. The zero time means the registry does not say.
func (c *Client) Created(ctx context.Context, ep Endpoint, repo string, doc ManifestDoc) (time.Time, error) {
	if t, ok := parseCreated(doc.Manifest.Annotations); ok {
		return t, nil
	}
	if doc.Manifest.IsIndex() {
		for _, child := range doc.Manifest.Manifests {
			if child.IsAttestation() || ValidateDescriptor(child) != nil {
				continue
			}
			if t, ok := parseCreated(child.Annotations); ok {
				return t, nil
			}
			sub, err := c.Manifest(ctx, ep, repo, child.Digest)
			if err != nil {
				return time.Time{}, err
			}
			if sub.Manifest.IsIndex() {
				return time.Time{}, nil
			}
			return c.configCreated(ctx, ep, repo, sub)
		}
		return time.Time{}, nil
	}
	return c.configCreated(ctx, ep, repo, doc)
}

func (c *Client) configCreated(ctx context.Context, ep Endpoint, repo string, doc ManifestDoc) (time.Time, error) {
	if t, ok := parseCreated(doc.Manifest.Annotations); ok {
		return t, nil
	}
	if ValidateDescriptor(doc.Manifest.Config) != nil || doc.Manifest.Config.Size > maxConfigBytes {
		return time.Time{}, nil
	}
	body, _, err := c.read(ctx, ep, repo, "/v2/"+repo+"/blobs/"+doc.Manifest.Config.Digest, "", maxConfigBytes)
	if err != nil {
		return time.Time{}, err
	}
	var config struct {
		Created time.Time `json:"created"`
	}
	if json.Unmarshal(body, &config) != nil {
		return time.Time{}, nil
	}
	return config.Created, nil
}

func parseCreated(annotations map[string]string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, annotations[annotationCreated])
	return t, err == nil
}

// read fetches one bounded document.
func (c *Client) read(ctx context.Context, ep Endpoint, repo, path, accept string, limit int64) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := c.get(ctx, ep, repo, path, accept)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, retry.Permanent(fmt.Errorf("oci %s response exceeds %d bytes", ep.Name, limit))
	}
	return data, resp.Header, nil
}

// get performs an authenticated GET on the endpoint's API with the token
// exchange and the retry policy of httpx.Get (429, 408 and 5xx retry).
func (c *Client) get(ctx context.Context, ep Endpoint, repo, path, accept string) (*http.Response, error) {
	raw := strings.TrimRight(ep.API, "/") + path
	if err := httpx.ValidateURL(raw); err != nil {
		return nil, retry.Permanent(err)
	}
	origin, _ := url.Parse(ep.API)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	shallow := *client
	shallow.CheckRedirect = redirectPolicy(origin)

	var response *http.Response
	throttled := 0
	err := retry.Do(ctx, c.attempts(), func() error {
		resp, err := c.once(ctx, &shallow, ep, repo, raw, accept)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusOK {
			response = resp
			return nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		err = &httpx.StatusError{URL: resp.Request.URL.Redacted(), Code: resp.StatusCode}
		if resp.StatusCode == http.StatusNotFound {
			return retry.Permanent(fmt.Errorf("%w: %w", ErrNotFound, err))
		}
		if resp.StatusCode != 429 && resp.StatusCode != 408 && resp.StatusCode < 500 {
			return retry.Permanent(err)
		}
		delay := retryAfter(resp.Header.Get("Retry-After"))
		if delay == 0 && resp.StatusCode == http.StatusTooManyRequests {
			// Docker Hub's anonymous limit is per hour; the generic backoff
			// is far shorter than the window.
			delay = min(60*time.Second, 5*time.Second<<min(throttled, 4))
			throttled++
		}
		if delay > 0 {
			if waitErr := retry.Wait(ctx, delay); waitErr != nil {
				return waitErr
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func retryAfter(value string) time.Duration {
	var seconds int
	if _, err := fmt.Sscanf(value, "%d", &seconds); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 3600)) * time.Second
	}
	return 0
}

// once sends the request, answering a Bearer challenge with one token exchange.
func (c *Client) once(ctx context.Context, client *http.Client, ep Endpoint, repo, raw, accept string) (*http.Response, error) {
	for exchange := 0; ; exchange++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, retry.Permanent(err)
		}
		req.Header.Set("User-Agent", "dug/0.1")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if t := c.cached(ep, repo); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || exchange > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if err := c.exchange(ctx, client, ep, repo, challenge); err != nil {
			return nil, err
		}
	}
}

func (c *Client) cached(ep Endpoint, repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[ep.Name+"|"+repo]
	if !ok || time.Now().After(t.expires) {
		return ""
	}
	return t.value
}

// exchange answers a Bearer challenge. The realm must be on the endpoint's own
// token hosts so a hostile registry response cannot point credentials or the
// token request at another server.
func (c *Client) exchange(ctx context.Context, client *http.Client, ep Endpoint, repo, challenge string) error {
	scheme, params, ok := strings.Cut(strings.TrimSpace(challenge), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return retry.Permanent(fmt.Errorf("oci %s requires unsupported authentication %q", ep.Name, scheme))
	}
	fields := parseChallenge(params)
	realm, err := url.Parse(fields["realm"])
	if err != nil || realm.User != nil || !ep.allowsRealm(realm) {
		return retry.Permanent(fmt.Errorf("oci %s token realm %q is not an allowed token host", ep.Name, fields["realm"]))
	}
	query := realm.Query()
	if service := fields["service"]; service != "" {
		query.Set("service", service)
	}
	query.Set("scope", "repository:"+repo+":pull")
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return retry.Permanent(err)
	}
	req.Header.Set("User-Agent", "dug/0.1")
	if cred, ok := c.Credentials[ep.Name]; ok && cred.Secret != "" {
		req.SetBasicAuth(cred.Username, cred.Secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		err := &httpx.StatusError{URL: req.URL.Redacted(), Code: resp.StatusCode}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return err
		}
		return retry.Permanent(err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes+1))
	if err != nil {
		return err
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if len(data) > maxTokenBytes || json.Unmarshal(data, &body) != nil {
		return retry.Permanent(fmt.Errorf("oci %s returned an unreadable token response", ep.Name))
	}
	value := body.Token
	if value == "" {
		value = body.AccessToken
	}
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return retry.Permanent(fmt.Errorf("oci %s returned no usable token", ep.Name))
	}
	ttl := defaultTokenExpiry
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]token{}
	}
	// Refresh early so a token never expires mid-request.
	c.tokens[ep.Name+"|"+repo] = token{value: value, expires: time.Now().Add(ttl - min(ttl/4, 10*time.Second))}
	return nil
}

// parseChallenge reads key="value" pairs from a WWW-Authenticate parameter list.
func parseChallenge(params string) map[string]string {
	out := map[string]string{}
	for params = strings.TrimSpace(params); params != ""; {
		key, rest, ok := strings.Cut(params, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			value, rest = rest[1:1+end], rest[end+2:]
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		out[key] = value
		params = strings.TrimLeft(rest, ", ")
	}
	return out
}

// linkNext extracts the rel="next" target from a Link header.
func linkNext(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, attrs, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(attrs, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(target), "<>")
	}
	return ""
}

// redirectPolicy lets a blob download follow the registry to its CDN, and
// nowhere unsafe: the scheme cannot downgrade, and a different host must be a
// public DNS name rather than an IP literal, localhost, or an internal name.
func redirectPolicy(origin *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return retry.Permanent(errors.New("oci too many redirects"))
		}
		if req.URL.Scheme != origin.Scheme || req.URL.User != nil {
			return retry.Permanent(fmt.Errorf("oci redirect to %s changes scheme or carries credentials", req.URL.Redacted()))
		}
		if strings.EqualFold(req.URL.Host, origin.Host) {
			return nil
		}
		host := strings.ToLower(req.URL.Hostname())
		if net.ParseIP(host) != nil || host == "localhost" || !strings.Contains(host, ".") ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
			return retry.Permanent(fmt.Errorf("oci redirect to %s is not a public host", req.URL.Redacted()))
		}
		return nil
	}
}
