// Package rustapi is the Storage API's outbound client for the Rust API's
// internal callback endpoints — the reverse direction of the trusted call
// the Rust API makes into the Storage API to mint sessions. Every such
// callback (upload results today, others later) funnels through the same
// Client.post so they all build, authenticate, and handle/log a failed
// request the same way, using nothing but net/http.
package rustapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// apiKeyHeader is the header the Rust API expects its internal callers to
// authenticate with.
const apiKeyHeader = "x-api-key"

// defaultTimeout bounds how long a single callback call may take.
const defaultTimeout = 10 * time.Second

// Client calls the Rust API's internal callback endpoints.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Config carries the dependencies used to construct a Client.
type Config struct {
	// BaseURL is the Rust API's own base URL, e.g.
	// "https://yfs-api.test.yukthi.net".
	BaseURL string
	// APIKey authenticates the Storage API to the Rust API, sent as the
	// x-api-key header.
	APIKey string
	// Timeout bounds a single callback call. Defaults to 10s.
	Timeout time.Duration
}

// New builds a Client from cfg.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// post sends body as JSON to c.baseURL+path, authenticated with the
// x-api-key header. It is the one place every Rust API callback goes
// through — new callback methods on Client should call this rather than
// building their own requests.
func (c *Client) post(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("rustapi: encoding request body: %w", err)
	}
	slog.Debug("rustapi request", "path", path, "body", string(payload))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("rustapi: building request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, c.apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rustapi: calling %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("rustapi: %s returned status %d: %s (sent body: %s)", path, resp.StatusCode, string(respBody), string(payload))
	}
	slog.DebugContext(ctx, "rustapi_callback", "url", c.baseURL+path, "status", resp.StatusCode, "latency", time.Since(start), "body", string(payload), "response", string(respBody))
	return nil
}
