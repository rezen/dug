package services

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientReusesConnections(t *testing.T) {
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("archive client requested automatic compression")
		}
		io.WriteString(w, "archive")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	client := NewDugHttpClient(WithConnections(1))
	defer client.CloseIdleConnections()
	for range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("opened %d connections for sequential requests", connections.Load())
	}
}

func TestHTTPClientTimeouts(t *testing.T) {
	for name, option := range map[string]HTTPClientOption{
		"whole request": WithTimeout(50 * time.Millisecond),
		"response headers": WithTransportOptions(func(transport *http.Transport) {
			transport.ResponseHeaderTimeout = 50 * time.Millisecond
		}),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			defer server.Close()
			client := NewDugHttpClient(option)
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil {
				t.Fatalf("expected configured client timeout before context deadline; got %v (context: %v)", err, ctx.Err())
			}
		})
	}
}

func TestHTTPClientStreamingCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewDugHttpClient()
	defer client.CloseIdleConnections()
	if client.Timeout != 0 {
		t.Fatal("default client must allow context-controlled streams")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream ignored cancellation: %v", err)
	}
}

func TestHTTPClientConfigurationIsIsolated(t *testing.T) {
	client := NewDugHttpClient(WithConnections(7), WithTransportOptions(func(transport *http.Transport) {
		transport.DisableCompression = false
	}))
	transport := client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != 7 || transport.MaxIdleConnsPerHost != 7 || transport.MaxIdleConns != 14 || transport.DisableCompression {
		t.Fatal("custom settings were not applied")
	}
	for _, other := range []*http.Client{NewDugHttpClient(), DefaultDugHttpClient()} {
		defaults := other.Transport.(*http.Transport)
		if defaults == transport || defaults.MaxConnsPerHost != 128 || !defaults.DisableCompression {
			t.Fatal("custom settings changed another client's transport")
		}
	}
	if DefaultDugHttpClient() != DefaultDugHttpClient() {
		t.Fatal("fallback requests must share a connection pool")
	}
}

func TestWithCacheServesFreshResponsesWithoutUpstream(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "body")
	}))
	defer server.Close()
	get := func(client *http.Client) {
		t.Helper()
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	dir := t.TempDir()
	client := NewDugHttpClient(WithCache(dir, time.Minute))
	get(client)
	get(client)
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	uncached := NewDugHttpClient(WithCache(dir, 0))
	get(uncached)
	if hits.Load() != 2 {
		t.Fatalf("ttl 0 must bypass the cache; hits = %d", hits.Load())
	}
}

func TestCacheTTLFromEnv(t *testing.T) {
	t.Setenv(EnvCacheTTL, "")
	if got := CacheTTLFromEnv(); got != DefaultCacheTTL {
		t.Fatalf("default = %v", got)
	}
	t.Setenv(EnvCacheTTL, "5m")
	if got := CacheTTLFromEnv(); got != 5*time.Minute {
		t.Fatalf("override = %v", got)
	}
	t.Setenv(EnvCacheTTL, "0")
	if got := CacheTTLFromEnv(); got != 0 {
		t.Fatalf("disable = %v", got)
	}
}
