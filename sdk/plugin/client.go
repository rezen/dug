package plugin

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"
	registryv1 "github.com/rezen/dug/sdk/gen/dug/registry/v1"
	"github.com/rezen/dug/sdk/gen/dug/registry/v1/registryv1connect"
	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services"
)

type ClientOptions struct {
	Registry string            // expected identity; checked during handshake
	Options  map[string]string // non-secret, plugin-specific query options
	Token    string
}

// Client implements the in-process engine interface using a remote plugin.
type Client struct {
	rpc      registryv1connect.RegistryServiceClient
	opts     ClientOptions
	provider registry.Provider
	version  string
}

var _ registry.Registry = (*Client)(nil)

// NewClient checks the plugin's identity and protocol version with a bounded
// handshake. The HTTP client must not set a short overall timeout for streams.
func NewClient(ctx context.Context, httpClient *http.Client, endpoint string, opts ClientOptions) (*Client, error) {
	if err := httpx.ValidateURL(endpoint); err != nil {
		return nil, err
	}
	if opts.Registry == "" {
		return nil, fmt.Errorf("expected registry identity is required")
	}
	if opts.Token != "" {
		if err := tokenTransportAllowed(endpoint); err != nil {
			return nil, err
		}
	}
	if httpClient == nil {
		httpClient = services.DefaultDugHttpClient()
	}
	opts.Options = maps.Clone(opts.Options)
	// Archives are typically already compressed; avoid gzip CPU/memory overhead.
	client := &Client{rpc: registryv1connect.NewRegistryServiceClient(httpClient, endpoint, connect.WithReadMaxBytes(maxMessageBytes), connect.WithSendMaxBytes(maxMessageBytes), connect.WithAcceptCompression("gzip", nil, nil)), opts: opts}
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := client.rpc.Info(handshake, request(&registryv1.InfoRequest{}, opts.Token))
	if err != nil {
		return nil, fmt.Errorf("plugin handshake: %w", clientError(err))
	}
	if info.Msg.Registry != opts.Registry || info.Msg.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("incompatible plugin: registry=%q protocol=%d (expected %q/%d)", info.Msg.Registry, info.Msg.ProtocolVersion, opts.Registry, ProtocolVersion)
	}
	client.provider = providerFromWire(info.Msg.Provider)
	client.version = info.Msg.SourceVersion
	if info.Msg.Provider != nil {
		if err := client.provider.Validate(); err != nil {
			return nil, fmt.Errorf("plugin provider: %w", err)
		}
	}
	if err := validateProvider(client.provider, opts.Registry); err != nil {
		return nil, fmt.Errorf("plugin provider: %w", err)
	}
	return client, nil
}

// SourceVersion returns the plugin's release version from the handshake, or ""
// when the plugin predates the field or was built without one.
func (c *Client) SourceVersion() string { return c.version }

// Provider returns the ecosystem and catalogs advertised during the handshake.
// Older plugins may omit this metadata, in which case the result is zero-valued.
// Registry identity checks remain mandatory regardless of provider metadata.
func (c *Client) Provider() registry.Provider { return c.provider.Clone() }

// tokenTransportAllowed refuses to send a bearer token where the network could
// read it: only TLS endpoints or loopback plugins may receive one.
func tokenTransportAllowed(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("plugin token would be sent in plaintext to %q; use an https plugin URL or a loopback address", u.Host)
}

func request[T any](message *T, token string) *connect.Request[T] {
	req := connect.NewRequest(message)
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	return req
}

func (c *Client) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.Discover(ctx, request(&registryv1.DiscoverRequest{Options: maps.Clone(c.opts.Options)}, c.opts.Token))
	if err != nil {
		return clientError(err)
	}
	defer stream.Close()
	for stream.Receive() {
		ref := stream.Msg().Reference
		if err := validateReference(c.opts.Registry, ref); err != nil {
			return err
		}
		if err := emit(referenceFromWire(ref)); err != nil {
			cancel()
			return err
		}
	}
	return clientError(stream.Err())
}

func (c *Client) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	response, err := c.rpc.Resolve(ctx, request(&registryv1.ResolveRequest{Reference: referenceToWire(ref), Options: maps.Clone(c.opts.Options)}, c.opts.Token))
	if err != nil {
		return registry.Artifact{}, clientError(err)
	}
	wire := response.Msg.Artifact
	if err := validateArtifact(c.opts.Registry, wire); err != nil {
		return registry.Artifact{}, clientError(connect.NewError(connect.CodeDataLoss, err))
	}
	artifact := registry.Artifact{Reference: referenceFromWire(wire.Reference), Revision: wire.Revision}
	for _, blob := range wire.Blobs {
		artifact.Blobs = append(artifact.Blobs, registry.Blob{Name: blob.Name, MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				ctx, cancel := context.WithCancel(ctx)
				stream, err := c.rpc.Download(ctx, request(&registryv1.DownloadRequest{Reference: referenceToWire(artifact.Reference), Revision: artifact.Revision, BlobName: blob.Name, Options: maps.Clone(c.opts.Options)}, c.opts.Token))
				if err != nil {
					cancel()
					return nil, clientError(err)
				}
				return &blobReader{stream: stream, cancel: cancel}, nil
			},
		})
	}
	return artifact, nil
}

type blobReader struct {
	stream  *connect.ServerStreamForClient[registryv1.DownloadResponse]
	cancel  context.CancelFunc
	pending []byte
	closed  bool
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, clientError(err)
			}
			return 0, io.EOF
		}
		r.pending = r.stream.Msg().Data
		if len(r.pending) == 0 || len(r.pending) > MaxChunkBytes {
			return 0, clientError(connect.NewError(connect.CodeDataLoss, fmt.Errorf("invalid download chunk size")))
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (r *blobReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.pending = nil
	r.cancel()
	return r.stream.Close()
}
