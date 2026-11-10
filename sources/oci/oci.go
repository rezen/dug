// Package oci archives container images and other OCI artifacts from
// distribution registries: Docker Hub, Quay, GHCR and ECR Public. Docker Hub
// and Quay enumerate through their vendor catalog APIs; GHCR and ECR have no
// catalog, so their repositories come from the repos option.
//
// A reference is one tag, pinned at listing time to the manifest or index
// digest it pointed at. Resolve fetches that digest, so a retagged or deleted
// upstream tag cannot change what stage two downloads.
package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services/oci"
)

// Identity is the registry name the host selects: `dug list oci`.
const Identity = "oci"

// DefaultPlatform is selected from multi-platform indexes unless the platform
// option says otherwise.
const DefaultPlatform = "linux/amd64"

// AllPlatforms selects every entry of an index, attestations included.
const AllPlatforms = "all"

// DefaultMaxTags bounds how many tags of a registry without a catalog API are
// inspected per repository.
const DefaultMaxTags = 1000

// DefaultRegistries are walked when no selector is given: Docker Hub's
// official images and every public Quay repository.
var DefaultRegistries = []string{"docker.io", "quay.io"}

var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(?:/[a-z0-9]+)?$`)

type Source struct {
	// OCI is the distribution client. Its token cache is shared by every call
	// on this Source, so build one per process, not one per request.
	OCI *oci.Client
	// Endpoints overrides oci.Endpoints(); tests point it at httptest.
	Endpoints map[string]oci.Endpoint

	// Query options. PluginFactory validates them; Discover re-checks what it
	// relies on because a Source can also be constructed directly.
	Repos        []string // HOST/REPOSITORY
	Namespaces   []string // HOST/NAMESPACE, only for registries with a catalog
	Registries   []string // registries walked when no selector is given
	Platform     string   // os/arch[/variant] or "all"; empty means DefaultPlatform
	MaxLayerSize int64    // layers above this many bytes are omitted; 0 means no limit
	MaxTags      int      // 0 means DefaultMaxTags
	VersionDepth string   // empty means the newest tag only
}

var _ registry.Registry = (*Source)(nil)

var errStop = errors.New("version depth reached")

func (s *Source) endpoint(name string) (oci.Endpoint, error) {
	endpoints := s.Endpoints
	if endpoints == nil {
		endpoints = oci.Endpoints()
	}
	ep, ok := endpoints[name]
	if !ok {
		names := make([]string, 0, len(endpoints))
		for n := range endpoints {
			names = append(names, n)
		}
		sort.Strings(names)
		return oci.Endpoint{}, fmt.Errorf("oci registry %q is not supported (supported: %s)", name, strings.Join(names, ", "))
	}
	return ep, nil
}

func (s *Source) platform() string {
	if s.Platform != "" {
		return s.Platform
	}
	return DefaultPlatform
}

// ValidatePlatform accepts "all" or os/arch[/variant].
func ValidatePlatform(value string) error {
	if value != AllPlatforms && !platformPattern.MatchString(value) {
		return fmt.Errorf("oci platform %q must be all or os/arch[/variant], for example linux/amd64", value)
	}
	return nil
}

// tagInfo is one tag to emit, newest first.
type tagInfo struct {
	name, digest, source string
	released             time.Time
}

// Discover emits one reference per selected tag. Without selectors it walks
// the catalogs of s.Registries. Repositories and tags are deduplicated because
// live catalogs shift under pagination.
func (s *Source) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	depth, err := registry.ParseVersionDepth(s.VersionDepth)
	if err != nil {
		return err
	}
	if err := ValidatePlatform(s.platform()); err != nil {
		return err
	}
	seen := map[string]bool{}
	visit := func(ep oci.Endpoint, repo oci.Repository) error {
		key := ep.Name + "/" + repo.Name
		if seen[key] {
			return nil
		}
		seen[key] = true
		return s.emitRepository(ctx, ep, repo, depth, emit)
	}
	switch {
	case len(s.Repos) > 0 || len(s.Namespaces) > 0:
		for _, name := range s.Repos {
			host, repo, err := oci.ParseReference(name)
			if err != nil {
				return err
			}
			ep, err := s.endpoint(host)
			if err != nil {
				return err
			}
			if seen[ep.Name+"/"+repo] {
				continue
			}
			// An explicit selection that does not exist is an error, not a skip.
			info, err := s.OCI.Describe(ctx, ep, repo)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", host, repo, err)
			}
			if err := visit(ep, info); err != nil {
				return err
			}
		}
		for _, name := range s.Namespaces {
			host, namespace, ok := strings.Cut(name, "/")
			if !ok || namespace == "" {
				return fmt.Errorf("oci namespace %q must look like HOST/NAMESPACE, for example quay.io/coreos", name)
			}
			ep, err := s.endpoint(host)
			if err != nil {
				return err
			}
			if err := s.OCI.Repositories(ctx, ep, namespace, func(r oci.Repository) error { return visit(ep, r) }); err != nil {
				return fmt.Errorf("%s/%s: %w", host, namespace, err)
			}
		}
		return nil
	}
	registries := s.Registries
	if len(registries) == 0 {
		registries = DefaultRegistries
	}
	for _, host := range registries {
		ep, err := s.endpoint(host)
		if err != nil {
			return err
		}
		namespace := ""
		if ep.Catalog == oci.CatalogDockerHub {
			// Docker Hub cannot list every namespace; official images are
			// the one namespace that is a catalog in itself.
			namespace = "library"
		}
		if err := s.OCI.Repositories(ctx, ep, namespace, func(r oci.Repository) error { return visit(ep, r) }); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
	}
	return nil
}

// emitRepository lists a repository's tags newest first and emits references
// while the depth allows.
func (s *Source) emitRepository(ctx context.Context, ep oci.Endpoint, repo oci.Repository, depth registry.VersionDepth, emit func(registry.Reference) error) error {
	// Repository-wide facts are attached to every tag, including old ones; they
	// are a listing-time snapshot, not per-tag history.
	facts := &registry.Facts{}
	if repo.Pulls != nil {
		facts.Downloads = &registry.DownloadStats{Total: registry.Int64(*repo.Pulls)}
	}
	if repo.UpdatedAt != nil {
		facts.UpdatedAt = registry.Time(*repo.UpdatedAt)
	}
	metadata := map[string]string{}
	if repo.Stars != nil {
		metadata["stars"] = strconv.FormatInt(*repo.Stars, 10)
	}
	count := 0
	seenTags := map[string]bool{}
	add := func(t tagInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seenTags[t.name] {
			return nil
		}
		seenTags[t.name] = true
		if !depth.Since.IsZero() {
			if t.released.IsZero() {
				return fmt.Errorf("%s/%s:%s: missing release date for a date cutoff", ep.Name, repo.Name, t.name)
			}
			if t.released.Before(depth.Since) {
				return nil
			}
		}
		if !oci.ValidTag(t.name) || !oci.ValidDigest(t.digest) {
			slog.Warn("oci skipping tag without a usable manifest digest", "registry", ep.Name, "repository", repo.Name, "tag", t.name)
			return nil
		}
		versionMetadata := maps.Clone(metadata)
		versionMetadata["version_rank"] = strconv.Itoa(count + 1)
		versionFacts := facts.Clone()
		if !t.released.IsZero() {
			versionFacts.ReleasedAt = registry.Time(t.released)
			versionMetadata["release_date_source"] = t.source
		}
		attributes := map[string]string{"manifest_digest": t.digest, "platform": s.platform()}
		if s.MaxLayerSize > 0 {
			attributes["max_layer_size"] = strconv.FormatInt(s.MaxLayerSize, 10)
		}
		ref := registry.Reference{
			Registry:  Identity,
			Namespace: ep.Name,
			// Name carries the registry host because the storage layout keys
			// artifacts by name and version alone: without it,
			// docker.io/grafana/grafana and ghcr.io/grafana/grafana collide.
			Name:       ep.Name + "/" + repo.Name,
			Version:    t.name,
			Attributes: attributes,
			Metadata:   versionMetadata,
			Facts:      versionFacts,
		}
		// Resolution fetches manifests, which would spend Docker Hub's hourly
		// pull allowance on every tag, so listing checks the pinned fields
		// offline and leaves the manifest walk to stage two.
		if _, err := s.validate(ref); err != nil {
			return fmt.Errorf("%s/%s:%s: %w", ep.Name, repo.Name, t.name, err)
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

	var err error
	if ep.Catalog != oci.CatalogNone {
		source := "dockerhub-tag-last-updated"
		if ep.Catalog == oci.CatalogQuay {
			source = "quay-tag-start"
		}
		err = s.OCI.CatalogTags(ctx, ep, repo.Name, func(t oci.Tag) error {
			return add(tagInfo{name: t.Name, digest: t.Digest, released: t.Updated, source: source})
		})
	} else {
		var tags []tagInfo
		tags, err = s.inspectTags(ctx, ep, repo.Name)
		for _, t := range tags {
			if err != nil {
				break
			}
			err = add(t)
		}
	}
	if errors.Is(err, errStop) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s/%s: %w", ep.Name, repo.Name, err)
	}
	return nil
}

// inspectTags handles registries with no catalog API. The distribution tag
// list is lexical, so recency comes from each image's creation time, which costs
// a manifest request per tag. A repository with more tags than MaxTags fails
// instead of being truncated.
func (s *Source) inspectTags(ctx context.Context, ep oci.Endpoint, repo string) ([]tagInfo, error) {
	limit := s.MaxTags
	if limit <= 0 {
		limit = DefaultMaxTags
	}
	var names []string
	if err := s.OCI.Tags(ctx, ep, repo, func(tag string) error {
		names = append(names, tag)
		if len(names) > limit {
			return retry.Permanent(fmt.Errorf("has more than %d tags; raise max_tags or narrow version_depth with a tag-light repository", limit))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	tags := make([]tagInfo, 0, len(names))
	for _, name := range names {
		doc, err := s.OCI.Manifest(ctx, ep, repo, name)
		if err != nil {
			return nil, fmt.Errorf("tag %s: %w", name, err)
		}
		created, err := s.OCI.Created(ctx, ep, repo, doc)
		if err != nil {
			return nil, fmt.Errorf("tag %s: %w", name, err)
		}
		tags = append(tags, tagInfo{name: name, digest: doc.Digest, released: created, source: "image-created"})
	}
	sort.SliceStable(tags, func(i, j int) bool {
		a, b := tags[i], tags[j]
		if a.released.IsZero() != b.released.IsZero() {
			return !a.released.IsZero()
		}
		if !a.released.Equal(b.released) {
			return a.released.After(b.released)
		}
		return a.name < b.name
	})
	return tags, nil
}

// validated is a reference after every persisted field has been checked.
type validated struct {
	ep           oci.Endpoint
	repo         string
	digest       string
	platform     string
	maxLayerSize int64
}

// validate trusts nothing in the reference. No URL is persisted: every request
// is built from the validated registry, repository and digest, and the
// endpoint's token realm and redirect rules apply on top.
func (s *Source) validate(ref registry.Reference) (validated, error) {
	if ref.Registry != Identity || ref.Version == "" || !oci.ValidTag(ref.Version) {
		return validated{}, fmt.Errorf("invalid %s reference", Identity)
	}
	ep, err := s.endpoint(ref.Namespace)
	if err != nil {
		return validated{}, err
	}
	repo, ok := strings.CutPrefix(ref.Name, ep.Name+"/")
	if !ok || !oci.ValidRepository(repo) {
		return validated{}, fmt.Errorf("invalid %s reference: name %q must be %s/REPOSITORY", Identity, ref.Name, ep.Name)
	}
	digest := ref.Attributes["manifest_digest"]
	if !oci.ValidDigest(digest) {
		return validated{}, fmt.Errorf("invalid %s manifest digest", Identity)
	}
	platform := ref.Attributes["platform"]
	if err := ValidatePlatform(platform); err != nil {
		return validated{}, err
	}
	v := validated{ep: ep, repo: repo, digest: digest, platform: platform}
	if raw, ok := ref.Attributes["max_layer_size"]; ok {
		v.maxLayerSize, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || v.maxLayerSize < 1 || strconv.FormatInt(v.maxLayerSize, 10) != raw {
			return validated{}, fmt.Errorf("invalid %s max_layer_size", Identity)
		}
	}
	return v, nil
}

// Resolve fetches the pinned manifest digest, selects the platform, and lists
// the manifest, config and layer blobs of every selected image. Revision is the
// pinned digest, so a tag that moved upstream resolves to the old content or
// fails when the registry has dropped it; it is never replaced by a newer one.
func (s *Source) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	v, err := s.validate(ref)
	if err != nil {
		return registry.Artifact{}, err
	}
	doc, err := s.OCI.Manifest(ctx, v.ep, v.repo, v.digest)
	if err != nil {
		return registry.Artifact{}, fmt.Errorf("%s/%s:%s: %w", v.ep.Name, v.repo, ref.Version, err)
	}
	b := &blobSet{s: s, v: v, byName: map[string]string{}}
	var images []oci.ManifestDoc
	if doc.Manifest.IsIndex() {
		if err := b.addManifest(doc, oci.MediaOCIIndex); err != nil {
			return registry.Artifact{}, err
		}
		// An index whose entries carry no platform is not a multi-arch image: it
		// is a signature, SBOM or referrer index (cosign publishes these under
		// sha256-<digest> tags). Platform selection cannot apply, and these are
		// exactly what a reviewer wants, so every entry is archived.
		platformless := !slices.ContainsFunc(doc.Manifest.Manifests, func(d oci.Descriptor) bool { return d.Platform != nil })
		var available []string
		for _, entry := range doc.Manifest.Manifests {
			if err := oci.ValidateDescriptor(entry); err != nil {
				return registry.Artifact{}, fmt.Errorf("%s/%s:%s: %w", v.ep.Name, v.repo, ref.Version, err)
			}
			label := "unknown"
			if entry.Platform != nil {
				label = entry.Platform.String()
			}
			if entry.IsAttestation() {
				label += " (attestation)"
			}
			available = append(available, label)
			if !platformless && !selects(v.platform, entry) {
				continue
			}
			child, err := s.OCI.Manifest(ctx, v.ep, v.repo, entry.Digest)
			if err != nil {
				return registry.Artifact{}, fmt.Errorf("%s/%s:%s %s: %w", v.ep.Name, v.repo, ref.Version, label, err)
			}
			if child.Manifest.IsIndex() {
				return registry.Artifact{}, fmt.Errorf("%s/%s:%s: nested index %s is not supported", v.ep.Name, v.repo, ref.Version, entry.Digest)
			}
			images = append(images, child)
		}
		if len(images) == 0 {
			return registry.Artifact{}, retry.Permanent(fmt.Errorf("%s/%s:%s has no %s image (available: %s)", v.ep.Name, v.repo, ref.Version, v.platform, strings.Join(available, ", ")))
		}
	} else {
		images = []oci.ManifestDoc{doc}
	}
	for _, image := range images {
		if err := b.addManifest(image, oci.MediaOCIManifest); err != nil {
			return registry.Artifact{}, err
		}
		if err := b.addDescriptor(image.Manifest.Config, "config"); err != nil {
			return registry.Artifact{}, err
		}
		for _, layer := range image.Manifest.Layers {
			if v.maxLayerSize > 0 && layer.Size > v.maxLayerSize {
				slog.Warn("oci omitting layer above max_layer_size", "repository", v.repo, "tag", ref.Version, "digest", layer.Digest, "size", layer.Size, "max_layer_size", v.maxLayerSize)
				continue
			}
			if err := b.addDescriptor(layer, "layer"); err != nil {
				return registry.Artifact{}, err
			}
		}
	}
	return registry.Artifact{Reference: ref, Revision: doc.Digest, Blobs: b.blobs}, nil
}

// selects reports whether an index entry belongs to the requested platform.
// A selection without a variant matches any variant of that os/arch.
func selects(platform string, entry oci.Descriptor) bool {
	if platform == AllPlatforms {
		return true
	}
	if entry.Platform == nil || entry.IsAttestation() {
		return false
	}
	want := strings.Split(platform, "/")
	have := entry.Platform
	if have.OS != want[0] || have.Architecture != want[1] {
		return false
	}
	return len(want) < 3 || have.Variant == want[2]
}

// blobSet accumulates uniquely named blobs and drops repeats of a digest that
// several platforms share.
type blobSet struct {
	s      *Source
	v      validated
	blobs  []registry.Blob
	byName map[string]string // name -> digest
}

func (b *blobSet) add(blob registry.Blob) error {
	if digest, ok := b.byName[blob.Name]; ok {
		if digest == blob.Digest {
			return nil
		}
		return fmt.Errorf("oci blob name %q is used by two digests", blob.Name)
	}
	b.byName[blob.Name] = blob.Digest
	b.blobs = append(b.blobs, blob)
	return nil
}

// addManifest serves a manifest from the bytes already fetched and verified.
func (b *blobSet) addManifest(doc oci.ManifestDoc, fallback string) error {
	mediaType := doc.Manifest.MediaType
	if mediaType == "" {
		mediaType = fallback
	}
	raw := doc.Raw
	kind := "manifest"
	if doc.Manifest.IsIndex() {
		kind = "index"
	}
	return b.add(registry.Blob{
		Name:      kind + "-" + shortHex(doc.Digest) + ".json",
		MediaType: mediaType,
		Digest:    doc.Digest,
		Size:      int64(len(raw)),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		},
	})
}

// addDescriptor adds a config or layer blob fetched from the registry by digest.
func (b *blobSet) addDescriptor(d oci.Descriptor, kind string) error {
	if err := oci.ValidateDescriptor(d); err != nil {
		return err
	}
	mediaType := d.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	ep, repo, digest := b.v.ep, b.v.repo, d.Digest
	client := b.s.OCI
	return b.add(registry.Blob{
		Name:      kind + "-" + shortHex(d.Digest) + extension(kind, mediaType),
		MediaType: mediaType,
		Digest:    d.Digest,
		Size:      d.Size,
		// Open uses its own context: the resolution context is cancelled
		// before the first transfer, and every call starts at byte zero.
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			return client.OpenBlob(ctx, ep, repo, digest)
		},
	})
}

func shortHex(digest string) string { return strings.TrimPrefix(digest, "sha256:")[:16] }

func extension(kind, mediaType string) string {
	switch {
	case strings.Contains(mediaType, "zstd"):
		return ".tar.zst"
	case strings.Contains(mediaType, "gzip"):
		return ".tar.gz"
	case kind == "layer" && strings.Contains(mediaType, "tar"):
		return ".tar"
	case strings.HasSuffix(mediaType, "json"):
		return ".json"
	}
	return ".blob"
}
