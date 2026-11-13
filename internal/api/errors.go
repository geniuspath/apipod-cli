package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

// Error is an HTTP error response from APIPod.
type Error struct {
	Method     string
	Path       string
	HTTPStatus int
	Code       string // machine code: error.code, data.error_code, or OAuth error
	Message    string
	RequestID  string
	RetryAfter string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString(http.StatusText(e.HTTPStatus))
	}
	fmt.Fprintf(&b, " (HTTP %d", e.HTTPStatus)
	if e.Code != "" {
		fmt.Fprintf(&b, ", %s", e.Code)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, ", request %s", e.RequestID)
	}
	b.WriteString(")")
	return b.String()
}

// ExitCode implements exitcode.Coder: the HTTP status picks the class, the
// machine code refines it.
func (e *Error) ExitCode() int {
	switch e.Code {
	case "insufficient_quota":
		return exitcode.Billing
	case "rate_limit_exceeded":
		return exitcode.RateLimit
	case "invalid_api_key":
		return exitcode.Auth
	case "model_not_available":
		return exitcode.NotFound
	}
	switch e.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		return exitcode.Auth
	case http.StatusPaymentRequired:
		return exitcode.Billing
	case http.StatusNotFound:
		return exitcode.NotFound
	case http.StatusTooManyRequests:
		return exitcode.RateLimit
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return exitcode.Timeout
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return exitcode.Validation
	}
	return exitcode.General
}

// TransportError is a failure to get any HTTP response (DNS, connect, reset).
type TransportError struct {
	Method string
	Path   string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("cannot reach APIPod (%s %s): %v", e.Method, e.Path, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is an API 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.HTTPStatus == http.StatusNotFound
}

// retryable decides whether a failed attempt may be repeated.
//
// replaySafe is true for GET/DELETE and for POSTs that carry an
// Idempotency-Key: repeating them cannot create (or bill) a second task.
// Other POSTs are only retried on 429, where the server has said it did not
// process the request. No other 4xx is ever retried.
func retryable(err error, replaySafe bool) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return replaySafe
	}
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.HTTPStatus {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return replaySafe
	}
	return false
}

// asAPIError is the package's errors.As shorthand for *Error.
func asAPIError(err error, target **Error) bool { return errors.As(err, target) }
