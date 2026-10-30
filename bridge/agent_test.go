package bridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

const (
	agentPath = "/api/v1/gh/acme-widgets/agents/01HZZTESTAGENT0000000000"
	etagV7    = "01J9ETAGV7JSON"
)

// baseAgentBody is a well-formed Agent as the cell returns it.
func baseAgentBody(etag string, version int64) map[string]any {
	return map[string]any{
		"id":                   "01HZZTESTAGENT0000000000",
		"project_id":           "01HZZTESTPROJECT000000000",
		"version":              version,
		"etag":                 etag,
		"name":                 "cpa-reviewer",
		"description":          "CPA driven review agent",
		"harness":              "codex",
		"model":                "gpt-5-codex",
		"reasoning_effort":     "high",
		"instructions":         "Review the change.",
		"is_enabled":           false,
		"environment_id":       "01HZZTESTENV0000000000000",
		"repository_ids":       []string{"01HZZTESTREPO000000000000"},
		"triggers":             []any{},
		"timeout_ms":           900000,
		"signal":               map[string]any{"value_type": "percent", "polarity": "higher_is_better"},
		"preset":               nil,
		"archived_at":          nil,
		"stale_repository_ids": []string{},
		"created_at":           "2026-10-01T00:00:00Z",
		"updated_at":           "2026-10-30T00:00:00Z",
	}
}

func TestReadAgentUsesJSONEtag(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.Header().Set("ETag", `W/"http-layer-etag"`)
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 7))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	agent, err := client.ReadAgent(context.Background(), "")
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if agent.ETag != etagV7 {
		t.Fatalf("etag = %q, want the body value %q", agent.ETag, etagV7)
	}
	if agent.Version != 7 || agent.Harness != "codex" || agent.Signal.ValueType != "percent" {
		t.Fatalf("unexpected agent: %+v", agent)
	}
	if client.LastETag() != etagV7 {
		t.Fatalf("LastETag = %q", client.LastETag())
	}
	req := cell.last(t)
	if req.Method != http.MethodGet || req.Path != agentPath {
		t.Fatalf("method=%s path=%s", req.Method, req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestReadAgentWithoutEtagFails(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		body := baseAgentBody("", 1)
		writeJSON(t, w, http.StatusOK, body)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	_, err := client.ReadAgent(context.Background(), "01HZZTESTAGENT0000000000")
	if err == nil || !strings.Contains(err.Error(), "no etag") {
		t.Fatalf("want a no-etag error, got %v", err)
	}
}

func TestReplaceAgentRequestShape(t *testing.T) {
	t.Parallel()
	var putBody map[string]any
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, req recordedRequest) {
		if r.Method == http.MethodPut {
			if err := json.Unmarshal(req.Body, &putBody); err != nil {
				t.Errorf("decode replace body: %v", err)
			}
			writeJSON(t, w, http.StatusOK, baseAgentBody("01J9ETAGV8JSON", 8))
			return
		}
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 7))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	agent, err := client.ReadAgent(context.Background(), "")
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	body := bridge.AgentFrom(agent)
	body.Instructions = "Rewrite every finding as a JSON array."
	updated, err := client.ReplaceAgent(context.Background(), "", "", body)
	if err != nil {
		t.Fatalf("ReplaceAgent: %v", err)
	}
	if updated.Version != 8 || updated.ETag != "01J9ETAGV8JSON" {
		t.Fatalf("unexpected replace response: %+v", updated)
	}

	reqs := cell.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	put := reqs[1]
	if put.Method != http.MethodPut || put.Path != agentPath {
		t.Fatalf("method=%s path=%s", put.Method, put.Path)
	}
	if got := put.Header.Get("If-Match"); got != etagV7 {
		t.Fatalf("If-Match = %q, want the cached JSON etag", got)
	}
	if got := put.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if putBody["instructions"] != "Rewrite every finding as a JSON array." {
		t.Fatalf("instructions not sent: %v", putBody["instructions"])
	}
	for _, key := range []string{"environment_id", "repository_ids"} {
		if _, ok := putBody[key]; !ok {
			t.Fatalf("required field %q missing from the replace body: %v", key, putBody)
		}
	}
	// Fields with no value must be omitted rather than sent as null, since
	// additionalProperties is false and null means "reset to default".
	if _, ok := putBody["$schema"]; ok {
		t.Fatalf("$schema sent despite being readOnly: %v", putBody)
	}
	assertNoTokenLeak(t, "replace body", string(put.Body))
}

func TestReplaceAgentRequiresIfMatch(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("no request should be sent without If-Match")
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	body := &bridge.AgentReplace{EnvironmentID: "01HZZTESTENV0000000000000", RepositoryIDs: []string{"01HZZTESTREPO000000000000"}}
	_, err := client.ReplaceAgent(context.Background(), "", "", body)
	if !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if n := len(cell.requests()); n != 0 {
		t.Fatalf("expected no requests, got %d", n)
	}
}

func TestReplaceAgentStaleEtagIsTyped(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusPreconditionFailed, map[string]any{
			"type": "about:blank", "title": "Precondition Failed", "status": 412,
			"detail": "etag is stale", "code": "precondition_failed", "request_id": "req_412",
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	body := &bridge.AgentReplace{
		EnvironmentID: "01HZZTESTENV0000000000000",
		RepositoryIDs: []string{"01HZZTESTREPO000000000000"},
		Instructions:  "x",
	}
	_, err := client.ReplaceAgent(context.Background(), "", "01J9STALE", body)
	if !errors.Is(err, bridge.ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if got := cell.last(t).Header.Get("If-Match"); got != "01J9STALE" {
		t.Fatalf("If-Match = %q", got)
	}
}

func TestReplaceAgentMissingIfMatchIsTyped(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusPreconditionRequired, map[string]any{
			"title": "Precondition Required", "status": 428, "detail": "If-Match required",
			"code": "precondition_required", "request_id": "req_428", "type": "about:blank",
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	body := &bridge.AgentReplace{EnvironmentID: "e", RepositoryIDs: []string{"r"}}
	_, err := client.ReplaceAgent(context.Background(), "", "some-etag", body)
	if !errors.Is(err, bridge.ErrPreconditionRequired) {
		t.Fatalf("want ErrPreconditionRequired, got %v", err)
	}
}

func TestAgentReplaceValidation(t *testing.T) {
	t.Parallel()
	ok := func() *bridge.AgentReplace {
		return &bridge.AgentReplace{
			EnvironmentID: "01HZZTESTENV0000000000000",
			RepositoryIDs: []string{"01HZZTESTREPO000000000000"},
			Name:          "n",
			Harness:       "claude",
			Signal:        &bridge.AgentSignalInput{ValueType: "boolean"},
		}
	}

	cases := []struct {
		name   string
		mutate func(*bridge.AgentReplace)
	}{
		{"missing environment", func(a *bridge.AgentReplace) { a.EnvironmentID = "" }},
		{"no repositories", func(a *bridge.AgentReplace) { a.RepositoryIDs = nil }},
		{"duplicate repository", func(a *bridge.AgentReplace) { a.RepositoryIDs = append(a.RepositoryIDs, a.RepositoryIDs[0]) }},
		{"too many repositories", func(a *bridge.AgentReplace) {
			a.RepositoryIDs = nil
			for i := 0; i < 101; i++ {
				a.RepositoryIDs = append(a.RepositoryIDs, "01HZZTESTREPO"+string(rune('A'+i%26))+string(rune('a'+i/26)))
			}
		}},
		{"bad harness", func(a *bridge.AgentReplace) { a.Harness = "gemini" }},
		{"bad effort", func(a *bridge.AgentReplace) { a.ReasoningEffort = bridge.NullableStringOf("extreme") }},
		{"model too long", func(a *bridge.AgentReplace) {
			a.Model = bridge.NullableStringOf(strings.Repeat("m", bridge.MaxModelIDLength+1))
		}},
		{"bad signal type", func(a *bridge.AgentReplace) { a.Signal = &bridge.AgentSignalInput{ValueType: "rating"} }},
		{"missing signal type", func(a *bridge.AgentReplace) { a.Signal = &bridge.AgentSignalInput{} }},
		{"bad polarity", func(a *bridge.AgentReplace) {
			p := "unspecified"
			a.Signal = &bridge.AgentSignalInput{ValueType: "size", Polarity: &p}
		}},
		{"timeout zero", func(a *bridge.AgentReplace) { v := int64(0); a.TimeoutMS = &v }},
		{"timeout too large", func(a *bridge.AgentReplace) { v := int64(1800001); a.TimeoutMS = &v }},
		{"two triggers", func(a *bridge.AgentReplace) {
			a.Triggers = []bridge.AgentTriggerInput{{Kind: "push"}, {Kind: "push"}}
		}},
		{"unknown trigger", func(a *bridge.AgentReplace) {
			a.Triggers = []bridge.AgentTriggerInput{{Kind: "cron"}}
		}},
		{"name too long", func(a *bridge.AgentReplace) { a.Name = strings.Repeat("n", bridge.MaxAgentNameLength+1) }},
		{"description too long", func(a *bridge.AgentReplace) {
			d := strings.Repeat("d", bridge.MaxDescriptionLength+1)
			a.Description = &d
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := ok()
			tc.mutate(a)
			if err := a.Validate(); !errors.Is(err, bridge.ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}

	if err := ok().Validate(); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestValidateInstructionsCeiling(t *testing.T) {
	t.Parallel()
	if err := bridge.ValidateInstructions(strings.Repeat("a", bridge.MaxInstructions)); err != nil {
		t.Fatalf("exactly %d chars rejected: %v", bridge.MaxInstructions, err)
	}
	err := bridge.ValidateInstructions(strings.Repeat("a", bridge.MaxInstructions+1))
	if !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig above the ceiling, got %v", err)
	}
	// Multi-byte runes: 16384 runes of 3 bytes each exceed only the byte count,
	// and the byte check must catch it so the server's limit never sees it.
	if err := bridge.ValidateInstructions(strings.Repeat("崔", bridge.MaxInstructions/2)); err == nil {
		t.Fatal("expected the byte ceiling to reject a multi-byte block")
	}
}

// TestOversizedInstructionsNeverLeaveTheProcess is the end-to-end guard: an
// over-limit prompt must be rejected locally, before the bearer is used.
func TestOversizedInstructionsNeverLeaveTheProcess(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("an over-limit replace must not reach the server")
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	body := &bridge.AgentReplace{
		EnvironmentID: "01HZZTESTENV0000000000000",
		RepositoryIDs: []string{"01HZZTESTREPO000000000000"},
		Instructions:  strings.Repeat("x", bridge.MaxInstructions+1),
	}
	_, err := client.ReplaceAgent(context.Background(), "", "etag", body)
	if !errors.Is(err, bridge.ErrInvalidConfig) || !strings.Contains(err.Error(), "16384") {
		t.Fatalf("want an ErrInvalidConfig naming the ceiling, got %v", err)
	}
	if n := len(cell.requests()); n != 0 {
		t.Fatalf("expected no requests, got %d", n)
	}
}

func TestAgentFromRoundTripsEveryField(t *testing.T) {
	t.Parallel()
	var read map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, baseAgentBody(etagV7, 3))), &read); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	var agent bridge.Agent
	raw := mustJSON(t, baseAgentBody(etagV7, 3))
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	replaced := bridge.AgentFrom(&agent)
	encoded, err := json.Marshal(replaced)
	if err != nil {
		t.Fatalf("marshal replace: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatalf("decode replace: %v", err)
	}
	for _, key := range []string{"name", "harness", "model", "reasoning_effort", "instructions", "is_enabled", "environment_id", "repository_ids", "timeout_ms", "signal", "description"} {
		if _, ok := sent[key]; !ok {
			t.Fatalf("AgentFrom dropped %q: %v", key, sent)
		}
	}
	for _, key := range []string{"id", "etag", "version", "project_id", "created_at", "archived_at"} {
		if _, ok := sent[key]; ok {
			t.Fatalf("AgentFrom copied read-only field %q: %v", key, sent)
		}
	}

	// A null read model must round-trip as an explicit null, which the schema
	// allows and means "harness default". An unset Agent has no model at all,
	// so AgentFrom must send null rather than dropping the field.
	nullModel := bridge.Agent{EnvironmentID: "e", RepositoryIDs: []string{"r"}, Model: nil}
	if encoded := mustJSON(t, bridge.AgentFrom(&nullModel)); !strings.Contains(encoded, `"model":null`) {
		t.Fatalf("expected model sent as null, got %s", encoded)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestListAgentModelsAndLookup(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"agents": []map[string]any{{
				"harness":                  "codex",
				"default_model":            "gpt-5-codex",
				"default_reasoning_effort": "medium",
				"reasoning_efforts":        []string{"low", "medium", "high"},
				"models": []map[string]any{
					{"id": "gpt-5-codex", "name": "GPT-5 Codex", "visibility": "current", "reasoning_efforts": []string{"low", "medium", "high"}},
					{"id": "gpt-5-codex-mini", "name": "GPT-5 Codex mini", "visibility": "more", "reasoning_efforts": []string{}},
				},
			}},
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	models, err := client.ListAgentModels(context.Background())
	if err != nil {
		t.Fatalf("ListAgentModels: %v", err)
	}
	if cell.last(t).Path != "/api/v1/agents/models" {
		t.Fatalf("path = %q", cell.last(t).Path)
	}
	h, m, err := models.FindAgentModel("codex", "")
	if err != nil || m == nil || m.ID != "gpt-5-codex" || h.Harness != "codex" {
		t.Fatalf("default lookup: harness=%v model=%v err=%v", h, m, err)
	}
	_, m, err = models.FindAgentModel("codex", "gpt-5-codex-mini")
	if err != nil || m.Visibility != "more" {
		t.Fatalf("explicit lookup failed: %v %v", m, err)
	}
	if _, _, err := models.FindAgentModel("claude", "gpt-5-codex"); err == nil {
		t.Fatal("expected an error for a harness/model pair that is not listed")
	}
}
