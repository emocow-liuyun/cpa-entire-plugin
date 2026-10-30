package bridge_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

// TestOriginAllowlistRejectsForeignHost proves the preflight runs before the
// transport: an insecure or malformed origin is refused by New, so no request
// is ever dialled.
func TestOriginAllowlistRejectsForeignHost(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("a request reached an origin outside the allowlist")
	})
	cfg := testConfig(core.URL, "https://cell.invalid")
	cfg.AllowHTTP = false // https required: an http base URL is refused first
	if _, err := bridge.New(cfg, core.Client().Transport); !errors.Is(err, bridge.ErrInsecureEndpoint) {
		t.Fatalf("want ErrInsecureEndpoint for an http origin, got %v", err)
	}
	if n := len(core.requests()); n != 0 {
		t.Fatalf("a request was sent during construction: %d", n)
	}

	// With AllowHTTP the scheme passes and the origin itself is checked.
	cfg.AllowHTTP = true
	if _, err := bridge.New(cfg, core.Client().Transport); err != nil {
		t.Fatalf("the configured origins should be allowed: %v", err)
	}
}

// TestAllowlistRejectsUnlistedOriginAtCallTime swaps the cell origin for a
// foreign one after construction and confirms the request is refused.
func TestAllowlistRejectsUnlistedOriginAtCallTime(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("a request reached an origin outside the allowlist")
	})
	// Both configured origins are loopback so DNS is never attempted. The
	// entry has no port, so it matches any port: "127.0.0.1:2" is therefore
	// allowed as a host, and the real differentiator is the host form below.
	// Both origins are listed, so this call reaches the network layer; the
	// point is that the stub's own origin is not listed, so it is never dialled.
	cfg := testConfig("http://core.invalid:1", "http://cell.invalid:2")
	cfg.AllowHTTP = true
	client, err := bridge.New(cfg, cell.Client().Transport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The configured origins are core.invalid:1 and cell.invalid:2; the stub's
	// origin is neither, so the transport is never reached.
	_, err = client.ReadAgent(context.Background(), "01HZZTESTAGENT0000000000")
	if err == nil {
		t.Fatal("the stub origin should not have been reachable")
	}
	var pre *bridge.PreflightError
	if errors.As(err, &pre) && !errors.Is(err, bridge.ErrOriginNotAllowed) {
		t.Fatalf("unexpected preflight reason: %v", err)
	}
	if n := len(cell.requests()); n != 0 {
		t.Fatalf("a request was sent despite the allowlist: %d", n)
	}
}

// TestExtraAllowedHostIsHonoured shows an explicit AllowedHosts entry is what
// makes a stub origin reachable.
func TestExtraAllowedHostIsHonoured(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 1))
	})
	cfg := testConfig("https://core.invalid", "https://cell.example")
	host := strings.TrimPrefix(cell.URL, "http://")
	cfg.AllowedHosts = []string{host}
	cfg.CellBaseURL = cell.URL
	client, err := bridge.New(cfg, cell.Client().Transport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	agent, err := client.ReadAgent(context.Background(), "01HZZTESTAGENT0000000000")
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if agent.Version != 1 {
		t.Fatalf("unexpected agent: %+v", agent)
	}
	if n := len(cell.requests()); n != 1 {
		t.Fatalf("want 1 request, got %d", n)
	}
}

// TestPortlessAllowlistEntryMatchesAnyPort covers the host-only form.
func TestPortlessAllowlistEntryMatchesAnyPort(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{"state": "active"})
	})
	cfg := testConfig("https://core.example", "https://cell.example")
	cfg.CoreBaseURL = core.URL
	cfg.AllowedHosts = []string{"127.0.0.1"}
	client := newClient(t, cfg, core)
	if _, err := client.GetBilling(context.Background(), false); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
}

func TestPreflightErrorIsTyped(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{CoreBaseURL: "ftp://core.example", CellBaseURL: "https://cell.example", Bearer: testToken}
	_, err := bridge.New(cfg, nil)
	var pre *bridge.PreflightError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PreflightError, got %T", err)
	}
	if !errors.Is(err, bridge.ErrInsecureEndpoint) {
		t.Fatalf("want ErrInsecureEndpoint, got %v", err)
	}
	if pre.Origin != "core.example" {
		t.Fatalf("origin = %q", pre.Origin)
	}
	assertNoTokenLeak(t, "preflight error", err.Error())
}

func TestBaseURLWithAPIVersionIsNotDoubled(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 1))
	})
	cfg := testConfig("https://core.invalid", cell.URL+"/api/v1")
	client := newClient(t, cfg, cell)
	if _, err := client.ReadAgent(context.Background(), "01HZZTESTAGENT0000000000"); err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if got := cell.last(t).Path; got != agentPath {
		t.Fatalf("path = %q, want a single /api/v1 prefix", got)
	}
}

func TestCustomAPIVersion(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 1))
	})
	cfg := testConfig("https://core.invalid", cell.URL)
	cfg.APIVersion = "/api/v2"
	client := newClient(t, cfg, cell)
	if _, err := client.ReadAgent(context.Background(), "01HZZTESTAGENT0000000000"); err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if got := cell.last(t).Path; got != "/api/v2/gh/acme-widgets/agents/01HZZTESTAGENT0000000000" {
		t.Fatalf("path = %q", got)
	}
}

// TestTransportIsInjected shows the injected transport sees the request, which
// is how a host attaches request logs without the bridge knowing about them.
func TestTransportIsInjected(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, map[string]any{"state": "active"})
	})
	var seen int
	client, err := bridge.New(testConfig(core.URL, core.URL), countingTransport{inner: core.Client().Transport, seen: &seen})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetBilling(context.Background(), false); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if seen != 1 {
		t.Fatalf("injected transport saw %d requests, want 1", seen)
	}

	// SetTransport replaces it for subsequent calls.
	seen2 := 0
	client.SetTransport(countingTransport{inner: core.Client().Transport, seen: &seen2})
	if _, err := client.GetBilling(context.Background(), false); err != nil {
		t.Fatalf("GetBilling: %v", err)
	}
	if seen2 != 1 || seen != 1 {
		t.Fatalf("SetTransport did not take effect: old=%d new=%d", seen, seen2)
	}
}

func TestSetAgentAndTarget(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, baseAgentBody(etagV7, 1))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	client.SetAgent("01HZZOTHERAGENT000000000")
	if _, err := client.ReadAgent(context.Background(), ""); err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if got := cell.last(t).Path; !strings.HasSuffix(got, "/agents/01HZZOTHERAGENT000000000") {
		t.Fatalf("path = %q", got)
	}
	client.SetTarget("T1", "C1", "H1")
	cfg := client.Config()
	if cfg.TrailID != "T1" || cfg.ChangeID != "C1" || cfg.HeadSHA != "H1" {
		t.Fatalf("target not applied: %+v", cfg)
	}
}

func TestMissingAgentIsNotConfigured(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		t.Error("no request without an agent id")
	})
	cfg := testConfig("https://core.invalid", cell.URL)
	cfg.AgentID = ""
	client := newClient(t, cfg, cell)
	if _, err := client.ReadAgent(context.Background(), ""); !errors.Is(err, bridge.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := client.StartRun(context.Background(), bridge.StartRunOptions{IdempotencyKey: stableKey}); !errors.Is(err, bridge.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// countingTransport records how many requests passed through it.
type countingTransport struct {
	inner http.RoundTripper
	seen  *int
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.seen++
	return t.inner.RoundTrip(r)
}

// TestRoutePathHardening rejects path shapes that could smuggle a host or a
// fragment into the request line.
func TestRoutePathHardening(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		// 200 with an empty body: the point is the path, not the payload.
		w.WriteHeader(http.StatusOK)
	})
	cfg := testConfig(core.URL, core.URL)
	client := newClient(t, cfg, core)

	// An agent id carrying a fragment or a slash is escaped, never spliced in
	// raw, so the request line cannot gain a fragment or a new path level.
	for _, id := range []string{"01HZZ#frag", "a/b", "..", "../admin", "x?y=1"} {
		_, err := client.ReadAgent(context.Background(), id)
		if !errors.Is(err, bridge.ErrOutputTruncated) && !errors.Is(err, bridge.ErrInvalidConfig) {
			t.Fatalf("id %q produced an unexpected error: %v", id, err)
		}
	}
	for _, req := range core.requests() {
		if strings.Contains(req.Path, "#") || strings.Contains(req.Path, "?") ||
			strings.Contains(req.Path, "/admin") || strings.HasSuffix(req.Path, "/..") {
			t.Fatalf("an id escaped its path segment: %q", req.Path)
		}
		if strings.Contains(req.Path, "/admin") {
			t.Fatalf("an id added a path level: %q", req.Path)
		}
	}
	if n := len(core.requests()); n != 5 {
		t.Fatalf("expected 5 requests, got %d", n)
	}
}

// TestRunIDWithSlashesIsEscaped proves ids are path-escaped rather than
// concatenated raw, so an id cannot select a different route.
func TestRunIDWithSlashesIsEscaped(t *testing.T) {
	t.Parallel()
	cell := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ recordedRequest) {
		writeJSON(t, w, http.StatusOK, runBody("01HZZTESTRUN0000000000100", bridge.RunSucceeded))
	})
	client := newClient(t, testConfig("https://core.invalid", cell.URL), cell)
	if _, err := client.GetRun(context.Background(), "x/../../me/billing"); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := cell.last(t).Path; !strings.HasPrefix(got, runPath) || strings.Contains(got, "/me/billing") {
		t.Fatalf("run id was not escaped: %q", got)
	}
}
