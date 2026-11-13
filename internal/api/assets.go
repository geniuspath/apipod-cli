package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Asset statuses that end a review.
const (
	AssetStatusActive = "ACTIVE"
	AssetStatusFailed = "FAILED"
)

// assetRefPrefix is how an asset is referenced inside generation parameters.
const assetRefPrefix = "assetId://"

// Asset is a reviewed reference asset (/v1/assets). Field names follow the
// server contract; Ref is added by the CLI.
type Asset struct {
	AssetID      string  `json:"assetId"`
	AssetType    string  `json:"assetType"`
	URL          string  `json:"url"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"errorMessage"`
	Name         string  `json:"name"`
	// Ref is the value to pass to generation parameters (assetId://…).
	Ref string `json:"ref"`
}

func (a Asset) withRef() Asset {
	a.Ref = assetRefPrefix + a.AssetID
	return a
}

// AssetID strips the assetId:// prefix, so users can paste either form.
func AssetID(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), assetRefPrefix)
}

// CreateAssetRequest is the body of POST /v1/assets.
type CreateAssetRequest struct {
	AssetType string `json:"assetType"`
	URL       string `json:"url"`
	Name      string `json:"name,omitempty"`
}

// CreateAsset submits a URL for review. It is not idempotent server-side, so
// it is not retried after an ambiguous failure.
func (c *Client) CreateAsset(ctx context.Context, req CreateAssetRequest) (Asset, error) {
	var out Asset
	err := c.do(ctx, call{method: http.MethodPost, path: "/v1/assets", body: req}, &out)
	return out.withRef(), err
}

// GetAsset reads one asset.
func (c *Client) GetAsset(ctx context.Context, id string) (Asset, error) {
	var out Asset
	err := c.do(ctx, call{method: http.MethodGet, path: "/v1/assets/" + url.PathEscape(AssetID(id))}, &out)
	return out.withRef(), err
}

// DeleteAsset removes one asset.
func (c *Client) DeleteAsset(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/v1/assets/" + url.PathEscape(AssetID(id))}, nil)
}

// AssetListParams filters GET /v1/assets.
type AssetListParams struct {
	Type     string
	Status   string
	Search   string
	Page     int
	PageSize int
}

// AssetList is one page of assets.
type AssetList struct {
	Items      []Asset `json:"items"`
	Total      int64   `json:"total"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	TotalPages int     `json:"total_pages"`
}

// ListAssets calls GET /v1/assets.
func (c *Client) ListAssets(ctx context.Context, p AssetListParams) (AssetList, error) {
	q := url.Values{}
	if p.Type != "" {
		q.Set("type", p.Type)
	}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	if p.Search != "" {
		q.Set("search", p.Search)
	}
	if p.Page > 0 {
		q.Set("page", strconv.Itoa(p.Page))
	}
	if p.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(p.PageSize))
	}
	path := "/v1/assets"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out AssetList
	if err := c.do(ctx, call{method: http.MethodGet, path: path}, &out); err != nil {
		return AssetList{}, err
	}
	items := make([]Asset, len(out.Items))
	for i, a := range out.Items {
		items[i] = a.withRef()
	}
	out.Items = items
	return out, nil
}

func asAPIError(err error, target **Error) bool { return errors.As(err, target) }
