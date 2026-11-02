package main

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginConfig is the parsed runtime configuration for this plugin instance.
// Values come from plugins.configs.<plugin-id> in the host configuration file
// (or from the YAML pushed through plugin.reconfigure).
type pluginConfig struct {
	// Enabled mirrors the host-owned toggle. It is surfaced for diagnostics only;
	// the host does not call into a disabled plugin instance.
	Enabled bool `yaml:"enabled"`
	// Priority mirrors the host-owned ordering field.
	Priority int `yaml:"priority"`
	// BaseURL overrides the billing control-plane base URL. The host is still
	// validated against the domain allow-list below.
	BaseURL string `yaml:"base-url"`
	// AllowDomains replaces the default allow-list when non-empty.
	AllowDomains []string `yaml:"allow-domains"`
	// RequireTopUpOnFailure makes a missing or unreadable automatic-top-up block
	// fail closed. Default true.
	RequireTopUpOnFailure *bool `yaml:"require-top-up-on-failure"`
	// TimeoutSeconds bounds one billing round trip. Default 15, capped at 60.
	TimeoutSeconds int `yaml:"timeout-seconds"`
	// OrgID switches the fetch target from /me/billing to /orgs/{org_id}/billing.
	OrgID string `yaml:"org-id"`
	// AutoRenew enables the automatic credential renewal that quota.fetch runs
	// when the stored access token is expired or the control plane answers 401.
	// Default true.
	AutoRenew *bool `yaml:"auto-renew"`

	// ExecutionEnabled is the master switch for relaying chat requests to
	// Entire tasks. Default false: catalogued models stay visible but refuse.
	ExecutionEnabled bool `yaml:"execution-enabled"`
	// CellBaseURL is the regional origin that serves the project task routes.
	CellBaseURL string `yaml:"cell-base-url"`
	// ProjectHost / Project address the project ("gh" + "owner").
	ProjectHost string `yaml:"project-host"`
	Project     string `yaml:"project"`
	// RepositoryID is the repository a relay run is pinned to.
	RepositoryID string `yaml:"repository-id"`
	// EnvironmentID is the reusable sandbox environment for relay runs.
	EnvironmentID string `yaml:"environment-id"`
	// TrailID / ChangeID / HeadSHA pin the run target.
	TrailID  string `yaml:"trail-id"`
	ChangeID string `yaml:"change-id"`
	HeadSHA  string `yaml:"head-sha"`
	// ReserveCents is the balance that must remain untouched before a dispatch.
	ReserveCents int64 `yaml:"reserve-cents"`
	// RunTimeoutMS bounds one task run.
	RunTimeoutMS int64 `yaml:"run-timeout-ms"`
}

// runtimeConfig is the normalized, validated configuration.
type runtimeConfig struct {
	Enabled               bool
	Priority              int
	Token                 string
	TokenEnv              string
	BaseURL               string
	AllowedHosts          []string
	RequireTopUpOnFailure bool
	TimeoutSeconds        int
	OrgID                 string
	AutoRenew             *bool
	ExecutionEnabled      bool
	CellBaseURL           string
	ProjectHost           string
	Project               string
	RepositoryID          string
	EnvironmentID         string
	TrailID               string
	ChangeID              string
	HeadSHA               string
	ReserveCents          int64
	RunTimeoutMS          int64
}

const (
	// maxInstructions is the AgentCreate.instructions maxLength (characters).
	maxInstructions = 16384
	// defaultReserveCents keeps a buffer of real credit before any dispatch.
	defaultReserveCents = 100
	// defaultRunTimeoutMS bounds one relay run.
	defaultRunTimeoutMS = 180000
)

// executionGate reports the first unmet precondition for relaying a chat request.
func (c runtimeConfig) executionGate() *pluginError {
	if !c.ExecutionEnabled {
		return newPluginError(codeExecutionDisabled,
			"Entire inference relay is disabled: set execution-enabled after configuring the project target", http.StatusServiceUnavailable)
	}
	missing := []string{}
	if strings.TrimSpace(c.CellBaseURL) == "" {
		missing = append(missing, "cell-base-url")
	}
	if strings.TrimSpace(c.ProjectHost) == "" || strings.TrimSpace(c.Project) == "" {
		missing = append(missing, "project-host/project")
	}
	if strings.TrimSpace(c.RepositoryID) == "" {
		missing = append(missing, "repository-id")
	}
	if strings.TrimSpace(c.TrailID) == "" || strings.TrimSpace(c.ChangeID) == "" {
		missing = append(missing, "trail-id/change-id")
	}
	if len(missing) > 0 {
		return newPluginError(codeExecutionDisabled,
			"Entire inference relay target is incomplete: "+strings.Join(missing, ", "), http.StatusServiceUnavailable)
	}
	return nil
}

// executionTimeout returns the per-request deadline for one relayed run.
func (c runtimeConfig) executionTimeout() time.Duration {
	ms := c.RunTimeoutMS
	if ms <= 0 {
		ms = defaultRunTimeoutMS
	}
	if ms > 1800000 {
		ms = 1800000
	}
	return time.Duration(ms) * time.Millisecond
}

const (
	defaultBaseURL       = "https://us.console.entire.io"
	defaultTimezoneSafe  = "UTC"
	defaultTimeoutSecond = 15
	maxTimeoutSeconds    = 60
)

// defaultAllowedHosts is the allow-list applied when the operator does not
// provide one. Only Entire control-plane hosts are reachable.
var defaultAllowedHosts = []string{
	"entire.io",
	"console.entire.io",
	"us.console.entire.io",
	"eu.console.entire.io",
	"api.entire.io",
}

// parseConfig decodes and normalizes the runtime YAML.
// A malformed document is still reported so the operator sees the failure,
// but every field falls back to its safe default.
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

	timeout := parsed.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSecond
	}
	if timeout > maxTimeoutSeconds {
		timeout = maxTimeoutSeconds
	}

	requireTopUp := true
	if parsed.RequireTopUpOnFailure != nil {
		requireTopUp = *parsed.RequireTopUpOnFailure
	}

	baseURL := strings.TrimSpace(parsed.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	allowed := normalizeHostList(parsed.AllowDomains)
	if len(allowed) == 0 {
		allowed = append([]string(nil), defaultAllowedHosts...)
	}

	reserve := parsed.ReserveCents
	if reserve < 0 {
		reserve = 0
	}
	if reserve == 0 {
		reserve = defaultReserveCents
	}
	runTimeout := parsed.RunTimeoutMS
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeoutMS
	}

	return runtimeConfig{
		Enabled:               parsed.Enabled,
		Priority:              parsed.Priority,
		BaseURL:               baseURL,
		AllowedHosts:          allowed,
		RequireTopUpOnFailure: requireTopUp,
		TimeoutSeconds:        timeout,
		OrgID:                 strings.TrimSpace(parsed.OrgID),
		AutoRenew:             parsed.AutoRenew,
		ExecutionEnabled:      parsed.ExecutionEnabled,
		CellBaseURL:           strings.TrimRight(strings.TrimSpace(parsed.CellBaseURL), "/"),
		ProjectHost:           strings.TrimSpace(parsed.ProjectHost),
		Project:               strings.TrimSpace(parsed.Project),
		RepositoryID:          strings.TrimSpace(parsed.RepositoryID),
		EnvironmentID:         strings.TrimSpace(parsed.EnvironmentID),
		TrailID:               strings.TrimSpace(parsed.TrailID),
		ChangeID:              strings.TrimSpace(parsed.ChangeID),
		HeadSHA:               strings.TrimSpace(parsed.HeadSHA),
		ReserveCents:          reserve,
		RunTimeoutMS:          runTimeout,
	}, errParse
}

// normalizeHostList lowercases entries and drops empties and duplicates.
func normalizeHostList(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		host := strings.ToLower(strings.TrimSpace(entry))
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
