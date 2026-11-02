package wm

import (
	"context"
	"net/url"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/ratelimit"
)

// Reindex item statuses.
const (
	ReindexSubmitted    = "submitted"
	ReindexFailed       = "failed"
	ReindexNotAttempted = "not_attempted"
	ReindexPlanned      = "planned"
)

// ReindexItem is the result for one URL.
type ReindexItem struct {
	URL    string        `json:"url"`
	Status string        `json:"status"`
	Reason string        `json:"reason,omitempty"`
	Error  *apperr.Error `json:"error,omitempty"`
}

// ReindexSummary aggregates a run.
type ReindexSummary struct {
	Total        int `json:"total"`
	Submitted    int `json:"submitted"`
	Failed       int `json:"failed"`
	NotAttempted int `json:"not_attempted"`
	Planned      int `json:"planned,omitempty"`
}

// ReindexData is the data of a reindex run or plan.
type ReindexData struct {
	DryRun           bool            `json:"dry_run"`
	Summary          ReindexSummary  `json:"summary"`
	Items            []ReindexItem   `json:"items"`
	Quota            ratelimit.Quota `json:"quota"`
	EstimatedSeconds int             `json:"estimated_seconds"`
}

// ReindexOptions control a run.
type ReindexOptions struct {
	URLs        []string
	DryRun      bool
	IgnoreQuota bool
	OnItem      func(ReindexItem)
}

// ReadOnlyError is returned when a write is blocked.
func ReadOnlyError() *apperr.Error {
	return apperr.New(apperr.CodeReadOnly, "read-only mode blocks reindexing").
		WithHint("Remove --read-only / SZWM_READ_ONLY if the write is intended, or use --dry-run.")
}

// Reindex wraps POST /web/document/reindex. URLs must already be cleaned.
//
// The URL list is cut to the estimated remaining quota before the first call,
// and each success is recorded immediately, so an interrupted batch leaves the
// ledger correct. Anything left over is reported as not_attempted with a
// reason, which makes resuming the next day a mechanical step.
func (s *Service) Reindex(ctx context.Context, o ReindexOptions) (Result, error) {
	if s.ReadOnly && !o.DryRun {
		return Result{}, ReadOnlyError()
	}
	data := ReindexData{DryRun: o.DryRun, Items: make([]ReindexItem, 0, len(o.URLs))}
	data.Summary.Total = len(o.URLs)
	data.Quota = s.Ledger.Status()
	allowed := len(o.URLs)
	if !o.IgnoreQuota {
		allowed = min(allowed, data.Quota.Remaining)
	}
	data.EstimatedSeconds = estimateSeconds(allowed)

	emit := func(item ReindexItem) {
		data.Items = append(data.Items, item)
		switch item.Status {
		case ReindexSubmitted:
			data.Summary.Submitted++
		case ReindexFailed:
			data.Summary.Failed++
		case ReindexPlanned:
			data.Summary.Planned++
		default:
			data.Summary.NotAttempted++
		}
		if o.OnItem != nil {
			o.OnItem(item)
		}
	}

	if o.DryRun {
		for i, u := range o.URLs {
			if i < allowed {
				emit(ReindexItem{URL: u, Status: ReindexPlanned})
			} else {
				emit(ReindexItem{URL: u, Status: ReindexNotAttempted, Reason: "quota"})
			}
		}
		return Result{Data: data, Hints: []string{"Dry run: nothing was sent to Seznam."}}, nil
	}

	var abort *apperr.Error
	reason := ""
	var firstFailure *apperr.Error
	for i, u := range o.URLs {
		switch {
		case i >= allowed:
			emit(ReindexItem{URL: u, Status: ReindexNotAttempted, Reason: "quota"})
			continue
		case abort != nil:
			emit(ReindexItem{URL: u, Status: ReindexNotAttempted, Reason: reason})
			continue
		case ctx.Err() != nil:
			abort = apperr.New(apperr.CodePartialFailure, "stopped before all URLs were submitted").
				WithHint("Raise --deadline or resubmit the not_attempted URLs.")
			reason = "deadline"
			emit(ReindexItem{URL: u, Status: ReindexNotAttempted, Reason: reason})
			continue
		}
		_, err := s.API.Do(ctx, api.Request{Method: "POST", Path: "/web/document/reindex", Query: url.Values{"url": {u}}})
		if err == nil {
			// Ledger trouble must not turn an accepted request into a failure.
			_ = s.Ledger.Add(1)
			data.Quota = s.Ledger.Status()
			emit(ReindexItem{URL: u, Status: ReindexSubmitted})
			continue
		}
		e := apperr.From(err)
		if firstFailure == nil {
			firstFailure = e
		}
		emit(ReindexItem{URL: u, Status: ReindexFailed, Error: e})
		switch e.Code {
		case apperr.CodeAuthInvalid, apperr.CodeAuthMissing:
			abort, reason = e, "auth"
		case apperr.CodeRateLimited:
			abort, reason = e, "rate_limited"
		}
	}

	res := Result{Data: data}
	if data.Summary.Submitted > 0 {
		res.Hints = append(res.Hints,
			"Submitted is a request, not a guarantee: results appear only after a later data release. Compare `szwm database-info`, then run `szwm document <url>`.")
	}
	sum := data.Summary
	switch {
	case sum.Submitted == sum.Total:
	case sum.Total == 1 && firstFailure != nil:
		res.Err = firstFailure // keep the precise exit code for the simple case
	case sum.Submitted == 0 && sum.Failed == 0 && allowed == 0:
		res.Err = apperr.New(apperr.CodeQuotaExhausted, "estimated daily reindex quota of %d is used up", ratelimit.ReindexPerDay).
			WithHint("Retry after midnight Europe/Prague. The count is a local estimate; --ignore-quota overrides it.")
	case abort != nil:
		res.Err = abort
	default:
		res.Err = apperr.New(apperr.CodePartialFailure, "%d submitted, %d failed, %d not attempted",
			sum.Submitted, sum.Failed, sum.NotAttempted).
			WithHint("Resubmit items whose status is not submitted; quota leftovers can run tomorrow.")
	}
	return res, nil
}

// estimateSeconds predicts the run time under the 100 requests/minute limit.
func estimateSeconds(n int) int {
	if n <= 100 {
		return (n + 4) / 5
	}
	return n * 60 / 100
}

// CheckBulk requires explicit confirmation for writes touching several URLs.
// szwm never prompts, because a prompt would hang a non-interactive agent.
func CheckBulk(n int, confirmed, dryRun bool) error {
	if n > 1 && !confirmed && !dryRun {
		return apperr.New(apperr.CodeConfirmationRequired, "reindexing %d URLs needs explicit confirmation", n).
			WithHint("Preview with --dry-run, then rerun with --yes.")
	}
	return nil
}
