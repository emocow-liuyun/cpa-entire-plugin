package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// GET {core} /me/billing
// ---------------------------------------------------------------------------

// BillingStatus is the subset of the Core BillingStatus schema the bridge
// exposes. The remaining sections (card, payment method, automatic top-up) are
// intentionally omitted so a plugin never reads or logs payment data.
type BillingStatus struct {
	State       string           `json:"state"` // not_started | pending_card | provisioning | active
	Currency    string           `json:"currency"`
	Trial       *BillingTrial    `json:"trial"`
	Balance     *BillingBalance  `json:"balance"`
	Overage     *BillingOverage  `json:"overage"`
	RunsPaused  *BillingRunPause `json:"runs_paused"`
	Unavailable []string         `json:"unavailable"`
}

// BillingTrial is BillingTrial: the one-time credit.
type BillingTrial struct {
	State                string `json:"state"` // not_started | awaiting_card | active | used_up | expired | withheld
	Currency             string `json:"currency"`
	CreditTotalCents     *int64 `json:"credit_total_cents"`
	CreditRemainingCents *int64 `json:"credit_remaining_cents"`
	CreditExpiredCents   *int64 `json:"credit_expired_cents"`
	ExpiresAt            string `json:"expires_at"`
}

// BillingBalance is BillingBalance: every credit bucket, in cents.
type BillingBalance struct {
	Currency       string `json:"currency"`
	TotalCents     *int64 `json:"total_cents"`
	TrialCents     *int64 `json:"trial_cents"`
	GrantedCents   *int64 `json:"granted_cents"`
	PurchasedCents *int64 `json:"purchased_cents"`
}

// BillingOverage is usage past all credit in the current period.
type BillingOverage struct {
	AmountCents *int64 `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// BillingRunPause reports whether billing is refusing the subject's runs.
type BillingRunPause struct {
	Paused bool   `json:"paused"`
	Reason string `json:"reason"` // credit_exhausted | over_limit | blocked
}

// GetBilling reads GET {core}/me/billing. refresh asks Core to re-read the
// upstream billing provider rather than serve its cache.
//
// It performs one authenticated read and starts nothing.
func (c *Client) GetBilling(ctx context.Context, refresh bool) (*BillingStatus, error) {
	query := ""
	if refresh {
		query = "?refresh=true"
	}
	u, err := c.coreURLTyped("/me/billing", query)
	if err != nil {
		return nil, err
	}
	raw, _, err := c.do(ctx, requestOptions{method: http.MethodGet, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out BillingStatus
	if err := decodeInto(http.MethodGet, u, "BillingStatus", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBillingSpend reads GET {core}/me/billing/spend for one month (YYYY-MM; an
// empty period asks for the current month).
func (c *Client) GetBillingSpend(ctx context.Context, period string) (*BillingSpend, error) {
	query := ""
	if p := strings.TrimSpace(period); p != "" {
		query = "?period=" + url.QueryEscape(p)
	}
	u, err := c.coreURLTyped("/me/billing/spend", query)
	if err != nil {
		return nil, err
	}
	raw, _, err := c.do(ctx, requestOptions{method: http.MethodGet, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out BillingSpend
	if err := decodeInto(http.MethodGet, u, "BillingSpend", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BillingSpend is the subset of BillingSpend the bridge exposes.
type BillingSpend struct {
	Period     string                `json:"period"`
	Final      bool                  `json:"final"`
	Currency   string                `json:"currency"`
	TotalCents *int64                `json:"total_cents"`
	Projects   []BillingSpendProject `json:"projects"`
	OtherUsage []struct {
		TotalCents *int64 `json:"total_cents"`
		Metric     string `json:"metric"`
	} `json:"other_usage"`
}

// BillingSpendProject is BillingSpendProject.
type BillingSpendProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Deleted    bool   `json:"deleted"`
	TotalCents *int64 `json:"total_cents"`
}

// ---------------------------------------------------------------------------
// GET {cell} /agents/models
// ---------------------------------------------------------------------------

// AgentModels is AgentModels: one entry per harness an agent can use.
type AgentModels struct {
	Agents []AgentHarnessModels `json:"agents"`
}

// AgentHarnessModels is AgentHarnessModels for one harness.
type AgentHarnessModels struct {
	Harness                string       `json:"harness"` // claude | codex
	DefaultModel           string       `json:"default_model"`
	DefaultReasoningEffort *string      `json:"default_reasoning_effort"`
	ReasoningEfforts       []string     `json:"reasoning_efforts"`
	Models                 []AgentModel `json:"models"`
}

// AgentModel is AgentModel.
type AgentModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Visibility       string   `json:"visibility"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

// ListAgentModels reads GET {cell}/agents/models. It is a catalogue read; use
// FindAgentModel to learn whether a model id and effort pair is accepted.
func (c *Client) ListAgentModels(ctx context.Context) (*AgentModels, error) {
	u, err := c.cellURL("/agents/models")
	if err != nil {
		return nil, err
	}
	raw, _, err := c.do(ctx, requestOptions{method: http.MethodGet, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out AgentModels
	if err := decodeInto(http.MethodGet, u, "AgentModels", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FindAgentModel returns the harness entry and model for a harness/model pair.
// An empty model resolves to the harness default.
func (m *AgentModels) FindAgentModel(harness, model string) (*AgentHarnessModels, *AgentModel, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("%w: nil model catalogue", ErrNotConfigured)
	}
	for i := range m.Agents {
		h := &m.Agents[i]
		if harness != "" && h.Harness != harness {
			continue
		}
		if model == "" {
			for j := range h.Models {
				if h.Models[j].ID == h.DefaultModel {
					return h, &h.Models[j], nil
				}
			}
			return h, nil, nil
		}
		for j := range h.Models {
			if h.Models[j].ID == model {
				return h, &h.Models[j], nil
			}
		}
	}
	return nil, nil, fmt.Errorf("%w: harness %q model %q is not listed", ErrInvalidConfig, harness, model)
}

// ---------------------------------------------------------------------------
// GET {cell} /{host}/{project}/agents/{id}
// ---------------------------------------------------------------------------

// agentPath builds /{host}/{project}/agents/{id}.
func (c *Client) agentPath(agentID string) (string, error) {
	if agentID == "" {
		agentID = c.cfg.AgentID
	}
	if agentID == "" {
		return "", fmt.Errorf("%w: agent_id is required", ErrNotConfigured)
	}
	prefix, err := c.cfg.ProjectPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/agents/" + escapeSegment(agentID), nil
}

// ReadAgent performs GET {cell}/{host}/{project}/agents/{id} and caches the
// JSON etag for the next replace. It also honours the HTTP ETag header when the
// body carries no etag, but the body value wins per the spec.
func (c *Client) ReadAgent(ctx context.Context, agentID string) (*Agent, error) {
	path, err := c.agentPath(agentID)
	if err != nil {
		return nil, err
	}
	u, err := c.cellURL(path)
	if err != nil {
		return nil, err
	}
	raw, resp, err := c.do(ctx, requestOptions{method: http.MethodGet, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out Agent
	if err := decodeInto(http.MethodGet, u, "Agent", raw, &out); err != nil {
		return nil, err
	}
	etag := strings.TrimSpace(out.ETag)
	if etag == "" && resp != nil {
		etag = strings.TrimSpace(resp.Header.Get("ETag"))
	}
	if etag == "" {
		return nil, fmt.Errorf("bridge: GET %s returned no etag; If-Match would fail", u)
	}
	out.ETag = etag
	c.lastETag = etag
	return &out, nil
}

// ---------------------------------------------------------------------------
// PUT {cell} /{host}/{project}/agents/{id}  (full replace)
// ---------------------------------------------------------------------------

// ReplaceAgent performs the full configuration replace:
//
//	PUT {cell}/{host}/{project}/agents/{id}   If-Match: <json etag>
//	body: AgentReplace
//
// If-Match is mandatory: the route answers 428 without it and 412 with a stale
// value, and both are surfaced as typed errors. The replacement is validated
// locally first, including the 16384-character ceiling on instructions, so an
// oversized prompt is rejected before the credential is used.
//
// When ifMatch is empty the etag cached by ReadAgent is used.
func (c *Client) ReplaceAgent(ctx context.Context, agentID, ifMatch string, body *AgentReplace) (*Agent, error) {
	if body == nil {
		return nil, fmt.Errorf("%w: nil AgentReplace body", ErrInvalidConfig)
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ifMatch) == "" {
		ifMatch = c.lastETag
	}
	if strings.TrimSpace(ifMatch) == "" {
		return nil, fmt.Errorf("%w: If-Match etag is required; call ReadAgent first", ErrInvalidConfig)
	}
	path, err := c.agentPath(agentID)
	if err != nil {
		return nil, err
	}
	u, err := c.cellURL(path)
	if err != nil {
		return nil, err
	}
	raw, resp, err := c.do(ctx, requestOptions{
		method:  http.MethodPut,
		url:     u,
		body:    body,
		ifMatch: strings.TrimSpace(ifMatch),
		accept:  "application/json",
	})
	if err != nil {
		return nil, err
	}
	var out Agent
	if err := decodeInto(http.MethodPut, u, "Agent", raw, &out); err != nil {
		return nil, err
	}
	etag := strings.TrimSpace(out.ETag)
	if etag == "" && resp != nil {
		etag = strings.TrimSpace(resp.Header.Get("ETag"))
	}
	if etag != "" {
		out.ETag = etag
		c.lastETag = etag
	}
	return &out, nil
}
