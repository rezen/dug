package plugin

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Serve runs a standalone Connect plugin until cancellation. The source owns
// its address, upstream client and credentials; the SDK supplies stream-safe
// HTTP defaults and graceful shutdown. Deployments needing custom TLS or HTTP/2
// can instead mount NewHandler in their own server.
func Serve(ctx context.Context, address string, opts ServerOptions) error {
	handler, err := NewHandler(opts)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	// No whole-response WriteTimeout: registry streams can be long-lived.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				server.Close()
			}
		case <-finished:
		}
	}()
	slog.Info("registry plugin listening", "registry", opts.Registry, "address", listener.Addr().String(), "protocol_version", ProtocolVersion, "version", opts.Version)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
