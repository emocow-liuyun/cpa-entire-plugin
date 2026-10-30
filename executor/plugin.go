package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// RPC methods this plugin implements. Only the executor capability family plus
// the lifecycle and model-registration methods the host always sends are
// handled; every other method is an explicit unknown-method error.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginShutdown    = "plugin.shutdown"

	methodModelRegister = "model.register"
	methodModelStatic   = "model.static"

	methodAuthParse   = "auth.parse"
	methodAuthRefresh = "auth.refresh"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"
	methodExecutorHTTPRequest   = "executor.http_request"

	methodHostHTTPDo = "host.http.do"
	methodHostLog    = "host.log"
)

// pluginState is the process-wide runtime state.
type pluginState struct {
	mu     sync.RWMutex
	config runtimeConfig
	// dispatcher is the execution seam. Production keeps it nil, which means
	// "no real dispatch implementation is installed" and forces the 503 path.
	dispatcher Dispatcher
	// lastConfigErr records the most recent configuration decode failure.
	lastConfigErr string
}

// Dispatcher is the seam a real Entire dispatch implementation plugs into.
//
// It exists so the fail-closed behaviour is testable and so a future
// implementation can be installed deliberately, in one place, under the same
// gates. A nil Dispatcher is the shipped state: every admitted request ends in
// ErrNotImplemented and returns HTTP 503 rather than fabricated content.
type Dispatcher interface {
	// Dispatch performs one non-streaming review run and returns the assistant text.
	Dispatch(ctx context.Context, call DispatcherContext, prompt string) (DispatcherResult, error)
}

// DispatcherContext carries the resolved call context for a dispatch.
type DispatcherContext struct {
	Model   string
	Harness string
	AuthID  string
	// HostClient performs upstream HTTP through the host transport policy.
	HostClient HostHTTPClient
	Config     runtimeConfig
	Credential credential
}

// DispatcherResult is the outcome of one review run.
type DispatcherResult struct {
	Text      string
	Truncated bool
	RunID     string
	// Usage mirrors the OpenAI usage object when the upstream reported it.
	Usage map[string]int64
}

// errNotImplemented marks the shipped state: no dispatcher installed.
var errNotImplemented = newPluginError(codeNotImplemented,
	"Entire execution is fail-closed in this build: no real dispatch implementation is installed, so nothing was sent upstream and no result can be reported. Configure the gates and install a dispatcher deliberately before expecting a response.",
	http.StatusServiceUnavailable)

var state = &pluginState{
	config: runtimeConfig{
		CoreBaseURL:  defaultCoreBaseURL,
		CellBaseURL:  defaultCellBaseURL,
		AllowedHosts: append([]string(nil), defaultAllowedHosts...),
	},
}

var shutdownFlag atomic.Bool

// shutdownRuntime releases per-instance state when the host unloads the plugin.
func shutdownRuntime() {
	shutdownFlag.Store(true)
	state.mu.Lock()
	state.config = runtimeConfig{
		CoreBaseURL:  defaultCoreBaseURL,
		CellBaseURL:  defaultCellBaseURL,
		AllowedHosts: append([]string(nil), defaultAllowedHosts...),
	}
	state.dispatcher = nil
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

// lastConfigError reports the most recent configuration decode failure.
func lastConfigError() string {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.lastConfigErr
}

// currentDispatcher returns the installed dispatcher, if any. Used by tests.
func currentDispatcher() Dispatcher {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.dispatcher
}

// setDispatcher installs an execution seam. nil restores the fail-closed default.
func setDispatcher(dispatcher Dispatcher) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.dispatcher = dispatcher
}

// lifecycleRequest is the payload of plugin.register and plugin.reconfigure.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// executorRequest mirrors pluginapi.ExecutorRequest as the host marshals it.
// Exported field names carry no JSON tags on the host side, so the wire names
// are the Go field names; unknown fields are ignored by encoding/json.
type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         http.Header         `json:"Headers"`
	Query           map[string][]string `json:"Query"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	Metadata        map[string]any      `json:"Metadata"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`
	// StreamID and HostCallbackID are added by the RPC layer for stream calls.
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
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

// registrationCapabilities declares the executor capability and nothing else.
// In particular quota_provider is false: the quota read path lives in ../quota
// and this module never implements quota.fetch or quota.reset.
type registrationCapabilities struct {
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	QuotaProvider         bool     `json:"quota_provider"`
}

// identifierResult is the executor.identifier result.
type identifierResult struct {
	Identifier string `json:"identifier"`
}

// modelInfo mirrors pluginapi.ModelInfo field names for model.register/model.static.
type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	Created                    int64    `json:"Created"`
	OwnedBy                    string   `json:"OwnedBy"`
	Type                       string   `json:"Type"`
	DisplayName                string   `json:"DisplayName"`
	Name                       string   `json:"Name"`
	Version                    string   `json:"Version"`
	Description                string   `json:"Description"`
	ContextLength              int64    `json:"ContextLength"`
	MaxCompletionTokens        int64    `json:"MaxCompletionTokens"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods"`
	SupportedParameters        []string `json:"SupportedParameters"`
	SupportedInputModalities   []string `json:"SupportedInputModalities"`
	SupportedOutputModalities  []string `json:"SupportedOutputModalities"`
	UserDefined                bool     `json:"UserDefined"`
}

// modelResponse is the model.register / model.static result.
type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

// registrationConfigFields documents the operator-facing configuration keys.
func registrationConfigFields() []registrationConfigField {
	return []registrationConfigField{
		{Name: "token", Type: "string", Description: "Entire bearer token supplied by the account owner. Never logged. Empty by default."},
		{Name: "token-env", Type: "string", Description: "Environment variable holding the bearer token; ignored when token is set."},
		{Name: "core-base-url", Type: "string", Default: defaultCoreBaseURL, Description: "Core origin serving /me/billing; its host must pass the allow-list."},
		{Name: "cell-base-url", Type: "string", Default: defaultCellBaseURL, Description: "Cell origin serving project agent and run routes; its host must pass the allow-list."},
		{Name: "allow-domains", Type: "array", Description: "Domain allow-list for outbound calls. Defaults to the Entire control-plane hosts."},
		{Name: "project-host", Type: "string", Description: "Project host slug (gh or et)."},
		{Name: "project", Type: "string", Description: "Canonical public project name."},
		{Name: "agent-id", Type: "string", Description: "Entire agent addressed by every run."},
		{Name: "trail-id", Type: "string", Description: "Trail id used in RunTargetInput."},
		{Name: "change-id", Type: "string", Description: "Change id used in RunTargetInput."},
		{Name: "execution-enabled", Type: "boolean", Default: false, Description: "Master switch. False keeps the executor fail-closed: execute returns 503 and nothing is sent upstream."},
		{Name: "auth-subject-verified", Type: "boolean", Default: false, Description: "Operator confirms the authenticated subject is the intended billing party."},
		{Name: "billing-limit-enforced", Type: "boolean", Default: false, Description: "Operator guarantees a hard spend limit covers each dispatch."},
		{Name: "allow-unbounded-spend", Type: "boolean", Default: false, Description: "Operator explicitly accepts that Entire publishes no hard per-request cost ceiling. Required for any dispatch."},
	}
}

// buildRegistration assembles the registration result.
func buildRegistration() registrationResult {
	return registrationResult{
		SchemaVersion: schemaVersion,
		Metadata: registrationMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			Logo:             "",
			ConfigFields:     registrationConfigFields(),
		},
		Capabilities: registrationCapabilities{
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			QuotaProvider:         false,
		},
	}
}

// handleMethod dispatches one RPC. It returns the marshaled success envelope or
// a plugin error carrying an HTTP status.
func handleMethod(method string, rawRequest []byte) ([]byte, *pluginError) {
	if shutdownFlag.Load() {
		return nil, newPluginError(codePluginError, "plugin is shut down", 0)
	}
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return handleLifecycle(method, rawRequest)
	case methodPluginQuiesce, methodPluginShutdown:
		return encodeOKEnvelope(struct{}{}), nil
	case methodExecutorIdentifier:
		return encodeOKEnvelope(identifierResult{Identifier: providerIdentifier}), nil
	case methodModelRegister, methodModelStatic:
		return encodeOKEnvelope(modelResponse{Provider: providerIdentifier, Models: modelCatalog()}), nil
	case methodAuthParse:
		return handleAuthParse(rawRequest)
	case methodAuthRefresh:
		return handleAuthRefresh(rawRequest)
	case methodExecutorExecute:
		return handleExecute(rawRequest)
	case methodExecutorExecuteStream:
		return nil, newPluginError(codeStreamRejected,
			"streaming is not implemented: this executor serves non-streaming chat-completions only. Request the model without stream=true.",
			http.StatusBadRequest)
	case methodExecutorCountTokens:
		return handleCountTokens(rawRequest)
	case methodExecutorHTTPRequest:
		return nil, newPluginError(codeNotImplemented,
			"executor.http_request is not implemented: this executor does not proxy raw provider HTTP. Capability is not advertised.", 0)
	default:
		return nil, newPluginError(codeUnknownMethod,
			"unknown method: "+trimForMessage(method), httpStatusForMethod(method))
	}
}

// httpStatusForMethod maps an unknown method onto a status the host can surface.
func httpStatusForMethod(method string) int {
	if strings.HasPrefix(method, "executor.") {
		return http.StatusNotImplemented
	}
	return 0
}

// handleLifecycle parses config_yaml and installs it.
func handleLifecycle(method string, rawRequest []byte) ([]byte, *pluginError) {
	var req lifecycleRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, method+" payload is not valid JSON", 0)
		}
	}
	cfg, errParse := parseConfig(req.ConfigYAML)
	// Register the credential before anything else so redaction is armed for the
	// whole lifetime of this instance.
	if cfg.Token != "" {
		registerSecret(cfg.Token)
	}
	setConfig(cfg, errParse)
	if errParse != nil {
		// A bad configuration never aborts registration — the plugin stays loaded
		// and fails closed at execute time — but it must not leave a half-applied
		// credential behind either.
		cfgCopy := cfg
		cfgCopy.Token = ""
		setConfig(cfgCopy, errParse)
	}
	logConfigState()
	return encodeOKEnvelope(buildRegistration()), nil
}

// logConfigState reports the effective gate state through the host logger so an
// operator can see fail-closed mode without reading the plugin binary.
func logConfigState() {
	cfg := currentConfig()
	if cfgErr := lastConfigError(); cfgErr != "" {
		emitHostLog("warn", "entire-executor: configuration rejected: "+cfgErr, nil)
	}
	level := "info"
	if cfg.ExecutionEnabled {
		level = "warn"
	}
	emitHostLog(level, "entire-executor: "+describeConfig(cfg), nil)
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
