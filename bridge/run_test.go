package bridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yg/cpa-entire-plugin/bridge"
)

const (
	runsPath = "/api/v1/gh/acme-widgets/agents/01HZZTESTAGENT0000000000/runs"
	runPath  = "/api/v1/gh/acme-widgets/runs/"
)

// stableKey is fixed so the test asserts the exact bytes on the wire.
const stableKey = "cpa-bridge-0123456789abcdef0123456789abcdef"

// runBody builds a Run at a given status.
func runBody(id, status string) map[string]any {
	return map[string]any{
		"id":            id,
		"project_id":    "01HZZTESTPROJECT000000000",
		"repository_id": "01HZZTESTREPO000000000000",
		"status":        status,
		"error":         nil,
		"diagnostic":    nil,
		"attempt":       1,
		"replayed":      false,
		"target": map[string]any{
			"repository_id": "01HZZTESTREPO000000000000",
			"trail_id":      "01HZZTESTTRAIL0000000000",
			"change_id":     "01HZZTESTCHANGE000000000",
			"ref":           "feature/x",
			"head_sha":      "abc123",
			"base_ref":      "main",
			"base_sha":      "def456",
		},
		"result":     map[string]any{"result_type": "review_changes", "data": map[string]any{}, "created_at": "2026-10-30T00:00:00Z"},
		"output":     map[string]any{"text": nil, "truncated": false},
		"created_at": "2026-10-30T00:00:00Z",
		"updated_at": "2026-10-30T00:00:01Z",
	}
}

func TestStartRunRequestShape(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusAccepted, runBody("01HZZTESTRUN0000000000000", bridge.RunQueued))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	run, err := client.StartRun(context.Background(), bridge.StartRunOptions{IdempotencyKey: stableKey})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if run.ID != "01HZZTESTRUN0000000000000" || run.Status != bridge.RunQueued {
		t.Fatalf("unexpected run: %+v", run)
	}

	req := cell.last(t)
	if req.Method != http.MethodPost || req.Path != runsPath {
		t.Fatalf("method=%s path=%s", req.Method, req.Path)
	}
	if got := req.Header.Get("Idempotency-Key"); got != stableKey {
		t.Fatalf("Idempotency-Key = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	target, ok := body["target"].(map[string]any)
	if !ok {
		t.Fatalf("body has no target object: %s", req.Body)
	}
	if target["trail_id"] != "01HZZTESTTRAIL0000000000" || target["change_id"] != "01HZZTESTCHANGE000000000" {
		t.Fatalf("target = %v", target)
	}
	if _, ok := body["head_sha"]; ok {
		t.Fatalf("head_sha sent although it was not configured: %s", req.Body)
	}
	// RunCreate has no chat surface. None of these may appear.
	for _, forbidden := range []string{"messages", "input", "tools", "stream", "model", "temperature"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("RunCreate body carries %q: %s", forbidden, req.Body)
		}
	}
	assertNoTokenLeak(t, "run body", string(req.Body))
}

func TestStartRunRequiresTarget(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("no request should be sent without a target")
	})
	cfg := testConfig("https://core.invalid", cell.URL)
	cfg.TrailID = ""
	cfg.ChangeID = ""
	client := newClient(t, cfg, cell)
	_, err := client.StartRun(context.Background(), bridge.StartRunOptions{})
	if !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if n := len(cell.requests()); n != 0 {
		t.Fatalf("expected no requests, got %d", n)
	}
}

func TestStartRunRejectsLongIdempotencyKey(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("no request should be sent with an over-long key")
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	_, err := client.StartRun(context.Background(), bridge.StartRunOptions{IdempotencyKey: strings.Repeat("k", bridge.MaxIdempotencyKeyLength+1)})
	if !errors.Is(err, bridge.ErrInvalidConfig) || !strings.Contains(err.Error(), "128") {
		t.Fatalf("want an ErrInvalidConfig naming the 128 ceiling, got %v", err)
	}
	if n := len(cell.requests()); n != 0 {
		t.Fatalf("expected no requests, got %d", n)
	}
}

func TestStartRunGeneratesIdempotencyKeyWhenUnset(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusAccepted, runBody("01HZZTESTRUN0000000000001", bridge.RunQueued))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	if _, err := client.StartRun(context.Background(), bridge.StartRunOptions{}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	key := cell.last(t).Header.Get("Idempotency-Key")
	if key == "" || len(key) > bridge.MaxIdempotencyKeyLength {
		t.Fatalf("generated key %q is empty or too long", key)
	}
	if !strings.HasPrefix(key, "cpa-bridge-") {
		t.Fatalf("generated key %q is not recognisable as ours", key)
	}
}

func TestStartRunReplayHeader(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(t, w, http.StatusAccepted, runBody("01HZZTESTRUN0000000000002", bridge.RunRunning))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	run, err := client.StartRun(context.Background(), bridge.StartRunOptions{IdempotencyKey: stableKey})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if !run.Replayed {
		t.Fatal("Idempotency-Replayed: true was not surfaced")
	}
}

func TestStartRunConflictIsTyped(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusConflict, map[string]any{
			"type": "about:blank", "title": "Conflict", "status": 409,
			"detail": "the same key was used with a different body", "code": "idempotency_conflict", "request_id": "req_409",
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	_, err := client.StartRun(context.Background(), bridge.StartRunOptions{IdempotencyKey: stableKey})
	if !errors.Is(err, bridge.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// sequence serves successive GET run responses, repeating the last once.
type sequence struct {
	mu    sync.Mutex
	steps []map[string]any
	calls int
}

func (s *sequence) next() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	return s.steps[i]
}

func TestPollRunReachesSucceededWithRationale(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000003", bridge.RunSucceeded)
	final["result"] = map[string]any{
		"result_type": "review_changes",
		"created_at":  "2026-10-30T00:00:05Z",
		"data": map[string]any{
			"summary": "Looks good overall.",
			"signal": map[string]any{
				"value": 87, "value_type": "percent", "polarity": "higher_is_better",
				"rationale": "The change is coherent and the tests match the behaviour.",
			},
		},
	}
	final["review_changes_result"] = map[string]any{
		"summary": "Looks good overall.",
		"signal": map[string]any{
			"value": 87, "value_type": "percent", "polarity": "higher_is_better",
			"rationale": "The change is coherent and the tests match the behaviour.",
		},
		"findings": []any{}, "prior_findings": []any{}, "review_id": "01HZZREVIEW",
	}
	final["output"] = map[string]any{"text": "ignored when the signal has a rationale", "truncated": false}

	seq := &sequence{steps: []map[string]any{
		runBody("01HZZTESTRUN0000000000003", bridge.RunQueued),
		runBody("01HZZTESTRUN0000000000003", bridge.RunRunning),
		final,
	}}
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, seq.next())
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	var seen []string
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000003", bridge.PollOptions{
		Interval: time.Millisecond,
		OnStatus: func(r *bridge.Run) { seen = append(seen, r.Status) },
	})
	if res.Err != nil {
		t.Fatalf("PollRun: %v", res.Err)
	}
	if res.Source != "result.signal.rationale" {
		t.Fatalf("source = %q", res.Source)
	}
	if !strings.Contains(res.Text, "coherent and the tests match") {
		t.Fatalf("rationale not extracted: %q", res.Text)
	}
	if strings.Join(seen, ",") != "queued,running,succeeded" {
		t.Fatalf("statuses = %v", seen)
	}
	if res.Run.Status != bridge.RunSucceeded || !res.Run.Terminal() {
		t.Fatalf("result run = %+v", res.Run)
	}
	// Every poll went to the project run route with the bearer attached.
	for _, req := range cell.requests() {
		if req.Method != http.MethodGet || req.Path != runPath+"01HZZTESTRUN0000000000003" {
			t.Fatalf("unexpected poll %s %s", req.Method, req.Path)
		}
		if req.Header.Get("Authorization") != "Bearer "+testToken {
			t.Fatalf("poll without bearer: %v", req.Header)
		}
	}
}

func TestPollRunFallsBackToOutput(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000004", bridge.RunSucceeded)
	final["review_changes_result"] = nil
	final["output"] = map[string]any{"text": "plain agent answer", "truncated": false}
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000004", bridge.PollOptions{Interval: time.Millisecond})
	if res.Err != nil {
		t.Fatalf("PollRun: %v", res.Err)
	}
	if res.Text != "plain agent answer" || res.Source != "output.text" || res.Truncated {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestPollRunTruncatedOutputIsAnError(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000005", bridge.RunSucceeded)
	final["review_changes_result"] = nil
	final["output"] = map[string]any{"text": "the first 64 KiB", "truncated": true}
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000005", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrOutputTruncated) {
		t.Fatalf("want ErrOutputTruncated, got %v", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "truncated") {
		t.Fatalf("error should say the output is truncated: %v", res.Err)
	}
	if res.Text != "the first 64 KiB" {
		t.Fatalf("the partial text should still be returned, got %q", res.Text)
	}
}

func TestPollRunSucceededButEmptyIsAnError(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000006", bridge.RunSucceeded)
	final["review_changes_result"] = nil
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000006", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrOutputEmpty) {
		t.Fatalf("want ErrOutputEmpty, got %v", res.Err)
	}
}

func TestPollRunFailedCarriesDiagnostic(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000007", bridge.RunFailed)
	final["error"] = map[string]any{"code": "billing_trial_ended", "message": "trial credit is exhausted"}
	final["diagnostic"] = "exit status 1\nmodel refused the request"
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000007", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrRunFailed) {
		t.Fatalf("want ErrRunFailed, got %v", res.Err)
	}
	for _, want := range []string{"billing_trial_ended", "trial credit is exhausted", "exit status 1"} {
		if !strings.Contains(res.Err.Error(), want) {
			t.Fatalf("error %q missing %q", res.Err.Error(), want)
		}
	}
}

func TestPollRunCanceledIsAnError(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000008", bridge.RunCanceled)
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000008", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrRunCanceled) {
		t.Fatalf("want ErrRunCanceled, got %v", res.Err)
	}
}

func TestPollRunTimedOutStatusIsAnError(t *testing.T) {
	t.Parallel()
	final := runBody("01HZZTESTRUN0000000000009", bridge.RunTimedOut)
	final["error"] = map[string]any{"message": "the harness exceeded timeout_ms"}
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, final)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000009", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrRunFailed) || !strings.Contains(res.Err.Error(), "timed_out") {
		t.Fatalf("want ErrRunFailed for timed_out, got %v", res.Err)
	}
}

// TestPollRunCancelsOnContextCancel is the cancellation path: a context that
// ends mid-run must produce a best-effort cancel against the run's cancel route.
func TestPollRunCancelsOnContextCancel(t *testing.T) {
	t.Parallel()
	var cancels int32
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			atomic.AddInt32(&cancels, 1)
			writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000010", bridge.RunCanceled))
			return
		}
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000010", bridge.RunRunning))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res := client.PollRun(ctx, "01HZZTESTRUN0000000000010", bridge.PollOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrContextCanceled) {
		t.Fatalf("want ErrContextCanceled, got %v", res.Err)
	}
	if got := atomic.LoadInt32(&cancels); got != 1 {
		t.Fatalf("cancel route hit %d times, want 1", got)
	}
	if !strings.Contains(res.Err.Error(), "cancel requested") {
		t.Fatalf("error should report the cancel attempt: %v", res.Err)
	}
	// The cancel must carry the bearer and hit the documented route.
	var cancelReq recordedRequest
	for _, req := range cell.requests() {
		if strings.HasSuffix(req.Path, "/cancel") {
			cancelReq = req
		}
	}
	if cancelReq.Method != http.MethodPost || cancelReq.Path != runPath+"01HZZTESTRUN0000000000010/cancel" {
		t.Fatalf("cancel request = %s %s", cancelReq.Method, cancelReq.Path)
	}
	if cancelReq.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("cancel without bearer: %v", cancelReq.Header)
	}
}

func TestPollRunCancelFailureIsReported(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			writeJSON(t, w, http.StatusForbidden, map[string]any{
				"title": "Forbidden", "status": 403, "detail": "no push on the repository",
				"code": "forbidden", "request_id": "req_403", "type": "about:blank",
			})
			return
		}
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000011", bridge.RunQueued))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res := client.PollRun(ctx, "01HZZTESTRUN0000000000011", bridge.PollOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrContextCanceled) {
		t.Fatalf("want ErrContextCanceled, got %v", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "cancel also failed") {
		t.Fatalf("the failed cancel must be reported: %v", res.Err)
	}
}

func TestPollRunCancelCanBeDisabled(t *testing.T) {
	t.Parallel()
	var cancels int32
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			atomic.AddInt32(&cancels, 1)
		}
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000012", bridge.RunRunning))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	off := false

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res := client.PollRun(ctx, "01HZZTESTRUN0000000000012", bridge.PollOptions{Interval: 5 * time.Millisecond, CancelOnAbort: &off})
	if !errors.Is(res.Err, bridge.ErrContextCanceled) {
		t.Fatalf("want ErrContextCanceled, got %v", res.Err)
	}
	if got := atomic.LoadInt32(&cancels); got != 0 {
		t.Fatalf("cancel route hit %d times with CancelOnAbort=false", got)
	}
}

func TestPollRunLocalDeadlineCancels(t *testing.T) {
	t.Parallel()
	var cancels int32
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			atomic.AddInt32(&cancels, 1)
			writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000013", bridge.RunCanceled))
			return
		}
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000013", bridge.RunQueued))
	})
	cfg := testConfig("https://core.invalid", cell.URL)
	cfg.PollTimeout = 25 * time.Millisecond
	cfg.PollInterval = 5 * time.Millisecond
	client := newClient(t, cfg, cell)

	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000013", bridge.PollOptions{})
	if !errors.Is(res.Err, bridge.ErrPollTimeout) {
		t.Fatalf("want ErrPollTimeout, got %v", res.Err)
	}
	if got := atomic.LoadInt32(&cancels); got != 1 {
		t.Fatalf("cancel route hit %d times, want 1", got)
	}
}

// TestRunOneShot drives StartRun and PollRun together through the public entry
// point, against one stub that serves both routes.
func TestRunOneShot(t *testing.T) {
	t.Parallel()
	var polls int32
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs"):
			writeJSON(t, w, http.StatusAccepted, runBody("01HZZTESTRUN0000000000014", bridge.RunQueued))
		case r.Method == http.MethodGet:
			n := atomic.AddInt32(&polls, 1)
			if n < 2 {
				writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000014", bridge.RunRunning))
				return
			}
			final := runBody("01HZZTESTRUN0000000000014", bridge.RunSucceeded)
			final["review_changes_result"] = map[string]any{
				"summary": "ok", "findings": []any{}, "prior_findings": []any{}, "review_id": nil,
				"signal": map[string]any{"value": "medium", "value_type": "size", "polarity": "lower_is_better", "rationale": "small, contained change"},
			}
			writeJSON(t, w, http.StatusOK, final)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeJSON(t, w, http.StatusNotImplemented, nil)
		}
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)

	res := client.Run(context.Background(), bridge.RunOptions{
		StartRunOptions: bridge.StartRunOptions{IdempotencyKey: stableKey},
		PollOptions:     bridge.PollOptions{Interval: time.Millisecond},
	})
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.Text != "small, contained change" || res.Source != "result.signal.rationale" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRunPropagatesStartError(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusForbidden, map[string]any{
			"title": "Forbidden", "status": 403, "detail": "canManageAgents is required",
			"code": "forbidden", "request_id": "req_403", "type": "about:blank",
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	res := client.Run(context.Background(), bridge.RunOptions{StartRunOptions: bridge.StartRunOptions{IdempotencyKey: stableKey}})
	if res.Err == nil {
		t.Fatal("expected the start failure to be returned")
	}
	var httpErr *bridge.HTTPError
	if !errors.As(res.Err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Fatalf("want a 403 HTTPError, got %v", res.Err)
	}
	for _, req := range cell.requests() {
		if req.Method == http.MethodGet {
			t.Fatal("a failed start must not be polled")
		}
	}
}

func TestGetRunNotFound(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"title": "Not Found", "status": 404, "detail": "run not found",
			"code": "not_found", "request_id": "req_404", "type": "about:blank",
		})
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	_, err := client.GetRun(context.Background(), "01HZZMISSING")
	if !errors.Is(err, bridge.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPollRunContextAlreadyCanceled(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000015", bridge.RunCanceled))
			return
		}
		t.Error("no poll should happen with a canceled context")
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000015", bridge.RunRunning))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := client.PollRun(ctx, "01HZZTESTRUN0000000000015", bridge.PollOptions{Interval: time.Millisecond})
	if !errors.Is(res.Err, bridge.ErrContextCanceled) {
		t.Fatalf("want ErrContextCanceled, got %v", res.Err)
	}
}

// TestPollRunIgnoresConfigPollTimeoutWhenOverridden pins the precedence: the
// per-call value wins over the configuration.
func TestPollRunIgnoresConfigPollTimeoutWhenOverridden(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000016", bridge.RunCanceled))
			return
		}
		final := runBody("01HZZTESTRUN0000000000016", bridge.RunSucceeded)
		final["output"] = map[string]any{"text": "done", "truncated": false}
		writeJSON(t, w, http.StatusOK, final)
	})
	cfg := testConfig("https://core.invalid", cell.URL)
	cfg.PollTimeout = time.Nanosecond
	client := newClient(t, cfg, cell)
	res := client.PollRun(context.Background(), "01HZZTESTRUN0000000000016", bridge.PollOptions{Timeout: time.Minute, Interval: time.Millisecond})
	if res.Err != nil {
		t.Fatalf("per-call timeout should win: %v", res.Err)
	}
	if res.Text != "done" {
		t.Fatalf("text = %q", res.Text)
	}
}

func ExampleClient_Run() {
	// The shape a CPA executor uses: one call, one answer, no stream.
	var c *bridge.Client
	if c == nil {
		fmt.Println("client is provided by New")
		return
	}
	res := c.Run(context.Background(), bridge.RunOptions{
		StartRunOptions: bridge.StartRunOptions{IdempotencyKey: "retryable-key"},
		PollOptions:     bridge.PollOptions{Interval: 10 * time.Second},
	})
	if res.Err != nil {
		fmt.Println("run failed:", res.Err)
		return
	}
	fmt.Println(res.Source, res.Text)
}
