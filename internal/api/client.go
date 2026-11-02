// Package api is a thin, secret-safe client for the Seznam Webmaster REST API.
//
// The API authenticates with a `key` query parameter, so the key is part of
// every request URL. This package therefore never lets a URL escape: transport
// errors are stripped of it, trace output is redacted, redirects are not
// followed and the base URL is restricted to the official host or loopback.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

// DefaultBaseURL is the production endpoint. The swagger.json `basePath`
// (/wm-api/wm-api) is wrong: the working prefix is a single /wm-api.
const DefaultBaseURL = "https://reporter.seznam.cz/wm-api"

// AllowAnyBaseURLEnv opts out of the base URL host restriction.
const AllowAnyBaseURLEnv = "SZWM_INSECURE_ALLOW_ANY_BASE_URL"

const (
	maxResponseBytes = 64 << 20
	maxBackoff       = 30 * time.Second
	// maxRetryAfter is the longest server-requested pause szwm sits through.
	// Beyond it the caller gets rate_limited with retry_after_seconds.
	maxRetryAfter = 60 * time.Second
)

// Limiter delays a request until it fits into the published rate limits.
type Limiter interface {
	Wait(ctx context.Context) error
}

// Options configure a Client.
type Options struct {
	BaseURL    string
	Key        string
	Timeout    time.Duration
	Retries    int
	UserAgent  string
	Limiter    Limiter
	Trace      io.Writer
	HTTPClient *http.Client
	// Sleep is replaceable so tests do not wait for real backoff.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client talks to the Webmaster API.
type Client struct {
	opts     Options
	base     string
	http     *http.Client
	requests atomic.Int64
	retries  atomic.Int64
}

// Request describes one API call. The API key is added automatically unless
// NoAuth is set.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	NoAuth bool
}

// Response is a successful (2xx with body) API response.
type Response struct {
	Status int
	Body   []byte
}

// Stats counts HTTP activity for result metadata.
type Stats struct {
	Requests int64 `json:"requests"`
	Retries  int64 `json:"retries"`
}

// ValidateBaseURL allows only the official host or loopback. An agent tricked
// into passing another host would otherwise hand the API key to that host.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return apperr.Usage("base-url", "base URL %q is not an absolute URL", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return apperr.Usage("base-url", "base URL must not contain a query, fragment or credentials")
	}
	host := u.Hostname()
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	switch {
	case loopback && (u.Scheme == "http" || u.Scheme == "https"):
		return nil
	case u.Scheme == "https" && host == "reporter.seznam.cz":
		return nil
	case os.Getenv(AllowAnyBaseURLEnv) == "1":
		return nil
	}
	return apperr.Usage("base-url",
		"refusing to send the API key to %q: only https://reporter.seznam.cz and loopback are allowed", host).
		WithHint("Set " + AllowAnyBaseURLEnv + "=1 only for a host you control.")
}

// New validates options and returns a Client.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if err := ValidateBaseURL(opts.BaseURL); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	clone := *hc
	clone.Timeout = opts.Timeout
	// The key lives in the query string, so a redirect must never be followed.
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{opts: opts, base: strings.TrimRight(opts.BaseURL, "/"), http: &clone}, nil
}

// HasKey reports whether an API key is configured.
func (c *Client) HasKey() bool { return c.opts.Key != "" }

// BaseURL returns the effective base URL.
func (c *Client) BaseURL() string { return c.base }

// Stats returns cumulative counters.
func (c *Client) Stats() Stats {
	return Stats{Requests: c.requests.Load(), Retries: c.retries.Load()}
}

// Do performs a request with rate limiting and retries.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if !req.NoAuth && c.opts.Key == "" {
		return nil, MissingKeyError()
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		if c.opts.Limiter != nil {
			if err := c.opts.Limiter.Wait(ctx); err != nil {
				return nil, contextError(err)
			}
		}
		c.requests.Add(1)
		resp, err := c.once(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		e := apperr.From(err)
		if !shouldRetry(ctx, e) || attempt >= c.opts.Retries {
			return nil, err
		}
		wait := backoff(attempt)
		if e.RetryAfterSeconds > 0 {
			wait = time.Duration(e.RetryAfterSeconds) * time.Second
			if wait > maxRetryAfter {
				return nil, err
			}
		}
		c.retries.Add(1)
		c.trace("retry %d after %s: %s", attempt+1, wait.Round(time.Millisecond), e.Code)
		if err := c.opts.Sleep(ctx, wait); err != nil {
			return nil, lastErr
		}
	}
}

// MissingKeyError is the error for a call that needs a key but has none.
func MissingKeyError() *apperr.Error {
	return apperr.New(apperr.CodeAuthMissing, "no API key configured").
		WithHint("Set SZWM_API_KEY, pass --api-key-file, or run `szwm auth login --profile <site>`.")
}

func shouldRetry(ctx context.Context, e *apperr.Error) bool {
	if ctx.Err() != nil {
		return false
	}
	switch e.Code {
	case apperr.CodeRateLimited, apperr.CodeServiceUnavailable, apperr.CodeNetworkError,
		apperr.CodeTimeout, apperr.CodeBadResponse:
		return true
	}
	return false
}

func (c *Client) once(ctx context.Context, req Request) (*Response, error) {
	full, err := c.buildURL(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, full, nil)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, "cannot build request: %s", c.Redact(err.Error()))
	}
	hreq.Header.Set("Accept", "application/json, application/problem+json")
	if c.opts.UserAgent != "" {
		hreq.Header.Set("User-Agent", c.opts.UserAgent)
	}
	started := time.Now()
	c.trace("-> %s %s", req.Method, c.Redact(full))
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	defer hresp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	c.trace("<- %d %s (%d bytes)", hresp.StatusCode, time.Since(started).Round(time.Millisecond), len(body))
	return c.interpret(hresp, body)
}

func (c *Client) buildURL(req Request) (string, error) {
	if err := ValidatePath(req.Path); err != nil {
		return "", err
	}
	q := url.Values{}
	for k, vs := range req.Query {
		q[k] = vs
	}
	if !req.NoAuth {
		q.Set("key", c.opts.Key)
	}
	u := c.base + req.Path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u, nil
}

// ValidatePath keeps a caller-supplied path on the API host.
func ValidatePath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"), strings.HasPrefix(p, "//"):
		return apperr.Usage("path", "API path must start with a single /")
	case strings.Contains(p, ".."), strings.ContainsAny(p, "?#\\ \t\r\n"):
		return apperr.Usage("path", "API path must not contain '..', query strings or whitespace; use --param for parameters")
	}
	return nil
}

func (c *Client) interpret(resp *http.Response, body []byte) (*Response, error) {
	st := resp.StatusCode
	switch {
	case st == http.StatusNoContent:
		return nil, apperr.New(apperr.CodeNoDataYet, "Seznam has no data for this request yet").
			WithHint("Check again after the next release; see `szwm database-info`.")
	case st >= 200 && st < 300:
		return &Response{Status: st, Body: body}, nil
	}
	detail := c.Redact(problemDetail(body))
	var e *apperr.Error
	switch {
	case st == http.StatusBadRequest:
		e = apperr.New(apperr.CodeInvalidArgument, "Seznam rejected the request: %s", detail)
	case st == http.StatusUnauthorized:
		e = apperr.New(apperr.CodeAuthInvalid, "Seznam rejected the API key: %s", detail).
			WithHint("Get the current key under Webmaster > API > Klíč k API, then run `szwm auth login`.")
	case st == http.StatusForbidden:
		e = apperr.New(apperr.CodeForbidden, "access denied: %s", detail).
			WithHint("The URL must belong to this key's site; reindexing needs admin or write rights.")
	case st == http.StatusNotFound:
		e = apperr.New(apperr.CodeNotFound, "not found: %s", detail)
	case st == http.StatusTooManyRequests:
		e = apperr.New(apperr.CodeRateLimited, "rate limit exceeded: %s", detail)
		e.RetryAfterSeconds = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	case st == http.StatusBadGateway, st == http.StatusServiceUnavailable, st == http.StatusGatewayTimeout:
		e = apperr.New(apperr.CodeServiceUnavailable, "Seznam service unavailable: %s", detail)
		e.RetryAfterSeconds = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	case st >= 300 && st < 400:
		e = apperr.New(apperr.CodeBadResponse, "unexpected redirect (HTTP %d); szwm does not follow redirects", st)
	default:
		e = apperr.New(apperr.CodeAPIError, "unexpected HTTP %d: %s", st, detail)
	}
	e.HTTPStatus = st
	return nil, e
}

// problemDetail extracts the message from an RFC 7807 body. The live API uses
// `detail`, while the published ProblemResult schema says `description`.
func problemDetail(body []byte) string {
	var p struct {
		Detail      string `json:"detail"`
		Description string `json:"description"`
		Title       string `json:"title"`
	}
	if json.Unmarshal(body, &p) == nil {
		for _, s := range []string{p.Detail, p.Description, p.Title} {
			if s != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "empty response body"
	}
	return s
}

func (c *Client) transportError(ctx context.Context, err error) error {
	// url.Error embeds the full URL, including the key: unwrap it first.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if ctx.Err() != nil {
		return contextError(ctx.Err())
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return apperr.New(apperr.CodeTimeout, "request timed out after %s", c.opts.Timeout).
			WithHint("Raise --timeout or retry later.")
	}
	return apperr.New(apperr.CodeNetworkError, "cannot reach Seznam: %s", c.Redact(err.Error()))
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.New(apperr.CodeTimeout, "command deadline exceeded").WithHint("Raise --deadline.")
	}
	if errors.Is(err, context.Canceled) {
		return apperr.New(apperr.CodeTimeout, "command was cancelled")
	}
	return apperr.From(err)
}

var keyParam = regexp.MustCompile(`([?&]key=)[^&\s"']+`)

// Redact removes the API key from a string.
func (c *Client) Redact(s string) string { return Redact(s, c.opts.Key) }

// Redact removes key, raw or URL-encoded, and any key= parameter from s.
func Redact(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "***")
		if esc := url.QueryEscape(key); esc != key {
			s = strings.ReplaceAll(s, esc, "***")
		}
	}
	return keyParam.ReplaceAllString(s, "${1}***")
}

func (c *Client) trace(format string, args ...any) {
	if c.opts.Trace != nil {
		fmt.Fprintln(c.opts.Trace, "szwm: "+c.Redact(fmt.Sprintf(format, args...)))
	}
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt, 10)
	if d > maxBackoff {
		d = maxBackoff
	}
	return d/2 + rand.N(d/2)
}

func retryAfter(v string, now time.Time) int {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return n
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := int(t.Sub(now).Seconds()); d > 0 {
			return d
		}
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Decode parses a JSON body keeping integers exact and unknown fields intact.
// szwm passes unknown fields through because the API already returns more than
// its schema documents.
func Decode(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apperr.New(apperr.CodeBadResponse, "Seznam returned invalid JSON: %s", err.Error())
	}
	return v, nil
}

// Int reads an integer from a decoded JSON value.
func Int(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}
