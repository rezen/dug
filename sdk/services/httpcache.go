package services

import (
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/bartventer/httpcache"
	_ "github.com/bartventer/httpcache/store/fscache" // registers the fscache:// DSN
)

// DefaultCacheTTL is how long a cached GET stays fresh without revalidation.
const DefaultCacheTTL = 2 * time.Hour

// Environment variables that override the cache settings; command-line flags
// take precedence over them.
const (
	EnvCacheTTL = "DUG_HTTP_CACHE_TTL" // Go duration such as 10m, or 0 to disable
	EnvCacheDir = "DUG_HTTP_CACHE_DIR" // default: the user cache directory
)

// CacheTTLFromEnv returns DUG_HTTP_CACHE_TTL, or DefaultCacheTTL when it is
// unset or not a valid duration. Zero (or negative) disables caching.
func CacheTTLFromEnv() time.Duration {
	if ttl, err := time.ParseDuration(os.Getenv(EnvCacheTTL)); err == nil {
		return ttl
	}
	return DefaultCacheTTL
}

// CacheDirFromEnv returns DUG_HTTP_CACHE_DIR; empty selects the user cache dir.
func CacheDirFromEnv() string { return os.Getenv(EnvCacheDir) }

// WithCache stores GET responses on disk and serves them without contacting
// the server for ttl. ttl overrides the upstream's freshness headers, because
// registry APIs rarely send useful ones; ETag and Last-Modified are kept, so
// an expired entry is revalidated rather than refetched. A ttl of zero or less
// disables caching, as does a cache directory that cannot be opened. dir
// empty selects the user cache directory. Apply it after the transport options,
// since it wraps the transport.
//
// Use it only for metadata clients: it also caches every body it stores.
func WithCache(dir string, ttl time.Duration) HTTPClientOption {
	return func(client *http.Client) {
		if ttl <= 0 {
			return
		}
		dsn := url.URL{Scheme: "fscache", Path: dir, RawQuery: "appname=dug-http"}
		defer func() { _ = recover() }() // NewTransport panics when the store cannot open
		upstream := client.Transport
		client.Transport = cached{
			RoundTripper: httpcache.NewTransport(dsn.String(),
				httpcache.WithUpstream(freshFor{upstream: upstream, ttl: ttl})),
			upstream: upstream,
		}
	}
}

// cached keeps http.Client.CloseIdleConnections reaching the real transport.
type cached struct {
	http.RoundTripper
	upstream http.RoundTripper
}

func (c cached) CloseIdleConnections() {
	if closer, ok := c.upstream.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// freshFor rewrites cacheable responses so the cache keeps them for ttl.
type freshFor struct {
	upstream http.RoundTripper
	ttl      time.Duration
}

func (f freshFor) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.upstream.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusOK && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		resp.Header.Set("Cache-Control", "max-age="+strconv.Itoa(int(f.ttl/time.Second)))
		resp.Header.Del("Expires")
		resp.Header.Del("Pragma")
	}
	return resp, err
}
