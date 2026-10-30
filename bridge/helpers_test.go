// Package bridge_test exercises the bridge against httptest servers only. No
// test reaches a real Entire account: every dial goes to a 127.0.0.1 listener
// and every bearer is a fixture value.
package bridge_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

const testToken = "fixture-bearer-do-not-log"

// recordedRequest is what the stub saw, captured before the handler answers.
// Path is the escaped form as it appeared on the request line, so a test can
// tell an id that was escaped (%2F) from one that was spliced in raw (/).
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// recorder is an httptest server plus the requests it handled. It is created
// per test and closed with t.Cleanup, so no listener outlives the test.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newRecorder(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body recordedRequest)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := recordedRequest{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   raw,
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, body)
		rec.mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(rec.Server.Close)
	return rec
}

func (r *recorder) requests() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

func (r *recorder) last(t *testing.T) recordedRequest {
	t.Helper()
	reqs := r.requests()
	if len(reqs) == 0 {
		t.Fatal("stub server received no request")
	}
	return reqs[len(reqs)-1]
}

// writeJSON answers with a status and a JSON body.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	switch v := body.(type) {
	case string:
		if _, err := io.WriteString(w, v); err != nil {
			t.Errorf("write body: %v", err)
		}
	default:
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode body: %v", err)
		}
	}
}

// testConfig is the shared fixture configuration. Both origins point at stub
// servers and the transport is the stub's own client.
func testConfig(core, cell string) *bridge.Config {
	return &bridge.Config{
		CoreBaseURL: core,
		CellBaseURL: cell,
		ProjectHost: "gh",
		Project:     "acme-widgets",
		Bearer:      testToken,
		AgentID:     "01HZZTESTAGENT0000000000",
		TrailID:     "01HZZTESTTRAIL0000000000",
		ChangeID:    "01HZZTESTCHANGE000000000",
		AllowHTTP:   true,
	}
}

func newClient(t *testing.T, cfg *bridge.Config, srv *recorder) *bridge.Client {
	t.Helper()
	cfg.AllowHTTP = true
	client, err := bridge.New(cfg, srv.Client().Transport)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	return client
}

// assertNoTokenLeak fails if the fixture bearer appears in any string.
func assertNoTokenLeak(t *testing.T, where string, values ...string) {
	t.Helper()
	for _, v := range values {
		if strings.Contains(v, testToken) {
			t.Fatalf("%s leaked the bearer: %q", where, v)
		}
	}
}
