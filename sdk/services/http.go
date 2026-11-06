// Package services provides shared infrastructure for Dug hosts and sources.
package services

import (
	"net"
	"net/http"
	"time"
)

// HTTPClientOption configures a client before it is used by any requests.
type HTTPClientOption func(*http.Client)

// NewDugHttpClient creates an independent client and connection pool. It uses
// environment proxies, HTTP/2, and bounded connection setup and response headers.
// Whole-request timeouts default to zero: download bodies and plugin streams
// use their request context's deadline. Use WithTimeout for bounded operations.
// Reuse the client across requests and call CloseIdleConnections when finished.
func NewDugHttpClient(options ...HTTPClientOption) *http.Client {
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Archives are already compressed; keep their bytes unchanged.
		DisableCompression: true,
	}}
	for _, option := range options {
		option(client)
	}
	return client
}

// WithTimeout sets a whole-request deadline, including reading the body.
// Zero disables this deadline; request context deadlines still apply.
func WithTimeout(timeout time.Duration) HTTPClientOption {
	return func(client *http.Client) { client.Timeout = timeout }
}

// WithConnections sets the per-host active and idle connection limits and a
// global idle pool twice that size. Nonpositive values retain the defaults.
func WithConnections(connections int) HTTPClientOption {
	return WithTransportOptions(func(transport *http.Transport) {
		if connections > 0 {
			transport.MaxIdleConns = connections * 2
			transport.MaxIdleConnsPerHost = connections
			transport.MaxConnsPerHost = connections
		}
	})
}

// WithTransportOptions customizes the client's private transport, for example
// its dialer, proxy, TLS settings, compression, or individual timeout values.
// Options run in order and must be applied before requests start.
func WithTransportOptions(configure func(*http.Transport)) HTTPClientOption {
	return func(client *http.Client) { configure(client.Transport.(*http.Transport)) }
}

var defaultHTTPClient = NewDugHttpClient()

// DefaultDugHttpClient returns the shared fallback for callers without an
// injected client. Do not modify it or its transport; use NewDugHttpClient when
// custom settings or ownership of the connection pool are needed.
func DefaultDugHttpClient() *http.Client { return defaultHTTPClient }
