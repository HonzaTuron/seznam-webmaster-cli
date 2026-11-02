package wm

import (
	"context"
	"net/url"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

// Page states reported by document lookups.
const (
	StateIndexed      = "indexed"
	StateNotIndexed   = "not_indexed"
	StateError        = "error"
	StateRedirect     = "redirect"
	StateNotFound     = "not_found"
	StateNoData       = "no_data"
	StateForbidden    = "forbidden"
	StateFailed       = "failed"
	StateNotAttempted = "not_attempted"
)

// UntrustedDocumentFields lists response fields copied from third-party pages.
// They may contain text written to manipulate an agent reading them.
var UntrustedDocumentFields = []string{"title", "meta", "openGraphData", "responseHeaders"}

// DocumentItem is one page's lookup result.
type DocumentItem struct {
	URL          string        `json:"url"`
	OK           bool          `json:"ok"`
	State        string        `json:"state"`
	DownloadedAt string        `json:"downloaded_at,omitempty"`
	IndexedAt    string        `json:"indexed_at,omitempty"`
	Document     any           `json:"document,omitempty"`
	Error        *apperr.Error `json:"error,omitempty"`
}

// BatchSummary aggregates a lookup batch.
type BatchSummary struct {
	Total  int            `json:"total"`
	OK     int            `json:"ok"`
	Failed int            `json:"failed"`
	States map[string]int `json:"states"`
}

// DocumentBatch is the data of a multi-URL lookup.
type DocumentBatch struct {
	Summary BatchSummary   `json:"summary"`
	Items   []DocumentItem `json:"items"`
}

// Document wraps GET /web/document for one URL.
func (s *Service) Document(ctx context.Context, rawURL string, brief bool) (Result, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	item, raw, err := s.lookup(ctx, u, brief)
	if err != nil {
		return Result{}, err
	}
	res := Result{Data: item, Raw: raw}
	if !brief {
		res.Untrusted = untrustedPaths("document.")
	}
	return res, nil
}

func untrustedPaths(prefix string) []string {
	out := make([]string, len(UntrustedDocumentFields))
	for i, f := range UntrustedDocumentFields {
		out[i] = prefix + f
	}
	return out
}

func (s *Service) lookup(ctx context.Context, u string, brief bool) (DocumentItem, []byte, error) {
	q := url.Values{"url": {u}}
	v, raw, err := s.getJSON(ctx, api.Request{Path: "/web/document", Query: q})
	if err != nil {
		return DocumentItem{}, nil, err
	}
	item := DocumentItem{URL: u, OK: true}
	if m, ok := v.(map[string]any); ok {
		item.State = documentState(m)
		item.DownloadedAt = timestamp(m["downloadTimestamp"])
		item.IndexedAt = timestamp(m["indexTimestamp"])
	}
	if !brief {
		item.Document = v
	}
	return item, raw, nil
}

// documentState collapses the flags into one value. Problems outrank indexing:
// a page can stay in the index after it started failing.
func documentState(m map[string]any) string {
	is := func(k string) bool { b, _ := m[k].(bool); return b }
	switch {
	case is("isError"):
		return StateError
	case is("isRedirect"):
		return StateRedirect
	case is("isIndexed"):
		return StateIndexed
	}
	return StateNotIndexed
}

// timestamp renders an API timestamp. The API does not state the unit, so
// values too large to be seconds are read as milliseconds.
func timestamp(v any) string {
	n, ok := api.Int(v)
	if !ok || n <= 0 {
		return ""
	}
	if n > 100_000_000_000 {
		return time.UnixMilli(n).UTC().Format(time.RFC3339)
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// CheckDocuments looks up many URLs sequentially, pacing itself through the
// limiter. A page Seznam does not know is a valid answer, not a failure; only
// transport, auth and access problems fail an item. onItem runs as each result
// arrives so a killed process still leaves partial output.
func (s *Service) CheckDocuments(ctx context.Context, urls []string, brief bool, onItem func(DocumentItem)) (Result, error) {
	batch := DocumentBatch{Items: make([]DocumentItem, 0, len(urls))}
	batch.Summary.States = map[string]int{}
	var abort *apperr.Error

	for _, u := range urls {
		var item DocumentItem
		switch {
		case abort != nil:
			item = DocumentItem{URL: u, State: StateNotAttempted, Error: abort}
		case ctx.Err() != nil:
			abort = apperr.New(apperr.CodePartialFailure, "stopped before all URLs were checked").
				WithHint("Raise --deadline or resubmit the not_attempted URLs.")
			item = DocumentItem{URL: u, State: StateNotAttempted, Error: abort}
		default:
			var err error
			item, _, err = s.lookup(ctx, u, brief)
			if err != nil {
				e := apperr.From(err)
				item = DocumentItem{URL: u, State: failureState(e), Error: e}
				switch e.Code {
				case apperr.CodeNotFound, apperr.CodeNoDataYet:
					item.OK = true
					item.Error = nil
				case apperr.CodeAuthInvalid, apperr.CodeAuthMissing:
					abort = e // every further call would fail the same way
				}
			}
		}
		batch.Items = append(batch.Items, item)
		batch.Summary.Total++
		batch.Summary.States[item.State]++
		if item.OK {
			batch.Summary.OK++
		} else {
			batch.Summary.Failed++
		}
		if onItem != nil {
			onItem(item)
		}
	}
	res := Result{Data: batch}
	if !brief {
		res.Untrusted = untrustedPaths("items[].document.")
	}
	switch {
	case abort != nil:
		res.Err = abort
	case batch.Summary.Failed > 0:
		res.Err = apperr.New(apperr.CodePartialFailure, "%d of %d URLs failed", batch.Summary.Failed, batch.Summary.Total).
			WithHint("Retry the items whose ok is false.")
	}
	return res, nil
}

func failureState(e *apperr.Error) string {
	switch e.Code {
	case apperr.CodeNotFound:
		return StateNotFound
	case apperr.CodeNoDataYet:
		return StateNoData
	case apperr.CodeForbidden:
		return StateForbidden
	}
	return StateFailed
}
