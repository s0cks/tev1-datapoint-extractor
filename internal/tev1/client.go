package tev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client talks to a local Ollama daemon.
type Client struct {
	BaseURL string        // e.g. http://localhost:11434
	HTTP    *http.Client  // optional; per-attempt timeouts use Timeout, not HTTP.Timeout
	Timeout time.Duration // per-attempt timeout; default 60s
	Retries int           // retries after the first attempt; default 3
	Backoff time.Duration // base backoff; doubles per retry; default 500ms
	Logger  *slog.Logger  // optional

	// sleep is replaceable in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) doSleep(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// Decide sends one request and returns the validated response. Transient
// failures (network errors, timeouts, HTTP 429/5xx) are retried with
// exponential backoff and jitter; malformed responses and other 4xx errors
// are not.
func (c *Client) Decide(ctx context.Context, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	if len(body) > MaxRequestBytes {
		return nil, fmt.Errorf("request is %d bytes, over the %d byte limit", len(body), MaxRequestBytes)
	}
	retries := c.Retries
	if retries < 0 {
		retries = 0
	}
	base := c.Backoff
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			d := base << (attempt - 1)
			d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
			c.logger().Warn("retrying /v1/systemone", "attempt", attempt+1, "of", retries+1, "backoff", d, "err", lastErr)
			if err := c.doSleep(ctx, d); err != nil {
				return nil, err
			}
		}
		resp, retry, err := c.once(ctx, body, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", retries+1, lastErr)
}

// once performs a single attempt. The bool reports whether a retry is sensible.
func (c *Client) once(ctx context.Context, body []byte, req Request) (*Response, bool, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, c.url("/v1/systemone"), bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hresp, err := c.httpc().Do(hreq)
	if err != nil {
		return nil, true, fmt.Errorf("request failed: %w", err)
	}
	defer hresp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, 8<<20))
	if err != nil {
		return nil, true, fmt.Errorf("reading response: %w", err)
	}
	if hresp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: hresp.StatusCode, Body: string(data)}
		retry := hresp.StatusCode == http.StatusTooManyRequests || hresp.StatusCode >= 500
		return nil, retry, apiErr
	}
	resp, err := ParseResponse(data, req)
	if err != nil {
		return nil, false, err
	}
	return resp, false, nil
}

// --- preflight -------------------------------------------------------------

// MinOllamaVersion is the first Ollama release with /v1/systemone.
const MinOllamaVersion = "0.35.0"

var reVersion = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// VersionAtLeast reports whether v ("0.35.1", "v0.36.0-rc1", ...) >= min.
func VersionAtLeast(v, min string) (bool, error) {
	a, err := parseVersion(v)
	if err != nil {
		return false, err
	}
	b, err := parseVersion(min)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return true, nil
}

func parseVersion(s string) ([3]int, error) {
	m := reVersion.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, fmt.Errorf("cannot parse version %q", s)
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	return json.Unmarshal(data, v)
}

// PreflightError carries an actionable message for the CLI.
type PreflightError struct{ Msg string }

func (e *PreflightError) Error() string { return e.Msg }

// Preflight verifies that Ollama is reachable, new enough, and has model
// pulled. It returns the Ollama version.
func (c *Client) Preflight(ctx context.Context, model string) (string, error) {
	var ver struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, "/api/version", &ver); err != nil {
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") {
			return "", &PreflightError{fmt.Sprintf("cannot reach Ollama at %s (%v). Start it with `ollama serve`, set --ollama-url / OLLAMA_HOST, or use --no-model", c.BaseURL, err)}
		}
		return "", &PreflightError{fmt.Sprintf("asking Ollama for its version at %s failed: %v", c.BaseURL, err)}
	}
	ok, err := VersionAtLeast(ver.Version, MinOllamaVersion)
	if err != nil {
		return ver.Version, &PreflightError{fmt.Sprintf("unrecognised Ollama version %q: %v", ver.Version, err)}
	}
	if !ok {
		return ver.Version, &PreflightError{fmt.Sprintf("Ollama %s is too old: /v1/systemone needs %s or newer. Upgrade Ollama", ver.Version, MinOllamaVersion)}
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := c.getJSON(ctx, "/api/tags", &tags); err != nil {
		return ver.Version, &PreflightError{fmt.Sprintf("listing local models failed: %v", err)}
	}
	want := normalizeModel(model)
	for _, m := range tags.Models {
		if normalizeModel(m.Name) == want || normalizeModel(m.Model) == want {
			return ver.Version, nil
		}
	}
	return ver.Version, &PreflightError{fmt.Sprintf("model %q is not pulled locally. Run: ollama pull %s", model, model)}
}

func normalizeModel(n string) string {
	if n != "" && !strings.Contains(n, ":") {
		return n + ":latest"
	}
	return n
}
