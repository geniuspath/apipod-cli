package api

import (
	"context"
	"net/http"
)

// Balance is the response of GET /v1/account/balance.
type Balance struct {
	BalanceUSD float64 `json:"balance_usd"`
	Level      string  `json:"level"`
}

// GetBalance calls GET /v1/account/balance.
func (c *Client) GetBalance(ctx context.Context) (Balance, error) {
	var out Balance
	err := c.do(ctx, call{method: http.MethodGet, path: "/v1/account/balance"}, &out)
	return out, err
}

// CheckCredential verifies that the client's credential is accepted, using
// the credential probe GET /v1/account/status.
func (c *Client) CheckCredential(ctx context.Context) error {
	return c.do(ctx, call{method: http.MethodGet, path: "/v1/account/status"}, nil)
}
