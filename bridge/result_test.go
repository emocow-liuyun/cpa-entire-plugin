package bridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

// TestExtractTextPrecedence pins the extraction order: a signal rationale wins
// over the summary, which wins over output.text.
func TestExtractTextPrecedence(t *testing.T) {
	t.Parallel()

	rationale := "signal rationale"
	summary := "review summary"
	output := "raw output"

	mk := func() *bridge.Run {
		run := &bridge.Run{ID: "run", Status: bridge.RunSucceeded}
		run.ReviewChangesResult = &bridge.RunReviewChangesResult{
			Summary: &summary,
			Signal:  &bridge.RunSignalReading{Rationale: rationale, ValueType: "percent", Polarity: "neutral"},
		}
		run.Output = &bridge.RunOutput{Text: &output, Truncated: false}
		return run
	}

	res := bridge.ExtractText(mk())
	if res.Err != nil || res.Text != rationale || res.Source != "result.signal.rationale" {
		t.Fatalf("rationale should win: %+v", res)
	}

	noRationale := mk()
	noRationale.ReviewChangesResult.Signal = &bridge.RunSignalReading{Rationale: "   "}
	res = bridge.ExtractText(noRationale)
	if res.Err != nil || res.Text != summary || res.Source != "summary" {
		t.Fatalf("blank rationale should fall through to summary: %+v", res)
	}

	outputOnly := mk()
	outputOnly.ReviewChangesResult = nil
	res = bridge.ExtractText(outputOnly)
	if res.Err != nil || res.Text != output || res.Source != "output.text" {
		t.Fatalf("output should be the last resort: %+v", res)
	}
}

func TestExtractTextTerminalChecks(t *testing.T) {
	t.Parallel()
	if res := bridge.ExtractText(nil); !errors.Is(res.Err, bridge.ErrInvalidConfig) {
		t.Fatalf("nil run: %v", res.Err)
	}

	running := &bridge.Run{ID: "r", Status: bridge.RunRunning}
	if res := bridge.ExtractText(running); !errors.Is(res.Err, bridge.ErrPollTimeout) || !strings.Contains(res.Err.Error(), "running") {
		t.Fatalf("non-terminal run: %v", res.Err)
	}

	msg := "backend exploded"
	code := "billing_over_limit"
	failed := &bridge.Run{ID: "r", Status: bridge.RunFailed, Error: &bridge.RunError{Code: &code, Message: msg}}
	if res := bridge.ExtractText(failed); !errors.Is(res.Err, bridge.ErrRunFailed) ||
		!strings.Contains(res.Err.Error(), code) || !strings.Contains(res.Err.Error(), msg) {
		t.Fatalf("failed run: %v", res.Err)
	}
}

func TestExtractTextRedactsDiagnostic(t *testing.T) {
	t.Parallel()
	diag := "stderr: Authorization: Bearer " + testToken + "\nboom"
	failed := &bridge.Run{ID: "r", Status: bridge.RunFailed, Diagnostic: &diag}
	res := bridge.ExtractText(failed)
	if res.Err == nil {
		t.Fatal("expected an error")
	}
	assertNoTokenLeak(t, "diagnostic", res.Err.Error())
}

func TestRunTerminal(t *testing.T) {
	t.Parallel()
	for _, status := range []string{bridge.RunQueued, bridge.RunRunning} {
		if (&bridge.Run{Status: status}).Terminal() {
			t.Fatalf("%s should not be terminal", status)
		}
	}
	for _, status := range []string{bridge.RunSucceeded, bridge.RunFailed, bridge.RunTimedOut, bridge.RunCanceled} {
		if !(&bridge.Run{Status: status}).Terminal() {
			t.Fatalf("%s should be terminal", status)
		}
	}
	var nilRun *bridge.Run
	if nilRun.Terminal() {
		t.Fatal("a nil run is not terminal")
	}
}

func TestRunCreateValidation(t *testing.T) {
	t.Parallel()
	if err := (&bridge.RunCreate{}).Validate(); !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if err := (&bridge.RunCreate{Target: bridge.RunTargetInput{TrailID: "t"}}).Validate(); !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig without change_id, got %v", err)
	}
	if err := (&bridge.RunCreate{Target: bridge.RunTargetInput{TrailID: "t", ChangeID: "c"}}).Validate(); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	var nilCreate *bridge.RunCreate
	if err := nilCreate.Validate(); !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("nil RunCreate: %v", err)
	}
}

// TestNullableStringWireForms pins the three encodings a plugin needs.
func TestNullableStringWireForms(t *testing.T) {
	t.Parallel()
	type probe struct {
		A *bridge.NullableString `json:"a"`
		B *bridge.NullableString `json:"b"`
		C *bridge.NullableString `json:"c,omitempty"`
	}
	out := mustJSON(t, probe{
		A: bridge.NullableStringNull(),
		B: bridge.NullableStringOf("gpt-5-codex"),
	})
	if !strings.Contains(out, `"a":null`) {
		t.Fatalf("explicit null not encoded: %s", out)
	}
	if !strings.Contains(out, `"b":"gpt-5-codex"`) {
		t.Fatalf("value not encoded: %s", out)
	}
	if strings.Contains(out, `"c"`) {
		t.Fatalf("a nil pointer should be omitted: %s", out)
	}

	var back probe
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !back.A.IsNull() {
		t.Fatal("null did not decode to the null form")
	}
	if v, ok := back.B.StringValue(); !ok || v != "gpt-5-codex" {
		t.Fatalf("value did not round-trip: %q %v", v, ok)
	}
}

// TestDecodeErrorUnwrapChain keeps the typed errors reachable through
// errors.Is/As and covers the truncation mapping.
func TestDecodeErrorUnwrapChain(t *testing.T) {
	t.Parallel()
	inner := errors.New("unexpected end of JSON input")
	wrapped := fmtError("decode %q: %w", "Run", inner)
	if !errors.Is(wrapped, inner) {
		t.Fatal("wrapping lost the inner error")
	}

	// An empty body decodes to io.ErrUnexpectedEOF, which the DecodeError maps
	// to ErrOutputTruncated so callers can tell a short response apart.
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.WriteHeader(http.StatusOK)
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	_, err := client.GetRun(context.Background(), "01HZZTESTRUNEMPTY")
	if !errors.Is(err, bridge.ErrOutputTruncated) {
		t.Fatalf("an empty 200 should map to ErrOutputTruncated, got %v", err)
	}
	var decodeErr *bridge.DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.What != "Run" {
		t.Fatalf("want a DecodeError for Run, got %v", err)
	}
	if decodeErr.Unwrap() == nil {
		t.Fatal("DecodeError should unwrap to its cause")
	}
}

// TestHTTPErrorIsAlsoMatchesTypedStatus keeps the status sentinels reachable
// through errors.Is on a bare HTTPError.
func TestHTTPErrorIsAlsoMatchesTypedStatus(t *testing.T) {
	t.Parallel()
	err := &bridge.HTTPError{StatusCode: 412}
	if !errors.Is(err, bridge.ErrPreconditionFailed) {
		t.Fatal("412 should match ErrPreconditionFailed")
	}
	if errors.Is(err, bridge.ErrNotFound) {
		t.Fatal("412 must not match ErrNotFound")
	}
	if got := err.Error(); !strings.Contains(got, "412") {
		t.Fatalf("HTTPError message = %q", got)
	}
	var nilErr *bridge.HTTPError
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil HTTPError = %q", got)
	}
}

// TestCustomUserAgentAndRequestTimeout pins the two remaining config knobs.
func TestCustomUserAgent(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{"state": "active"})
	})
	cfg := testConfig(core.URL, core.URL)
	cfg.UserAgent = "cpa-plugin/9.9"
	client := newClient(t, cfg, core)
	if _, err := client.GetBilling(context.Background(), false); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if got := core.last(t).Header.Get("User-Agent"); got != "cpa-plugin/9.9" {
		t.Fatalf("User-Agent = %q", got)
	}

	cfg.UserAgent = ""
	client = newClient(t, cfg, core)
	if _, err := client.GetBilling(context.Background(), false); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if got := core.last(t).Header.Get("User-Agent"); !strings.Contains(got, "cpa-entire-plugin-bridge") ||
		!strings.Contains(got, "not a model gateway") {
		t.Fatalf("default User-Agent should disclose the bridge's limits, got %q", got)
	}
}

// TestRunPathRequiresID covers the run-id precondition.
func TestRunPathRequiresID(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("no request without a run id")
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	if _, err := client.GetRun(context.Background(), ""); !errors.Is(err, bridge.ErrNotConfigured) {
		t.Fatalf("GetRun: %v", err)
	}
	if _, err := client.CancelRun(context.Background(), ""); !errors.Is(err, bridge.ErrNotConfigured) {
		t.Fatalf("CancelRun: %v", err)
	}
}

// TestClientAccessorsTolerateNil keeps the public helpers panic-free on a nil
// receiver, which matters for a plugin field that may not be initialised.
func TestClientAccessorsTolerateNil(t *testing.T) {
	t.Parallel()
	var c *bridge.Client
	_ = c.Config()
	c.SetAgent("x")
	c.SetTarget("t", "c", "h")
	c.SetTransport(nil)
	if got := c.LastETag(); got != "" {
		t.Fatalf("LastETag on nil = %q", got)
	}
	if got := c.Config().Bearer; got != "" {
		t.Fatalf("nil client config bearer = %q", got)
	}
}

// TestRedactErrorBody covers the scrubber directly through a deterministic
// path: a 500 with a bearer embedded mid-line.
func TestRedactErrorBodyMultipleOccurrences(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("first Bearer " + testToken + " second Bearer " + testToken + " end"))
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	_, err := client.GetBilling(context.Background(), false)
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoTokenLeak(t, "multi-occurrence body", err.Error())
	if strings.Count(err.Error(), "[redacted]") != 2 {
		t.Fatalf("both occurrences should be redacted: %q", err.Error())
	}
}

func fmtError(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// TestRationaleFromResultData covers the reading where the review result sits
// in Run.result.data instead of review_changes_result.
func TestRationaleFromResultData(t *testing.T) {
	t.Parallel()
	run := &bridge.Run{ID: "r", Status: bridge.RunSucceeded}
	run.Result.ResultType = "review_changes"
	run.Result.Data = json.RawMessage(`{"summary":"s","signal":{"value":42,"value_type":"percent","polarity":"neutral","rationale":"from result.data"}}`)
	res := bridge.ExtractText(run)
	if res.Err != nil || res.Text != "from result.data" || res.Source != "result.signal.rationale" {
		t.Fatalf("result.data rationale not used: %+v", res)
	}

	// A malformed data blob must not panic or win over a valid mirror.
	broken := &bridge.Run{ID: "r", Status: bridge.RunSucceeded}
	broken.Result.Data = json.RawMessage(`{"signal":`)
	out := &bridge.RunOutput{}
	text := "mirrored"
	out.Text = &text
	broken.Output = out
	if got := bridge.ExtractText(broken); got.Err != nil || got.Text != "mirrored" {
		t.Fatalf("malformed result.data should fall through: %+v", got)
	}
}
