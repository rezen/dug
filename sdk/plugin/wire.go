// Package plugin connects registry implementations to hosts over Connect RPC.
package plugin

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	registryv1 "github.com/rezen/dug/sdk/gen/dug/registry/v1"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/retry"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const ProtocolVersion = 1
const MaxChunkBytes = 256 << 10
const maxMessageBytes = 24 << 20

func referenceToWire(ref registry.Reference) *registryv1.Reference {
	return &registryv1.Reference{Registry: ref.Registry, Namespace: ref.Namespace, Name: ref.Name, Version: ref.Version, Attributes: maps.Clone(ref.Attributes), Metadata: maps.Clone(ref.Metadata), Facts: factsToWire(ref.Facts), Tags: slices.Clone(ref.Tags), Author: authorToWire(ref.Author), Raw: slices.Clone(ref.Raw)}
}
func referenceFromWire(ref *registryv1.Reference) registry.Reference {
	return registry.Reference{Registry: ref.GetRegistry(), Namespace: ref.GetNamespace(), Name: ref.GetName(), Version: ref.GetVersion(), Attributes: maps.Clone(ref.GetAttributes()), Metadata: maps.Clone(ref.GetMetadata()), Facts: factsFromWire(ref.GetFacts()), Tags: slices.Clone(ref.GetTags()), Author: authorFromWire(ref.GetAuthor()), Raw: slices.Clone(ref.GetRaw())}
}
func authorToWire(a *registry.Author) *registryv1.Author {
	if a == nil {
		return nil
	}
	return &registryv1.Author{Name: a.Name, Username: a.Username, Id: a.ID, Url: a.URL, Email: a.Email}
}
func authorFromWire(a *registryv1.Author) *registry.Author {
	if a == nil {
		return nil
	}
	return &registry.Author{Name: a.GetName(), Username: a.GetUsername(), ID: a.GetId(), URL: a.GetUrl(), Email: a.GetEmail()}
}
func factsToWire(f *registry.Facts) *registryv1.Facts {
	if f == nil {
		return nil
	}
	wire := &registryv1.Facts{Downloads: statsToWire(f.Downloads), VersionDownloads: statsToWire(f.VersionDownloads), Installs: cloneInt64(f.Installs)}
	if r := f.Repository; r != nil {
		wire.Repository = &registryv1.Repository{Url: r.URL, Directory: r.Directory, Commit: r.Commit, Source: r.Source}
	}
	if f.ReleasedAt != nil {
		wire.ReleasedAt = timestamppb.New(*f.ReleasedAt)
	}
	if f.UpdatedAt != nil {
		wire.UpdatedAt = timestamppb.New(*f.UpdatedAt)
	}
	return wire
}
func statsToWire(d *registry.DownloadStats) *registryv1.DownloadStats {
	if d == nil {
		return nil
	}
	return &registryv1.DownloadStats{Total: cloneInt64(d.Total), Day: cloneInt64(d.Day), Week: cloneInt64(d.Week), Month: cloneInt64(d.Month), Quarter: cloneInt64(d.Quarter), Year: cloneInt64(d.Year)}
}
func factsFromWire(f *registryv1.Facts) *registry.Facts {
	if f == nil {
		return nil
	}
	facts := &registry.Facts{Downloads: statsFromWire(f.GetDownloads()), VersionDownloads: statsFromWire(f.GetVersionDownloads()), Installs: cloneInt64(f.Installs)}
	if r := f.GetRepository(); r != nil {
		facts.Repository = &registry.Repository{URL: r.GetUrl(), Directory: r.GetDirectory(), Commit: r.GetCommit(), Source: r.GetSource()}
	}
	if t := f.GetReleasedAt(); t != nil {
		facts.ReleasedAt = registry.Time(t.AsTime())
	}
	if t := f.GetUpdatedAt(); t != nil {
		facts.UpdatedAt = registry.Time(t.AsTime())
	}
	return facts
}
func statsFromWire(d *registryv1.DownloadStats) *registry.DownloadStats {
	if d == nil {
		return nil
	}
	return &registry.DownloadStats{Total: cloneInt64(d.Total), Day: cloneInt64(d.Day), Week: cloneInt64(d.Week), Month: cloneInt64(d.Month), Quarter: cloneInt64(d.Quarter), Year: cloneInt64(d.Year)}
}
func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	return registry.Int64(*v)
}
func validateReference(id string, ref *registryv1.Reference) error {
	if ref.GetRegistry() != id || ref.GetName() == "" || ref.GetVersion() == "" {
		return fmt.Errorf("reference must identify a versioned resource in registry %q", id)
	}
	for _, t := range []*timestamppb.Timestamp{ref.GetFacts().GetReleasedAt(), ref.GetFacts().GetUpdatedAt()} {
		if t != nil && t.CheckValid() != nil {
			return fmt.Errorf("reference facts: invalid timestamp")
		}
	}
	if err := referenceFromWire(ref).ValidateListing(); err != nil {
		return fmt.Errorf("reference facts: %w", err)
	}
	return nil
}
func validateArtifact(id string, artifact *registryv1.Artifact) error {
	if err := validateReference(id, artifact.GetReference()); err != nil {
		return err
	}
	if artifact.GetRevision() == "" || len(artifact.GetBlobs()) == 0 {
		return fmt.Errorf("artifact requires a revision and blobs")
	}
	seen := make(map[string]bool)
	for _, blob := range artifact.GetBlobs() {
		if !validBlobName(blob.GetName()) || seen[blob.GetName()] || blob.GetSize() < -1 {
			return fmt.Errorf("blob names must be unique plain file names without path separators, with size >= -1")
		}
		seen[blob.GetName()] = true
		if digest := blob.GetDigest(); digest != "" {
			raw := strings.TrimPrefix(digest, "sha256:")
			decoded, err := hex.DecodeString(raw)
			if err != nil || len(decoded) != 32 || digest != "sha256:"+hex.EncodeToString(decoded) {
				return fmt.Errorf("invalid blob SHA-256")
			}
		}
	}
	return nil
}

// validBlobName rejects names that downstream tools could treat as paths:
// manifests record blob names and consumers write them to disk.
func validBlobName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func artifactToWire(artifact registry.Artifact) *registryv1.Artifact {
	result := &registryv1.Artifact{Reference: referenceToWire(artifact.Reference), Revision: artifact.Revision}
	for _, blob := range artifact.Blobs {
		result.Blobs = append(result.Blobs, &registryv1.Blob{Name: blob.Name, MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size})
	}
	return result
}

func serverError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr *connect.Error
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	if retry.IsPermanent(err) {
		// Keep the plugin's do-not-retry verdict across the wire; the client
		// maps failed_precondition back to a permanent error.
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeUnavailable, err)
}

func clientError(err error) error {
	if err == nil {
		return nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeUnimplemented, connect.CodeDataLoss:
		return retry.Permanent(err)
	default:
		return err
	}
}
