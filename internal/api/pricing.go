package api

import (
	"context"
	"net/http"
)

// EstimateRequest is the body of POST /v1/pricing/estimate.
type EstimateRequest struct {
	ModelID          string            `json:"model_id"`
	Dimensions       map[string]string `json:"dimensions,omitempty"`
	HasVideoMaterial bool              `json:"has_video_material,omitempty"`
}

// Estimate is a price quote.
type Estimate struct {
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	// IsPreDeduct marks usage-billed models: Price is reserved at submit time
	// and settled to actual usage on completion.
	IsPreDeduct bool `json:"is_pre_deduct"`
}

// EstimateCost calls POST /v1/pricing/estimate. It changes nothing
// server-side, so it is retried like a read.
func (c *Client) EstimateCost(ctx context.Context, req EstimateRequest) (Estimate, error) {
	var out Estimate
	err := c.do(ctx, call{method: http.MethodPost, path: "/v1/pricing/estimate", body: req, readOnly: true}, &out)
	return out, err
}
