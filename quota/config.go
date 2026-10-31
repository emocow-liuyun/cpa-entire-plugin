package main

import (
	"bytes"
	"strings"

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

	return runtimeConfig{
		Enabled:               parsed.Enabled,
		Priority:              parsed.Priority,
		BaseURL:               baseURL,
		AllowedHosts:          allowed,
		RequireTopUpOnFailure: requireTopUp,
		TimeoutSeconds:        timeout,
		OrgID:                 strings.TrimSpace(parsed.OrgID),
		AutoRenew:             parsed.AutoRenew,
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
