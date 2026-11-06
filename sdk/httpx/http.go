package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services"
)

// Transport reuses connections; body deadlines belong to each transfer context.
// Deprecated: use services.NewDugHttpClient with services.WithConnections.
func Transport(connections int) *http.Transport {
	return services.NewDugHttpClient(services.WithConnections(connections)).Transport.(*http.Transport)
}

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return fmt.Errorf("expected an http(s) URL without embedded credentials")
	}
	return nil
}

// ValidateURLHost is ValidateURL plus a host allowlist: the URL's host must
// equal one entry, with or without its port, case-insensitively. Plugins use it
// so persisted download URLs cannot steer them to arbitrary servers.
func ValidateURLHost(raw string, hosts []string) error {
	if err := ValidateURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	for _, host := range hosts {
		if strings.EqualFold(u.Host, host) || strings.EqualFold(u.Hostname(), host) {
			return nil
		}
	}
	return fmt.Errorf("URL host %q is not an allowed download host", u.Host)
}

// StatusError is the failure Get reports for a non-200 response. Callers use
// errors.As to tell a missing resource (404) from an outage.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Code) }

func Get(ctx context.Context, client *http.Client, raw string, attempts int) (*http.Response, error) {
	if client == nil {
		client = services.DefaultDugHttpClient()
	}
	if attempts <= 0 {
		attempts = 4
	}
	if err := ValidateURL(raw); err != nil {
		return nil, retry.Permanent(err)
	}
	var response *http.Response
	throttled := 0
	err := retry.Do(ctx, attempts, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return retry.Permanent(err)
		}
		req.Header.Set("User-Agent", "dug/0.1")
		response, err = client.Do(req)
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusOK {
			return nil
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		err = &StatusError{URL: req.URL.Redacted(), Code: response.StatusCode}
		if response.StatusCode != 429 && response.StatusCode != 408 && response.StatusCode < 500 {
			return retry.Permanent(err)
		}
		delay := retryAfter(response.Header.Get("Retry-After"))
		if delay == 0 && response.StatusCode == http.StatusTooManyRequests {
			// The generic backoff is far shorter than a rate-limit window.
			delay = min(60*time.Second, 5*time.Second<<min(throttled, 4))
			throttled++
		}
		if delay > 0 {
			if waitErr := retry.Wait(ctx, delay); waitErr != nil {
				return waitErr
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(0, time.Duration(min(seconds, 3600))*time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(time.Hour, max(0, time.Until(date)))
	}
	return 0
}
