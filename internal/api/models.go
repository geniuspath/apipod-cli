package api

import (
	"context"
	"net/http"
	"net/url"
)

// Model is one entry of GET /public/models.
type Model struct {
	ModelID         string   `json:"model_id"`
	DisplayName     *string  `json:"display_name"`
	Classifications []string `json:"classifications"`
	InputTypes      []string `json:"input_types"`
	Lifecycle       string   `json:"lifecycle"`
	ProviderID      string   `json:"provider_id"`
	Description     string   `json:"description"`
	SeriesID        string   `json:"series_id"`
}

// ListModels calls GET /public/models, optionally filtered by classification
// (text_to_image, image_to_video, …).
func (c *Client) ListModels(ctx context.Context, classification string) ([]Model, error) {
	path := "/public/models"
	if classification != "" {
		path += "?" + url.Values{"classification": {classification}}.Encode()
	}
	var out []Model
	err := c.do(ctx, call{method: http.MethodGet, path: path}, &out)
	return out, err
}

// ModelSchema is the OpenAPI document served by GET /public/schemas/:id.
type ModelSchema struct {
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
}

// Input returns the request-body schema, or nil when the model publishes none.
func (s ModelSchema) Input() map[string]any { return s.Components.Schemas["Input"] }

// GetModelSchema calls GET /public/schemas/:model.
func (c *Client) GetModelSchema(ctx context.Context, model string) (ModelSchema, error) {
	var out ModelSchema
	err := c.do(ctx, call{method: http.MethodGet, path: "/public/schemas/" + url.PathEscape(model)}, &out)
	return out, err
}
