package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rezen/dug/sdk/retry"
)

// SignatureHeader carries "sha256=" plus the hex HMAC-SHA256 of the request
// body, present only when a secret is configured.
const SignatureHeader = "X-Dug-Signature"

// Webhook POSTs each batch as {"events": [...]} JSON. Network errors, 429 and
// 5xx are retried; other statuses fail the batch at once. References are sent
// without their Raw upstream record.
type Webhook struct {
	URL      string
	Secret   string
	Client   *http.Client
	Attempts int           // default 3
	Timeout  time.Duration // per attempt, default 30s
}

// NewWebhook validates the URL, which must be http or https.
func NewWebhook(endpoint, secret string, client *http.Client) (*Webhook, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("webhook URL must be an absolute http(s) URL")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Webhook{URL: endpoint, Secret: secret, Client: client}, nil
}

func (w *Webhook) Handle(ctx context.Context, batch []Event) error {
	slim := make([]Event, len(batch))
	copy(slim, batch)
	for i := range slim {
		slim[i].Reference.Raw = nil
	}
	body, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{slim})
	if err != nil {
		return err
	}
	attempts, timeout := w.Attempts, w.Timeout
	if attempts == 0 {
		attempts = 3
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return retry.Do(ctx, attempts, func() error {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(attempt, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			return retry.Permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if w.Secret != "" {
			mac := hmac.New(sha256.New, []byte(w.Secret))
			mac.Write(body)
			req.Header.Set(SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := w.Client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		switch {
		case resp.StatusCode/100 == 2:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return fmt.Errorf("webhook returned %s", resp.Status)
		default:
			return retry.Permanent(fmt.Errorf("webhook returned %s", resp.Status))
		}
	})
}
