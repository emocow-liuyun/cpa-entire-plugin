package main

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultAllowedHosts restricts every outbound call to Entire control-plane
// hosts. The executor never talks to anything else.
var defaultAllowedHosts = []string{
	"entire.io",
	"console.entire.io",
	"us.console.entire.io",
	"eu.console.entire.io",
	"api.entire.io",
}

const (
	defaultCoreBaseURL = "https://us.console.entire.io"
	defaultCellBaseURL = "https://aws-us-east-2.api.entire.io"
	apiVersion         = "/api/v1"
)

// pluginConfig is the operator-facing YAML block under
// plugins.configs.cpa-entire-executor.
//
// The three execution gates are deliberately separate and all default to
// false. Real dispatch requires all three to be true *and* an authenticated
// subject *and* a readable billing spend limit. Since Entire exposes no hard
// per-request cost ceiling, executionAllowed() additionally requires
// allow_unbounded_spend, which this module never sets on the operator's behalf.
type pluginConfig struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`

	// Token is the Entire bearer token supplied by the account owner. Never logged.
	Token string `yaml:"token"`
	// TokenEnv names an environment variable holding the bearer; used when Token is empty.
	TokenEnv string `yaml:"token-env"`

	// CoreBaseURL serves /me/billing.
	CoreBaseURL string `yaml:"core-base-url"`
	// CellBaseURL serves the project-scoped agent and run routes.
	CellBaseURL string `yaml:"cell-base-url"`
	// AllowDomains replaces the default host allow-list when non-empty.
	AllowDomains []string `yaml:"allow-domains"`

	// ProjectHost is the project host slug (gh or et); Project is the canonical
	// public project name. Both are needed to address an agent route.
	ProjectHost string `yaml:"project-host"`
	Project     string `yaml:"project"`
	// AgentID is the agent whose configuration and runs are addressed.
	AgentID string `yaml:"agent-id"`
	// TrailID and ChangeID make up RunTargetInput; both are required for a run.
	TrailID  string `yaml:"trail-id"`
	ChangeID string `yaml:"change-id"`

	// --- execution gates (all default false) ---

	// ExecutionEnabled is the master switch. False keeps the executor in strict
	// fail-closed mode: every executor.execute returns 503 and no upstream call
	// is attempted.
	ExecutionEnabled bool `yaml:"execution-enabled"`
	// AuthSubjectVerified records that the operator has verified the
	// authenticated subject (the bearer owner) is the intended billing party.
	AuthSubjectVerified bool `yaml:"auth-subject-verified"`
	// BillingLimitEnforced records that the operator guarantees a hard spend
	// limit covers the dispatch. Entire publishes no hard per-request ceiling,
	// so this alone is not sufficient; see AllowUnboundedSpend.
	BillingLimitEnforced bool `yaml:"billing-limit-enforced"`
	// AllowUnboundedSpend acknowledges that Entire has no hard cost ceiling and
	// that the operator accepts that risk explicitly. Never defaulted to true.
	AllowUnboundedSpend bool `yaml:"allow-unbounded-spend"`
}

// runtimeConfig is the normalized configuration.
type runtimeConfig struct {
	Enabled  bool
	Priority int

	Token    string
	TokenEnv string

	CoreBaseURL  string
	CellBaseURL  string
	AllowedHosts []string

	ProjectHost string
	Project     string
	AgentID     string
	TrailID     string
	ChangeID    string

	ExecutionEnabled     bool
	AuthSubjectVerified  bool
	BillingLimitEnforced bool
	AllowUnboundedSpend  bool
}

// executionAllowed reports whether a real dispatch may be attempted.
//
// This is the single source of truth for the fail-closed guarantee. It returns
// the first unmet gate as a refusal so the caller can surface a precise reason
// instead of a generic 503.
//
// AllowUnboundedSpend is required because Entire has no hard per-request cost
// limit: a run can consume arbitrary credit and the control plane has no
// pre-dispatch ceiling the plugin can enforce. With unbounded cost an
// "authenticated + billing-limited" dispatch would be a dispatch with a
// guarantee the upstream cannot provide, so the executor stays closed.
func (c runtimeConfig) executionAllowed() *pluginError {
	if !c.ExecutionEnabled {
		return newPluginError(codeExecutionDisabled,
			"Entire executor is fail-closed: execution-enabled is false", 503)
	}
	if strings.TrimSpace(c.CoreBaseURL) == "" || strings.TrimSpace(c.CellBaseURL) == "" {
		return newPluginError(codeExecutionDisabled, "Entire executor is not configured", 503)
	}
	if !c.AuthSubjectVerified {
		return newPluginError(codeExecutionInsecure,
			"Entire executor refused: the authenticated subject has not been verified by the operator (auth-subject-verified is false)", 503)
	}
	if !c.BillingLimitEnforced {
		return newPluginError(codeBillingUnavailable,
			"Entire executor refused: no guaranteed billing spend limit covers this dispatch (billing-limit-enforced is false)", 503)
	}
	if !c.AllowUnboundedSpend {
		return newPluginError(codeBillingUnavailable,
			"Entire executor refused: Entire exposes no hard per-request cost ceiling, so a dispatch cannot be bounded (allow-unbounded-spend is false)", 503)
	}
	return nil
}

// targetConfigured reports whether a run target is addressable.
func (c runtimeConfig) targetConfigured() bool {
	return strings.TrimSpace(c.ProjectHost) != "" &&
		strings.TrimSpace(c.Project) != "" &&
		strings.TrimSpace(c.AgentID) != "" &&
		strings.TrimSpace(c.TrailID) != "" &&
		strings.TrimSpace(c.ChangeID) != ""
}

// parseConfig decodes and normalizes the runtime YAML. A malformed document is
// reported but every field falls back to its safe default, so a bad config can
// never widen permissions.
func parseConfig(raw []byte) (runtimeConfig, error) {
	var parsed pluginConfig
	var errParse error
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 {
		if errUnmarshal := yaml.Unmarshal(trimmed, &parsed); errUnmarshal != nil {
			errParse = errUnmarshal
			parsed = pluginConfig{}
		}
	}

	allowed := normalizeHostList(parsed.AllowDomains)
	if len(allowed) == 0 {
		allowed = append([]string(nil), defaultAllowedHosts...)
	}

	coreBase := strings.TrimRight(strings.TrimSpace(parsed.CoreBaseURL), "/")
	if coreBase == "" {
		coreBase = defaultCoreBaseURL
	}
	cellBase := strings.TrimRight(strings.TrimSpace(parsed.CellBaseURL), "/")
	if cellBase == "" {
		cellBase = defaultCellBaseURL
	}

	return runtimeConfig{
		Enabled:              parsed.Enabled,
		Priority:             parsed.Priority,
		Token:                strings.TrimSpace(parsed.Token),
		TokenEnv:             strings.TrimSpace(parsed.TokenEnv),
		CoreBaseURL:          coreBase,
		CellBaseURL:          cellBase,
		AllowedHosts:         allowed,
		ProjectHost:          strings.TrimSpace(parsed.ProjectHost),
		Project:              strings.TrimSpace(parsed.Project),
		AgentID:              strings.TrimSpace(parsed.AgentID),
		TrailID:              strings.TrimSpace(parsed.TrailID),
		ChangeID:             strings.TrimSpace(parsed.ChangeID),
		ExecutionEnabled:     parsed.ExecutionEnabled,
		AuthSubjectVerified:  parsed.AuthSubjectVerified,
		BillingLimitEnforced: parsed.BillingLimitEnforced,
		AllowUnboundedSpend:  parsed.AllowUnboundedSpend,
	}, errParse
}

// normalizeHostList lowercases, trims and de-duplicates an allow-list.
func normalizeHostList(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		host := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), ".")))
		if host == "" {
			continue
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// hostAllowed reports whether host matches an allow-list entry exactly or as a
// subdomain of one.
func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, entry := range allowed {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// describeConfig renders the configuration without the credential.
func describeConfig(c runtimeConfig) string {
	token := "unset"
	if c.Token != "" || c.TokenEnv != "" {
		token = "set"
	}
	return fmt.Sprintf("core=%s cell=%s project=%s/%s agent=%s execution=%t subject_verified=%t billing_limit=%t unbounded=%t token=%s",
		c.CoreBaseURL, c.CellBaseURL, c.ProjectHost, c.Project, c.AgentID,
		c.ExecutionEnabled, c.AuthSubjectVerified, c.BillingLimitEnforced, c.AllowUnboundedSpend, token)
}
