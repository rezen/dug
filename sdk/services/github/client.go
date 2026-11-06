// Package github is the shared client for the GitHub REST API: origin-pinned
// requests, rate-limit waits, Link pagination and the repository and release
// records that sources built on GitHub hosting need.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"regexp"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/retry"
	"github.com/rezen/dug/sdk/services"
)

// DefaultEndpoint is the public GitHub REST API. GitHub Enterprise Server uses
// https://HOST/api/v3.
const DefaultEndpoint = "https://api.github.com"

const apiVersion = "2022-11-28"

// MaxMetadataBytes caps one metadata response.
const MaxMetadataBytes = 32 << 20

// Client talks to one GitHub REST API. Token is sent as a bearer token to the
// API origin only, never in plaintext across a network, and is never written
// to references.
type Client struct {
	HTTP *http.Client
	// Endpoint is the REST API base; empty means DefaultEndpoint.
	Endpoint string
	Token    string
	// Attempts bounds retries per request; below 1 means 4.
	Attempts int
}

// Error is a non-retryable HTTP failure. Callers inspect Status to tell a
// missing branch or repository from other failures.
type Error struct {
	Status int
	URL    string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("GitHub %s: HTTP %d: %s", e.URL, e.Status, e.Detail)
	}
	return fmt.Sprintf("GitHub %s: HTTP %d", e.URL, e.Status)
}

// Status is the HTTP status of err, or 0 when it is not a GitHub HTTP error.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// A bearer token is only ever sent to this
// origin, and never in plaintext across a network.
// API parses the configured endpoint.
func (s *Client) API() (*url.URL, error) {
	raw := s.Endpoint
	if raw == "" {
		raw = DefaultEndpoint
	}
	if err := httpx.ValidateURL(raw); err != nil {
		return nil, fmt.Errorf("github api endpoint: %w", err)
	}
	u, _ := url.Parse(strings.TrimRight(raw, "/"))
	if s.Token != "" && u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("refusing to send GITHUB_TOKEN over plaintext to %s", u.Host)
		}
	}
	return u, nil
}

// URL joins escaped path segments under the API base.
func (s *Client) URL(segments ...string) string {
	base := s.Endpoint
	if base == "" {
		base = DefaultEndpoint
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	for _, segment := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(segment))
	}
	return b.String()
}

// RefPath escapes a branch or tag name one path segment at a time, so names
// containing slashes keep their shape in git/ref/tags/NAME routes.
func RefPath(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// Do performs one authenticated GET with retries. It waits out primary and
// secondary rate limits, retries transient failures, and marks client errors
// permanent. Redirects to download hosts drop the Authorization header
// because Go only forwards it to the same domain. Callers close the body;
// closing it also ends the per-attempt deadline when timeout is positive.
func (s *Client) Do(ctx context.Context, raw, accept string, timeout time.Duration) (*http.Response, error) {
	base, err := s.API()
	if err != nil {
		return nil, retry.Permanent(err)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, base.Path) {
		return nil, retry.Permanent(fmt.Errorf("github request %q left the API origin", raw))
	}
	client := s.HTTP
	if client == nil {
		client = services.DefaultDugHttpClient()
	}
	attempts := s.Attempts
	if attempts < 1 {
		attempts = 4
	}
	var response *http.Response
	err = retry.Do(ctx, attempts, func() error {
		attemptCtx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			cancel()
			return retry.Permanent(err)
		}
		req.Header.Set("User-Agent", "dug/0.1")
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		if s.Token != "" {
			req.Header.Set("Authorization", "Bearer "+s.Token)
		}
		response, err = client.Do(req)
		if err != nil {
			cancel()
			return err
		}
		if response.StatusCode == http.StatusOK {
			response.Body = cancelBody{response.Body, cancel}
			return nil
		}
		detail := errorDetail(response.Body)
		response.Body.Close()
		cancel()
		err = &Error{Status: response.StatusCode, URL: req.URL.Redacted(), Detail: detail}
		if delay, limited := RateLimitDelay(response.StatusCode, response.Header, time.Now()); limited {
			// Wait against the caller's context so a long primary-limit reset
			// is not cut short by the per-attempt deadline.
			if waitErr := retry.Wait(ctx, delay); waitErr != nil {
				return waitErr
			}
			return err
		}
		if response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500 {
			return err
		}
		return retry.Permanent(err)
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// RateLimitDelay reports how long GitHub asked us to wait. Primary limits
// answer 403 or 429 with X-RateLimit-Remaining: 0 and a reset time; secondary
// limits answer with Retry-After. Other 403s are real denials.
func RateLimitDelay(status int, header http.Header, now time.Time) (time.Duration, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return 0, false
	}
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil {
		return time.Duration(min(max(seconds, 0), 3600)) * time.Second, true
	}
	if header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return min(time.Hour, max(0, time.Unix(reset, 0).Sub(now))) + time.Second, true
		}
		return time.Minute, true
	}
	if status == http.StatusTooManyRequests {
		return time.Minute, true
	}
	return 0, false
}

func errorDetail(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, 4096))
	var message struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &message) == nil && message.Message != "" {
		return message.Message
	}
	return ""
}

// GetJSON fetches one metadata document and returns the response headers for
// pagination.
func (s *Client) GetJSON(ctx context.Context, raw string, out any) (http.Header, error) {
	response, err := s.Do(ctx, raw, "application/vnd.github+json", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMetadataBytes {
		return nil, retry.Permanent(fmt.Errorf("github metadata exceeds 32 MiB"))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, retry.Permanent(fmt.Errorf("github %s: %w", raw, err))
	}
	return response.Header, nil
}

// NextLink returns the rel="next" target of a Link header, or "".
func NextLink(header http.Header) string {
	for _, field := range header.Values("Link") {
		for _, part := range strings.Split(field, ",") {
			target, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.Contains(params, `rel="next"`) && !strings.Contains(params, "rel=next") {
				continue
			}
			target = strings.TrimSpace(target)
			if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
				return target[1 : len(target)-1]
			}
		}
	}
	return ""
}

// Pages walks a Link-paginated array listing until visit returns an error or
// the last page. Off-origin links are rejected by do.
func Pages[T any](ctx context.Context, s *Client, raw string, visit func([]T) error) error {
	seen := map[string]bool{raw: true}
	for raw != "" {
		var items []T
		header, err := s.GetJSON(ctx, raw, &items)
		if err != nil {
			return err
		}
		if err := visit(items); err != nil {
			return err
		}
		raw = NextLink(header)
		if raw != "" && seen[raw] {
			return fmt.Errorf("github pagination cycle at %s", raw)
		}
		seen[raw] = true
	}
	return nil
}

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// ValidOwner reports whether name is a well-formed user or organization login.
func ValidOwner(name string) bool { return ownerPattern.MatchString(name) }

// ParseRepo splits and validates owner/name.
func ParseRepo(full string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || !ownerPattern.MatchString(owner) || !repoPattern.MatchString(name) || strings.Trim(name, ".") == "" {
		return "", "", fmt.Errorf("invalid GitHub repository %q (expected owner/name)", full)
	}
	return owner, name, nil
}
