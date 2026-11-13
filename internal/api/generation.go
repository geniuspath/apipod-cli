package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// Job statuses reported by the status endpoints.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Job kinds select the generation and status endpoints.
const (
	KindImage = "image"
	KindVideo = "video"
)

// Task is the response of a generation POST. Both endpoints are asynchronous.
type Task struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// Generate submits a generation request for kind. The request carries an
// Idempotency-Key, so the client's retries can never create a second task.
func (c *Client) Generate(ctx context.Context, kind string, body map[string]any) (Task, error) {
	var out Task
	err := c.do(ctx, call{
		method:     http.MethodPost,
		path:       "/v1/" + kind + "s/generations",
		body:       body,
		idempotent: true,
	}, &out)
	if err == nil && out.Status == "" {
		out.Status = StatusPending
	}
	return out, err
}

// Job is the CLI's job view. The JSON shape matches the MCP check_job tool so
// agents see the same fields on both surfaces.
type Job struct {
	JobID        string     `json:"job_id"`
	Kind         string     `json:"kind,omitempty"`
	Status       string     `json:"status"`
	Progress     *int       `json:"progress,omitempty"`
	Results      []JobAsset `json:"results,omitempty"`
	Text         string     `json:"text,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CostUSD      *float64   `json:"cost_usd,omitempty"`
	CompletedAt  *int64     `json:"completed_at,omitempty"`
}

// JobAsset is one generated file. Path is set once the CLI has downloaded it.
type JobAsset struct {
	URL  string `json:"url"`
	Path string `json:"path,omitempty"`
}

// Done reports whether the job reached a terminal status.
func (j Job) Done() bool { return j.Status == StatusCompleted || j.Status == StatusFailed }

// taskStatus is the union of the image and video status payloads.
type taskStatus struct {
	TaskID       string            `json:"task_id"`
	Status       string            `json:"status"`
	Progress     *int              `json:"progress"`
	Result       []json.RawMessage `json:"result"`
	Text         string            `json:"text"`
	Error        string            `json:"error"`
	ErrorCode    string            `json:"error_code"`
	ErrorMessage string            `json:"error_message"`
	CompletedAt  *int64            `json:"completed_at"`
	Usage        *struct {
		Cost float64 `json:"cost"`
	} `json:"usage"`
}

func (t taskStatus) job(kind string) Job {
	j := Job{
		JobID:        t.TaskID,
		Kind:         kind,
		Status:       t.Status,
		Progress:     t.Progress,
		Text:         t.Text,
		ErrorCode:    t.ErrorCode,
		ErrorMessage: t.ErrorMessage,
		CompletedAt:  t.CompletedAt,
	}
	if j.ErrorMessage == "" {
		j.ErrorMessage = t.Error
	}
	// `result` is a list of URLs, or of {url, …} objects when the upstream
	// attached per-image metadata.
	for _, raw := range t.Result {
		var u string
		if json.Unmarshal(raw, &u) != nil {
			var obj struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &obj) != nil {
				continue
			}
			u = obj.URL
		}
		if u != "" {
			j.Results = append(j.Results, JobAsset{URL: u})
		}
	}
	if t.Usage != nil {
		cost := t.Usage.Cost
		j.CostUSD = &cost
	}
	return j
}

// GetJobOfKind reads the status of a job whose kind is known.
func (c *Client) GetJobOfKind(ctx context.Context, kind, id string) (Job, error) {
	var out taskStatus
	err := c.do(ctx, call{method: http.MethodGet, path: "/v1/" + kind + "s/status/" + url.PathEscape(id)}, &out)
	if err != nil {
		return Job{}, err
	}
	if out.TaskID == "" {
		out.TaskID = id
	}
	return out.job(kind), nil
}

// GetJob reads a job of unknown kind: video first, then image — the same
// order as the MCP check_job tool.
//
// The video endpoint does not answer 404 for an id it does not know (the
// lookup error surfaces as a 5xx), so any API error falls through to the
// image lookup, whose 404 is authoritative. Credential, rate-limit and
// network failures are reported as they are: retrying them against another
// endpoint would only hide the real problem.
func (c *Client) GetJob(ctx context.Context, id string) (Job, error) {
	job, err := c.GetJobOfKind(ctx, KindVideo, id)
	if err == nil {
		return job, nil
	}
	var apiErr *Error
	if !asAPIError(err, &apiErr) {
		return Job{}, err
	}
	switch apiErr.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return Job{}, err
	}
	return c.GetJobOfKind(ctx, KindImage, id)
}
