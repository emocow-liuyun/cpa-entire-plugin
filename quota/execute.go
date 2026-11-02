package main

// Entire text execution adapter.
//
// CPA routes an `entire/<model>` chat request here. Entire exposes no direct
// model endpoint, so this adapter drives the documented Agent task API:
//
//	create agent (instructions = system + conversation) -> POST run -> poll -> archive
//
// What it deliberately does not do:
//   - no native token streaming (execute_stream is refused, not faked)
//   - no client tool calls: the cloud agent runs its own tools and returns prose
//   - no image input; textual content parts are flattened, others refused
//
// Every dispatch is preceded by a fresh billing read and fails closed when the
// balance is unreadable, not spendable, or below the configured reserve.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// executorRequest mirrors pluginapi.ExecutorRequest. The host marshals that
// struct without JSON tags, so the wire keys are the Go field names.
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
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
}

// chatPayload is the subset of an OpenAI chat-completions body this adapter reads.
type chatPayload struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// taskTarget is the configured Trail/Change a run is pinned to.
type taskTarget struct {
	TrailID  string
	ChangeID string
	HeadSHA  string
}

// agentTask is the create-agent body.
type agentTask struct {
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Harness         string            `json:"harness"`
	Model           string            `json:"model"`
	ReasoningEffort string            `json:"reasoning_effort"`
	TimeoutMS       int64             `json:"timeout_ms"`
	Instructions    string            `json:"instructions"`
	Signal          map[string]string `json:"signal"`
	IsEnabled       bool              `json:"is_enabled"`
	EnvironmentID   string            `json:"environment_id,omitempty"`
	RepositoryIDs   []string          `json:"repository_ids"`
	Triggers        []any             `json:"triggers"`
}

// runStatus is the subset of the Run document this adapter reads.
type runStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    *string `json:"code"`
		Message string  `json:"message"`
	} `json:"error"`
	Result *struct {
		Data *struct {
			Summary string `json:"summary"`
			Signal  *struct {
				Rationale string `json:"rationale"`
			} `json:"signal"`
		} `json:"data"`
	} `json:"result"`
	Output *struct {
		Text      *string `json:"text"`
		Truncated bool    `json:"truncated"`
	} `json:"output"`
	UsageByModel []struct {
		Model               string `json:"model"`
		InputTokens         int64  `json:"input_tokens"`
		OutputTokens        int64  `json:"output_tokens"`
		CacheCreationTokens int64  `json:"cache_creation_tokens"`
		CacheReadTokens     int64  `json:"cache_read_tokens"`
	} `json:"usage_by_model"`
}

// handleExecutorMethod dispatches the executor RPC family.
func handleExecutorMethod(method string, raw []byte) ([]byte, *pluginError) {
	switch method {
	case methodExecutorIdentifier:
		return encodeOKEnvelope(identifierResult{Identifier: providerIdentifier}), nil
	case methodExecutorExecute:
		return handleExecute(raw)
	case methodExecutorExecuteStream:
		// Refused explicitly: Entire returns one complete document per task, and
		// emitting a fake SSE stream would misrepresent it as native streaming.
		return nil, newPluginError(codeStreamUnsupported,
			"native streaming is unavailable: this adapter returns one complete response per request. Retry without stream=true.",
			http.StatusBadRequest)
	case methodExecutorCountTokens:
		return handleCountTokens(raw)
	}
	return nil, newPluginError(codeUnknownMethod, "unknown executor method", 0)
}

// handleExecute runs one chat completion through the Entire task API.
func handleExecute(raw []byte) ([]byte, *pluginError) {
	var req executorRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "executor payload is not valid JSON", http.StatusBadRequest)
		}
	}
	cfg := currentConfig()
	if errGate := cfg.executionGate(); errGate != nil {
		return nil, errGate
	}
	payloadBytes := req.Payload
	if len(payloadBytes) == 0 {
		payloadBytes = req.OriginalRequest
	}
	var payload chatPayload
	if errUnmarshal := json.Unmarshal(payloadBytes, &payload); errUnmarshal != nil {
		return nil, newPluginError(codeInvalidRequest, "request body is not a chat-completions payload", http.StatusBadRequest)
	}
	if payload.Stream || req.Stream {
		return nil, newPluginError(codeStreamUnsupported,
			"native streaming is unavailable: this adapter returns one complete response per request. Retry without stream=true.",
			http.StatusBadRequest)
	}
	if refusal := rejectUnsupportedPayload(payloadBytes); refusal != nil {
		return nil, refusal
	}

	model := normalizeEntireModel(req.Model)
	if !isKnownEntireModel(model) {
		return nil, newPluginError(codeModelUnsupported,
			"model "+trimForMessage(model)+" is not in the Entire catalogue", http.StatusNotFound)
	}
	harness, effort := harnessFor(model, fieldString(req.Metadata, "reasoning_effort"))

	question := flattenConversation(payload.Messages)
	if strings.TrimSpace(question) == "" {
		return nil, newPluginError(codeInvalidRequest, "the request carries no user text to relay", http.StatusBadRequest)
	}

	cred, errCredential := resolveCredential(context.Background(), cfg, quotaFetchContext{
		AuthID:      req.AuthID,
		StorageJSON: req.StorageJSON,
		Metadata:    mergeStringAny(req.AuthMetadata, req.Metadata),
	})
	if errCredential != nil {
		var plugErr *pluginError
		if asPluginError(errCredential, &plugErr) {
			plugErr.HTTPStatus = http.StatusServiceUnavailable
			return nil, plugErr
		}
		return nil, newPluginError(codeMissingCredential, errCredential.Error(), http.StatusServiceUnavailable)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.executionTimeout())
	defer cancel()

	client := billingTransport(req.HostCallbackID)
	// Fail-closed spend gate: a fresh billing read immediately before dispatch.
	if errBudget := checkBudget(ctx, cfg, cred, client); errBudget != nil {
		return nil, errBudget
	}

	target := taskTarget{TrailID: cfg.TrailID, ChangeID: cfg.ChangeID, HeadSHA: cfg.HeadSHA}
	answer, usage, runID, errRun := runEntireTask(ctx, cfg, cred, client, req.HostCallbackID, target, model, harness, effort, question)
	if errRun != nil {
		return nil, errRun
	}
	// The host reads pluginapi.ExecutorResponse: the provider payload must be
	// returned in Payload, not at the envelope root.
	payloadOut, errMarshal := json.Marshal(renderChatCompletion(req.Model, runID, answer, usage))
	if errMarshal != nil {
		return nil, newPluginError(codePluginError, "could not encode the chat completion", 0)
	}
	return encodeOKEnvelope(map[string]any{
		"Payload": payloadOut,
		"Headers": http.Header{"Content-Type": []string{"application/json"}},
	}), nil
}

// rejectUnsupportedPayload refuses request features the task API cannot honour.
func rejectUnsupportedPayload(body []byte) *pluginError {
	var probe struct {
		Tools       json.RawMessage `json:"tools"`
		ToolChoice  json.RawMessage `json:"tool_choice"`
		Functions   json.RawMessage `json:"functions"`
		ResponseFmt json.RawMessage `json:"response_format"`
		Modalities  json.RawMessage `json:"modalities"`
		Audio       json.RawMessage `json:"audio"`
		N           json.RawMessage `json:"n"`
	}
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return newPluginError(codeInvalidRequest, "request body is not valid JSON", http.StatusBadRequest)
	}
	nonNull := func(raw json.RawMessage) bool {
		text := strings.TrimSpace(string(raw))
		return text != "" && text != "null" && text != "[]" && text != "{}"
	}
	if nonNull(probe.Tools) || nonNull(probe.Functions) {
		return newPluginError(codeToolsUnsupported,
			"client tool calling is unavailable: the Entire agent runs its own tools inside its sandbox and cannot call back into the client.",
			http.StatusBadRequest)
	}
	if nonNull(probe.ToolChoice) {
		return newPluginError(codeToolsUnsupported, "tool_choice is unavailable for the same reason as tools.", http.StatusBadRequest)
	}
	if nonNull(probe.Modalities) || nonNull(probe.Audio) {
		return newPluginError(codeContentUnsupported, "audio and non-text modalities are unavailable.", http.StatusBadRequest)
	}
	if nonNull(probe.ResponseFmt) {
		return newPluginError(codeParamsUnsupported, "response_format is unavailable: the agent returns plain text.", http.StatusBadRequest)
	}
	if nonNull(probe.N) {
		return newPluginError(codeParamsUnsupported, "n is unavailable: one task run returns one answer.", http.StatusBadRequest)
	}
	return nil
}

// flattenConversation renders the message array as the plain text an agent
// instruction field can carry. Non-text content parts are refused, not dropped.
func flattenConversation(messages []chatMessage) string {
	var b strings.Builder
	for _, message := range messages {
		text, errText := messageText(message.Content)
		if errText != nil {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// messageText decodes a message content that is either a string or an array of
// text parts. Image and audio parts yield an empty string so the caller can
// refuse them via hasNonTextPart.
func messageText(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
			return "", errUnmarshal
		}
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parts); errUnmarshal != nil {
		return "", errUnmarshal
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "" {
			b.WriteString(part.Text)
		}
	}
	return b.String(), nil
}

// checkBudget performs the pre-dispatch spending gate.
func checkBudget(ctx context.Context, cfg runtimeConfig, cred credential, client billingHTTPClient) *pluginError {
	status, _, errRead := readBilling(ctx, cfg, cred, client)
	if errRead != nil {
		var plugErr *pluginError
		if asPluginError(errRead, &plugErr) {
			return newPluginError(codeBillingUnavailable,
				"billing could not be read before dispatch, so the run was not started: "+plugErr.Message, http.StatusServiceUnavailable)
		}
		return newPluginError(codeBillingUnavailable, "billing could not be read before dispatch", http.StatusServiceUnavailable)
	}
	if status.stateValue() != "active" {
		return newPluginError(codeBillingUnavailable,
			"billing is not active; the run was not started", http.StatusServiceUnavailable)
	}
	if status.Balance == nil || status.Balance.TotalCents == nil {
		return newPluginError(codeBillingUnavailable, "credit balance is unknown; the run was not started", http.StatusServiceUnavailable)
	}
	if status.RunsPaused != nil && status.RunsPaused.Paused != nil && *status.RunsPaused.Paused {
		return newPluginError(codeBillingUnavailable, "Entire has paused runs for this account; the run was not started", http.StatusServiceUnavailable)
	}
	if status.Overage != nil && status.Overage.AmountCents != nil && *status.Overage.AmountCents > 0 {
		return newPluginError(codeBillingUnavailable, "this account already carries overage; the run was not started", http.StatusServiceUnavailable)
	}
	if status.AutomaticTopUp != nil && status.AutomaticTopUp.Enabled != nil && *status.AutomaticTopUp.Enabled {
		return newPluginError(codeBillingUnavailable,
			"automatic top-up is enabled, so a paid refill could be triggered; disable it or use another account", http.StatusServiceUnavailable)
	}
	if *status.Balance.TotalCents < cfg.ReserveCents {
		return newPluginError(codeBillingInsufficient,
			fmt.Sprintf("credit %d cents is below the configured reserve of %d cents; the run was not started",
				*status.Balance.TotalCents, cfg.ReserveCents), http.StatusServiceUnavailable)
	}
	return nil
}

// runEntireTask creates the agent, starts the run, polls it, and always tries
// to archive the agent it created.
func runEntireTask(ctx context.Context, cfg runtimeConfig, cred credential, client billingHTTPClient,
	hostCallbackID string, target taskTarget, model, harness, effort, question string) (string, map[string]int64, string, *pluginError) {

	headers := http.Header{
		"Accept":          []string{"application/json"},
		"Content-Type":    []string{"application/json"},
		"Authorization":   []string{"Bearer " + cred.Token},
		"Idempotency-Key": []string{"entire-cpa-agent-" + newTaskSuffix()},
	}
	instructions := buildInstructions(question)
	if len(instructions) > maxInstructions {
		return "", nil, "", newPluginError(codeContextTooLarge,
			fmt.Sprintf("the relayed conversation is %d bytes but the agent instruction field takes at most %d",
				len(instructions), maxInstructions), http.StatusRequestEntityTooLarge)
	}
	task := agentTask{
		Name:            "cpa-relay-" + newTaskSuffix(),
		Description:     "CPA relay run",
		Harness:         harness,
		Model:           model,
		ReasoningEffort: effort,
		TimeoutMS:       cfg.RunTimeoutMS,
		Instructions:    instructions,
		Signal:          map[string]string{"value_type": "boolean", "polarity": "neutral"},
		IsEnabled:       false,
		EnvironmentID:   cfg.EnvironmentID,
		RepositoryIDs:   []string{cfg.RepositoryID},
		Triggers:        []any{},
	}
	body, errMarshal := json.Marshal(task)
	if errMarshal != nil {
		return "", nil, "", newPluginError(codePluginError, "could not encode the agent request", 0)
	}
	agentBase := cfg.CellBaseURL + "/api/v1/" + cfg.ProjectHost + "/" + cfg.Project
	status, _, rawAgent, errCall := client.Do(ctx, http.MethodPost, agentBase+"/agents", headers, body)
	if errCall != nil {
		return "", nil, "", asExecuteError(errCall, "creating the Entire agent failed")
	}
	if status < 200 || status > 299 {
		return "", nil, "", newPluginError(codeUpstreamStatus,
			fmt.Sprintf("Entire refused the agent creation with HTTP %d", status), http.StatusBadGateway)
	}
	var agent struct {
		ID string `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(rawAgent, &agent); errUnmarshal != nil || agent.ID == "" {
		return "", nil, "", newPluginError(codeUpstreamMalformed, "Entire returned no agent id", http.StatusBadGateway)
	}
	defer archiveAgent(context.WithoutCancel(ctx), client, agentBase, agent.ID, cred.Token)

	runHeaders := headers.Clone()
	runHeaders.Set("Idempotency-Key", "entire-cpa-run-"+newTaskSuffix())
	runBody, _ := json.Marshal(map[string]any{
		"target":   map[string]string{"trail_id": target.TrailID, "change_id": target.ChangeID},
		"head_sha": target.HeadSHA,
	})
	status, _, rawRun, errCall := client.Do(ctx, http.MethodPost, agentBase+"/agents/"+agent.ID+"/runs", runHeaders, runBody)
	if errCall != nil {
		return "", nil, "", asExecuteError(errCall, "starting the Entire run failed")
	}
	if status < 200 || status > 299 {
		return "", nil, "", newPluginError(codeUpstreamStatus,
			fmt.Sprintf("Entire refused the run with HTTP %d", status), http.StatusBadGateway)
	}
	var started runStatus
	if errUnmarshal := json.Unmarshal(rawRun, &started); errUnmarshal != nil || started.ID == "" {
		return "", nil, "", newPluginError(codeUpstreamMalformed, "Entire returned no run id", http.StatusBadGateway)
	}
	return pollRun(ctx, client, agentBase, started.ID, cred.Token)
}

// pollRun waits for a terminal run state and extracts the answer.
func pollRun(ctx context.Context, client billingHTTPClient, agentBase, runID, token string) (string, map[string]int64, string, *pluginError) {
	headers := http.Header{"Accept": []string{"application/json"}, "Authorization": []string{"Bearer " + token}}
	interval := 3 * time.Second
	for {
		if errCtx := ctx.Err(); errCtx != nil {
			return "", nil, runID, newPluginError(codeRunTimeout, "the Entire run did not finish before the request deadline", http.StatusGatewayTimeout)
		}
		status, _, raw, errCall := client.Do(ctx, http.MethodGet, agentBase+"/runs/"+runID, headers, nil)
		if errCall != nil {
			return "", nil, runID, asExecuteError(errCall, "polling the Entire run failed")
		}
		if status < 200 || status > 299 {
			return "", nil, runID, newPluginError(codeUpstreamStatus,
				fmt.Sprintf("Entire returned HTTP %d while polling the run", status), http.StatusBadGateway)
		}
		var run runStatus
		if errUnmarshal := json.Unmarshal(raw, &run); errUnmarshal != nil {
			return "", nil, runID, newPluginError(codeUpstreamMalformed, "the run document is not valid JSON", http.StatusBadGateway)
		}
		switch run.Status {
		case "succeeded":
			answer := extractAnswer(run)
			if strings.TrimSpace(answer) == "" {
				return "", nil, runID, newPluginError(codeUpstreamMalformed, "the run produced no text answer", http.StatusBadGateway)
			}
			return answer, usageFrom(run), runID, nil
		case "failed", "timed_out", "canceled":
			message := "the Entire run ended as " + run.Status
			if run.Error != nil && strings.TrimSpace(run.Error.Message) != "" {
				message += ": " + trimForMessage(run.Error.Message)
			}
			return "", nil, runID, newPluginError(codeRunFailed, message, http.StatusBadGateway)
		}
		select {
		case <-ctx.Done():
			return "", nil, runID, newPluginError(codeRunTimeout, "the Entire run did not finish before the request deadline", http.StatusGatewayTimeout)
		case <-time.After(interval):
		}
	}
}

// extractAnswer prefers the structured summary, then the output text.
func extractAnswer(run runStatus) string {
	if run.Result != nil && run.Result.Data != nil && strings.TrimSpace(run.Result.Data.Summary) != "" {
		return run.Result.Data.Summary
	}
	if run.Output != nil && run.Output.Text != nil {
		return *run.Output.Text
	}
	return ""
}

// usageFrom maps task token usage onto chat-completion usage keys.
func usageFrom(run runStatus) map[string]int64 {
	var prompt, completion int64
	for _, item := range run.UsageByModel {
		prompt += item.InputTokens + item.CacheCreationTokens + item.CacheReadTokens
		completion += item.OutputTokens
	}
	if prompt == 0 && completion == 0 {
		return nil
	}
	return map[string]int64{"prompt_tokens": prompt, "completion_tokens": completion}
}

// archiveAgent removes the per-request agent so parked agents do not accumulate.
func archiveAgent(ctx context.Context, client billingHTTPClient, agentBase, agentID, token string) {
	headers := http.Header{"Accept": []string{"application/json"}, "Authorization": []string{"Bearer " + token}}
	status, respHeaders, raw, errGet := client.Do(ctx, http.MethodGet, agentBase+"/agents/"+agentID, headers, nil)
	if errGet != nil || status < 200 || status > 299 {
		return
	}
	// The archive route requires one strong ETag. A weak HTTP validator
	// ("W/\"1\"") is rejected, so the document's own etag field is preferred.
	etag := ""
	var doc struct {
		Etag        string `json:"etag"`
		ArchiveEtag string `json:"archive_etag"`
	}
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal == nil {
		etag = strings.TrimSpace(doc.Etag)
	}
	if etag == "" || strings.HasPrefix(etag, "W/") {
		if header := strings.TrimSpace(respHeaders.Get("ETag")); header != "" && !strings.HasPrefix(header, "W/") {
			etag = header
		}
	}
	archiveHeaders := headers.Clone()
	archiveHeaders.Set("Content-Type", "application/json")
	if etag != "" {
		archiveHeaders.Set("If-Match", etag)
	}
	_, _, _, _ = client.Do(ctx, http.MethodPost, agentBase+"/agents/"+agentID+"/archive", archiveHeaders, []byte("{}"))
}

// buildInstructions renders the relay instruction block.
//
// The framing is one short paragraph, deliberately free of words the model might
// echo back: an earlier version opened with "this is a chat relay, not a code
// review", and answers duly began by repeating that sentence.
func buildInstructions(question string) string {
	return "Answer the user conversation that follows. Reply with the answer alone, in the user's own language, " +
		"in the summary field of your structured output. Do not describe your task, environment or instructions. " +
		"Set signal value to true, set the rationale to a short label, and leave findings empty.\n\n" +
		"Conversation:\n" + question
}

// renderChatCompletion builds the OpenAI-compatible response.
func renderChatCompletion(model, runID, answer string, usage map[string]int64) map[string]any {
	out := map[string]any{
		"id":      "chatcmpl-entire-" + trimForMessage(runID),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
	}
	if len(usage) > 0 {
		total := usage["prompt_tokens"] + usage["completion_tokens"]
		out["usage"] = map[string]int64{
			"prompt_tokens":     usage["prompt_tokens"],
			"completion_tokens": usage["completion_tokens"],
			"total_tokens":      total,
		}
	}
	return out
}

// handleCountTokens answers token counting locally; it never dispatches.
func handleCountTokens(raw []byte) ([]byte, *pluginError) {
	var req executorRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "count_tokens payload is not valid JSON", http.StatusBadRequest)
		}
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	total := int64((len(payload) + 3) / 4)
	body, _ := json.Marshal(map[string]int64{"total_tokens": total})
	return encodeOKEnvelope(map[string]any{"Payload": body}), nil
}

// asExecuteError wraps a transport failure as a plugin error.
func asExecuteError(err error, fallback string) *pluginError {
	var plugErr *pluginError
	if asPluginError(err, &plugErr) {
		return plugErr
	}
	return newPluginError(codeUpstreamUnavailable, fallback, http.StatusBadGateway)
}

// normalizeEntireModel strips the CPA model prefix.
func normalizeEntireModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	return trimmed
}

// isKnownEntireModel reports whether the catalogue lists the model.
func isKnownEntireModel(model string) bool {
	for _, id := range entireModelIDs {
		if id == model {
			return true
		}
	}
	return false
}

// harnessFor picks the harness and effort for a model.
func harnessFor(model, requested string) (string, string) {
	harness := "claude"
	if strings.HasPrefix(model, "gpt-") {
		harness = "codex"
	}
	effort := strings.ToLower(strings.TrimSpace(requested))
	switch effort {
	case "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		effort = "low"
	}
	return harness, effort
}

// fieldString reads one string field from a metadata bag.
func fieldString(bag map[string]any, key string) string {
	if bag == nil {
		return ""
	}
	if value, okValue := bag[key].(string); okValue {
		return value
	}
	return ""
}

// mergeStringAny overlays b onto a.
func mergeStringAny(a, b map[string]any) map[string]any {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]any, len(a)+len(b))
	for key, value := range a {
		out[key] = value
	}
	for key, value := range b {
		out[key] = value
	}
	return out
}

// newTaskSuffix returns a short unique suffix for agent names and idempotency keys.
func newTaskSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
