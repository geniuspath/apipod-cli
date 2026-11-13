package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// balanceResult matches the MCP get_balance output.
type balanceResult struct {
	BalanceUSD float64 `json:"balance_usd"`
	Level      string  `json:"level"`
	TopUpURL   string  `json:"topup_url"`
}

func (a *App) newBalanceCmd(flags *globalFlags) *cobra.Command {
	var open bool
	cmd := &cobra.Command{
		Use:   "balance",
		Short: "Show the account balance",
		Long: `Show the remaining balance of the signed-in account.

Payments are not made from the CLI. --open opens the billing page of the web
console, where funds can be added.`,
		Args: positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error { return rt.balance(open) }),
	}
	cmd.Flags().BoolVar(&open, "open", false, "open the console billing page in the browser")
	return cmd
}

func (rt *runtime) balance(open bool) error {
	console, err := rt.consoleURL()
	if err != nil {
		return err
	}
	client, err := rt.client()
	if err != nil {
		return err
	}
	bal, err := client.GetBalance(rt.cmd.Context())
	if err != nil {
		return err
	}
	result := balanceResult{BalanceUSD: bal.BalanceUSD, Level: bal.Level, TopUpURL: console + "/console/billing"}
	if open {
		if rt.app.OpenBrowser == nil || rt.app.OpenBrowser(result.TopUpURL) != nil {
			rt.p.Warnf("could not open the browser; visit %s", result.TopUpURL)
		}
	}
	return rt.p.Result(result, func(w io.Writer) {
		rt.p.KV(w, "Balance", formatUSD(bal.BalanceUSD), "Level", bal.Level, "Top up", result.TopUpURL)
	})
}
