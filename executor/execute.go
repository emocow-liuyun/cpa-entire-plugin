package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// dispatchTimeout bounds one review run end to end.
const dispatchTimeout = 30 * time.Minute

// HostHTTPClient is the host transport seam (pluginapi.HostHTTPClient).
// Only Do is used; this executor never opens a stream.
type HostHTTPClient interface {
	Do(ctx context.Context, req HostHTTPRequest) (HostHTTPResponse, error)
}

// HostHTTPRequest mirrors pluginapi.HTTPRequest field names.
type HostHTTPRequest struct {
	Method  string      `json:"Method"`
	URL     string      `json:"URL"`
	Headers http.Header `json:"Headers"`
	Body    []byte      `json:"Body"`
}

// HostHTTPResponse mirrors pluginapi.HTTPResponse field names. Body is a Go
// []byte, so it crosses the RPC boundary as a base64 string.
type HostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// hostTransport is the production HostHTTPClient: it forwards one HTTP request
// through host.http.do with the host_callback_id of the current RPC.
type hostTransport struct {
	callbackID string
}

// Do performs one host-mediated HTTP request.
func (t hostTransport) Do(ctx context.Context, req HostHTTPRequest) (HostHTTPResponse, error) {
	if errCtx := ctx.Err(); errCtx != nil {
		return HostHTTPResponse{}, newPluginError(codeHostCallFailed, "host http call cancelled: "+errCtx.Error(), 0)
	}
	payload := map[string]any{
		"host_callback_id": strings.TrimSpace(t.callbackID),
		"method":           req.Method,
		"url":              req.URL,
		"headers":          req.Headers,
		"body":             req.Body,
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return HostHTTPResponse{}, newPluginError(codeHostCallFailed, "encode host http request: "+errMarshal.Error(), 0)
	}
	result, errCall := callHost(methodHostHTTPDo, raw)
	if errCall != nil {
		return HostHTTPResponse{}, errCall
	}
	var resp HostHTTPResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return HostHTTPResponse{}, newPluginError(codeHostCallFailed, "host.http.do returned malformed JSON: "+errUnmarshal.Error(), 0)
	}
	return resp, nil
}

// unknownTransport is installed when the host did not hand over a callback id.
// Any dispatch through it fails before a request is built.
type unknownTransport struct{}

func (unknownTransport) Do(context.Context, HostHTTPRequest) (HostHTTPResponse, error) {
	return HostHTTPResponse{}, newPluginError(codeHostUnavailable,
		"no host_callback_id on this executor call: upstream dispatch is unavailable", 0)
}

// hostClientFor resolves the transport for one executor call.
func hostClientFor(req executorRequest) HostHTTPClient {
	if strings.TrimSpace(req.HostCallbackID) == "" {
		return unknownTransport{}
	}
	return hostTransport{callbackID: req.HostCallbackID}
}

// handleExecute is the non-streaming executor entry point.
//
// Order is deliberate: cheap local refusals first (model namespace, payload
// shape, unsupported features), then the fail-closed execution gates, and only
// then the dispatcher seam. Nothing reaches the network unless every gate passes,
// and with no dispatcher installed the call still ends in 503.
func handleExecute(rawRequest []byte) ([]byte, *pluginError) {
	var req executorRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "executor.execute payload is not valid JSON", 0)
		}
	}

	// 1. Only the models this executor claims.
	if _, _, _, errModel := resolveModel(req.Model); errModel != nil {
		return nil, errModel
	}

	// 2. Parse the provider payload and refuse anything this executor cannot honour.
	// The payload's own stream flag is checked inside rejectUnsupported, which
	// gives the more specific reason; the routed flag is checked after it.
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	chat, errParse := parseChatRequest(payload)
	if errParse != nil {
		return nil, errParse.toPluginError()
	}
	if chat.Model != "" && !sameModel(chat.Model, req.Model) {
		// The provider payload names a different model than the routing decision.
		if _, _, _, errModel := resolveModel(chat.Model); errModel != nil {
			return nil, errModel
		}
	}
	if errReject := rejectUnsupported(chat); errReject != nil {
		return nil, errReject.toPluginError()
	}
	// The host also reports stream intent on the request itself. Both signals
	// must agree that this is a non-streaming call: a client error is written as a
	// failed envelope carrying http_status 400, so the host can surface it.
	if req.Stream {
		return encodeErrorEnvelope(newPluginError(codeStreamRejected,
			"stream=true reached the non-streaming executor; this executor serves complete responses only and does not synthesize an SSE stream.",
			http.StatusBadRequest)), nil
	}

	// 3. Fail-closed execution gates.
	cfg := currentConfig()
	if cfgErr := lastConfigError(); cfgErr != "" {
		return nil, newPluginError(codeExecutionDisabled,
			"plugin configuration was rejected at registration: "+cfgErr, http.StatusServiceUnavailable)
	}
	if errGate := cfg.executionAllowed(); errGate != nil {
		emitHostLog("warn", "entire-executor: execute refused: "+errGate.Message, map[string]any{
			"auth_id": req.AuthID,
			"model":   req.Model,
			"code":    errGate.Code,
		})
		return nil, errGate
	}

	// 4. Credential resolution. A gate-passing call still needs a verified bearer.
	cred, errCredential := resolveCredential(cfg, req.StorageJSON, mergeStringKeyed(req.AuthMetadata, req.Metadata))
	if errCredential != nil {
		var plugErr *pluginError
		if asPluginError(errCredential, &plugErr) {
			plugErr.HTTPStatus = http.StatusServiceUnavailable
			return nil, plugErr
		}
		return nil, newPluginError(codeCredentialMissing, errCredential.Error(), http.StatusServiceUnavailable)
	}
	if !cfg.targetConfigured() {
		return nil, newPluginError(codeExecutionDisabled,
			"Entire executor refused: no run target configured (project-host, project, agent-id, trail-id, change-id are all required)", http.StatusServiceUnavailable)
	}

	// 5. Dispatch. With no dispatcher installed this is where the build stays
	// honest: one error, HTTP 503, no fabricated assistant message.
	dispatcher := currentDispatcher()
	if dispatcher == nil {
		emitHostLog("warn", "entire-executor: dispatch requested but no dispatcher is installed", map[string]any{
			"auth_id": req.AuthID,
			"model":   req.Model,
			"code":    errNotImplemented.Code,
		})
		return nil, errNotImplemented
	}

	_, key, harness, _ := resolveModel(req.Model)
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	result, errDispatch := dispatcher.Dispatch(ctx, DispatcherContext{
		Model:      req.Model,
		Harness:    harness,
		AuthID:     req.AuthID,
		HostClient: hostClientFor(req),
		Config:     cfg,
		Credential: cred,
	}, flattenPrompt(chat.Messages))
	if errDispatch != nil {
		var plugErr *pluginError
		if asPluginError(errDispatch, &plugErr) {
			if plugErr.HTTPStatus == 0 {
				plugErr.HTTPStatus = http.StatusBadGateway
			}
			return nil, plugErr
		}
		return nil, newPluginError(codePluginError, errDispatch.Error(), http.StatusBadGateway)
	}

	// 6. Render a real chat-completions response from the dispatcher's text.
	body, errRender := renderCompletion(req.Model, key, harness, result)
	if errRender != nil {
		return nil, errRender
	}
	return encodeOKEnvelope(body), nil
}

// handleCountTokens serves executor.count_tokens with a deterministic local
// estimate. It never calls upstream, so it cannot consume credit.
func handleCountTokens(rawRequest []byte) ([]byte, *pluginError) {
	var req executorRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "executor.count_tokens payload is not valid JSON", 0)
		}
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	chat, errParse := parseChatRequest(payload)
	if errParse != nil {
		return nil, errParse.toPluginError()
	}
	if errReject := rejectUnsupported(chat); errReject != nil {
		return nil, errReject.toPluginError()
	}
	prompt := flattenPrompt(chat.Messages)
	return encodeOKEnvelope(map[string]int64{
		"total_tokens": int64(estimateTokens(prompt)),
	}), nil
}

// estimateTokens is the coarse 4-characters-per-token heuristic. It is labelled
// as an estimate in the response headers of execute; here it is the only value
// available without an upstream round trip.
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text) + 3) / 4
}

// completionResponse is the OpenAI chat-completions response shape.
type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *completionUsage   `json:"usage,omitempty"`
}

type completionChoice struct {
	Index        int           `json:"index"`
	Message      completionMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type completionMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// renderCompletion builds the provider payload returned to the host.
//
// The finish_reason is "length" when the upstream reported a truncated output,
// so a caller can tell a complete report from a prefix instead of trusting a
// "stop" that was never observed.
func renderCompletion(model, key, harness string, result DispatcherResult) (json.RawMessage, *pluginError) {
	text := result.Text
	finish := "stop"
	if result.Truncated {
		finish = "length"
	}
	resp := completionResponse{
		ID:      "chatcmpl-entire-" + sanitizeID(result.RunID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      completionMsg{Role: "assistant", Content: text},
			FinishReason: finish,
		}},
	}
	if len(result.Usage) > 0 {
		usage := completionUsage{}
		if value, ok := result.Usage["prompt_tokens"]; ok {
			usage.PromptTokens = value
		}
		if value, ok := result.Usage["completion_tokens"]; ok {
			usage.CompletionTokens = value
		} else {
			usage.CompletionTokens = int64(estimateTokens(text))
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if value, ok := result.Usage["total_tokens"]; ok {
			usage.TotalTokens = value
		}
		resp.Usage = &usage
	}
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return nil, newPluginError(codePluginError, "encode completion: "+errMarshal.Error(), http.StatusInternalServerError)
	}
	_ = key
	_ = harness
	return json.RawMessage(raw), nil
}

// sanitizeID keeps a run id safe for an OpenAI response id.
func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "run"
	}
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// sameModel compares a payload model against the routed model, ignoring any
// reasoning-effort suffix on either side.
func sameModel(left, right string) bool {
	return strings.EqualFold(stripThinkingSuffix(strings.TrimSpace(left)), stripThinkingSuffix(strings.TrimSpace(right)))
}

// mergeStringKeyed merges metadata maps, keeping storage-shaped values.
func mergeStringKeyed(first, second map[string]any) map[string]any {
	if len(first) == 0 && len(second) == 0 {
		return nil
	}
	merged := make(map[string]any, len(first)+len(second))
	for key, value := range second {
		merged[key] = value
	}
	for key, value := range first {
		merged[key] = value
	}
	return merged
}

// asPluginError extracts a *pluginError from an error chain.
func asPluginError(err error, target **pluginError) bool {
	if err == nil {
		return false
	}
	if plugErr, ok := err.(*pluginError); ok {
		*target = plugErr
		return true
	}
	return false
}
