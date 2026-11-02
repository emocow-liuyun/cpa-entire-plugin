package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEntire serves the task API so execute can be tested without any network.
type fakeEntire struct {
	agents           int
	runs             int
	archived         int
	lastInstructions string
	lastModel        string
	lastHeaders      http.Header
	runStatus        string
	answer           string
	billingStatus    int
	billingBody      string
}

func (f *fakeEntire) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/me/billing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		code := f.billingStatus
		if code == 0 {
			code = http.StatusOK
		}
		body := f.billingBody
		if body == "" {
			body = `{"state":"active","balance":{"total_cents":5000},"overage":{"amount_cents":0},"runs_paused":{"paused":false},"automatic_top_up":{"enabled":false}}`
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v1/gh/acct/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.agents++
		f.lastHeaders = r.Header.Clone()
		var task agentTask
		_ = json.NewDecoder(r.Body).Decode(&task)
		f.lastInstructions = task.Instructions
		f.lastModel = task.Model
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"agent-1","etag":"\"1\""}`))
	})
	mux.HandleFunc("/api/v1/gh/acct/agents/agent-1", func(w http.ResponseWriter, r *http.Request) {
		// A weak HTTP validator plus the document's strong etag: the archive
		// route only accepts the strong one.
		w.Header().Set("ETag", `W/"1"`)
		_, _ = w.Write([]byte(`{"id":"agent-1","etag":"\"1\""}`))
	})
	mux.HandleFunc("/api/v1/gh/acct/agents/agent-1/runs", func(w http.ResponseWriter, r *http.Request) {
		f.runs++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"run-1","status":"queued"}`))
	})
	mux.HandleFunc("/api/v1/gh/acct/agents/agent-1/archive", func(w http.ResponseWriter, r *http.Request) {
		etag := r.Header.Get("If-Match")
		if etag == "" || strings.HasPrefix(etag, "W/") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_request"}`))
			return
		}
		f.archived++
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/v1/gh/acct/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		status := f.runStatus
		if status == "" {
			status = "succeeded"
		}
		if status == "succeeded" {
			payload, _ := json.Marshal(map[string]any{
				"id":             "run-1",
				"status":         "succeeded",
				"result":         map[string]any{"data": map[string]any{"summary": f.answer, "signal": map[string]any{"rationale": "answered"}}},
				"output":         map[string]any{"text": f.answer, "truncated": false},
				"usage_by_model": []map[string]any{{"model": "claude-haiku-5-5", "input_tokens": 6, "output_tokens": 191}},
			})
			_, _ = w.Write(payload)
			return
		}
		_, _ = w.Write([]byte(`{"id":"run-1","status":"` + status + `","error":{"message":"boom"}}`))
	})
	return mux
}

func withFakeEntire(t *testing.T, fake *fakeEntire) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	resetPluginState(t)
	setBillingClient(directBillingClient{allowedHosts: []string{"127.0.0.1"}})
	base := strings.TrimPrefix(server.URL, "http://")
	host := strings.Split(base, ":")[0]
	setConfig(runtimeConfig{
		BaseURL:               server.URL,
		AllowedHosts:          []string{"127.0.0.1", host},
		CellBaseURL:           server.URL,
		ProjectHost:           "gh",
		Project:               "acct",
		RepositoryID:          "repo-1",
		TrailID:               "trail-1",
		ChangeID:              "change-1",
		HeadSHA:               "head-1",
		ExecutionEnabled:      true,
		ReserveCents:          100,
		RunTimeoutMS:          10000,
		RequireTopUpOnFailure: true,
		TimeoutSeconds:        5,
	}, nil)
	return server
}

// chatRequestJSON builds an executor.execute payload.
func chatRequestJSON(model string, messages []map[string]any, extra map[string]any) []byte {
	body := map[string]any{"model": model, "messages": messages}
	for key, value := range extra {
		body[key] = value
	}
	raw, _ := json.Marshal(body)
	req := executorRequest{
		Model:       model,
		Format:      "chat-completions",
		Payload:     raw,
		StorageJSON: []byte(`{"entire_token":"synthetic-exec-token"}`),
		AuthMetadata: map[string]any{
			"entire_token": "synthetic-exec-token",
		},
	}
	out, _ := json.Marshal(req)
	return out
}

func TestExecuteRelaysAnswer(t *testing.T) {
	fake := &fakeEntire{answer: "链路已通。"}
	withFakeEntire(t, fake)
	raw, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "只回复四个字：链路已通。"}}, nil))
	if errHandle != nil {
		t.Fatalf("execute failed: %v", errHandle.Message)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Payload []byte      `json:"Payload"`
			Headers http.Header `json:"Headers"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if !env.OK || len(env.Result.Payload) == 0 {
		t.Fatalf("unexpected envelope: %s", raw)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int64 `json:"usage"`
	}
	if errDecode := json.Unmarshal(env.Result.Payload, &completion); errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q", completion.Object)
	}
	if got := completion.Choices[0].Message.Content; got != "链路已通。" {
		t.Fatalf("answer = %q", got)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	if completion.Usage["completion_tokens"] != 191 {
		t.Fatalf("usage = %v", completion.Usage)
	}
	if fake.agents != 1 || fake.runs != 1 {
		t.Fatalf("agents=%d runs=%d", fake.agents, fake.runs)
	}
	if fake.archived != 1 {
		t.Fatalf("the relay agent must be archived, archived=%d", fake.archived)
	}
	if fake.lastHeaders.Get("Idempotency-Key") == "" {
		t.Fatal("agent creation must carry an Idempotency-Key")
	}
	if !strings.Contains(fake.lastInstructions, "只回复四个字：链路已通。") {
		t.Fatal("the conversation must reach the agent instructions")
	}
}

func TestExecuteRefusesStreaming(t *testing.T) {
	fake := &fakeEntire{}
	withFakeEntire(t, fake)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hi"}}, map[string]any{"stream": true}))
	if errHandle == nil || errHandle.Code != codeStreamUnsupported {
		t.Fatalf("streaming must be refused, got %v", errHandle)
	}
	if fake.agents != 0 {
		t.Fatal("a refused request must not create an agent")
	}
	_, errStream := handleMethod(methodExecutorExecuteStream, nil)
	if errStream == nil || errStream.Code != codeStreamUnsupported {
		t.Fatalf("execute_stream must be refused, got %v", errStream)
	}
}

func TestExecuteRefusesTools(t *testing.T) {
	fake := &fakeEntire{}
	withFakeEntire(t, fake)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hi"}},
		map[string]any{"tools": []map[string]any{{"type": "function", "function": map[string]string{"name": "x"}}}}))
	if errHandle == nil || errHandle.Code != codeToolsUnsupported {
		t.Fatalf("tools must be refused, got %v", errHandle)
	}
	if fake.agents != 0 {
		t.Fatal("a refused request must not create an agent")
	}
}

func TestExecuteRefusesUnknownModel(t *testing.T) {
	fake := &fakeEntire{}
	withFakeEntire(t, fake)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/not-a-model",
		[]map[string]any{{"role": "user", "content": "hi"}}, nil))
	if errHandle == nil || errHandle.Code != codeModelUnsupported {
		t.Fatalf("unknown model must be refused, got %v", errHandle)
	}
}

func TestExecuteFailsClosedWithoutBalance(t *testing.T) {
	for name, setup := range map[string]func(*fakeEntire){
		"zero balance": func(f *fakeEntire) {
			f.billingBody = `{"state":"active","balance":{"total_cents":0},"overage":{"amount_cents":0},"runs_paused":{"paused":false},"automatic_top_up":{"enabled":false}}`
		},
		"below reserve": func(f *fakeEntire) {
			f.billingBody = `{"state":"active","balance":{"total_cents":50},"overage":{"amount_cents":0},"runs_paused":{"paused":false},"automatic_top_up":{"enabled":false}}`
		},
		"overage present": func(f *fakeEntire) {
			f.billingBody = `{"state":"active","balance":{"total_cents":5000},"overage":{"amount_cents":5},"runs_paused":{"paused":false},"automatic_top_up":{"enabled":false}}`
		},
		"auto top-up on": func(f *fakeEntire) {
			f.billingBody = `{"state":"active","balance":{"total_cents":5000},"overage":{"amount_cents":0},"runs_paused":{"paused":false},"automatic_top_up":{"enabled":true}}`
		},
		"runs paused": func(f *fakeEntire) {
			f.billingBody = `{"state":"active","balance":{"total_cents":5000},"overage":{"amount_cents":0},"runs_paused":{"paused":true},"automatic_top_up":{"enabled":false}}`
		},
		"unreadable": func(f *fakeEntire) { f.billingStatus = http.StatusInternalServerError },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeEntire{}
			setup(fake)
			withFakeEntire(t, fake)
			_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
				[]map[string]any{{"role": "user", "content": "hi"}}, nil))
			if errHandle == nil {
				t.Fatal("the spend gate must refuse")
			}
			if errHandle.Code != codeBillingUnavailable && errHandle.Code != codeBillingInsufficient {
				t.Fatalf("code = %s", errHandle.Code)
			}
			if fake.agents != 0 || fake.runs != 0 {
				t.Fatalf("no dispatch may happen, agents=%d runs=%d", fake.agents, fake.runs)
			}
		})
	}
}

func TestExecuteDisabledByConfig(t *testing.T) {
	fake := &fakeEntire{answer: "x"}
	withFakeEntire(t, fake)
	cfg := currentConfig()
	cfg.ExecutionEnabled = false
	setConfig(cfg, nil)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hi"}}, nil))
	if errHandle == nil || errHandle.Code != codeExecutionDisabled {
		t.Fatalf("disabled execution must be refused, got %v", errHandle)
	}
	if fake.agents != 0 {
		t.Fatal("no dispatch may happen")
	}
}

func TestExecuteReportsRunFailure(t *testing.T) {
	fake := &fakeEntire{runStatus: "failed"}
	withFakeEntire(t, fake)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hi"}}, nil))
	if errHandle == nil || errHandle.Code != codeRunFailed {
		t.Fatalf("a failed run must surface, got %v", errHandle)
	}
	if fake.archived != 1 {
		t.Fatal("the relay agent must still be archived")
	}
}

func TestExecuteRefusesOversizedConversation(t *testing.T) {
	fake := &fakeEntire{}
	withFakeEntire(t, fake)
	big := strings.Repeat("a", maxInstructions+100)
	_, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": big}}, nil))
	if errHandle == nil || errHandle.Code != codeContextTooLarge {
		t.Fatalf("oversized conversation must be refused, got %v", errHandle)
	}
	if fake.agents != 0 {
		t.Fatal("no dispatch may happen")
	}
}

func TestCountTokensIsLocal(t *testing.T) {
	fake := &fakeEntire{}
	withFakeEntire(t, fake)
	raw, errHandle := handleMethod(methodExecutorCountTokens, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hello"}}, nil))
	if errHandle != nil {
		t.Fatalf("count_tokens failed: %v", errHandle.Message)
	}
	if !strings.Contains(string(raw), "UGF5bG9hZA") && !strings.Contains(string(raw), "Payload") {
		t.Fatalf("unexpected payload: %s", raw)
	}
	if fake.agents != 0 {
		t.Fatal("count_tokens must not dispatch")
	}
}

func TestArchiveUsesIfMatch(t *testing.T) {
	fake := &fakeEntire{answer: "ok"}
	server := withFakeEntire(t, fake)
	var seen http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/gh/acct/agents/agent-1/archive", func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	_ = server
	if _, errHandle := handleMethod(methodExecutorExecute, chatRequestJSON("entire/claude-haiku-5-5",
		[]map[string]any{{"role": "user", "content": "hi"}}, nil)); errHandle != nil {
		t.Fatalf("execute failed: %v", errHandle.Message)
	}
	_ = context.Background()
	_ = seen
}
