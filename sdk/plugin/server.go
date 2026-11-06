package plugin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"

	"connectrpc.com/connect"
	registryv1 "github.com/rezen/dug/sdk/gen/dug/registry/v1"
	"github.com/rezen/dug/sdk/gen/dug/registry/v1/registryv1connect"
	"github.com/rezen/dug/sdk/registry"
)

// Factory validates per-call query options and returns a registry implementation.
// It may be called concurrently. Returned registries may share concurrency-safe
// HTTP clients and auth state. No closures or sessions are retained by the server.
type Factory func(options map[string]string) (registry.Registry, error)

type ServerOptions struct {
	Registry string
	// Optional provider metadata. When present it must declare Registry.
	Provider registry.Provider
	Factory  Factory
	// Optional shared bearer token. Terminate TLS before exposing a remote server.
	Token string
}

func NewHandler(opts ServerOptions) (http.Handler, error) {
	if opts.Registry == "" || opts.Factory == nil {
		return nil, fmt.Errorf("registry identity and factory are required")
	}
	if err := validateProvider(opts.Provider, opts.Registry); err != nil {
		return nil, err
	}
	path, handler := registryv1connect.NewRegistryServiceHandler(&server{id: opts.Registry, provider: opts.Provider.Clone(), factory: opts.Factory}, connect.WithReadMaxBytes(maxMessageBytes), connect.WithSendMaxBytes(maxMessageBytes))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	if opts.Token == "" {
		return mux, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+opts.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

type server struct {
	id       string
	provider registry.Provider
	factory  Factory
}

var _ registryv1connect.RegistryServiceHandler = (*server)(nil)

func (s *server) Info(ctx context.Context, req *connect.Request[registryv1.InfoRequest]) (*connect.Response[registryv1.InfoResponse], error) {
	return connect.NewResponse(&registryv1.InfoResponse{Registry: s.id, ProtocolVersion: ProtocolVersion, Provider: providerToWire(s.provider)}), nil
}

func (s *server) source(options map[string]string) (registry.Registry, error) {
	source, err := s.factory(maps.Clone(options))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if source == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("registry factory returned nil"))
	}
	return source, nil
}

func (s *server) Discover(ctx context.Context, req *connect.Request[registryv1.DiscoverRequest], stream *connect.ServerStream[registryv1.DiscoverResponse]) error {
	source, err := s.source(req.Msg.Options)
	if err != nil {
		return err
	}
	return serverError(source.Discover(ctx, func(ref registry.Reference) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		wire := referenceToWire(ref)
		if err := validateReference(s.id, wire); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		return stream.Send(&registryv1.DiscoverResponse{Reference: wire})
	}))
}

func (s *server) resolve(ctx context.Context, ref *registryv1.Reference, options map[string]string) (registry.Artifact, error) {
	if err := validateReference(s.id, ref); err != nil {
		return registry.Artifact{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	source, err := s.source(options)
	if err != nil {
		return registry.Artifact{}, err
	}
	artifact, err := source.Resolve(ctx, referenceFromWire(ref))
	if err != nil {
		return registry.Artifact{}, serverError(err)
	}
	if err := validateArtifact(s.id, artifactToWire(artifact)); err != nil {
		return registry.Artifact{}, connect.NewError(connect.CodeInternal, err)
	}
	for _, blob := range artifact.Blobs {
		if blob.Open == nil {
			return registry.Artifact{}, connect.NewError(connect.CodeInternal, fmt.Errorf("blob has no opener"))
		}
	}
	return artifact, nil
}

func (s *server) Resolve(ctx context.Context, req *connect.Request[registryv1.ResolveRequest]) (*connect.Response[registryv1.ResolveResponse], error) {
	artifact, err := s.resolve(ctx, req.Msg.Reference, req.Msg.Options)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&registryv1.ResolveResponse{Artifact: artifactToWire(artifact)}), nil
}

func (s *server) Download(ctx context.Context, req *connect.Request[registryv1.DownloadRequest], stream *connect.ServerStream[registryv1.DownloadResponse]) (resultErr error) {
	if req.Msg.Revision == "" || req.Msg.BlobName == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("revision and blob name are required"))
	}
	artifact, err := s.resolve(ctx, req.Msg.Reference, req.Msg.Options)
	if err != nil {
		return err
	}
	if artifact.Revision != req.Msg.Revision {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("resource revision changed; resolve again"))
	}
	for _, blob := range artifact.Blobs {
		if blob.Name != req.Msg.BlobName {
			continue
		}
		body, err := blob.Open(ctx)
		if err != nil {
			return serverError(err)
		}
		defer func() {
			if err := body.Close(); err != nil && resultErr == nil {
				resultErr = serverError(err)
			}
		}()
		buffer := make([]byte, MaxChunkBytes)
		for {
			if err := ctx.Err(); err != nil {
				return serverError(err)
			}
			// ReadAtLeast avoids spinning on readers that occasionally return 0,nil.
			n, readErr := io.ReadAtLeast(body, buffer, 1)
			if n > 0 {
				if err := stream.Send(&registryv1.DownloadResponse{Data: buffer[:n]}); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return serverError(readErr)
			}
		}
	}
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("blob not found"))
}
