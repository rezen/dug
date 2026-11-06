package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransientAndPermanentResponses(t *testing.T) {
	for _, code := range []int{429, 503, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.WriteHeader(code)
					return
				}
				io.WriteString(w, "ok")
			}))
			defer server.Close()
			response, err := Get(context.Background(), server.Client(), server.URL, 2)
			if code == 404 {
				var status *StatusError
				if !errors.As(err, &status) || status.Code != 404 || requests.Load() != 1 {
					t.Fatalf("err=%v requests=%d", err, requests.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if requests.Load() != 2 {
				t.Fatal(requests.Load())
			}
		})
	}
}
func TestRetryAfterCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := Get(ctx, server.Client(), server.URL, 3); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestValidateURLHost(t *testing.T) {
	hosts := []string{"downloads.example.org", "127.0.0.1:8080"}
	for raw, ok := range map[string]bool{
		"https://downloads.example.org/a.zip":         true,
		"https://DOWNLOADS.example.org:443/a.zip":     true,
		"http://127.0.0.1:8080/a.zip":                 true,
		"http://127.0.0.1:9090/a.zip":                 false,
		"https://evil.example.org/a.zip":              false,
		"https://downloads.example.org.evil/a.zip":    false,
		"http://169.254.169.254/latest/meta-data":     false,
		"ftp://downloads.example.org/a":               false,
		"https://user:pw@downloads.example.org/a.zip": false,
	} {
		if err := ValidateURLHost(raw, hosts); (err == nil) != ok {
			t.Errorf("%s: err=%v, want allowed=%v", raw, err, ok)
		}
	}
}
