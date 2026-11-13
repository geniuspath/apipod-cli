package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/auth"
)

// costResult matches the MCP estimate_cost output. Balance fields are absent
// when the balance could not be read.
type costResult struct {
	Model        string   `json:"model"`
	EstimatedUSD float64  `json:"estimated_usd"`
	IsPreDeduct  bool     `json:"is_pre_deduct,omitempty"`
	BalanceUSD   *float64 `json:"balance_usd,omitempty"`
	Sufficient   *bool    `json:"sufficient,omitempty"`
}

func (a *App) newCostCmd(flags *globalFlags) *cobra.Command {
	var dims []string
	var hasVideo bool
	cmd := &cobra.Command{
		Use:   "cost <model>",
		Short: "Estimate what a generation will cost",
		Long: `Quote the price of a generation before running it, with the same pricing
the generation commands are billed by.

Pricing dimensions are the model parameters that affect the price; see
"apipod models params <model>". Omit them for the model's defaults.`,
		Example: `  apipod cost kling-3.0-turbo-t2v -d duration=10 -d resolution=1080p
  apipod cost gpt-image-2 -d n=2 --json`,
		Args: positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error { return rt.cost(args[0], dims, hasVideo) }),
	}
	cmd.Flags().StringArrayVarP(&dims, "dimension", "d", nil, "pricing dimension as key=value (repeatable)")
	cmd.Flags().BoolVar(&hasVideo, "has-video", false, "a reference video will be supplied (some models bill it extra)")
	return cmd
}

func (rt *runtime) cost(model string, dimPairs []string, hasVideo bool) error {
	dims, err := parseDimensions(dimPairs)
	if err != nil {
		return err
	}
	// Prices can be account-specific, so the estimate is authenticated when a
	// credential exists; without one the public price is quoted.
	signedIn := true
	client, err := rt.client()
	if errors.Is(err, auth.ErrLoginRequired) {
		signedIn = false
		client, err = rt.publicClient()
	}
	if err != nil {
		return err
	}

	ctx := rt.cmd.Context()
	est, err := client.EstimateCost(ctx, api.EstimateRequest{ModelID: model, Dimensions: dims, HasVideoMaterial: hasVideo})
	if err != nil {
		return err
	}
	result := costResult{Model: model, EstimatedUSD: est.Price, IsPreDeduct: est.IsPreDeduct}
	if signedIn {
		if bal, err := client.GetBalance(ctx); err != nil {
			rt.p.Warnf("could not read the account balance: %v", err)
		} else {
			sufficient := bal.BalanceUSD >= est.Price
			result.BalanceUSD, result.Sufficient = &bal.BalanceUSD, &sufficient
		}
	}

	return rt.p.Result(result, func(w io.Writer) {
		pairs := []string{"Model", model, "Estimated cost", formatUSD(est.Price)}
		if est.IsPreDeduct {
			pairs = append(pairs, "Billing", "by actual usage; this amount is reserved and settled on completion")
		}
		if result.BalanceUSD != nil {
			pairs = append(pairs, "Balance", formatUSD(*result.BalanceUSD))
		}
		rt.p.KV(w, pairs...)
		if result.Sufficient != nil && !*result.Sufficient {
			fmt.Fprintln(w, "The balance does not cover this estimate. Top up with `apipod balance --open`.")
		}
	})
}
