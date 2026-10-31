package main

// Tests for the Entire OAuth device-flow capability (oauth.go).
//
// Nothing here touches a real identity provider, a real login or a real
// credential: every upstream call is served by an in-process stub through the
// authHTTPClient seam, and every fixture token is synthetic.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// Sanctioned synthetic values. They are shaped like real values only so the
// redaction and storage paths behave identically.
const (
	fixtureAccessToken  = "entire_at_0000000000000000000000000000fixture"
	fixtureRefreshToken = "entire_rt_1111111111111111111111111111fixture"
	fixtureDeviceCode   = "entire_dc_2222222222222222222222222222fixture"
	fixtureUserCode     = "ABCD-EFGH"
	fixtureVerifURI     = "https://us.auth.entire.io/device"
)

// authStubCall records one request served by authStubClient.
type authStubCall struct {
	Method  string
	URL     string
	Headers http.Header
	Body    string
}

// authStubResponse is one canned answer.
type authStubResponse struct {
	Status  int
	Headers http.Header
	Body    string
	Err     error
}

// authStubClient is the authHTTPClient test double. It answers in order and
// records every request so a test can assert on the wire shape.
type authStubClient struct {
	mu        sync.Mutex
	responses []authStubResponse
	calls     []authStubCall
}

func (s *authStubClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, authStubCall{
		Method:  method,
		URL:     rawURL,
		Headers: headers.Clone(),
		Body:    string(body),
	})
	if len(s.responses) == 0 {
		return http.StatusInternalServerError, nil, nil, newPluginError(codeAuthTransport, "stub exhausted", 0)
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	if next.Err != nil {
		return 0, nil, nil, next.Err
	}
	status := next.Status
	if status == 0 {
		status = http.StatusOK
	}
	responseHeaders := next.Headers
	if responseHeaders == nil {
		responseHeaders = http.Header{"Content-Type": []string{"application/json"}}
	}
	return status, responseHeaders, []byte(next.Body), nil
}

// callCount reports how many requests the stub served.
func (s *authStubClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// lastCall returns the most recent recorded request.
func (s *authStubClient) lastCall(t *testing.T) authStubCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("stub served no requests")
	}
	return s.calls[len(s.calls)-1]
}

// installAuthStub routes every auth call in the current test to stub and
// restores the direct transport afterwards.
func installAuthStub(t *testing.T, stub *authStubClient) {
	t.Helper()
	previous := installAuthClientOverride(stub)
	t.Cleanup(func() {
		installAuthClientOverride(previous)
		setAuthCallbackProbe(nil)
	})
}

// deviceAuthBody renders a device authorization response.
func deviceAuthBody(deviceCode, userCode, verification string, expiresIn, interval int64) string {
	payload, errMarshal := json.Marshal(map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          verification,
		"verification_uri_complete": verification,
		"expires_in":                expiresIn,
		"interval":                  interval,
	})
	if errMarshal != nil {
		panic(errMarshal)
	}
	return string(payload)
}

// tokenBody renders a token endpoint body.
func tokenBody(fields map[string]any) string {
	payload, errMarshal := json.Marshal(fields)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return string(payload)
}

// successTokenBody is a complete token response with a refresh token.
func successTokenBody() string {
	return tokenBody(map[string]any{
		"access_token":  fixtureAccessToken,
		"refresh_token": fixtureRefreshToken,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         "cli offline_access",
	})
}

// resetAuthState clears plugin state, the flow store and the redaction registry.
func resetAuthState(t *testing.T) {
	t.Helper()
	resetPluginState(t)
	authFlows.reset()
	authFlows.now = time.Now
	installAuthClientOverride(nil)
}

// startDeviceFlow drives auth.login.start against a stub and returns the state.
func startDeviceFlow(t *testing.T, stub *authStubClient) authLoginStartResponse {
	t.Helper()
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusOK,
		Body:   deviceAuthBody(fixtureDeviceCode, fixtureUserCode, fixtureVerifURI, 600, 5),
	})
	raw, errStart := handleAuthMethod(methodAuthLoginStart, nil)
	if errStart != nil {
		t.Fatalf("auth.login.start failed: %v", errStart)
	}
	var decoded struct {
		OK     bool                   `json:"ok"`
		Result authLoginStartResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode start envelope: %v", errUnmarshal)
	}
	if !decoded.OK {
		t.Fatalf("start envelope not ok: %s", raw)
	}
	return decoded.Result
}

// pollOnce runs auth.login.poll for state and returns the decoded response.
func pollOnce(t *testing.T, state string) authLoginPollResponse {
	t.Helper()
	payload, errMarshal := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: state})
	if errMarshal != nil {
		t.Fatalf("marshal poll request: %v", errMarshal)
	}
	raw, errPoll := handleAuthMethod(methodAuthLoginPoll, payload)
	if errPoll != nil {
		t.Fatalf("auth.login.poll failed: %v", errPoll)
	}
	var decoded struct {
		OK     bool                  `json:"ok"`
		Result authLoginPollResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode poll envelope: %v", errUnmarshal)
	}
	if !decoded.OK {
		t.Fatalf("poll envelope not ok: %s", raw)
	}
	return decoded.Result
}

// decodeStorage decodes an AuthData.StorageJSON payload.
func decodeStorage(t *testing.T, auth authData) map[string]any {
	t.Helper()
	if len(auth.StorageJSON) == 0 {
		t.Fatal("AuthData.StorageJSON is empty")
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(auth.StorageJSON, &doc); errUnmarshal != nil {
		t.Fatalf("decode storage json: %v", errUnmarshal)
	}
	return doc
}

// skipPollInterval rewinds the recorded poll clock so a test can poll twice.
func skipPollInterval(t *testing.T, state string) {
	t.Helper()
	flow, errGet := authFlows.get(state)
	if errGet != nil {
		t.Fatalf("flow lookup: %v", errGet)
	}
	flow.LastPoll = time.Now().Add(-2 * flow.Interval)
}

// ---------------------------------------------------------------------------
// Transport policy
// ---------------------------------------------------------------------------

// TestAuthRejectsForeignIdentityProvider covers the Microsoft-tenant guard.
func TestAuthRejectsForeignIdentityProvider(t *testing.T) {
	resetAuthState(t)
	for _, rawURL := range []string{
		"https://login.microsoftonline.com/common/oauth2/v2.0/devicecode",
		"https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
		"https://LOGIN.MICROSOFTONLINE.COM:443/oauth/token",
		"https://login.microsoft.com/entra/oauth/token",
		"https://sts.windows.net/oauth/token",
	} {
		if _, errValidate := validateAuthURL(rawURL, entireAuthAllowedDomains); errValidate == nil {
			t.Fatalf("%s must be refused", rawURL)
		} else if errValidate.Code != codeAuthForeignIdP {
			t.Fatalf("%s: code = %s, want %s", rawURL, errValidate.Code, codeAuthForeignIdP)
		}
	}
}

// TestAuthTransportPolicyEnforced covers the remaining URL rules.
func TestAuthTransportPolicyEnforced(t *testing.T) {
	resetAuthState(t)
	cases := []struct{ name, rawURL string }{
		{"plain http", "http://us.auth.entire.io/oauth/token"},
		{"ip literal", "https://192.0.2.10/oauth/token"},
		{"userinfo", "https://user:pass@us.auth.entire.io/oauth/token"},
		{"fragment", "https://us.auth.entire.io/oauth/token#x"},
		{"foreign path", "https://us.auth.entire.io/admin/delete"},
		{"unlisted host", "https://evil.example/oauth/token"},
		{"lookalike suffix", "https://entire.io.evil.example/oauth/token"},
	}
	for _, tc := range cases {
		if _, errValidate := validateAuthURL(tc.rawURL, entireAuthAllowedDomains); errValidate == nil {
			t.Fatalf("%s (%s) must be refused", tc.name, tc.rawURL)
		}
	}
	for _, rawURL := range []string{
		"https://us.auth.entire.io/device_authorization",
		"https://us.auth.entire.io/oauth/token",
		"https://auth.entire.io/oauth/token",
	} {
		if _, errValidate := validateAuthURL(rawURL, entireAuthAllowedDomains); errValidate != nil {
			t.Fatalf("%s must be accepted: %v", rawURL, errValidate)
		}
	}
}

// TestForeignBaseURLFallsBackToOfficialLoginServer covers BaseURL scrubbing.
func TestForeignBaseURLFallsBackToOfficialLoginServer(t *testing.T) {
	resetAuthState(t)
	for _, base := range []string{
		"",
		"https://login.microsoftonline.com",
		"https://evil.example",
		"not a url",
		"https://entire.io.evil.example",
	} {
		if got := resolveAuthBaseURL(base); got != entireAuthBaseURL {
			t.Fatalf("resolveAuthBaseURL(%q) = %q, want %q", base, got, entireAuthBaseURL)
		}
	}
	if got := resolveAuthBaseURL("https://us.auth.entire.io/"); got != "https://us.auth.entire.io" {
		t.Fatalf("official base = %q", got)
	}
}

// TestDirectAuthClientRefusesForeignHost ensures the fallback transport carries
// the same policy gate before it dials anything.
func TestDirectAuthClientRefusesForeignHost(t *testing.T) {
	resetAuthState(t)
	client := newDirectAuthClient(time.Second)
	if _, _, _, errDo := client.Do(context.Background(), http.MethodPost,
		"https://login.microsoftonline.com/common/oauth2/v2.0/token", nil, []byte("x")); errDo == nil {
		t.Fatal("direct client must refuse a Microsoft tenant host")
	}
	if _, _, _, errDo := client.Do(context.Background(), http.MethodPost,
		"http://us.auth.entire.io/oauth/token", nil, []byte("x")); errDo == nil {
		t.Fatal("direct client must refuse plain http")
	}
}

// TestRedirectPolicyRefusesForeignHost covers the redirect hook.
func TestRedirectPolicyRefusesForeignHost(t *testing.T) {
	resetAuthState(t)
	client := newDirectAuthClient(time.Second)
	if client.httpClient == nil || client.httpClient.CheckRedirect == nil {
		t.Fatal("direct client must install a redirect policy")
	}
	request, errRequest := http.NewRequest(http.MethodGet, "https://login.microsoftonline.com/oauth/token", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	if errRedirect := client.httpClient.CheckRedirect(request, []*http.Request{request}); errRedirect == nil {
		t.Fatal("redirect to a foreign identity provider must be refused")
	}
	sameHost, errSame := http.NewRequest(http.MethodGet, "https://us.auth.entire.io/oauth/token", nil)
	if errSame != nil {
		t.Fatalf("build request: %v", errSame)
	}
	sameHost.Header.Set("Authorization", "Bearer should-not-survive")
	if errRedirect := client.httpClient.CheckRedirect(sameHost, []*http.Request{sameHost}); errRedirect != nil {
		t.Fatalf("same-host redirect must be followed: %v", errRedirect)
	}
	if sameHost.Header.Get("Authorization") != "" {
		t.Fatal("redirect must drop the Authorization header")
	}
}

// ---------------------------------------------------------------------------
// RPC surface
// ---------------------------------------------------------------------------

// TestAuthIdentifierIsEntire covers auth.identifier.
func TestAuthIdentifierIsEntire(t *testing.T) {
	resetAuthState(t)
	raw, errIdentify := handleAuthMethod(methodAuthIdentifier, nil)
	if errIdentify != nil {
		t.Fatalf("auth.identifier failed: %v", errIdentify)
	}
	if !strings.Contains(string(raw), `"identifier":"entire"`) {
		t.Fatalf("identifier envelope = %s", raw)
	}
	if entireAuthIdentifier != providerIdentifier {
		t.Fatalf("auth identifier %q must match quota identifier %q", entireAuthIdentifier, providerIdentifier)
	}
}

// TestAuthUnknownMethodFails covers the dispatcher default branch.
func TestAuthUnknownMethodFails(t *testing.T) {
	resetAuthState(t)
	if _, errHandle := handleAuthMethod("auth.login.finish", nil); errHandle == nil {
		t.Fatal("auth.login.finish must not be served")
	} else if errHandle.Code != codeUnknownMethod {
		t.Fatalf("code = %s, want %s", errHandle.Code, codeUnknownMethod)
	}
	if _, errQuota := handleAuthMethod("quota.fetch", nil); errQuota == nil {
		t.Fatal("handleAuthMethod must not serve quota.fetch")
	}
}

// TestAuthMethodsCoverAllProviderRPCs pins the method list.
func TestAuthMethodsCoverAllProviderRPCs(t *testing.T) {
	resetAuthState(t)
	want := map[string]bool{
		methodAuthIdentifier: false,
		methodAuthParse:      false,
		methodAuthLoginStart: false,
		methodAuthLoginPoll:  false,
		methodAuthRefresh:    false,
	}
	for _, method := range authProviderMethods() {
		if _, known := want[method]; !known {
			t.Fatalf("unexpected auth method %q", method)
		}
		want[method] = true
	}
	for method, seen := range want {
		if !seen {
			t.Fatalf("auth method %q missing from authProviderMethods", method)
		}
	}
}

// ---------------------------------------------------------------------------
// Device flow: start
// ---------------------------------------------------------------------------

// TestLoginStartUsesOfficialDeviceFlow covers the request shape and the state.
func TestLoginStartUsesOfficialDeviceFlow(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)

	start := startDeviceFlow(t, stub)

	call := stub.lastCall(t)
	if call.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", call.Method)
	}
	if call.URL != "https://us.auth.entire.io/device_authorization" {
		t.Fatalf("url = %s", call.URL)
	}
	if got := call.Headers.Get("Content-Type"); !strings.Contains(got, "application/x-www-form-urlencoded") {
		t.Fatalf("Content-Type = %q", got)
	}
	sent, errForm := url.ParseQuery(call.Body)
	if errForm != nil {
		t.Fatal(errForm)
	}
	if sent.Get("client_id") != "entire-cli" || sent.Get("scope") != "cli offline_access" {
		t.Fatalf("unexpected device form: %v", sent)
	}

	if start.Provider != entireAuthIdentifier {
		t.Fatalf("provider = %q", start.Provider)
	}
	if start.URL != fixtureVerifURI {
		t.Fatalf("url = %q", start.URL)
	}
	if !isValidLoginState(start.State) {
		t.Fatalf("state %q fails the host charset", start.State)
	}
	if strings.Contains(start.State, fixtureDeviceCode) {
		t.Fatal("state must not embed the device code")
	}
	if start.ExpiresAt.Before(time.Now()) {
		t.Fatal("expires_at must be in the future")
	}
	if got, _ := start.Metadata["user_code"].(string); got != fixtureUserCode {
		t.Fatalf("metadata.user_code = %v", start.Metadata["user_code"])
	}
}

// TestLoginStartRejectsForeignVerificationURI covers a hostile device doc.
func TestLoginStartRejectsForeignVerificationURI(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusOK,
		Body: deviceAuthBody(fixtureDeviceCode, fixtureUserCode,
			"https://login.microsoftonline.com/device", 600, 5),
	})
	if _, errStart := handleAuthMethod(methodAuthLoginStart, nil); errStart == nil {
		t.Fatal("a foreign verification URL must be refused")
	} else if errStart.Code != codeBlockedHost {
		t.Fatalf("code = %s, want %s", errStart.Code, codeBlockedHost)
	}
	if authFlows.len() != 0 {
		t.Fatal("a refused start must not leave a state behind")
	}
}

// TestLoginStartMalformedResponseFailsClosed covers a truncated device doc.
func TestLoginStartMalformedResponseFailsClosed(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	for _, body := range []string{`{"user_code":"ABCD"}`, `not json`, ``} {
		stub.responses = append(stub.responses, authStubResponse{Status: http.StatusOK, Body: body})
		if _, errStart := handleAuthMethod(methodAuthLoginStart, nil); errStart == nil {
			t.Fatalf("body %q must fail", body)
		}
	}
}

// TestLoginStartRejectsForeignProviderKey covers the provider filter.
func TestLoginStartRejectsForeignProviderKey(t *testing.T) {
	resetAuthState(t)
	payload, errMarshal := json.Marshal(authLoginStartRequest{Provider: "openai"})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, errStart := handleAuthMethod(methodAuthLoginStart, payload); errStart == nil {
		t.Fatal("a non-entire provider must be refused")
	}
}

// ---------------------------------------------------------------------------
// Device flow: poll
// ---------------------------------------------------------------------------

// TestLoginPollPendingThenSuccess covers the RFC 8628 happy path.
func TestLoginPollPendingThenSuccess(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	// First poll: authorization_pending.
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "authorization_pending"}),
	})
	pending := pollOnce(t, start.State)
	if pending.Status != authLoginStatusPending {
		t.Fatalf("status = %q, want pending (%s)", pending.Status, pending.Message)
	}
	if pending.Auth != nil {
		t.Fatal("a pending poll must not carry auth data")
	}
	tokenCall := stub.lastCall(t)
	if tokenCall.URL != "https://us.auth.entire.io/oauth/token" {
		t.Fatalf("token url = %s", tokenCall.URL)
	}
	if got := tokenCall.Headers.Get("Authorization"); got != "" {
		t.Fatalf("device poll must not send an Authorization header, got %q", got)
	}
	form, errForm := url.ParseQuery(tokenCall.Body)
	if errForm != nil {
		t.Fatalf("parse token form: %v", errForm)
	}
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("device_code") != fixtureDeviceCode {
		t.Fatalf("device_code = %q", form.Get("device_code"))
	}
	if form.Get("client_id") != "entire-cli" {
		t.Fatalf("client_id = %q", form.Get("client_id"))
	}
	if form.Get("scope") != "cli offline_access" {
		t.Fatalf("scope = %q", form.Get("scope"))
	}

	// Second poll: success.
	skipPollInterval(t, start.State)
	stub.responses = append(stub.responses, authStubResponse{Status: http.StatusOK, Body: successTokenBody()})
	success := pollOnce(t, start.State)
	if success.Status != authLoginStatusSuccess {
		t.Fatalf("status = %q, want success (%s)", success.Status, success.Message)
	}
	if success.Auth == nil {
		t.Fatal("a successful poll must return auth data")
	}
	storage := decodeStorage(t, *success.Auth)
	if storage["entire_token"] != fixtureAccessToken {
		t.Fatalf("storage entire_token mismatch: %v", storage["entire_token"])
	}
	// access_token is the compatibility alias the host and CLI expect next to
	// entire_token, so the same bearer is stored under both keys.
	if storage["access_token"] != fixtureAccessToken {
		t.Fatalf("storage access_token mismatch: %v", storage["access_token"])
	}
	if storage["refresh_token"] != fixtureRefreshToken {
		t.Fatalf("storage refresh_token mismatch")
	}
	if storage["issuer"] != entireAuthBaseURL {
		t.Fatalf("storage issuer = %v", storage["issuer"])
	}
	if storage["token_type"] != "Bearer" {
		t.Fatalf("storage token_type = %v", storage["token_type"])
	}
	issued, okIssued := storage["expires_at"].(float64)
	if !okIssued || issued <= float64(time.Now().Unix()) {
		t.Fatalf("storage expires_at = %v", storage["expires_at"])
	}
	if success.Auth.Provider != entireAuthIdentifier {
		t.Fatalf("auth provider = %q", success.Auth.Provider)
	}
	// The label identifies the account using a non-secret digest.
	if !strings.HasPrefix(success.Auth.Label, "Entire ") {
		t.Fatalf("label = %q", success.Auth.Label)
	}
	if strings.Contains(success.Auth.FileName, fixtureAccessToken) ||
		strings.Contains(success.Auth.Label, fixtureAccessToken) ||
		strings.Contains(success.Auth.ID, fixtureAccessToken) {
		t.Fatal("label/file name/id must never contain a token")
	}
	// The state is consumed on success.
	if authFlows.len() != 0 {
		t.Fatal("a completed flow must drop its state")
	}
}

// TestLoginPollEnforcesInterval covers the anti-hammer gate.
func TestLoginPollEnforcesInterval(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "authorization_pending"}),
	})
	if status := pollOnce(t, start.State).Status; status != authLoginStatusPending {
		t.Fatalf("first poll status = %q", status)
	}
	callsAfterFirst := stub.callCount()

	// An immediate second poll is answered pending without touching the network.
	if status := pollOnce(t, start.State).Status; status != authLoginStatusPending {
		t.Fatalf("rate-limited poll status = %q, want pending", status)
	}
	if stub.callCount() != callsAfterFirst {
		t.Fatal("a rate-limited poll must not reach the token endpoint")
	}
}

// TestLoginPollSlowDownIncreasesInterval covers the RFC 8628 back-off.
func TestLoginPollSlowDownIncreasesInterval(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	flow, errFlow := authFlows.get(start.State)
	if errFlow != nil {
		t.Fatalf("flow lookup: %v", errFlow)
	}
	before := flow.Interval
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "slow_down"}),
	})
	if status := pollOnce(t, start.State).Status; status != authLoginStatusPending {
		t.Fatalf("slow_down status = %q, want pending", status)
	}
	if flow.Interval != before+slowDownIncrement {
		t.Fatalf("interval = %v, want %v", flow.Interval, before+slowDownIncrement)
	}
}

// TestLoginPollDeniedIsTerminal covers access_denied.
func TestLoginPollDeniedIsTerminal(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "access_denied"}),
	})
	result := pollOnce(t, start.State)
	if result.Status != authLoginStatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Auth != nil {
		t.Fatal("a denied login must not carry auth data")
	}
	if authFlows.len() != 0 {
		t.Fatal("a denied login must drop its state")
	}
	// A second poll of the consumed state fails loudly instead of faking success.
	payload, _ := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: start.State})
	if _, errPoll := handleAuthMethod(methodAuthLoginPoll, payload); errPoll == nil {
		t.Fatal("a consumed state must fail")
	}
}

// TestLoginPollInvalidClientIsTerminal covers the HTTP 401 invalid_client case
// the provider source claims, and proves the plugin reports it instead of
// inventing a session.
func TestLoginPollInvalidClientIsTerminal(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusUnauthorized,
		Body:   tokenBody(map[string]any{"error": "invalid_client", "error_description": "invalid client credentials"}),
	})
	payload, _ := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: start.State})
	raw, errPoll := handleAuthMethod(methodAuthLoginPoll, payload)
	if errPoll == nil {
		t.Fatalf("invalid_client must surface as an error, got envelope %s", raw)
	}
	if errPoll.Code != codeAuthClientRejected {
		t.Fatalf("code = %s, want %s", errPoll.Code, codeAuthClientRejected)
	}
	if errPoll.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("http status = %d, want 401", errPoll.StatusCode())
	}
	if strings.Contains(errPoll.Message, fixtureDeviceCode) {
		t.Fatal("the device code must never appear in an error message")
	}
	if authFlows.len() != 0 {
		t.Fatal("a rejected client must drop its state")
	}
}

// TestLoginPollNeverFakesSuccess covers the empty-token and unknown-error paths.
func TestLoginPollNeverFakesSuccess(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"empty success body", http.StatusOK, tokenBody(map[string]any{"token_type": "Bearer"}), true},
		{"blank access token", http.StatusOK, tokenBody(map[string]any{"access_token": "   "}), true},
		{"unknown error", http.StatusBadRequest, tokenBody(map[string]any{"error": "weird_failure"}), true},
		{"server error", http.StatusInternalServerError, tokenBody(map[string]any{}), true},
		{"non-json", http.StatusBadGateway, "<html>bad gateway</html>", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAuthState(t)
			stub := &authStubClient{}
			installAuthStub(t, stub)
			start := startDeviceFlow(t, stub)
			stub.responses = append(stub.responses, authStubResponse{Status: tc.status, Body: tc.body})
			payload, _ := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: start.State})
			raw, errPoll := handleAuthMethod(methodAuthLoginPoll, payload)
			if !tc.wantErr {
				return
			}
			if errPoll == nil {
				t.Fatalf("expected an error, got envelope %s", raw)
			}
			if errPoll.Code == "" {
				t.Fatal("error must carry a code")
			}
			if strings.Contains(errPoll.Message, fixtureAccessToken) {
				t.Fatal("error message leaked a token")
			}
		})
	}
}

// TestLoginPollUnknownStateFails covers the state map miss.
func TestLoginPollUnknownStateFails(t *testing.T) {
	resetAuthState(t)
	for _, state := range []string{"", "entire_deadbeef", "../../etc/passwd", "entire/../x"} {
		payload, _ := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: state})
		if _, errPoll := handleAuthMethod(methodAuthLoginPoll, payload); errPoll == nil {
			t.Fatalf("state %q must fail", state)
		}
	}
}

// TestLoginStateTTLExpires covers the in-memory state TTL.
func TestLoginStateTTLExpires(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)

	base := time.Now()
	authFlows.now = func() time.Time { return base.Add(time.Hour) }
	payload, _ := json.Marshal(authLoginPollRequest{Provider: entireAuthIdentifier, State: start.State})
	raw, errPoll := handleAuthMethod(methodAuthLoginPoll, payload)
	if errPoll == nil {
		t.Fatalf("an expired state must fail, got %s", raw)
	}
	if errPoll.Code != codeAuthFlowExpired {
		t.Fatalf("code = %s, want %s", errPoll.Code, codeAuthFlowExpired)
	}
	if authFlows.len() != 0 {
		t.Fatal("an expired state must be purged")
	}
}

// TestConcurrentLoginStatesAreIndependent covers the mutex-protected map under
// parallel start/poll traffic.
func TestConcurrentLoginStatesAreIndependent(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)

	const workers = 8
	states := make([]string, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			stub.mu.Lock()
			stub.responses = append(stub.responses, authStubResponse{
				Status: http.StatusOK,
				Body:   deviceAuthBody(fixtureDeviceCode, fixtureUserCode, fixtureVerifURI, 600, 5),
			})
			stub.mu.Unlock()
			raw, errStart := handleAuthMethod(methodAuthLoginStart, nil)
			if errStart != nil {
				t.Errorf("start failed: %v", errStart)
				return
			}
			var decoded struct {
				Result authLoginStartResponse `json:"result"`
			}
			if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
				t.Errorf("decode: %v", errUnmarshal)
				return
			}
			mu.Lock()
			states[slot] = decoded.Result.State
			mu.Unlock()
		}(index)
	}
	wg.Wait()

	seen := make(map[string]struct{}, workers)
	for _, state := range states {
		if state == "" {
			t.Fatal("a worker produced no state")
		}
		if _, dup := seen[state]; dup {
			t.Fatalf("duplicate state %q", state)
		}
		seen[state] = struct{}{}
	}
	if authFlows.len() != workers {
		t.Fatalf("flow count = %d, want %d", authFlows.len(), workers)
	}
	authFlows.reset()
	if authFlows.len() != 0 {
		t.Fatal("reset must clear every state")
	}
}

// TestNoSecretInLogsOrMessages covers the redaction contract end to end.
func TestNoSecretInLogsOrMessages(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)
	skipPollInterval(t, start.State)
	stub.responses = append(stub.responses, authStubResponse{Status: http.StatusOK, Body: successTokenBody()})
	success := pollOnce(t, start.State)

	serialized, errMarshal := json.Marshal(success)
	if errMarshal != nil {
		t.Fatalf("marshal poll response: %v", errMarshal)
	}
	// StorageJSON is base64 on the wire; the plain access token must not be
	// readable in Label/FileName/ID/Attributes/Message.
	for _, field := range []string{success.Auth.Label, success.Auth.FileName, success.Auth.ID, success.Message} {
		if strings.Contains(field, fixtureAccessToken) || strings.Contains(field, fixtureRefreshToken) {
			t.Fatalf("token leaked in %q", field)
		}
	}
	for key, value := range success.Auth.Attributes {
		if strings.Contains(value, fixtureAccessToken) {
			t.Fatalf("token leaked in attribute %q", key)
		}
	}
	if strings.Contains(string(serialized), fixtureDeviceCode) {
		t.Fatal("the device code must not appear in the poll response")
	}

	// Every token is registered for redaction, so an error built from a body
	// that echoes one comes out scrubbed.
	redacted := redactSecrets("failed with " + fixtureRefreshToken)
	if strings.Contains(redacted, fixtureRefreshToken) {
		t.Fatalf("redaction failed: %q", redacted)
	}
	if !strings.Contains(redacted, secretPlaceholder) {
		t.Fatalf("redaction marker missing: %q", redacted)
	}
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

// TestLabelSanitization covers the label / file name rules.
func TestLabelSanitization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", entireAuthIdentifier},
		{"us.auth.entire.io", "us.auth.entire.io"},
		{"octocat@entire.io", entireAuthIdentifier},
		{fixtureAccessToken, entireAuthIdentifier},
		{"a/b", entireAuthIdentifier},
		// A 200-character blob has no hostname shape: it is replaced outright
		// rather than truncated into a token-looking label.
		{strings.Repeat("a", 200), entireAuthIdentifier},
		{strings.Repeat("a.", 20) + "entire.io", entireAuthIdentifier},
		{"entire_at_0000000000000000000000000000fixture", entireAuthIdentifier},
		{"sk-proj-abcdefghijklmnop", entireAuthIdentifier},
	}
	for _, tc := range cases {
		if got := sanitizeLabel(tc.in); got != tc.want {
			t.Fatalf("sanitizeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := authFileNameFor("us.auth.entire.io"); got != "entire-us-auth-entire-io-auth.json" {
		t.Fatalf("authFileNameFor = %q", got)
	}
	if got := labelForAuthBase("https://us.auth.entire.io"); got != "us.auth.entire.io" {
		t.Fatalf("labelForAuthBase = %q", got)
	}
	if got := labelForAuthBase("::bad::"); got != entireAuthIdentifier {
		t.Fatalf("labelForAuthBase(bad) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// auth.parse
// ---------------------------------------------------------------------------

// TestAuthParseOwnDocument covers a round trip of the produced auth document.
func TestAuthParseOwnDocument(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	start := startDeviceFlow(t, stub)
	skipPollInterval(t, start.State)
	stub.responses = append(stub.responses, authStubResponse{Status: http.StatusOK, Body: successTokenBody()})
	success := pollOnce(t, start.State)

	document, errMarshal := json.Marshal(map[string]any{
		storageTypeKey:        storageTypeValue,
		storageKeyEntireToken: fixtureAccessToken,
		storageKeyAccessToken: fixtureAccessToken,
		storageKeyRefreshTok:  fixtureRefreshToken,
		storageKeyIssuer:      entireAuthBaseURL,
		storageKeyLabel:       success.Auth.Label,
	})
	if errMarshal != nil {
		t.Fatalf("marshal document: %v", errMarshal)
	}
	payload, errPayload := json.Marshal(authParseRequest{
		Provider: entireAuthIdentifier,
		FileName: "entire-us-auth-entire-io-auth.json",
		RawJSON:  document,
	})
	if errPayload != nil {
		t.Fatalf("marshal parse request: %v", errPayload)
	}
	raw, errParse := handleAuthMethod(methodAuthParse, payload)
	if errParse != nil {
		t.Fatalf("auth.parse failed: %v", errParse)
	}
	var decoded struct {
		Result authParseResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode parse envelope: %v", errUnmarshal)
	}
	if !decoded.Result.Handled {
		t.Fatal("an Entire auth document must be handled")
	}
	if decoded.Result.Auth == nil {
		t.Fatal("handled parse must return auth data")
	}
	storage := decodeStorage(t, *decoded.Result.Auth)
	if storage["entire_token"] != fixtureAccessToken {
		t.Fatalf("re-parsed token mismatch: %v", storage["entire_token"])
	}
	if decoded.Result.Auth.Provider != entireAuthIdentifier {
		t.Fatalf("provider = %q", decoded.Result.Auth.Provider)
	}
}

// TestAuthParseForeignDocumentIsUnhandled covers the not-mine path.
func TestAuthParseForeignDocumentIsUnhandled(t *testing.T) {
	resetAuthState(t)
	for _, body := range []string{
		`{"type":"openai","access_token":"sk-not-an-entire-token-value"}`,
		`{}`,
		`not json`,
		`{"type":"entire"}`,
	} {
		payload, errPayload := json.Marshal(authParseRequest{Provider: entireAuthIdentifier, RawJSON: []byte(body)})
		if errPayload != nil {
			t.Fatalf("marshal: %v", errPayload)
		}
		raw, errParse := handleAuthMethod(methodAuthParse, payload)
		if errParse != nil {
			t.Fatalf("auth.parse must not fail on %q: %v", body, errParse)
		}
		var decoded struct {
			Result authParseResponse `json:"result"`
		}
		if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
			t.Fatalf("decode: %v", errUnmarshal)
		}
		if decoded.Result.Handled {
			t.Fatalf("%q must not be claimed as an Entire auth", body)
		}
	}
}

// ---------------------------------------------------------------------------
// auth.refresh
// ---------------------------------------------------------------------------

// TestAuthRefreshUsesRefreshTokenGrant covers the refresh round trip.
func TestAuthRefreshUsesRefreshTokenGrant(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)

	storage, errMarshal := json.Marshal(map[string]any{
		storageKeyEntireToken: fixtureAccessToken,
		storageKeyRefreshTok:  fixtureRefreshToken,
		storageKeyIssuer:      entireAuthBaseURL,
	})
	if errMarshal != nil {
		t.Fatalf("marshal storage: %v", errMarshal)
	}
	const rotated = "entire_at_3333333333333333333333333333fixture"
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusOK,
		Body: tokenBody(map[string]any{
			"access_token":  rotated,
			"refresh_token": fixtureRefreshToken,
			"token_type":    "Bearer",
			"expires_in":    1800,
			"scope":         "cli offline_access",
		}),
	})
	payload, errPayload := json.Marshal(authRefreshRequest{
		AuthID:       "entire-us-auth-entire-io-auth.json",
		AuthProvider: entireAuthIdentifier,
		StorageJSON:  storage,
	})
	if errPayload != nil {
		t.Fatalf("marshal refresh request: %v", errPayload)
	}
	raw, errRefresh := handleAuthMethod(methodAuthRefresh, payload)
	if errRefresh != nil {
		t.Fatalf("auth.refresh failed: %v", errRefresh)
	}
	call := stub.lastCall(t)
	if call.URL != "https://us.auth.entire.io/oauth/token" {
		t.Fatalf("refresh url = %s", call.URL)
	}
	form, errForm := url.ParseQuery(call.Body)
	if errForm != nil {
		t.Fatalf("parse refresh form: %v", errForm)
	}
	if form.Get("grant_type") != "refresh_token" {
		t.Fatalf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != fixtureRefreshToken {
		t.Fatal("refresh_token not sent")
	}
	if form.Get("client_id") != "entire-cli" {
		t.Fatalf("client_id = %q", form.Get("client_id"))
	}

	var decoded struct {
		Result authRefreshResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode refresh envelope: %v", errUnmarshal)
	}
	updated := decodeStorage(t, decoded.Result.Auth)
	if updated["entire_token"] != rotated {
		t.Fatalf("rotated token not stored: %v", updated["entire_token"])
	}
	if decoded.Result.Auth.ID != "entire-us-auth-entire-io-auth.json" {
		t.Fatalf("auth id = %q", decoded.Result.Auth.ID)
	}
	if decoded.Result.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter must be set for an expiring token")
	}
	if !decoded.Result.NextRefreshAfter.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("NextRefreshAfter = %v", decoded.Result.NextRefreshAfter)
	}
}

// TestAuthRefreshWithoutRefreshTokenFails covers the missing-secret path.
func TestAuthRefreshWithoutRefreshTokenFails(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	storage, _ := json.Marshal(map[string]any{storageKeyEntireToken: fixtureAccessToken})
	payload, _ := json.Marshal(authRefreshRequest{
		AuthID:       "entire-auth.json",
		AuthProvider: entireAuthIdentifier,
		StorageJSON:  storage,
	})
	if _, errRefresh := handleAuthMethod(methodAuthRefresh, payload); errRefresh == nil {
		t.Fatal("refresh without a refresh_token must fail")
	} else if errRefresh.Code != codeAuthTokenUnavailable {
		t.Fatalf("code = %s, want %s", errRefresh.Code, codeAuthTokenUnavailable)
	}
	if stub.callCount() != 0 {
		t.Fatal("no upstream call must be made without a refresh token")
	}
}

// TestAuthRefreshRejectionIsReported covers a provider-side refresh rejection.
func TestAuthRefreshRejectionIsReported(t *testing.T) {
	resetAuthState(t)
	stub := &authStubClient{}
	installAuthStub(t, stub)
	stub.responses = append(stub.responses, authStubResponse{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "invalid_grant", "error_description": "refresh token expired"}),
	})
	storage, _ := json.Marshal(map[string]any{storageKeyRefreshTok: fixtureRefreshToken})
	payload, _ := json.Marshal(authRefreshRequest{
		AuthID:       "entire-auth.json",
		AuthProvider: entireAuthIdentifier,
		StorageJSON:  storage,
	})
	raw, errRefresh := handleAuthMethod(methodAuthRefresh, payload)
	if errRefresh == nil {
		t.Fatalf("a rejected refresh must fail, got %s", raw)
	}
	// A dead refresh token is a sign-in problem: the code says so explicitly
	// and the message tells the operator what to do.
	if errRefresh.Code != codeAuthReloginRequired {
		t.Fatalf("code = %s, want %s", errRefresh.Code, codeAuthReloginRequired)
	}
	if errRefresh.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("http status = %d, want 401", errRefresh.HTTPStatus)
	}
	if !strings.Contains(errRefresh.Message, "sign in again") {
		t.Fatalf("message must instruct a sign-in: %q", errRefresh.Message)
	}
	if strings.Contains(errRefresh.Message, fixtureRefreshToken) {
		t.Fatal("refresh token leaked into the error message")
	}
}

// TestAuthRefreshForeignProviderFails covers the provider filter on refresh.
func TestAuthRefreshForeignProviderFails(t *testing.T) {
	resetAuthState(t)
	payload, _ := json.Marshal(authRefreshRequest{AuthProvider: "openai"})
	if _, errRefresh := handleAuthMethod(methodAuthRefresh, payload); errRefresh == nil {
		t.Fatal("a non-entire provider must be refused on refresh")
	}
}

// ---------------------------------------------------------------------------
// Registration wiring
// ---------------------------------------------------------------------------

// TestAuthCapabilityBlock covers the capability block the integration merges.
func TestAuthCapabilityBlock(t *testing.T) {
	resetAuthState(t)
	capabilities := authProviderCapabilities()
	if !capabilities.AuthProvider {
		t.Fatal("auth_provider must be declared for the login flow to be routable")
	}
	raw, errMarshal := json.Marshal(capabilities)
	if errMarshal != nil {
		t.Fatalf("marshal capabilities: %v", errMarshal)
	}
	if !strings.Contains(string(raw), `"auth_provider":true`) {
		t.Fatalf("capability json = %s", raw)
	}
	if got := authIdentifierResultForRegistration(); got != "entire" {
		t.Fatalf("registration identifier = %q", got)
	}
}

// TestTokenRequestModes covers the single documented knob for adjusting the
// token request if the endpoint starts requiring client authentication.
func TestTokenRequestModes(t *testing.T) {
	resetAuthState(t)
	form, errBuild := buildTokenRequest(tokenRequestForm, entireAuthBaseURL, tokenRequestFormDoc{
		GrantType:  entireDeviceGrantType,
		DeviceCode: fixtureDeviceCode,
	})
	if errBuild != nil {
		t.Fatalf("build token request: %v", errBuild)
	}
	values, errParse := url.ParseQuery(string(form))
	if errParse != nil {
		t.Fatalf("parse form: %v", errParse)
	}
	if values.Get("client_id") != entireDeviceClientID {
		t.Fatalf("client_id = %q", values.Get("client_id"))
	}
	if values.Get("scope") != entireDeviceScope {
		t.Fatalf("scope = %q", values.Get("scope"))
	}
	if _, errUnsupported := buildTokenRequest(tokenRequestForm, entireAuthBaseURL, tokenRequestFormDoc{GrantType: "password"}); errUnsupported == nil {
		t.Fatal("an unsupported grant must fail")
	}
	if _, errEmpty := buildTokenRequest(tokenRequestForm, entireAuthBaseURL, tokenRequestFormDoc{GrantType: entireDeviceGrantType}); errEmpty == nil {
		t.Fatal("a device grant without a device code must fail")
	}
}

// TestFingerprintAuthStateIsNotReversible covers the log correlation helper.
func TestFingerprintAuthStateIsNotReversible(t *testing.T) {
	resetAuthState(t)
	state, errState := newLoginState()
	if errState != nil {
		t.Fatalf("newLoginState: %v", errState)
	}
	fingerprint := fingerprintAuthState(state)
	if len(fingerprint) != 8 {
		t.Fatalf("fingerprint = %q", fingerprint)
	}
	if strings.Contains(fingerprint, state) || fingerprint == state {
		t.Fatal("fingerprint must not contain the state")
	}
	if fingerprintAuthState(state) != fingerprint {
		t.Fatal("fingerprint must be stable")
	}
}
