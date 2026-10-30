package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// RPC methods this plugin implements. Only the quota capability family plus the
// lifecycle methods the host always sends are handled; everything else is an error.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginShutdown    = "plugin.shutdown"

	methodQuotaIdentifier = "quota.identifier"
	methodQuotaDescribe   = "quota.describe"
	methodQuotaFetch      = "quota.fetch"
	methodQuotaReset      = "quota.reset"

	methodHostHTTPDo = "host.http.do"
	methodHostLog    = "host.log"
)

// pluginState is the process-wide runtime state.
type pluginState struct {
	mu     sync.RWMutex
	config runtimeConfig
	// billingClient is the transport seam; production installs the host callback.
	clientOverride billingHTTPClient
	lastConfigErr  string
}

var state = &pluginState{
	config: runtimeConfig{
		BaseURL:               defaultBaseURL,
		AllowedHosts:          append([]string(nil), defaultAllowedHosts...),
		RequireTopUpOnFailure: true,
		TimeoutSeconds:        defaultTimeoutSecond,
	},
}

var shutdownFlag atomic.Bool

// shutdownRuntime releases per-instance state when the host unloads the plugin.
func shutdownRuntime() {
	shutdownFlag.Store(true)
	state.mu.Lock()
	state.config = runtimeConfig{
		BaseURL:               defaultBaseURL,
		AllowedHosts:          append([]string(nil), defaultAllowedHosts...),
		RequireTopUpOnFailure: true,
		TimeoutSeconds:        defaultTimeoutSecond,
	}
	state.mu.Unlock()
}

// currentConfig snapshots the active configuration.
func currentConfig() runtimeConfig {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.config
}

// setConfig installs a parsed configuration.
func setConfig(cfg runtimeConfig, errParse error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	if errParse != nil {
		state.lastConfigErr = redactSecrets(errParse.Error())
		return
	}
	state.lastConfigErr = ""
}

// lastConfigError reports the most recent configuration decode failure, if any.
func lastConfigError() string {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.lastConfigErr
}

// billingTransport returns the transport used for billing reads.
// In production it is the host callback bound to the current fetch request;
// tests override it through setBillingClient.
func billingTransport(hostCallbackID string) billingHTTPClient {
	state.mu.RLock()
	override := state.clientOverride
	state.mu.RUnlock()
	if override != nil {
		return override
	}
	return hostBillingClient{hostCallbackID: hostCallbackID}
}

// setBillingClient installs a transport override (tests and the CLI harness).
func setBillingClient(client billingHTTPClient) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.clientOverride = client
}

// lifecycleRequest is the payload of plugin.register and plugin.reconfigure.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// quotaFetchRequest is the host payload for quota.fetch, including the
// callback handle used to reach host.http.do.
type quotaFetchRequest struct {
	quotaFetchContext
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// quotaResetRequest is the host payload for quota.reset.
type quotaResetRequest struct {
	quotaFetchContext
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// quotaDescribeRequest is the host payload for quota.describe.
type quotaDescribeRequest struct {
	Plugin struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	} `json:"plugin"`
}

// registrationResult is the plugin.register / plugin.reconfigure result.
type registrationResult struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      registrationMetadata     `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

// registrationMetadata mirrors pluginapi.Metadata field names.
type registrationMetadata struct {
	Name             string                    `json:"Name"`
	Version          string                    `json:"Version"`
	Author           string                    `json:"Author"`
	GitHubRepository string                    `json:"GitHubRepository"`
	Logo             string                    `json:"Logo"`
	ConfigFields     []registrationConfigField `json:"ConfigFields"`
}

// registrationConfigField mirrors pluginapi.ConfigField field names.
type registrationConfigField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Default     any    `json:"Default,omitempty"`
	Description string `json:"Description"`
}

// registrationCapabilities declares only the quota provider capability.
type registrationCapabilities struct {
	QuotaProvider bool `json:"quota_provider"`
	AuthProvider  bool `json:"auth_provider"`
	ModelProvider bool `json:"model_provider"`
}

// identifierResult is the quota.identifier result.
type identifierResult struct {
	Identifier string `json:"identifier"`
}

// describeResult is the quota.describe result.
type describeResult struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset"`
}

// resetResult is the quota.reset result.
type resetResult struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// registrationConfigFields documents the operator-facing configuration keys.
func registrationConfigFields() []registrationConfigField {
	return []registrationConfigField{
		{Name: "base-url", Type: "string", Default: defaultBaseURL, Description: "Entire control-plane base URL; its host must pass the allow-list."},
		{Name: "allow-domains", Type: "array", Description: "Domain allow-list for billing calls. Defaults to the Entire control-plane hosts."},
		{Name: "org-id", Type: "string", Description: "Optional organization ULID; switches the read from /me/billing to /orgs/{org_id}/billing."},
		{Name: "require-top-up-on-failure", Type: "boolean", Default: true, Description: "Fail closed when an automatic top-up block is missing, unreadable or failed."},
		{Name: "timeout-seconds", Type: "integer", Default: defaultTimeoutSecond, Description: "Per-call timeout in seconds (1-60)."},
	}
}

// buildRegistration assembles the registration result.
func buildRegistration(requested uint32) registrationResult {
	negotiated := schemaVersion
	if requested == 0 {
		negotiated = 1
	} else if requested < negotiated {
		negotiated = requested
	}
	return registrationResult{
		SchemaVersion: negotiated,
		Metadata: registrationMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			Logo:             "",
			ConfigFields:     registrationConfigFields(),
		},
		Capabilities: registrationCapabilities{QuotaProvider: true, AuthProvider: true, ModelProvider: true},
	}
}

// handleMethod dispatches one RPC. It returns the marshaled success envelope or a plugin error.
func handleMethod(method string, rawRequest []byte) ([]byte, *pluginError) {
	if shutdownFlag.Load() {
		return nil, newPluginError(codePluginError, "plugin is shut down", 0)
	}
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return handleLifecycle(method, rawRequest)
	case methodPluginQuiesce:
		return encodeOKEnvelope(struct{}{}), nil
	case methodPluginShutdown:
		return encodeOKEnvelope(struct{}{}), nil
	case methodQuotaIdentifier:
		return encodeOKEnvelope(identifierResult{Identifier: providerIdentifier}), nil
	case methodQuotaDescribe:
		return handleDescribe(rawRequest)
	case methodQuotaFetch:
		return handleFetch(rawRequest)
	case methodQuotaReset:
		return handleReset(rawRequest)
	case "model.static", "model.for_auth":
		return handleModels(method, rawRequest)
	case methodAuthIdentifier, methodAuthParse, methodAuthLoginStart, methodAuthLoginPoll, methodAuthRefresh:
		return handleAuthMethod(method, rawRequest)
	default:
		return nil, newPluginError(codeUnknownMethod,
			"unknown method: "+trimForMessage(method), httpStatusForMethod(method))
	}
}

// httpStatusForMethod maps an unknown method onto a status the host can surface.
func httpStatusForMethod(method string) int {
	if strings.HasPrefix(method, "quota.") {
		return 500
	}
	return 0
}

// handleLifecycle parses config_yaml and installs it.
func handleLifecycle(method string, rawRequest []byte) ([]byte, *pluginError) {
	var req lifecycleRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest,
				method+" payload is not valid JSON", 0)
		}
	}
	cfg, errParse := parseConfig(req.ConfigYAML)
	setConfig(cfg, errParse)
	// Configuration faults never abort registration: the plugin stays loaded
	// and fails closed at fetch time instead of leaving the host without a provider.
	logConfigError()
	return encodeOKEnvelope(buildRegistration(req.SchemaVersion)), nil
}

// logConfigError reports a rejected configuration through the host logger.
func logConfigError() {
	if cfgErr := lastConfigError(); cfgErr != "" {
		emitHostLog("warn", "entire-quota: configuration rejected: "+cfgErr, map[string]any{
			"plugin": pluginName,
		})
	}
}

// emitHostLog sends one host.log RPC when the host callback API is available.
func emitHostLog(level, message string, fields map[string]any) {
	payload := map[string]any{
		"level":   level,
		"message": redactSecrets(message),
	}
	if len(fields) > 0 {
		safe := make(map[string]any, len(fields))
		for key, value := range fields {
			safe[key] = redactSecrets(fmt.Sprint(value))
		}
		payload["fields"] = safe
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return
	}
	_, _ = callHost(methodHostLog, raw)
}

// handleDescribe reports the provider capability metadata.
func handleDescribe(rawRequest []byte) ([]byte, *pluginError) {
	var req quotaDescribeRequest
	if len(rawRequest) > 0 {
		_ = json.Unmarshal(rawRequest, &req)
	}
	_ = req
	return encodeOKEnvelope(describeResult{
		SupportedProviders: []string{providerIdentifier},
		DisplayName:        pluginDisplayName,
		SupportsReset:      false,
	}), nil
}

// handleFetch resolves credentials, reads billing, and maps the quota.
func handleFetch(rawRequest []byte) ([]byte, *pluginError) {
	var req quotaFetchRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "quota.fetch payload is not valid JSON", 0)
		}
	}
	cfg := currentConfig()
	if cfgErr := lastConfigError(); cfgErr != "" {
		return nil, newPluginError(codePluginError,
			"plugin configuration was rejected at registration: "+cfgErr, 0)
	}
	if len(req.StorageJSON) == 0 && len(req.Metadata) == 0 {
		return nil, newPluginError(codeMissingCredential,
			"no Entire bearer token available for this credential: set entire_token / access_token / token / bearer_token in its auth storage or metadata", 0)
	}

	ctx := context.Background()
	cred, errCredential := resolveCredential(ctx, cfg, req.quotaFetchContext)
	if errCredential != nil {
		var plugErr *pluginError
		if asPluginError(errCredential, &plugErr) {
			return nil, plugErr
		}
		return nil, newPluginError(codeMissingCredential, errCredential.Error(), 0)
	}

	status, _, errRead := readBilling(ctx, cfg, cred, billingTransport(req.HostCallbackID))
	if errRead != nil {
		var plugErr *pluginError
		if asPluginError(errRead, &plugErr) {
			emitHostLog("warn", "entire-quota: billing read failed for provider "+req.Provider, map[string]any{
				"code":      plugErr.Code,
				"auth_id":   req.AuthID,
				"auth_idx":  req.AuthIndex,
				"token_src": string(cred.Source),
			})
			return nil, plugErr
		}
		return nil, newPluginError(codePluginError, errRead.Error(), 0)
	}

	response, errMap := evaluateQuota(quotaEvaluation{
		Status:         status,
		RequireTopUp:   cfg.RequireTopUpOnFailure,
		ServerOffsetMs: status.serverOffsetMs,
	})
	if errMap != nil {
		var plugErr *pluginError
		if asPluginError(errMap, &plugErr) {
			emitHostLog("warn", "entire-quota: quota unknown for provider "+req.Provider, map[string]any{
				"code":     plugErr.Code,
				"auth_id":  req.AuthID,
				"auth_idx": req.AuthIndex,
			})
			return nil, plugErr
		}
		return nil, newPluginError(codePluginError, errMap.Error(), 0)
	}

	if total, okTotal := balanceTotal(status); okTotal && total == 0 {
		emitHostLog("info", "entire-quota: credit balance is zero", map[string]any{
			"auth_id":  req.AuthID,
			"auth_idx": req.AuthIndex,
		})
	}
	return encodeOKEnvelope(response), nil
}

// balanceTotal returns the reported total credit in cents when present.
func balanceTotal(status billingStatus) (int64, bool) {
	if status.Balance == nil || status.Balance.TotalCents == nil {
		return 0, false
	}
	return *status.Balance.TotalCents, true
}

// handleReset rejects resets: Entire exposes no reset through the read-only path.
func handleReset(rawRequest []byte) ([]byte, *pluginError) {
	var req quotaResetRequest
	if len(rawRequest) > 0 {
		_ = json.Unmarshal(rawRequest, &req)
	}
	return encodeOKEnvelope(resetResult{
		Success: false,
		Message: "Entire has no quota reset; credit is spent by Agent runs and refilled by purchase or automatic top-up",
	}), nil
}

// asPluginError unwraps a *pluginError from an error chain.
func asPluginError(err error, target **pluginError) bool {
	if err == nil {
		return false
	}
	if direct, okDirect := err.(*pluginError); okDirect {
		*target = direct
		return true
	}
	type unwrapper interface{ Unwrap() error }
	for depth := 0; depth < 8; depth++ {
		unwrapper, okUnwrap := err.(unwrapper)
		if !okUnwrap {
			return false
		}
		err = unwrapper.Unwrap()
		if err == nil {
			return false
		}
		if direct, okDirect := err.(*pluginError); okDirect {
			*target = direct
			return true
		}
	}
	return false
}
