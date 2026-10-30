package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// synthetic token used by every test. It never leaves the process.
const syntheticToken = "entire-synthetic-token-abc123456"

// resetPluginState restores defaults between tests.
func resetPluginState(t *testing.T) {
	t.Helper()
	shutdownFlag.Store(false)
	resetSecretRegistry()
	setDispatcher(nil)
	setConfig(runtimeConfig{
		CoreBaseURL:  defaultCoreBaseURL,
		CellBaseURL:  defaultCellBaseURL,
		AllowedHosts: append([]string(nil), defaultAllowedHosts...),
	}, nil)
}

// gatedConfig returns a configuration with every gate satisfied.
func gatedConfig() runtimeConfig {
	return runtimeConfig{
		Enabled:              true,
		Token:                syntheticToken,
		CoreBaseURL:          defaultCoreBaseURL,
		CellBaseURL:          defaultCellBaseURL,
		AllowedHosts:         append([]string(nil), defaultAllowedHosts...),
		ProjectHost:          "gh",
		Project:              "acme/widgets",
		AgentID:              "agent-1",
		TrailID:              "trail-1",
		ChangeID:             "change-1",
		ExecutionEnabled:     true,
		AuthSubjectVerified:  true,
		BillingLimitEnforced: true,
		AllowUnboundedSpend:  true,
	}
}

// decodeEnvelope splits a marshaled envelope into ok-flag, result and error.
func decodeEnvelope(t *testing.T, raw []byte) (bool, map[string]any, map[string]any) {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("envelope is not JSON: %v (%s)", errUnmarshal, raw)
	}
	var result map[string]any
	if len(envelope.Result) > 0 {
		if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
			t.Fatalf("result is not a JSON object: %v (%s)", errUnmarshal, envelope.Result)
		}
	}
	var errObj map[string]any
	if len(envelope.Error) > 0 {
		if errUnmarshal := json.Unmarshal(envelope.Error, &errObj); errUnmarshal != nil {
			t.Fatalf("error is not a JSON object: %v (%s)", errUnmarshal, envelope.Error)
		}
	}
	return envelope.OK, result, errObj
}

// callExecute marshals an executorRequest and runs executor.execute.
func callExecute(t *testing.T, req executorRequest) (map[string]any, *pluginError) {
	t.Helper()
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal executor request: %v", errMarshal)
	}
	out, errHandle := handleMethod(methodExecutorExecute, raw)
	if errHandle != nil {
		return nil, errHandle
	}
	ok, result, errObj := decodeEnvelope(t, out)
	if !ok {
		t.Fatalf("expected success envelope, got error %v", errObj)
	}
	return result, nil
}

// chatPayload builds a minimal chat-completions payload body.
func chatPayload(model, userText string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": userText},
		},
	})
	return body
}

// refusalCode asserts a refusal code and HTTP status.
func assertRefusal(t *testing.T, err *pluginError, wantCode string, wantStatus int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected refusal %s, got success", wantCode)
	}
	if err.Code != wantCode {
		t.Fatalf("refusal code = %q, want %q (%s)", err.Code, wantCode, err.Message)
	}
	if wantStatus != 0 && err.HTTPStatus != wantStatus {
		t.Fatalf("refusal status = %d, want %d (%s)", err.HTTPStatus, wantStatus, err.Message)
	}
}

// ---------------------------------------------------------------------------
// ABI / lifecycle
// ---------------------------------------------------------------------------

func TestRegisterDeclaresExecutorOnly(t *testing.T) {
	resetPluginState(t)

	configYAML := "enabled: true\npriority: 1\ntoken: \"" + syntheticToken + "\"\n"
	// The host marshals Go []byte as base64, so config_yaml travels encoded.
	raw, errMarshal := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte(configYAML)),
		"schema_version": 8,
	})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}

	out, errHandle := handleMethod(methodPluginRegister, raw)
	if errHandle != nil {
		t.Fatalf("plugin.register failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok {
		t.Fatalf("plugin.register returned an error envelope")
	}
	if got := result["schema_version"]; got != float64(8) {
		t.Fatalf("schema_version = %v, want 8", got)
	}

	caps, _ := result["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatalf("capabilities missing")
	}
	if caps["executor"] != true {
		t.Fatalf("executor capability must be true, got %v", caps["executor"])
	}
	// The quota read path belongs to ../quota. Declaring it here would register a
	// second quota provider for the same account.
	if caps["quota_provider"] != false {
		t.Fatalf("quota_provider must be false, got %v", caps["quota_provider"])
	}
	if caps["executor_model_scope"] != "both" {
		t.Fatalf("executor_model_scope = %v", caps["executor_model_scope"])
	}
	formats, _ := caps["executor_input_formats"].([]any)
	if len(formats) != 1 || formats[0] != "chat-completions" {
		t.Fatalf("executor_input_formats = %v", caps["executor_input_formats"])
	}
	outFormats, _ := caps["executor_output_formats"].([]any)
	if len(outFormats) != 1 || outFormats[0] != "chat-completions" {
		t.Fatalf("executor_output_formats = %v", caps["executor_output_formats"])
	}

	// Gates must stay closed when the operator configured nothing.
	cfg := currentConfig()
	if cfg.ExecutionEnabled || cfg.AuthSubjectVerified || cfg.BillingLimitEnforced || cfg.AllowUnboundedSpend {
		t.Fatalf("gates must default to false, got %s", describeConfig(cfg))
	}
	if registeredSecretCount() == 0 {
		t.Fatalf("configured token must be registered for redaction")
	}
}

func TestRegisterWithMalformedConfigFailsClosed(t *testing.T) {
	resetPluginState(t)

	raw := []byte(`{"config_yaml":"` + base64.StdEncoding.EncodeToString([]byte("execution-enabled: [unclosed")) + `","schema_version":8}`)
	out, errHandle := handleMethod(methodPluginRegister, raw)
	if errHandle != nil {
		t.Fatalf("register must not abort on a bad config: %v", errHandle)
	}
	if ok, _, _ := decodeEnvelope(t, out); !ok {
		t.Fatalf("register must still return a registration envelope")
	}
	if lastConfigError() == "" {
		t.Fatalf("a rejected configuration must be recorded")
	}

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review this"),
	})
	assertRefusal(t, errExec, codeExecutionDisabled, http.StatusServiceUnavailable)
}

func TestIdentifierAndModels(t *testing.T) {
	resetPluginState(t)

	out, errHandle := handleMethod(methodExecutorIdentifier, nil)
	if errHandle != nil {
		t.Fatalf("executor.identifier failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok || result["identifier"] != providerIdentifier {
		t.Fatalf("identifier = %v, want %s", result["identifier"], providerIdentifier)
	}

	out, errHandle = handleMethod(methodModelRegister, nil)
	if errHandle != nil {
		t.Fatalf("model.register failed: %v", errHandle)
	}
	ok, result, _ = decodeEnvelope(t, out)
	if !ok {
		t.Fatalf("model.register returned an error envelope")
	}
	if result["Provider"] != providerIdentifier {
		t.Fatalf("provider = %v", result["Provider"])
	}
	models, _ := result["Models"].([]any)
	if len(models) == 0 {
		t.Fatalf("model catalog is empty")
	}
	for _, entry := range models {
		model, _ := entry.(map[string]any)
		id, _ := model["ID"].(string)
		if !strings.HasPrefix(id, modelPrefix) {
			t.Fatalf("model %q is outside the %s namespace", id, modelPrefix)
		}
	}
}

func TestUnknownMethodIsReported(t *testing.T) {
	resetPluginState(t)

	_, errHandle := handleMethod("quota.fetch", nil)
	assertRefusal(t, errHandle, codeUnknownMethod, 0)

	_, errHandle = handleMethod("executor.http_request", nil)
	assertRefusal(t, errHandle, codeNotImplemented, 0)
}

// ---------------------------------------------------------------------------
// Fail-closed gates
// ---------------------------------------------------------------------------

func TestExecuteRefusesWhenExecutionDisabled(t *testing.T) {
	resetPluginState(t)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeExecutionDisabled, http.StatusServiceUnavailable)
	if errExec.Code == "success" {
		t.Fatalf("unreachable")
	}
}

func TestExecuteRefusesWithoutVerifiedSubject(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	cfg.AuthSubjectVerified = false
	setConfig(cfg, nil)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeExecutionInsecure, http.StatusServiceUnavailable)
}

func TestExecuteRefusesWithoutBillingLimit(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	cfg.BillingLimitEnforced = false
	setConfig(cfg, nil)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeBillingUnavailable, http.StatusServiceUnavailable)
}

func TestExecuteRefusesWithoutUnboundedSpendAck(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	cfg.AllowUnboundedSpend = false
	setConfig(cfg, nil)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeBillingUnavailable, http.StatusServiceUnavailable)
	if !strings.Contains(errExec.Message, "no hard per-request cost ceiling") {
		t.Fatalf("refusal must name the unbounded-cost reason, got %q", errExec.Message)
	}
}

func TestExecuteAllGatesPassStillRefusesWith503(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	setConfig(cfg, nil)
	registerSecret(syntheticToken)

	// Every gate is satisfied and a harness/catalog entry exists, yet no real
	// dispatcher is installed. This is the shipped state: the honest answer is
	// 503 with no content, never a synthesized assistant message.
	result, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	if result != nil {
		t.Fatalf("expected no result payload, got %v", result)
	}
	assertRefusal(t, errExec, codeNotImplemented, http.StatusServiceUnavailable)
	if !strings.Contains(errExec.Message, "fail-closed") {
		t.Fatalf("message must state fail-closed, got %q", errExec.Message)
	}
}

func TestExecuteRefusesWithoutCredential(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	cfg.Token = ""
	cfg.TokenEnv = ""
	setConfig(cfg, nil)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeCredentialMissing, http.StatusServiceUnavailable)
}

func TestExecuteRefusesWithoutRunTarget(t *testing.T) {
	resetPluginState(t)
	cfg := gatedConfig()
	cfg.TrailID = ""
	setConfig(cfg, nil)

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review the diff"),
	})
	assertRefusal(t, errExec, codeExecutionDisabled, http.StatusServiceUnavailable)
}

// ---------------------------------------------------------------------------
// Strict request rejection
// ---------------------------------------------------------------------------

func TestRejectTools(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	body, _ := json.Marshal(map[string]any{
		"model": "entire-review-review",
		"messages": []map[string]any{
			{"role": "user", "content": "review"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "read_file"}},
		},
	})
	_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: body})
	assertRefusal(t, errExec, codeToolsRejected, http.StatusBadRequest)
}

func TestRejectToolChoiceAndLegacyFunctions(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := []string{
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"tool_choice":"auto"}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"functions":[{"name":"f"}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"function_call":"auto"}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"parallel_tool_calls":true}`,
	}
	for _, body := range cases {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		assertRefusal(t, errExec, codeToolsRejected, http.StatusBadRequest)
	}
}

func TestRejectToolRoleMessages(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := []string{
		`{"model":"entire-review-review","messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"1","content":"r"}]}`,
		`{"model":"entire-review-review","messages":[{"role":"function","name":"f","content":"r"}]}`,
		`{"model":"entire-review-review","messages":[{"role":"assistant","content":"x","function_call":{"name":"f","arguments":"{}"}}]}`,
	}
	for _, body := range cases {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		assertRefusal(t, errExec, codeToolsRejected, http.StatusBadRequest)
	}
}

func TestRejectImageParts(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := []string{
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"image"}]}]}`,
	}
	for _, body := range cases {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		assertRefusal(t, errExec, codeImageRejected, http.StatusBadRequest)
	}
}

func TestRejectNonTextContent(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := []string{
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA"}}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"text","text":"x","image_url":"y"}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":[{"type":"video"}]}]}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"audio":{"format":"wav"}}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"modalities":["text","image"]}`,
	}
	for _, body := range cases {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		if errExec == nil {
			t.Fatalf("payload must be refused: %s", body)
		}
		if errExec.Code != codeContentRejected && errExec.Code != codeImageRejected {
			t.Fatalf("code = %q for %s", errExec.Code, body)
		}
		if errExec.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("status = %d for %s", errExec.HTTPStatus, body)
		}
	}
}

func TestRejectStreamParameter(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	body, _ := json.Marshal(map[string]any{
		"model":    "entire-review-review",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "review"}},
	})
	_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: body})
	assertRefusal(t, errExec, codeStreamRejected, http.StatusBadRequest)

	// The routed flag is checked too, and is answered as a failed envelope so the
	// host can surface http_status 400 to the client.
	routedStream, _ := json.Marshal(executorRequest{
		Model:   "entire-review-review",
		Stream:  true,
		Payload: chatPayload("entire-review-review", "review"),
	})
	routedOut, routedHandle := handleMethod(methodExecutorExecute, routedStream)
	if routedHandle != nil {
		t.Fatalf("routed stream flag must be a failed envelope, got handle error %v", routedHandle)
	}
	routedOK, _, routedErr := decodeEnvelope(t, routedOut)
	if routedOK {
		t.Fatalf("routed stream flag must not succeed")
	}
	if routedErr["code"] != codeStreamRejected || routedErr["http_status"] != float64(http.StatusBadRequest) {
		t.Fatalf("routed stream flag error = %v", routedErr)
	}

	// executor.execute_stream is refused outright rather than faked.
	_, errStream := handleMethod(methodExecutorExecuteStream, nil)
	assertRefusal(t, errStream, codeStreamRejected, http.StatusBadRequest)
}

func TestRejectUnsupportedParams(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := []string{
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{}}}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"n":2}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"logprobs":true}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"top_logprobs":3}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"reasoning_effort":"high"}`,
	}
	for _, body := range cases {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		assertRefusal(t, errExec, codeExtrasRejected, http.StatusBadRequest)
	}

	// Benign defaults are accepted and fall through to the gate check.
	for _, body := range []string{
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"n":1}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"logprobs":false}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"response_format":{"type":"text"}}`,
		`{"model":"entire-review-review","messages":[{"role":"user","content":"x"}],"parallel_tool_calls":false}`,
	} {
		_, errExec := callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(body)})
		assertRefusal(t, errExec, codeNotImplemented, http.StatusServiceUnavailable)
	}
}

func TestRejectEmptyAndMalformedPayload(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	_, errExec := callExecute(t, executorRequest{Model: "entire-review-review"})
	assertRefusal(t, errExec, codePayloadRejected, http.StatusBadRequest)

	_, errExec = callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte("not json")})
	assertRefusal(t, errExec, codePayloadRejected, http.StatusBadRequest)

	_, errExec = callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(`{"messages":[]}`)})
	assertRefusal(t, errExec, codePayloadRejected, http.StatusBadRequest)

	_, errExec = callExecute(t, executorRequest{Model: "entire-review-review", Payload: []byte(`{"messages":[{"role":"user","content":"x"}]} trailing`)})
	assertRefusal(t, errExec, codePayloadRejected, http.StatusBadRequest)
}

// ---------------------------------------------------------------------------
// Model namespace
// ---------------------------------------------------------------------------

func TestRejectUnsupportedModel(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)

	cases := map[string]string{
		"gpt-4o":                    codeModelUnsupported,
		"entire-review-":            codeModelUnsupported,
		"entire-review-nonexistent": codeModelUnsupported,
		"":                          codeModelUnsupported,
	}
	for model, wantCode := range cases {
		_, errExec := callExecute(t, executorRequest{
			Model:   model,
			Payload: chatPayload(model, "review"),
		})
		assertRefusal(t, errExec, wantCode, 0)
	}
}

func TestResolveModelAcceptsCatalogAndThinkingSuffix(t *testing.T) {
	base, key, harness, errModel := resolveModel("entire-review-review(high)")
	if errModel != nil {
		t.Fatalf("unexpected refusal: %v", errModel)
	}
	if base != "entire-review-review" || key != "review" || harness != "trail-review" {
		t.Fatalf("resolve = %q %q %q", base, key, harness)
	}
	for _, entry := range modelCatalog() {
		_, _, _, errEntry := resolveModel(entry.ID)
		if errEntry != nil {
			t.Fatalf("catalog entry %q must resolve: %v", entry.ID, errEntry)
		}
	}
	for _, bad := range []string{"entire-review-unknown", "claude-3", "entire-review-review(nested(x))"} {
		if _, _, _, errBad := resolveModel(bad); errBad == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Dispatcher seam
// ---------------------------------------------------------------------------

// fakeDispatcher records the call and returns canned text.
type fakeDispatcher struct {
	calls   int32
	prompt  string
	result  DispatcherResult
	err     error
	lastCtx DispatcherContext
}

func (d *fakeDispatcher) Dispatch(ctx context.Context, call DispatcherContext, prompt string) (DispatcherResult, error) {
	atomic.AddInt32(&d.calls, 1)
	d.prompt = prompt
	d.lastCtx = call
	if d.err != nil {
		return DispatcherResult{}, d.err
	}
	return d.result, nil
}

func TestDispatcherRendersCompletion(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)
	dispatcher := &fakeDispatcher{result: DispatcherResult{
		Text:  `{"summary":"","comments":[]}`,
		RunID: "run_abc-123",
		Usage: map[string]int64{"prompt_tokens": 12, "completion_tokens": 34},
	}}
	setDispatcher(dispatcher)

	result, errExec := callExecute(t, executorRequest{
		Model:          "entire-review-review(high)",
		AuthID:         "auth-1",
		StorageJSON:    []byte(`{"entire_token":"` + syntheticToken + `"}`),
		HostCallbackID: "cb-1",
		Payload:        chatPayload("entire-review-review", "review the diff"),
	})
	if errExec != nil {
		t.Fatalf("dispatch path refused: %v", errExec)
	}
	if result["object"] != "chat.completion" {
		t.Fatalf("object = %v", result["object"])
	}
	if result["model"] != "entire-review-review(high)" {
		t.Fatalf("model = %v", result["model"])
	}
	choices, _ := result["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", result["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" {
		t.Fatalf("role = %v", message["role"])
	}
	if message["content"] != `{"summary":"","comments":[]}` {
		t.Fatalf("content = %v", message["content"])
	}
	usage, _ := result["usage"].(map[string]any)
	if usage["total_tokens"] != float64(46) {
		t.Fatalf("usage = %v", usage)
	}

	if atomic.LoadInt32(&dispatcher.calls) != 1 {
		t.Fatalf("dispatcher calls = %d", dispatcher.calls)
	}
	if !strings.Contains(dispatcher.prompt, "review the diff") {
		t.Fatalf("prompt = %q", dispatcher.prompt)
	}
	if dispatcher.lastCtx.Harness != "trail-review" {
		t.Fatalf("harness = %q", dispatcher.lastCtx.Harness)
	}
	if dispatcher.lastCtx.Credential.Token != syntheticToken {
		t.Fatalf("credential not resolved")
	}
	if _, isHost := dispatcher.lastCtx.HostClient.(hostTransport); !isHost {
		t.Fatalf("host client = %T, want hostTransport", dispatcher.lastCtx.HostClient)
	}
}

func TestDispatcherMarksTruncatedOutput(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)
	setDispatcher(&fakeDispatcher{result: DispatcherResult{Text: "partial", Truncated: true}})

	result, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review"),
	})
	if errExec != nil {
		t.Fatalf("dispatch refused: %v", errExec)
	}
	choices, _ := result["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	// A truncated upstream result must not be reported as a clean stop.
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v, want length", choice["finish_reason"])
	}
}

func TestDispatcherErrorBecomes502(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)
	setDispatcher(&fakeDispatcher{err: newPluginError("entire_run_failed", "run failed upstream", 0)})

	_, errExec := callExecute(t, executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", "review"),
	})
	if errExec == nil {
		t.Fatalf("expected dispatch failure")
	}
	if errExec.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", errExec.HTTPStatus)
	}
}

func TestDispatcherNotCalledWhenAnyGateIsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*runtimeConfig){
		"execution":  func(c *runtimeConfig) { c.ExecutionEnabled = false },
		"subject":    func(c *runtimeConfig) { c.AuthSubjectVerified = false },
		"billing":    func(c *runtimeConfig) { c.BillingLimitEnforced = false },
		"unbounded":  func(c *runtimeConfig) { c.AllowUnboundedSpend = false },
		"target":     func(c *runtimeConfig) { c.AgentID = "" },
		"credential": func(c *runtimeConfig) { c.Token = "" },
	} {
		t.Run(name, func(t *testing.T) {
			resetPluginState(t)
			resetPluginState(t)
			cfg := gatedConfig()
			mutate(&cfg)
			setConfig(cfg, nil)
			dispatcher := &fakeDispatcher{result: DispatcherResult{Text: "should not run"}}
			setDispatcher(dispatcher)

			_, errExec := callExecute(t, executorRequest{
				Model:   "entire-review-review",
				Payload: chatPayload("entire-review-review", "review"),
			})
			if errExec == nil {
				t.Fatalf("expected refusal for %s", name)
			}
			if atomic.LoadInt32(&dispatcher.calls) != 0 {
				t.Fatalf("dispatcher ran %d times with the %s gate closed", dispatcher.calls, name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Auth parsing
// ---------------------------------------------------------------------------

func TestAuthParseRecognizesEntireToken(t *testing.T) {
	resetPluginState(t)

	raw, _ := json.Marshal(map[string]any{
		"Provider": providerIdentifier,
		"FileName": "entire.json",
		"RawJSON":  []byte(`{"entire_token":"Bearer ` + syntheticToken + `","note":"x"}`),
	})
	out, errHandle := handleMethod(methodAuthParse, raw)
	if errHandle != nil {
		t.Fatalf("auth.parse failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok || result["Handled"] != true {
		t.Fatalf("expected Handled=true, got %v", result)
	}
	auth, _ := result["Auth"].(map[string]any)
	if auth["Provider"] != providerIdentifier {
		t.Fatalf("provider = %v", auth["Provider"])
	}
	storageB64, _ := auth["StorageJSON"].(string)
	storage, _ := base64Decode(t, storageB64)
	var stored map[string]any
	if errUnmarshal := json.Unmarshal(storage, &stored); errUnmarshal != nil {
		t.Fatalf("storage JSON: %v", errUnmarshal)
	}
	// The stored token is normalized: no "Bearer " prefix survives.
	if stored["entire_token"] != syntheticToken {
		t.Fatalf("stored token = %v", stored["entire_token"])
	}
	if stored["note"] != "x" {
		t.Fatalf("unrelated keys must be preserved, got %v", stored)
	}
	attrs, _ := auth["Attributes"].(map[string]any)
	if attrs["entire_origin"] != "file" {
		t.Fatalf("attributes = %v", auth["Attributes"])
	}
	if registeredSecretCount() == 0 {
		t.Fatalf("parsed token must be registered for redaction")
	}
}

func TestAuthParseAcceptsBareStringToken(t *testing.T) {
	resetPluginState(t)

	raw, _ := json.Marshal(map[string]any{
		"Provider": "entire",
		"RawJSON":  []byte(`"` + syntheticToken + `"`),
	})
	out, errHandle := handleMethod(methodAuthParse, raw)
	if errHandle != nil {
		t.Fatalf("auth.parse failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok || result["Handled"] != true {
		t.Fatalf("expected Handled=true, got %v", result)
	}
}

func TestAuthParseLeavesForeignMaterialAlone(t *testing.T) {
	resetPluginState(t)

	cases := []map[string]any{
		{"Provider": "claude", "RawJSON": []byte(`{"access_token":"` + syntheticToken + `"}`)},
		{"Provider": providerIdentifier, "RawJSON": []byte(`{}`)},
		{"Provider": providerIdentifier, "RawJSON": []byte(`not json at all`)},
	}
	for _, body := range cases {
		raw, _ := json.Marshal(body)
		out, errHandle := handleMethod(methodAuthParse, raw)
		if errHandle != nil {
			t.Fatalf("auth.parse must not error on foreign material: %v", errHandle)
		}
		ok, result, _ := decodeEnvelope(t, out)
		if !ok {
			t.Fatalf("expected ok envelope")
		}
		if result["Handled"] != false {
			t.Fatalf("Handled = %v for %v", result["Handled"], body["RawJSON"])
		}
	}
}

func TestAuthParseRejectsEmptyMaterial(t *testing.T) {
	resetPluginState(t)

	raw, _ := json.Marshal(map[string]any{"Provider": providerIdentifier})
	_, errHandle := handleMethod(methodAuthParse, raw)
	assertRefusal(t, errHandle, codeAuthParseFailed, http.StatusBadRequest)
}

func TestAuthRefreshIsNoOpCopy(t *testing.T) {
	resetPluginState(t)

	raw, _ := json.Marshal(map[string]any{
		"AuthID":       "auth-1",
		"AuthProvider": providerIdentifier,
		"StorageJSON":  []byte(`{"entire_token":"` + syntheticToken + `"}`),
	})
	out, errHandle := handleMethod(methodAuthRefresh, raw)
	if errHandle != nil {
		t.Fatalf("auth.refresh failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok {
		t.Fatalf("auth.refresh returned an error envelope")
	}
	auth, _ := result["Auth"].(map[string]any)
	if auth["ID"] != "auth-1" {
		t.Fatalf("id = %v", auth["ID"])
	}
	if _, hasNext := result["NextRefreshAfter"]; hasNext {
		// An empty NextRefreshAfter must not be sent, or the host schedules a
		// refresh that cannot exist.
		if result["NextRefreshAfter"] != "" {
			t.Fatalf("NextRefreshAfter = %v", result["NextRefreshAfter"])
		}
	}
	attrs, _ := auth["Attributes"].(map[string]any)
	if attrs["entire_refresh"] != "unsupported" {
		t.Fatalf("attributes = %v", auth["Attributes"])
	}
}

// ---------------------------------------------------------------------------
// Count tokens and transport
// ---------------------------------------------------------------------------

func TestCountTokensIsLocal(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)
	dispatcher := &fakeDispatcher{}
	setDispatcher(dispatcher)

	raw, _ := json.Marshal(executorRequest{
		Model:   "entire-review-review",
		Payload: chatPayload("entire-review-review", strings.Repeat("a", 400)),
	})
	out, errHandle := handleMethod(methodExecutorCountTokens, raw)
	if errHandle != nil {
		t.Fatalf("count_tokens failed: %v", errHandle)
	}
	ok, result, _ := decodeEnvelope(t, out)
	if !ok {
		t.Fatalf("count_tokens returned an error envelope")
	}
	if result["total_tokens"] != float64(100) {
		t.Fatalf("total_tokens = %v, want 100", result["total_tokens"])
	}
	if atomic.LoadInt32(&dispatcher.calls) != 0 {
		t.Fatalf("count_tokens must never dispatch upstream")
	}
}

func TestHostClientForWithoutCallbackIsUnavailable(t *testing.T) {
	client := hostClientFor(executorRequest{})
	if _, ok := client.(unknownTransport); !ok {
		t.Fatalf("client = %T, want unknownTransport", client)
	}
	if _, errDo := client.Do(nil, HostHTTPRequest{}); errDo == nil {
		t.Fatalf("unknownTransport must fail")
	}
	if _, ok := hostClientFor(executorRequest{HostCallbackID: "cb"}).(hostTransport); !ok {
		t.Fatalf("expected hostTransport when a callback id exists")
	}
}

func TestValidateUpstreamURLChecksAllowList(t *testing.T) {
	allowed := defaultAllowedHosts

	if _, errOK := validateUpstreamURL("https://us.console.entire.io/api/v1/me/billing", allowed); errOK != nil {
		t.Fatalf("entire host must be allowed: %v", errOK)
	}
	cases := []string{
		"http://us.console.entire.io/api/v1/me/billing",
		"https://evil.example.com/api/v1/me/billing",
		"https://us.console.entire.io.evil.example/api",
		"https://user:pass@us.console.entire.io/api",
		"https://1.1.1.1/api",
		"ftp://us.console.entire.io/api",
	}
	for _, raw := range cases {
		if _, errBad := validateUpstreamURL(raw, allowed); errBad == nil {
			t.Fatalf("%q must be rejected", raw)
		}
	}
	if _, errLoop := validateUpstreamURL("http://127.0.0.1:8080/api", []string{"127.0.0.1"}); errLoop != nil {
		t.Fatalf("loopback fixture must be allowed when listed: %v", errLoop)
	}
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

func TestSecretsNeverLeak(t *testing.T) {
	resetPluginState(t)
	setConfig(gatedConfig(), nil)
	registerSecret(syntheticToken)

	// A dispatcher that echoes the credential into its error must produce a
	// redacted message.
	setDispatcher(&fakeDispatcher{err: newPluginError("entire_run_failed", "upstream rejected "+syntheticToken, 0)})
	_, errExec := callExecute(t, executorRequest{
		Model:        "entire-review-review",
		AuthMetadata: map[string]any{"token": syntheticToken},
		Payload:      chatPayload("entire-review-review", "review"),
	})
	if errExec == nil {
		t.Fatalf("expected an error")
	}
	if strings.Contains(errExec.Message, syntheticToken) {
		t.Fatalf("secret leaked in message: %q", errExec.Message)
	}
	if !strings.Contains(errExec.Message, secretPlaceholder) {
		t.Fatalf("expected a redaction placeholder, got %q", errExec.Message)
	}

	// describeConfig never prints the token value.
	desc := describeConfig(currentConfig())
	if strings.Contains(desc, syntheticToken) {
		t.Fatalf("describeConfig leaked the token: %q", desc)
	}
}

func TestErrorEnvelopeCarriesHTTPStatus(t *testing.T) {
	resetPluginState(t)

	// A routed stream flag on a supported model is a client error, delivered as a
	// failed envelope carrying http_status 400.
	raw, _ := json.Marshal(executorRequest{
		Model:   "entire-review-review",
		Stream:  true,
		Payload: chatPayload("entire-review-review", "review"),
	})
	out, errHandle := handleMethod(methodExecutorExecute, raw)
	if errHandle != nil {
		t.Fatalf("execute returned a handle error: %v", errHandle)
	}
	_, _, errObj := decodeEnvelope(t, out)
	if errObj["http_status"] != float64(http.StatusBadRequest) {
		t.Fatalf("http_status = %v", errObj["http_status"])
	}
	if errObj["code"] != codeStreamRejected {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestQuiesceAndShutdown(t *testing.T) {
	resetPluginState(t)

	for _, method := range []string{methodPluginQuiesce, methodPluginShutdown} {
		out, errHandle := handleMethod(method, nil)
		if errHandle != nil {
			t.Fatalf("%s failed: %v", method, errHandle)
		}
		if ok, _, _ := decodeEnvelope(t, out); !ok {
			t.Fatalf("%s must return ok", method)
		}
	}
	shutdownRuntime()
	_, errAfter := handleMethod(methodExecutorIdentifier, nil)
	assertRefusal(t, errAfter, codePluginError, 0)
}

// base64Decode decodes a base64 string (Go []byte on the wire).
func base64Decode(t *testing.T, value string) ([]byte, error) {
	t.Helper()
	if value == "" {
		return nil, nil
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(value)
	if errDecode != nil {
		t.Fatalf("base64 decode: %v", errDecode)
	}
	return decoded, nil
}
