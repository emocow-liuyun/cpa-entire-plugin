package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Run status values from the Run schema.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunTimedOut  = "timed_out"
	RunCanceled  = "canceled"
)

// RunTargetInput is the exact RunCreate target: a Trail and one of its
// Changes. A Trail may hold several Changes, so the Change is always named.
type RunTargetInput struct {
	TrailID  string `json:"trail_id"`
	ChangeID string `json:"change_id"`
}

// RunCreate is the exact RunCreate request body. There is no messages, input,
// tools or stream member: this is not a chat request.
type RunCreate struct {
	Target  RunTargetInput `json:"target"`
	HeadSHA string         `json:"head_sha,omitempty"`
}

// Validate checks the local requirement: both ULIDs present.
func (r *RunCreate) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil RunCreate", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.Target.TrailID) == "" {
		return fmt.Errorf("%w: target.trail_id is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.Target.ChangeID) == "" {
		return fmt.Errorf("%w: target.change_id is required", ErrInvalidConfig)
	}
	return nil
}

// RunOutput is RunOutput. truncated true means text is a prefix of the full
// output, and no full-text route is implemented here.
type RunOutput struct {
	Text      *string `json:"text"`
	Truncated bool    `json:"truncated"`
}

// RunSignalReading is RunSignalReading: value, rationale, value_type and
// polarity.
type RunSignalReading struct {
	Value     json.RawMessage `json:"value"`
	Rationale string          `json:"rationale"`
	ValueType string          `json:"value_type"`
	Polarity  string          `json:"polarity"`
}

// RunReviewChangesResult is RunReviewChangesResult. The run's prose answer is
// usually summary plus signal.rationale; findings carry structured review
// output and are not rendered as chat text.
type RunReviewChangesResult struct {
	Summary       *string           `json:"summary"`
	Signal        *RunSignalReading `json:"signal"`
	Findings      json.RawMessage   `json:"findings"`
	PriorFindings json.RawMessage   `json:"prior_findings"`
	ReviewID      *string           `json:"review_id"`
}

// RunError is RunError: why the run failed, timed out or was canceled by the
// system. code is set only for billing refusals.
type RunError struct {
	Code    *string `json:"code"`
	Message string  `json:"message"`
}

// Run is the subset of the Run schema the bridge uses. result.data is kept raw
// because RunResult.data is a free-form object.
type Run struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Error      *RunError `json:"error"`
	Diagnostic *string   `json:"diagnostic"`
	Attempt    int64     `json:"attempt"`
	Replayed   bool      `json:"replayed"`
	Target     struct {
		RepositoryID string `json:"repository_id"`
		TrailID      string `json:"trail_id"`
		ChangeID     string `json:"change_id"`
		Ref          string `json:"ref"`
		HeadSHA      string `json:"head_sha"`
	} `json:"target"`
	Agent struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
	} `json:"agent"`
	Runtime struct {
		Harness         string  `json:"harness"`
		Model           *string `json:"model"`
		ReasoningEffort *string `json:"reasoning_effort"`
		TimeoutMS       int64   `json:"timeout_ms"`
	} `json:"runtime"`
	Result struct {
		ResultType string          `json:"result_type"`
		Data       json.RawMessage `json:"data"`
		CreatedAt  string          `json:"created_at"`
	} `json:"result"`
	ReviewChangesResult *RunReviewChangesResult `json:"review_changes_result"`
	Output              *RunOutput              `json:"output"`
	UsageByModel        json.RawMessage         `json:"usage_by_model"`
	CreatedAt           string                  `json:"created_at"`
	UpdatedAt           string                  `json:"updated_at"`
}

// Terminal reports whether no further status change can occur.
func (r *Run) Terminal() bool {
	if r == nil {
		return false
	}
	switch r.Status {
	case RunSucceeded, RunFailed, RunTimedOut, RunCanceled:
		return true
	}
	return false
}

// Result describes what a finished run produced and, when it did not succeed,
// why. Text is never truncated silently: Truncated is set and ExtractText
// reports ErrOutputTruncated.
type Result struct {
	Run *Run
	// Text is the run's answer, from one of the two documented sources.
	Text string
	// Source names where Text came from: "result.signal.rationale", "summary",
	// or "output.text".
	Source string
	// Truncated reports RunOutput.truncated for output-sourced text.
	Truncated bool
	// Replay reports that the Idempotency-Key named an earlier identical run,
	// so this run may already have happened.
	Replayed bool
	// Err is nil on success; otherwise a sentinel from this package, with the
	// diagnostic attached where the server provided one.
	Err error
}

// ExtractText splits a terminal Run into text and verdict.
//
// Preference order, matching what the routes actually carry:
//
//  1. result.signal.rationale (RunReviewChangesResult.signal.rationale)
//  2. review_changes_result.summary
//  3. output.text
//
// A non-succeeded run yields ErrRunFailed or ErrRunCanceled with the run's
// error message and the credential-redacted diagnostic tail. A succeeded run
// with neither source yields ErrOutputEmpty; a succeeded run whose only source
// is truncated output yields both the text and ErrOutputTruncated.
func ExtractText(run *Run) Result {
	res := Result{Run: run}
	if run == nil {
		res.Err = fmt.Errorf("%w: nil run", ErrInvalidConfig)
		return res
	}
	res.Replayed = run.Replayed

	if rationale := rationaleOf(run); rationale != "" {
		res.Text = rationale
		res.Source = "result.signal.rationale"
	}
	if res.Text == "" && run.ReviewChangesResult != nil && run.ReviewChangesResult.Summary != nil {
		if s := strings.TrimSpace(*run.ReviewChangesResult.Summary); s != "" {
			res.Text = *run.ReviewChangesResult.Summary
			res.Source = "summary"
		}
	}
	if res.Text == "" && run.Output != nil && run.Output.Text != nil && *run.Output.Text != "" {
		res.Text = *run.Output.Text
		res.Source = "output.text"
		res.Truncated = run.Output.Truncated
	}

	switch run.Status {
	case RunSucceeded:
		if res.Truncated {
			res.Err = fmt.Errorf("%w: output.text is %d chars and marked truncated; no full-text route is implemented", ErrOutputTruncated, len(res.Text))
			return res
		}
		if res.Text == "" {
			res.Err = fmt.Errorf("%w: status succeeded but result.signal.rationale, summary and output.text are all empty", ErrOutputEmpty)
		}
		return res
	case RunFailed, RunTimedOut:
		res.Err = fmt.Errorf("%w: status %s%s", ErrRunFailed, run.Status, runFailureDetail(run))
		return res
	case RunCanceled:
		res.Err = fmt.Errorf("%w: status canceled%s", ErrRunCanceled, runFailureDetail(run))
		return res
	default:
		res.Err = fmt.Errorf("%w: run %s is still %q", ErrPollTimeout, run.ID, run.Status)
		return res
	}
}

// rationaleOf finds result.signal.rationale.
//
// The published Run.result is RunResult{result_type, data, created_at} with
// data as a free-form object; RunReviewChangesResult is what a review run puts
// there, and the same object is also mirrored at review_changes_result. Both
// are checked so neither reading is silently skipped.
func rationaleOf(run *Run) string {
	if run.ReviewChangesResult != nil && run.ReviewChangesResult.Signal != nil {
		if r := strings.TrimSpace(run.ReviewChangesResult.Signal.Rationale); r != "" {
			return r
		}
	}
	if len(run.Result.Data) == 0 {
		return ""
	}
	var data struct {
		Signal *RunSignalReading `json:"signal"`
	}
	if err := json.Unmarshal(run.Result.Data, &data); err != nil || data.Signal == nil {
		return ""
	}
	return strings.TrimSpace(data.Signal.Rationale)
}

func runFailureDetail(run *Run) string {
	var parts []string
	if run.Error != nil {
		if run.Error.Code != nil && *run.Error.Code != "" {
			parts = append(parts, "code="+*run.Error.Code)
		}
		if msg := strings.TrimSpace(run.Error.Message); msg != "" {
			parts = append(parts, "error="+redactErrorBody(msg))
		}
	}
	if run.Diagnostic != nil {
		if d := redactErrorBody(*run.Diagnostic); d != "" {
			parts = append(parts, "diagnostic="+d)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// ---------------------------------------------------------------------------
// POST run, poll, cancel
// ---------------------------------------------------------------------------

// StartRunOptions carries the per-run inputs.
type StartRunOptions struct {
	// TrailID, ChangeID and HeadSHA default to the configured target.
	TrailID  string
	ChangeID string
	HeadSHA  string
	// AgentID defaults to the configured agent.
	AgentID string
	// IdempotencyKey is required by the route (1-128 chars). When empty one is
	// generated; because a generated key is new on every call, a retry of an
	// interrupted start must pass the same key explicitly.
	IdempotencyKey string
}

// newIdempotencyKey returns a random 128-bit key in hex.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("bridge: generate idempotency key: %w", err)
	}
	return "cpa-bridge-" + hex.EncodeToString(b[:]), nil
}

// StartRun performs POST {cell}/{host}/{project}/agents/{id}/runs with
// Idempotency-Key and returns the accepted Run (202). It starts an agent run:
// this is the one call in the package that can consume credit, so it is never
// invoked implicitly.
func (c *Client) StartRun(ctx context.Context, opts StartRunOptions) (*Run, error) {
	agentID := opts.AgentID
	if agentID == "" {
		agentID = c.cfg.AgentID
	}
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", ErrNotConfigured)
	}
	trail, change, head := opts.TrailID, opts.ChangeID, opts.HeadSHA
	if trail == "" {
		trail = c.cfg.TrailID
	}
	if change == "" {
		change = c.cfg.ChangeID
	}
	if head == "" {
		head = c.cfg.HeadSHA
	}
	body := &RunCreate{Target: RunTargetInput{TrailID: strings.TrimSpace(trail), ChangeID: strings.TrimSpace(change)}, HeadSHA: strings.TrimSpace(head)}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(opts.IdempotencyKey)
	if key == "" {
		generated, err := newIdempotencyKey()
		if err != nil {
			return nil, err
		}
		key = generated
	}
	if len(key) > MaxIdempotencyKeyLength {
		return nil, fmt.Errorf("%w: idempotency key is %d chars, maximum %d", ErrInvalidConfig, len(key), MaxIdempotencyKeyLength)
	}
	path, err := c.agentPath(agentID)
	if err != nil {
		return nil, err
	}
	u, err := c.cellURL(path + "/runs")
	if err != nil {
		return nil, err
	}
	raw, resp, err := c.do(ctx, requestOptions{
		method:         http.MethodPost,
		url:            u,
		body:           body,
		idempotencyKey: key,
		accept:         "application/json",
	})
	if err != nil {
		return nil, err
	}
	var out Run
	if err := decodeInto(http.MethodPost, u, "Run", raw, &out); err != nil {
		return nil, err
	}
	if resp != nil && strings.EqualFold(resp.Header.Get("Idempotency-Replayed"), "true") {
		out.Replayed = true
	}
	if out.ID == "" {
		return nil, fmt.Errorf("bridge: POST %s accepted a run without an id", u)
	}
	return &out, nil
}

// RunPath is /{host}/{project}/runs/{id}.
func (c *Client) runPath(runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", fmt.Errorf("%w: run id is required", ErrNotConfigured)
	}
	prefix, err := c.cfg.ProjectPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/runs/" + escapeSegment(runID), nil
}

// GetRun performs GET {cell}/{host}/{project}/runs/{id}.
func (c *Client) GetRun(ctx context.Context, runID string) (*Run, error) {
	path, err := c.runPath(runID)
	if err != nil {
		return nil, err
	}
	u, err := c.cellURL(path)
	if err != nil {
		return nil, err
	}
	raw, _, err := c.do(ctx, requestOptions{method: http.MethodGet, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out Run
	if err := decodeInto(http.MethodGet, u, "Run", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelRun performs POST {cell}/{host}/{project}/runs/{id}/cancel and returns
// the run as it stands. Canceling a finished run changes nothing and returns it
// as it ended, so this is safe to call best-effort.
func (c *Client) CancelRun(ctx context.Context, runID string) (*Run, error) {
	path, err := c.runPath(runID)
	if err != nil {
		return nil, err
	}
	u, err := c.cellURL(path + "/cancel")
	if err != nil {
		return nil, err
	}
	raw, _, err := c.do(ctx, requestOptions{method: http.MethodPost, url: u, accept: "application/json"})
	if err != nil {
		return nil, err
	}
	var out Run
	if err := decodeInto(http.MethodPost, u, "Run", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollOptions tunes PollRun beyond the configuration defaults.
type PollOptions struct {
	// Interval is the gap between status reads; 0 uses cfg.PollInterval.
	Interval time.Duration
	// Timeout is the local deadline for the whole poll; 0 uses cfg.PollTimeout.
	Timeout time.Duration
	// OnStatus is called after each observed status, including the first, so a
	// caller can surface queued/running progress. It runs on the polling
	// goroutine and must not block.
	OnStatus func(*Run)
	// CancelOnAbort controls the best-effort cancel described on PollRun. nil
	// means true; set it false to leave an interrupted run alive server-side.
	CancelOnAbort *bool
}

// PollRun reads GET run until the status is terminal, the local deadline
// passes, or ctx ends.
//
// Cancellation path: when ctx ends (or the poll deadline is reached) the run is
// still in flight, so cancel(ctx-independent background context) is attempted
// best-effort. A cancel that fails is reported in the returned error; a cancel
// that succeeds does not hide the original cause. The returned *Result always
// carries the last observed Run when one was read.
func (c *Client) PollRun(ctx context.Context, runID string, opts PollOptions) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = c.cfg.PollInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = c.cfg.PollTimeout
	}
	deadline := c.now().Add(timeout)
	cancelOnAbort := opts.CancelOnAbort == nil || *opts.CancelOnAbort
	abort := func(last *Run, cause error) Result {
		if !cancelOnAbort {
			return Result{Run: last, Err: cause}
		}
		return c.abortPoll(ctx, runID, last, cause)
	}

	var last *Run
	for {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return abort(last, fmt.Errorf("%w: %v", ErrContextCanceled, err))
			}
			return Result{Run: last, Err: err}
		}
		last = run
		if opts.OnStatus != nil {
			opts.OnStatus(run)
		}
		if run.Terminal() {
			return ExtractText(run)
		}
		if c.now().After(deadline) {
			return abort(last, fmt.Errorf("%w after %s while status %q", ErrPollTimeout, timeout, run.Status))
		}
		if err := c.sleep(ctx, interval); err != nil {
			return abort(last, fmt.Errorf("%w: %v", ErrContextCanceled, err))
		}
		if c.now().After(deadline) {
			return abort(last, fmt.Errorf("%w after %s while status %q", ErrPollTimeout, timeout, run.Status))
		}
	}
}

// abortPoll attempts a best-effort cancel with a bounded, caller-independent
// context, and folds the outcome into the error text.
func (c *Client) abortPoll(ctx context.Context, runID string, last *Run, cause error) Result {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.RequestTimeout)
	defer cancel()
	canceled, err := c.CancelRun(cancelCtx, runID)
	if err != nil {
		return Result{Run: last, Err: fmt.Errorf("%w; cancel also failed: %v", cause, err)}
	}
	if canceled != nil {
		if canceled.Terminal() {
			last = canceled
		} else if last == nil {
			last = canceled
		}
	}
	return Result{Run: last, Err: fmt.Errorf("%w; cancel requested (status now %q)", cause, statusOf(last))}
}

func statusOf(r *Run) string {
	if r == nil {
		return "unknown"
	}
	return r.Status
}

// RunOptions is the one-shot entry point: start a run, poll it to a terminal
// status and extract the text.
type RunOptions struct {
	StartRunOptions
	PollOptions
}

// Run starts a run and polls it. It never retries the start: an uncertain
// start is the caller's decision, using the same Idempotency-Key.
func (c *Client) Run(ctx context.Context, opts RunOptions) Result {
	started, err := c.StartRun(ctx, opts.StartRunOptions)
	if err != nil {
		return Result{Err: err}
	}
	res := c.PollRun(ctx, started.ID, opts.PollOptions)
	if res.Run != nil && started.Replayed {
		// The start named an earlier identical run; keep that fact visible.
		res.Replayed = true
	}
	return res
}
