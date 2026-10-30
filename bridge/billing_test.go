package bridge_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

func TestGetBillingRequestShape(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"state":       "active",
			"currency":    "usd",
			"trial":       map[string]any{"state": "active", "currency": "usd", "credit_total_cents": 10000, "credit_remaining_cents": 2500},
			"balance":     map[string]any{"currency": "usd", "total_cents": 2500, "trial_cents": 2500, "granted_cents": 0, "purchased_cents": 0},
			"overage":     map[string]any{"amount_cents": 0, "currency": "usd"},
			"runs_paused": map[string]any{"paused": false},
			"card":        map[string]any{"brand": "visa", "last4": "4242"},
		})
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)

	billing, err := client.GetBilling(context.Background(), false)
	if err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if billing.State != "active" || billing.Balance == nil || *billing.Balance.TotalCents != 2500 {
		t.Fatalf("unexpected billing status: %+v", billing)
	}
	if billing.RunsPaused == nil || billing.RunsPaused.Paused {
		t.Fatalf("runs_paused not decoded: %+v", billing.RunsPaused)
	}

	req := core.last(t)
	if req.Method != http.MethodGet {
		t.Fatalf("method = %s", req.Method)
	}
	if req.Path != "/api/v1/me/billing" {
		t.Fatalf("path = %q", req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization header = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if strings.Contains(string(req.Body), testToken) {
		t.Fatal("bearer appears in the request body")
	}
}

func TestGetBillingRefreshQuery(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{"state": "not_started"})
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	if _, err := client.GetBilling(context.Background(), true); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if got := core.last(t).Query; got != "refresh=true" {
		t.Fatalf("query = %q", got)
	}
}

func TestGetBillingProblemDocument(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.Header().Set("Content-Type", "application/problem+json")
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{
			"type": "https://entire.com/errors/unauthenticated", "title": "Unauthorized",
			"status": 401, "detail": "authentication required", "code": "unauthenticated",
			"request_id": "req_01H",
		})
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)

	_, err := client.GetBilling(context.Background(), false)
	if err == nil {
		t.Fatal("expected an error")
	}
	var httpErr *bridge.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("want *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized || httpErr.Problem.Code != "unauthenticated" {
		t.Fatalf("unexpected error: %+v", httpErr)
	}
	msg := err.Error()
	for _, want := range []string{"http 401", "unauthenticated", "authentication required", "req_01H"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	assertNoTokenLeak(t, "HTTPError.Error", msg)
}

// TestErrorRedactsEchoedCredential covers a server that reflects the request
// headers back inside a problem document.
func TestErrorRedactsEchoedCredential(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusBadRequest, map[string]any{
			"type": "about:blank", "title": "Bad Request", "status": 400,
			"detail": "rejected header Authorization: Bearer " + r.Header.Get("Authorization")[len("Bearer "):],
			"code":   "bad_request", "request_id": "req_echo",
		})
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	_, err := client.GetBilling(context.Background(), false)
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoTokenLeak(t, "echoed problem detail", err.Error())
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected the echoed credential to be marked redacted, got %q", err.Error())
	}
}

func TestNonProblemErrorBodyIsRedacted(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream said Bearer " + testToken + "</html>"))
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	_, err := client.GetBilling(context.Background(), false)
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoTokenLeak(t, "raw error body", err.Error())
}

func TestGetBillingDecodeError(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, `{"state": `)
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	_, err := client.GetBilling(context.Background(), false)
	var decodeErr *bridge.DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.What != "BillingStatus" {
		t.Fatalf("want DecodeError for BillingStatus, got %v", err)
	}
}

func TestGetBillingSpend(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"period": "2026-10", "final": false, "currency": "usd", "total_cents": 1234,
			"available_periods": []string{"2026-10"},
			"projects":          []map[string]any{{"id": "01HZZPROJ", "name": "acme-widgets", "deleted": false, "total_cents": 1234, "repos": []any{}}},
			"other_usage":       []any{},
		})
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	spend, err := client.GetBillingSpend(context.Background(), "2026-10")
	if err != nil {
		t.Fatalf("GetBillingSpend: %v", err)
	}
	if spend.Period != "2026-10" || *spend.TotalCents != 1234 || len(spend.Projects) != 1 {
		t.Fatalf("unexpected spend: %+v", spend)
	}
	req := core.last(t)
	if req.Path != "/api/v1/me/billing/spend" || req.Query != "period=2026-10" {
		t.Fatalf("path=%q query=%q", req.Path, req.Query)
	}
}
