package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// DefaultEndpoint is TypeSafe's System One endpoint.
	DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	// DefaultModel always resolves to the latest Jev release.
	DefaultModel = "jev-latest"

	defaultMaxRetries = 3
	defaultBaseDelay  = 250 * time.Millisecond
	defaultMaxDelay   = 5 * time.Second
	// statusOverloaded is TypeSafe's non-standard "529 Overloaded" status.
	statusOverloaded = 529
	maxErrorBody     = 4 << 10
)

// APIError is returned when the API answers with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("jev: unexpected status %d: %s", e.StatusCode, e.Body)
}

// Temporary reports whether retrying the same request may succeed.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode == statusOverloaded ||
		e.StatusCode >= http.StatusInternalServerError
}

// Client calls the Jev System One API. It is safe for concurrent use.
type Client struct {
	apiKey     string
	endpoint   string
	model      string
	httpClient *http.Client
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
}

// Option customises a Client.
type Option func(*Client)

// WithEndpoint overrides the API endpoint, for example to route through
// OpenRouter's Decisions API or a test server.
//
// Parameters:
//   - endpoint (string): absolute URL that accepts a Request via POST.
func WithEndpoint(endpoint string) Option {
	return func(c *Client) { c.endpoint = endpoint }
}

// WithModel sets the model used when a Request leaves Model empty.
//
// Parameters:
//   - model (string): model alias or pinned version, e.g. "jev-1.13.0".
func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

// WithHTTPClient replaces the underlying HTTP client.
//
// Parameters:
//   - hc (*http.Client): client used for every request.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithRetry configures retries for 429, 529 and 5xx responses and transport
// errors. Delays grow exponentially from base up to max, with full jitter.
//
// Parameters:
//   - maxRetries (int): retries after the first attempt; 0 disables them.
//   - base (time.Duration): delay before the first retry.
//   - maxDelay (time.Duration): upper bound for any single delay.
func WithRetry(maxRetries int, base, maxDelay time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = maxRetries
		c.baseDelay = base
		c.maxDelay = maxDelay
	}
}

// NewClient returns a Client authenticated with apiKey.
//
// Parameters:
//   - apiKey (string): TypeSafe API key sent as a bearer token.
//   - opts (...Option): optional overrides applied in order.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		endpoint:   DefaultEndpoint,
		model:      DefaultModel,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		maxRetries: defaultMaxRetries,
		baseDelay:  defaultBaseDelay,
		maxDelay:   defaultMaxDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Evaluate sends req to Jev and returns its answers, retrying transient
// failures with exponential backoff.
//
// Parameters:
//   - ctx (context.Context): bounds the whole call, retries included.
//   - req (Request): state and questions to evaluate.
func (c *Client) Evaluate(ctx context.Context, req Request) (*Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.do(ctx, body)
		if err == nil {
			return resp, nil
		}
		if attempt >= c.maxRetries || !retryable(err) {
			return nil, err
		}

		delay := retryAfter
		if delay <= 0 {
			delay = c.backoff(attempt)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// do performs a single HTTP round trip.
//
// Parameters:
//   - ctx (context.Context): request context.
//   - body ([]byte): encoded Request.
//
// It returns the decoded response, the server-requested Retry-After delay (or
// zero), and any error.
func (c *Client) do(ctx context.Context, body []byte) (*Response, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("jev: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("jev: send request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBody))
		apiErr := &APIError{StatusCode: httpResp.StatusCode, Body: string(bytes.TrimSpace(msg))}
		return nil, parseRetryAfter(httpResp.Header.Get("Retry-After")), apiErr
	}

	var out Response
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("jev: decode response: %w", err)
	}
	return &out, 0, nil
}

// backoff returns a jittered exponential delay for the given attempt.
//
// Parameters:
//   - attempt (int): zero-based index of the attempt that just failed.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << attempt
	if d <= 0 || d > c.maxDelay {
		d = c.maxDelay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d))) + 1 //nolint:gosec // Jitter needs no CSPRNG.
}

// retryable reports whether err is worth retrying. Context cancellation and
// non-transient API errors are not.
//
// Parameters:
//   - err (error): error returned by a single attempt.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Temporary()
	}
	// Transport errors (connection reset, DNS hiccups, …) are worth a retry;
	// a malformed response body is not.
	_, isTransport := errors.AsType[*url.Error](err)
	return isTransport
}

// parseRetryAfter reads a Retry-After header expressed in seconds.
//
// Parameters:
//   - v (string): raw header value; HTTP-date values are ignored.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
