package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

const secret = "s3cret+key/value"

func noSleep(context.Context, time.Duration) error { return nil }

func newClient(t *testing.T, url string, retries int) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: url, Key: secret, Retries: retries, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		code   apperr.Code
		exit   int
	}{
		{204, apperr.CodeNoDataYet, apperr.ExitNoData},
		{400, apperr.CodeInvalidArgument, apperr.ExitUsage},
		{401, apperr.CodeAuthInvalid, apperr.ExitAuth},
		{403, apperr.CodeForbidden, apperr.ExitForbidden},
		{404, apperr.CodeNotFound, apperr.ExitNotFound},
		{429, apperr.CodeRateLimited, apperr.ExitRateLimited},
		{502, apperr.CodeServiceUnavailable, apperr.ExitUnavailable},
		{503, apperr.CodeServiceUnavailable, apperr.ExitUnavailable},
		{418, apperr.CodeAPIError, apperr.ExitInternal},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			if tc.status != 204 {
				w.Write([]byte(`{"detail":"boom","status":1,"title":"t","type":"about:blank"}`))
			}
		}))
		_, err := newClient(t, srv.URL, 0).Do(context.Background(), Request{Method: "GET", Path: "/web"})
		srv.Close()
		e := apperr.From(err)
		if e.Code != tc.code || e.ExitCode != tc.exit {
			t.Errorf("status %d: got %s/%d, want %s/%d", tc.status, e.Code, e.ExitCode, tc.code, tc.exit)
		}
	}
}

func TestProblemDetailFields(t *testing.T) {
	cases := map[string]string{
		`{"detail":"live field"}`:            "live field",
		`{"description":"spec field"}`:       "spec field",
		`{"detail":["bad date","notadate"]}`: "bad date; notadate",
		`{"title":"only title"}`:             "only title",
		`not json`:                           "not json",
		``:                                   "empty response body",
	}
	for body, want := range cases {
		if got := problemDetail([]byte(body)); got != want {
			t.Errorf("problemDetail(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestKeyNeverLeaksFromTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused, which makes net/http embed the full URL
	_, err := newClient(t, url, 0).Do(context.Background(), Request{Method: "GET", Path: "/web"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if e := apperr.From(err); e.Code != apperr.CodeNetworkError {
		t.Errorf("code = %s", e.Code)
	}
	for _, leak := range []string{secret, "s3cret", "key="} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestKeyNeverLeaksFromTraceOrServerEcho(t *testing.T) {
	var trace strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"detail":"bad url ` + r.URL.String() + `"}`))
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Key: secret, Trace: &trace, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), Request{Method: "GET", Path: "/web"})
	all := trace.String() + err.Error()
	if strings.Contains(all, "s3cret") {
		t.Errorf("key leaked: %s", all)
	}
	if !strings.Contains(trace.String(), "key=***") {
		t.Errorf("trace should show the redacted parameter: %s", trace.String())
	}
}

func TestRedactVariants(t *testing.T) {
	for _, in := range []string{
		"x " + secret + " y",
		"https://h/p?key=s3cret%2Bkey%2Fvalue&url=a",
		"https://h/p?a=1&key=other-value",
	} {
		out := Redact(in, secret)
		if strings.Contains(out, "s3cret") || strings.Contains(out, "other-value") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL, 0).Do(context.Background(), Request{Method: "GET", Path: "/web"})
	if e := apperr.From(err); e.Code != apperr.CodeBadResponse {
		t.Errorf("code = %s", e.Code)
	}
	if hits.Load() != 0 {
		t.Error("redirect was followed and the key was sent to another host")
	}
}

func TestRetryOnTransientThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 3)
	if _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/web"}); err != nil {
		t.Fatal(err)
	}
	if st := c.Stats(); st.Requests != 3 || st.Retries != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	newClient(t, srv.URL, 5).Do(context.Background(), Request{Method: "GET", Path: "/web"})
	if calls.Load() != 1 {
		t.Errorf("401 retried: %d calls", calls.Load())
	}
}

func TestRetryAfterIsHonoredAndCapped(t *testing.T) {
	var slept []time.Duration
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
		} else {
			w.Header().Set("Retry-After", "3600")
		}
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, Key: secret, Retries: 5, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/web"})
	e := apperr.From(err)
	if e.Code != apperr.CodeRateLimited || e.RetryAfterSeconds != 3600 {
		t.Errorf("got %+v", e)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v", slept)
	}
}

func TestMissingKey(t *testing.T) {
	c, _ := New(Options{BaseURL: "http://127.0.0.1:1"})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/web"})
	if !apperr.IsCode(err, apperr.CodeAuthMissing) {
		t.Errorf("err = %v", err)
	}
}

func TestDeadlineMapsToTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newClient(t, srv.URL, 3).Do(ctx, Request{Method: "GET", Path: "/web"})
	if !apperr.IsCode(err, apperr.CodeTimeout) {
		t.Errorf("err = %v", err)
	}
}

func TestValidateBaseURL(t *testing.T) {
	good := []string{DefaultBaseURL, "http://127.0.0.1:8080/wm-api", "http://localhost:1", "https://[::1]:1"}
	bad := []string{
		"https://evil.example/wm-api", "http://reporter.seznam.cz/wm-api",
		"https://reporter.seznam.cz.evil.example/", "https://reporter.seznam.cz@evil.example/",
		"https://reporter.seznam.cz/?x=1", "not a url", "ftp://localhost/",
	}
	for _, u := range good {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	t.Setenv(AllowAnyBaseURLEnv, "1")
	if err := ValidateBaseURL("https://proxy.internal/"); err != nil {
		t.Errorf("override ignored: %v", err)
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"/web", "/web/document/reindex", "/__healthcheck"} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("%s rejected", p)
		}
	}
	for _, p := range []string{"web", "//evil.example/x", "/../x", "/a?key=1", "/a#b", "/a b", `/a\b`} {
		if err := ValidatePath(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestDecodeKeepsLargeIntegersExact(t *testing.T) {
	v, err := Decode([]byte(`{"n":1793581224204123456,"extra":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if n, ok := Int(m["n"]); !ok || n != 1793581224204123456 {
		t.Errorf("n = %v", m["n"])
	}
	if _, ok := m["extra"]; !ok {
		t.Error("unknown field dropped")
	}
}

func TestErrorsAsChainIntact(t *testing.T) {
	var e *apperr.Error
	if !errors.As(error(MissingKeyError()), &e) {
		t.Error("errors.As failed")
	}
}
