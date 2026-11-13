// Package exitcode defines the CLI's semantic exit codes and the error type
// that carries them up to main.
package exitcode

import (
	"context"
	"errors"
	"fmt"
)

// Exit codes are part of the public contract: scripts and agents branch on them.
const (
	OK         = 0
	General    = 1
	Usage      = 2
	Auth       = 3
	RateLimit  = 4
	NotFound   = 5
	Billing    = 6 // insufficient balance, over --max-cost, or spend refused
	Timeout    = 7
	Validation = 8 // request or local input rejected
)

// Error attaches an exit code to an error.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// New builds an Error from a format string.
func New(code int, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Wrap attaches code to err; nil stays nil.
func Wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

// Coder is implemented by errors that know their own exit code (API errors).
type Coder interface {
	ExitCode() int
}

// From maps any error to an exit code.
func From(err error) int {
	if err == nil {
		return OK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var c Coder
	if errors.As(err, &c) {
		return c.ExitCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	return General
}
