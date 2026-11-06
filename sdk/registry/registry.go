// Package registry defines the source-independent discovery and download contract.
package registry

import (
	"context"
	"encoding/json"
	"io"
)

// Reference identifies one version in a registry. Attributes carry adapter-specific
// discovery metadata and must not contain credentials (they are persisted).
type Reference struct {
	Registry string `json:"registry"`
	// Provider and Kind explicitly identify the catalog, when known. Registry
	// remains the stable artifact identity; these labels make saved references
	// self-describing without inferring catalog type from the registry ID.
	Provider   string            `json:"provider,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Namespace  string            `json:"namespace,omitempty"`
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Attributes map[string]string `json:"attributes,omitempty"`
	// Metadata contains mutable registry-specific listing facts, such as star
	// counts or rating histograms. It is excluded from artifact identity.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Facts contains mutable listing facts that mean the same thing in every
	// registry: download counts, install base, source repository and dates.
	// Hosts order and filter on them by path. Excluded from artifact identity.
	Facts *Facts `json:"facts,omitempty"`
	// Tags are the registry's keywords, topics or category slugs for the
	// package, normalized with NormalizeTags. Hosts filter on membership.
	// Excluded from artifact identity.
	Tags []string `json:"tags,omitempty"`
	// Author is the publishing account as the registry describes it.
	// Excluded from artifact identity.
	Author *Author `json:"author,omitempty"`
	// Raw is the whole original upstream record the reference was derived
	// from, verbatim, so nothing the registry published is lost to the typed
	// fields. Excluded from artifact identity.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Registry adapts a resource registry to the shared transfer engine. Implementations
// own discovery, authentication and resource resolution; the engine owns transfer
// concurrency, retries, hashing and storage. No storage backend is needed here.
type Registry interface {
	Lister
	Resolver
}

// Lister is stage one: enumerate resources without resolving or downloading them.
type Lister interface {
	// Discover streams versioned references, calling emit serially. It must wait
	// for emit to return (backpressure), propagate callback errors, and stop when
	// ctx is cancelled. References and their attributes must not be mutated after
	// emission. The host persists the complete result before starting stage two.
	Discover(ctx context.Context, emit func(Reference) error) error
}

// Resolver is stage two: resolve resources selected from a persisted inventory.
// It has no discovery method, so the download engine cannot re-enumerate upstream.
type Resolver interface {
	// Resolve describes every blob needed to archive a discovered reference.
	// Calls may run concurrently and may be retried. The returned artifact must
	// have a stable identity and revision. Resolve must honor ctx, but blob Open
	// functions must use their own supplied contexts: this context is cancelled
	// as soon as resolution finishes. Bulk content is read later through Open.
	Resolve(ctx context.Context, ref Reference) (Artifact, error)
}

// Entries iterates a saved inventory in scheduling order with backpressure.
type Entries func(ctx context.Context, emit func(Reference) error) error

// Source is the original name for Registry, kept for source compatibility.
type Source = Registry

// Artifact is a complete downloadable resource. Revision must change when its
// content changes: for example, a package version or an immutable content digest.
type Artifact struct {
	Reference Reference `json:"reference"`
	Revision  string    `json:"revision"`
	Blobs     []Blob    `json:"-"`
}

// Blob describes one downloadable object. Open must return a fresh reader at
// byte zero on every call, support concurrent calls, and honor its supplied ctx
// throughout reading. The engine closes the reader. Callers may retry Open.
// Open may return before the transfer starts (the Connect client does), so an
// upstream failure can surface from the first Read instead of from Open.
// Size is -1 when unknown. Digest, when supplied, must be a lowercase
// sha256:<hex> digest. Authentication belongs in Open, not persisted attributes.
// Connect plugins require Name to be nonempty, unique within the artifact, and
// stable across repeated resolutions of the same revision.
type Blob struct {
	Name      string
	MediaType string
	Digest    string
	Size      int64
	Open      func(context.Context) (io.ReadCloser, error)
}
