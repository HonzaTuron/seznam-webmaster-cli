// Package apperr defines the machine-readable error contract of szwm.
//
// Error codes and exit codes are a public, versioned interface: agents branch
// on them. Add new values freely, but never change the meaning of existing ones
// without bumping output.SchemaVersion.
package apperr

import (
	"errors"
	"fmt"
)

// Process exit codes.
const (
	ExitOK          = 0
	ExitInternal    = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitForbidden   = 4
	ExitNotFound    = 5
	ExitNoData      = 6
	ExitRateLimited = 7
	ExitUnavailable = 8
	ExitPartial     = 9
	ExitQuota       = 10
	ExitReadOnly    = 11
	ExitFindings    = 12
)

// Code is a stable identifier for a failure class.
type Code string

// Stable error codes.
const (
	CodeInternal             Code = "internal"
	CodeInvalidArgument      Code = "invalid_argument"
	CodeConfirmationRequired Code = "confirmation_required"
	CodeAuthMissing          Code = "auth_missing"
	CodeAuthInvalid          Code = "auth_invalid"
	CodeForbidden            Code = "forbidden"
	CodeNotFound             Code = "not_found"
	CodeNoDataYet            Code = "no_data_yet"
	CodeRateLimited          Code = "rate_limited"
	CodeServiceUnavailable   Code = "service_unavailable"
	CodeNetworkError         Code = "network_error"
	CodeTimeout              Code = "timeout"
	CodeBadResponse          Code = "bad_response"
	CodeAPIError             Code = "api_error"
	CodePartialFailure       Code = "partial_failure"
	CodeQuotaExhausted       Code = "quota_exhausted"
	CodeReadOnly             Code = "read_only"
	CodeFindingsThreshold    Code = "findings_threshold"
	CodeDoctorFailed         Code = "doctor_failed"
)

// Entry documents one error code. Catalog drives both behaviour and `szwm schema`.
type Entry struct {
	Code        Code   `json:"code"`
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

// Catalog lists every error code szwm can emit.
var Catalog = []Entry{
	{CodeInternal, ExitInternal, false, "Unexpected failure inside szwm."},
	{CodeInvalidArgument, ExitUsage, false, "Bad flag, argument or input value; fix the command, do not retry it unchanged."},
	{CodeConfirmationRequired, ExitUsage, false, "A bulk write needs an explicit --yes; preview it with --dry-run first."},
	{CodeAuthMissing, ExitAuth, false, "No API key could be resolved."},
	{CodeAuthInvalid, ExitAuth, false, "Seznam rejected the API key (HTTP 401)."},
	{CodeForbidden, ExitForbidden, false, "The key lacks access to this URL or write permission (HTTP 403)."},
	{CodeNotFound, ExitNotFound, false, "Seznam has no data for this page or site (HTTP 404)."},
	{CodeNoDataYet, ExitNoData, true, "The request was fine but data is not available yet (HTTP 204); retry after the next database release."},
	{CodeRateLimited, ExitRateLimited, true, "Rate limit reached after retries; wait retry_after_seconds."},
	{CodeServiceUnavailable, ExitUnavailable, true, "Seznam service is down (HTTP 502/503/504)."},
	{CodeNetworkError, ExitUnavailable, true, "Could not reach Seznam (DNS, TLS, connection)."},
	{CodeTimeout, ExitUnavailable, true, "A request or the whole command ran out of time."},
	{CodeBadResponse, ExitUnavailable, true, "Seznam returned a response szwm could not interpret."},
	{CodeAPIError, ExitInternal, false, "Seznam returned an unexpected HTTP error."},
	{CodePartialFailure, ExitPartial, true, "A batch finished only partly; data.items says which URLs need another attempt."},
	{CodeQuotaExhausted, ExitQuota, true, "The estimated daily reindex quota (500) is used up; retry after midnight Europe/Prague."},
	{CodeReadOnly, ExitReadOnly, false, "Read-only mode blocked a write operation."},
	{CodeFindingsThreshold, ExitFindings, false, "`report --fail-on` matched a finding; the report itself succeeded."},
	{CodeDoctorFailed, ExitInternal, false, "At least one `doctor` check failed; data.checks has the fix."},
}

var byCode = func() map[Code]Entry {
	m := make(map[Code]Entry, len(Catalog))
	for _, e := range Catalog {
		m[e.Code] = e
	}
	return m
}()

// Error is the structured failure object emitted to callers.
type Error struct {
	Code              Code   `json:"code"`
	Message           string `json:"message"`
	Hint              string `json:"hint,omitempty"`
	Field             string `json:"field,omitempty"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	ExitCode          int    `json:"exit_code"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// New builds an Error whose retryability and exit code come from Catalog.
func New(code Code, format string, args ...any) *Error {
	entry, ok := byCode[code]
	if !ok {
		entry = byCode[CodeInternal]
	}
	return &Error{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: entry.Retryable,
		ExitCode:  entry.ExitCode,
	}
}

// WithHint attaches a suggested next action.
func (e *Error) WithHint(hint string) *Error {
	e.Hint = hint
	return e
}

// WithField names the input that caused the error.
func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

// Usage is shorthand for an invalid_argument error on a named field.
func Usage(field, format string, args ...any) *Error {
	return New(CodeInvalidArgument, format, args...).WithField(field)
}

// From converts any error into an *Error, wrapping unknown ones as internal.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(CodeInternal, "%s", err.Error())
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
