package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/output"
)

// spendCheck is the input of the spending guard.
type spendCheck struct {
	// estimate is the quoted price in USD; nil when it could not be obtained.
	estimate *float64
	// threshold is spend.confirm_above: estimates above it need approval.
	threshold float64
	// maxCost is --max-cost; nil when the flag was not given.
	maxCost *float64
	// yes is --yes.
	yes bool
}

// guardSpend decides whether a generation may start. It exists so that an
// agent driven by injected instructions cannot spend the balance unattended:
// anything above the threshold needs an explicit approval, and when nobody
// can be asked (no terminal) the default answer is no.
//
//	--max-cost N   hard cap: proceeds only when the estimate is known and <= N
//	--yes          approves spending without a prompt
//	terminal       asks the user
//	otherwise      refuses with exit code 6
func guardSpend(ctx context.Context, p *output.Printer, in io.Reader, c spendCheck) error {
	if c.maxCost != nil {
		if c.estimate == nil {
			return exitcode.New(exitcode.Billing, "cannot verify --max-cost: the cost of this request could not be estimated")
		}
		if *c.estimate > *c.maxCost {
			return exitcode.New(exitcode.Billing, "estimated cost %s exceeds --max-cost %s", formatUSD(*c.estimate), formatUSD(*c.maxCost))
		}
		return nil
	}
	if c.estimate != nil && *c.estimate <= c.threshold {
		return nil
	}
	if c.yes {
		if c.estimate == nil {
			p.Warnf("the cost of this request could not be estimated; continuing because of --yes")
		}
		return nil
	}

	what := "This request has no cost estimate."
	if c.estimate != nil {
		what = fmt.Sprintf("Estimated cost %s is above the confirmation threshold %s.", formatUSD(*c.estimate), formatUSD(c.threshold))
	}
	if !p.Interactive() {
		return exitcode.New(exitcode.Billing,
			"%s Refusing to spend without confirmation: pass --yes to approve, or --max-cost <usd> to set a cap", what)
	}
	ok, err := confirm(ctx, p, in, what+" Continue?")
	if err != nil {
		return err
	}
	if !ok {
		return exitcode.New(exitcode.Billing, "cancelled: spending was not confirmed")
	}
	return nil
}

// confirm asks a yes/no question on stderr; anything but y/yes is a no.
// Ctrl-C ends the wait instead of leaving the command stuck on stdin.
func confirm(ctx context.Context, p *output.Printer, in io.Reader, question string) (bool, error) {
	fmt.Fprintf(p.Err, "%s [y/N] ", question)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(p.Err)
		return false, ctx.Err()
	case line := <-answer:
		if !strings.HasSuffix(line, "\n") {
			fmt.Fprintln(p.Err) // stdin closed without a newline
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}
