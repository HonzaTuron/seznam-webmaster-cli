// Package wm is the application layer shared by the CLI and the MCP server, so
// both expose identical semantics, safety checks and output shapes.
package wm

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/ratelimit"
)

// DefaultURLLimit caps URLs per category unless the caller asks otherwise. The
// API returns up to 1,000 per category, too much for an agent's context window.
const DefaultURLLimit = 100

const maxURLLength = 2048

// Service executes Webmaster operations.
type Service struct {
	API      *api.Client
	Ledger   *ratelimit.Ledger
	ReadOnly bool
	Now      func() time.Time
}

// Result is the outcome of an operation.
type Result struct {
	Data      any
	Raw       []byte
	Hints     []string
	Warnings  []string
	Untrusted []string
	Truncated bool
	// Err marks a failure that still produced data, such as a partial batch.
	Err *apperr.Error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) getJSON(ctx context.Context, req api.Request) (any, []byte, error) {
	req.Method = "GET"
	resp, err := s.API.Do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	v, err := api.Decode(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return v, resp.Body, nil
}

// DatabaseInfo returns the release time of the data behind the API.
func (s *Service) DatabaseInfo(ctx context.Context) (Result, error) {
	v, raw, err := s.getJSON(ctx, api.Request{Path: "/database-info", NoAuth: true})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: v, Raw: raw}, nil
}

// ViewOptions shape URL-bearing responses.
type ViewOptions struct {
	CountsOnly bool
	Limit      int // URLs per category; 0 means all
	Categories []string
	HistoryDay int // keep only the newest N history rows; 0 means all
}

const sampleNote = "Each category's urls are a random sample of at most 1,000 pages, not a full list: use count for totals."

// Overview wraps GET /web.
func (s *Service) Overview(ctx context.Context, o ViewOptions) (Result, error) {
	v, raw, err := s.getJSON(ctx, api.Request{Path: "/web"})
	if err != nil {
		return Result{}, err
	}
	res := Result{Data: v, Raw: raw}
	m, ok := v.(map[string]any)
	if !ok {
		return res, nil
	}
	if docs, ok := m["documents"].(map[string]any); ok {
		shapeDocuments(docs, o, &res)
	}
	if h, ok := m["history"].([]any); ok {
		sortHistory(h)
		if o.HistoryDay > 0 && len(h) > o.HistoryDay {
			m["history"] = h[len(h)-o.HistoryDay:]
			res.Truncated = true
		}
	}
	return res, nil
}

// Documents wraps GET /web/documents.
func (s *Service) Documents(ctx context.Context, o ViewOptions) (Result, error) {
	v, raw, err := s.getJSON(ctx, api.Request{Path: "/web/documents"})
	if err != nil {
		return Result{}, err
	}
	res := Result{Data: v, Raw: raw}
	if m, ok := v.(map[string]any); ok {
		shapeDocuments(m, o, &res)
	}
	return res, nil
}

// shapeDocuments works on generic maps because the API returns categories that
// its schema omits (doc_count).
func shapeDocuments(docs map[string]any, o ViewOptions, res *Result) {
	if len(o.Categories) > 0 {
		keep := map[string]bool{}
		for _, c := range o.Categories {
			keep[c] = true
		}
		for k := range docs {
			if !keep[k] {
				delete(docs, k)
			}
		}
		for _, c := range o.Categories {
			if _, ok := docs[c]; !ok {
				res.Warnings = append(res.Warnings, "category "+c+" is not in the response")
			}
		}
	}
	hasURLs := false
	for _, v := range docs {
		cat, ok := v.(map[string]any)
		if !ok {
			continue
		}
		urls, ok := cat["urls"].([]any)
		if !ok {
			continue
		}
		switch {
		case o.CountsOnly:
			delete(cat, "urls")
		default:
			hasURLs = hasURLs || len(urls) > 0
			if o.Limit > 0 && len(urls) > o.Limit {
				cat["urls"] = urls[:o.Limit]
				cat["urls_total_in_sample"] = len(urls)
				res.Truncated = true
			}
		}
	}
	if hasURLs {
		res.Hints = append(res.Hints, sampleNote)
	}
	if res.Truncated {
		res.Hints = append(res.Hints, "URL lists were shortened: pass --limit 0 for the whole sample.")
	}
}

// History wraps GET /web/documents-history. Dates are YYYY-MM-DD or empty.
func (s *Service) History(ctx context.Context, from, to string) (Result, error) {
	q := url.Values{}
	if from != "" {
		if err := validateDay("from", from); err != nil {
			return Result{}, err
		}
		q.Set("date_from", from)
	}
	if to != "" {
		if err := validateDay("to", to); err != nil {
			return Result{}, err
		}
		q.Set("date_to", to)
	}
	if from != "" && to != "" && from > to {
		return Result{}, apperr.Usage("from", "--from %s is after --to %s", from, to)
	}
	v, raw, err := s.getJSON(ctx, api.Request{Path: "/web/documents-history", Query: q})
	if err != nil {
		return Result{}, err
	}
	if h, ok := v.([]any); ok {
		sortHistory(h)
	}
	return Result{Data: v, Raw: raw}, nil
}

func validateDay(field, v string) error {
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return apperr.Usage(field, "%q is not a YYYY-MM-DD date", v)
	}
	return nil
}

func sortHistory(h []any) {
	sort.SliceStable(h, func(i, j int) bool { return historyDate(h[i]) < historyDate(h[j]) })
}

func historyDate(v any) string {
	if m, ok := v.(map[string]any); ok {
		if d, ok := m["date"].(string); ok {
			return d
		}
	}
	return ""
}

// SiteFromWebserver converts the API's reversed form, such as
// com.example.!443, into a host name such as example.com.
func SiteFromWebserver(v string) string {
	parts := strings.Split(v, ".")
	if n := len(parts); strings.HasPrefix(parts[n-1], "!") {
		parts = parts[:n-1]
	}
	slices.Reverse(parts)
	return strings.Join(parts, ".")
}

// Site returns the host name of the site the key belongs to. It returns "" when
// the key is accepted but the site has no data yet.
func (s *Service) Site(ctx context.Context) (string, error) {
	return SiteOf(ctx, s.API)
}

// SiteOf is Site for a bare client, used to check a key before it is saved.
func SiteOf(ctx context.Context, c *api.Client) (string, error) {
	resp, err := c.Do(ctx, api.Request{Method: "GET", Path: "/web"})
	if err != nil {
		e := apperr.From(err)
		switch e.Code {
		case apperr.CodeNoDataYet, apperr.CodeNotFound:
			return "", nil
		}
		return "", err
	}
	v, err := api.Decode(resp.Body)
	if err != nil {
		return "", err
	}
	if m, ok := v.(map[string]any); ok {
		if w, ok := m["webserver"].(string); ok {
			return SiteFromWebserver(w), nil
		}
	}
	return "", nil
}

var lastRe = regexp.MustCompile(`^(\d{1,4})([dw])$`)

// ParseLast converts a relative window such as 30d or 8w into a start date.
func ParseLast(v string, now time.Time) (string, error) {
	m := lastRe.FindStringSubmatch(v)
	if m == nil {
		return "", apperr.Usage("last", "%q is not a window such as 30d or 8w", v)
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	if m[2] == "w" {
		n *= 7
	}
	return now.AddDate(0, 0, -n).Format("2006-01-02"), nil
}

// ValidateURL checks that raw is an absolute http(s) page URL.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", apperr.Usage("url", "URL is empty")
	}
	if len(raw) > maxURLLength {
		return "", apperr.Usage("url", "URL is longer than %d characters", maxURLLength)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return "", apperr.Usage("url", "URL contains whitespace or control characters: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", apperr.Usage("url", "%q is not an absolute http(s) URL", raw)
	}
	if u.User != nil {
		return "", apperr.Usage("url", "URL must not contain credentials")
	}
	return raw, nil
}

// CleanURLs validates everything up front and removes duplicates. Failing
// before the first call keeps a typo from burning half of a batch's quota.
func CleanURLs(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	var bad []string
	var first error
	for _, r := range raw {
		u, err := ValidateURL(r)
		if err != nil {
			if first == nil {
				first = err
			}
			bad = append(bad, r)
			continue
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	switch {
	case len(bad) == 1:
		return nil, first
	case len(bad) > 1:
		shown := bad[:min(len(bad), 5)]
		return nil, apperr.Usage("url", "%d invalid URLs, for example: %s", len(bad), strings.Join(shown, ", "))
	case len(out) == 0:
		return nil, apperr.Usage("url", "no URLs given").
			WithHint("Pass URLs as arguments, or use --stdin, --file or --sitemap.")
	}
	return out, nil
}
