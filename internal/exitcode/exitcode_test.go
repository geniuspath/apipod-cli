package exitcode

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type coded struct{ code int }

func (c coded) Error() string { return "coded" }
func (c coded) ExitCode() int { return c.code }

func TestFrom(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, OK},
		{errors.New("boom"), General},
		{New(Usage, "bad flag"), Usage},
		{New(Billing, "over budget"), Billing},
		{fmt.Errorf("wrapped: %w", New(Auth, "no credential")), Auth},
		{coded{RateLimit}, RateLimit},
		{fmt.Errorf("wrapped: %w", coded{NotFound}), NotFound},
		{context.DeadlineExceeded, Timeout},
		{fmt.Errorf("wait: %w", context.DeadlineExceeded), Timeout},
		{Wrap(Validation, errors.New("bad file")), Validation},
	}
	for _, tc := range cases {
		if got := From(tc.err); got != tc.want {
			t.Errorf("From(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if Wrap(Auth, nil) != nil {
		t.Error("Wrap(nil) is not nil")
	}
}

// The numbers are a public contract: scripts and agents branch on them.
func TestExitCodeTable(t *testing.T) {
	want := map[string]int{
		"OK": 0, "General": 1, "Usage": 2, "Auth": 3, "RateLimit": 4,
		"NotFound": 5, "Billing": 6, "Timeout": 7, "Validation": 8,
	}
	got := map[string]int{
		"OK": OK, "General": General, "Usage": Usage, "Auth": Auth, "RateLimit": RateLimit,
		"NotFound": NotFound, "Billing": Billing, "Timeout": Timeout, "Validation": Validation,
	}
	for name, code := range want {
		if got[name] != code {
			t.Errorf("%s = %d, want %d", name, got[name], code)
		}
	}
}
