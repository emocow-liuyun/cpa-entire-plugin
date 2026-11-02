package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubBillingClient records requests and returns a canned response.
type stubBillingClient struct {
	status  int
	headers http.Header
	body    []byte
	err     error
	// calls records every request in order.
	calls []stubCall
}

type stubCall struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

func (s *stubBillingClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	s.calls = append(s.calls, stubCall{Method: method, URL: rawURL, Headers: headers.Clone(), Body: body})
	if s.err != nil {
		return 0, nil, nil, s.err
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, s.headers, s.body, nil
}

// newStubClient builds a stub returning body as JSON with the given status.
func newStubClient(status int, body string) *stubBillingClient {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return &stubBillingClient{status: status, headers: headers, body: []byte(body)}
}

// metadataFetchReq builds a quota.fetch request carrying a per-credential token.
func metadataFetchReq() quotaFetchRequest {
	return quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		Metadata: map[string]any{"entire_token": "cred-metadata-token-9911"},
	}}
}

// resetPluginState restores defaults between tests.
func resetPluginState(t *testing.T) {
	t.Helper()
	shutdownFlag.Store(false)
	resetSecretRegistry()
	setBillingClient(nil)
	setConfig(runtimeConfig{
		BaseURL:               defaultBaseURL,
		AllowedHosts:          append([]string(nil), defaultAllowedHosts...),
		RequireTopUpOnFailure: true,
		TimeoutSeconds:        defaultTimeoutSecond,
	}, nil)
}

// registerWithConfig runs plugin.register with the given YAML and returns the envelope.
func registerWithConfig(t *testing.T, yamlConfig string) map[string]any {
	t.Helper()
	payload, errMarshal := json.Marshal(lifecycleRequest{
		ConfigYAML:    []byte(yamlConfig),
		SchemaVersion: schemaVersion,
	})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}
	raw, errHandle := handleMethod("plugin.register", payload)
	if errHandle != nil {
		t.Fatalf("plugin.register failed: %v", errHandle)
	}
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode register envelope: %v", errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("register envelope not ok: %s", raw)
	}
	return envelope.Result
}

func TestSchemaNegotiation(t *testing.T) {
	for _, tc := range []struct{ request, want uint32 }{{0, 1}, {6, 6}, {8, 8}, {9, 8}} {
		got := buildRegistration(tc.request).SchemaVersion
		if got != tc.want {
			t.Fatalf("request %d: got schema %d, want %d", tc.request, got, tc.want)
		}
	}
}

// fetch invokes quota.fetch and returns the decoded envelope plus any plugin error.
func fetch(t *testing.T, req quotaFetchRequest) (map[string]any, *pluginError) {
	t.Helper()
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal fetch request: %v", errMarshal)
	}
	raw, errHandle := handleMethod("quota.fetch", payload)
	if errHandle != nil {
		return nil, errHandle
	}
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode fetch envelope: %v", errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("fetch envelope not ok: %s", raw)
	}
	return envelope.Result, nil
}

// configWithToken registers a minimal plugin config. Tokens are per-credential
// (auth storage / metadata); the plugin config itself carries no token.
const configWithToken = "enabled: true\npriority: 1\n"

// TestRegisterDeclaresQuotaProviderOnly covers the registration contract.
func TestRegisterDeclaresQuotaProviderOnly(t *testing.T) {
	resetPluginState(t)
	result := registerWithConfig(t, configWithToken)

	if got := result["schema_version"]; got != float64(schemaVersion) {
		t.Fatalf("schema_version = %v, want %d", got, schemaVersion)
	}
	caps, okCaps := result["capabilities"].(map[string]any)
	if !okCaps {
		t.Fatalf("capabilities missing: %v", result)
	}
	if caps["quota_provider"] != true {
		t.Fatalf("quota_provider = %v, want true", caps["quota_provider"])
	}
	if caps["auth_provider"] != true {
		t.Fatal("auth_provider must be declared for the Entire OAuth card")
	}
	for _, forbidden := range []string{"executor", "management_api", "frontend_auth_provider"} {
		if _, present := caps[forbidden]; present {
			t.Fatalf("capability %q must not be declared: %v", forbidden, caps)
		}
	}
	metadata, okMeta := result["metadata"].(map[string]any)
	if !okMeta {
		t.Fatalf("metadata missing: %v", result)
	}
	if metadata["Name"] != pluginName {
		t.Fatalf("metadata.Name = %v, want %s", metadata["Name"], pluginName)
	}
	if strings.TrimSpace(metadata["GitHubRepository"].(string)) == "" {
		t.Fatal("GitHubRepository must be non-empty or the host rejects the plugin")
	}
	fields, okFields := metadata["ConfigFields"].([]any)
	if !okFields || len(fields) == 0 {
		t.Fatalf("ConfigFields must be declared: %v", metadata["ConfigFields"])
	}
}

// TestIdentifyAndDescribe covers the identifier and describe RPCs.
func TestIdentifyAndDescribe(t *testing.T) {
	resetPluginState(t)
	raw, errIdentify := handleMethod("quota.identifier", nil)
	if errIdentify != nil {
		t.Fatalf("quota.identifier failed: %v", errIdentify)
	}
	if !strings.Contains(string(raw), `"identifier":"entire"`) {
		t.Fatalf("identifier envelope = %s", raw)
	}

	raw, errDescribe := handleMethod("quota.describe", []byte(`{"plugin":{"Name":"cpa-entire-quota"}}`))
	if errDescribe != nil {
		t.Fatalf("quota.describe failed: %v", errDescribe)
	}
	var decoded struct {
		OK     bool           `json:"ok"`
		Result describeResult `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode describe: %v", errUnmarshal)
	}
	if !decoded.Result.SupportsReset {
		// SupportsReset must be false: Entire has no reset endpoint.
	} else {
		t.Fatal("SupportsReset must be false")
	}
	if len(decoded.Result.SupportedProviders) != 1 || decoded.Result.SupportedProviders[0] != providerIdentifier {
		t.Fatalf("supported providers = %v", decoded.Result.SupportedProviders)
	}
}

// TestUnknownMethodFails ensures unknown RPCs are rejected.
func TestUnknownMethodFails(t *testing.T) {
	resetPluginState(t)
	if _, errHandle := handleMethod("executor.execute", nil); errHandle == nil {
		t.Fatal("executor.execute must not be served by a quota-only plugin")
	} else if errHandle.Code != codeUnknownMethod {
		t.Fatalf("code = %s, want %s", errHandle.Code, codeUnknownMethod)
	}
}

// TestFetchReportsBalance covers the happy path.
func TestFetchReportsBalance(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	client := newStubClient(http.StatusOK, `{
		"state": "active",
		"currency": "usd",
		"balance": {"total_cents": 12345, "trial_cents": 10000, "purchased_cents": 2345, "granted_cents": 0, "currency": "usd"},
		"automatic_top_up": {"enabled": true, "threshold_cents": 1000, "target_cents": 5000, "currency": "usd"},
		"overage": {"amount_cents": 0, "currency": "usd"},
		"runs_paused": {"paused": false},
		"trial": {"state": "active", "currency": "usd"}
	}`)
	setBillingClient(client)

	result, errFetch := fetch(t, quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		AuthIndex: "idx-1", AuthID: "auth-1", Provider: providerIdentifier,
		Metadata: map[string]any{"entire_token": "entire-test-token-0123456789"},
	}, HostCallbackID: "42"})
	if errFetch != nil {
		t.Fatalf("fetch failed: %v", errFetch)
	}

	summary, okSummary := result["summary"].([]any)
	if !okSummary || len(summary) == 0 {
		t.Fatalf("summary missing: %v", result)
	}
	balance := metricByKey(t, summary, metricCreditBalance)
	if balance["value"] != float64(123.45) {
		t.Fatalf("balance value = %v, want 123.45", balance["value"])
	}
	if balance["format"] != "currency" || balance["currency"] != "USD" {
		t.Fatalf("balance metric format/currency = %v/%v", balance["format"], balance["currency"])
	}

	groups, okGroups := result["groups"].([]any)
	if !okGroups || len(groups) != 1 {
		t.Fatalf("groups = %v, want a single Entire credits group (no spending limits)", result["groups"])
	}
	if got := groups[0].(map[string]any)["displayName"]; got != groupCredit {
		t.Fatalf("group displayName = %v, want %s", got, groupCredit)
	}
	statusMetric := metricByKey(t, summary, metricAutoTopUpStatus)
	if statusMetric["value"] != float64(1) || statusMetric["format"] != "number" {
		t.Fatalf("autotopup status metric = %v, want 1/number", statusMetric)
	}
	for _, banned := range []string{"entire_spend_limit", "budget", "spend-limit"} {
		for _, item := range summary {
			metric := item.(map[string]any)
			if metric["key"] == banned {
				t.Fatalf("banned metric %q present: %v", banned, summary)
			}
		}
	}
	if len(client.calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(client.calls))
	}
	if got := client.calls[0].URL; got != "https://us.console.entire.io/api/v1/me/billing" {
		t.Fatalf("upstream URL = %s", got)
	}
	if got := client.calls[0].Headers.Get("Authorization"); got != "Bearer entire-test-token-0123456789" {
		t.Fatalf("Authorization header = %q", got)
	}
}

func metricByKey(t *testing.T, summary []any, key string) map[string]any {
	t.Helper()
	for _, item := range summary {
		metric, okMetric := item.(map[string]any)
		if !okMetric {
			continue
		}
		if metric["key"] == key {
			return metric
		}
	}
	t.Fatalf("metric %q not found in %v", key, summary)
	return nil
}

// TestZeroBalanceIsExplicitZero is the central requirement: zero is zero, not unknown.
func TestZeroBalanceIsExplicitZero(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"currency": "usd",
		"balance": {"total_cents": 0, "trial_cents": 0, "purchased_cents": 0, "granted_cents": 0, "currency": "usd"},
		"automatic_top_up": {"enabled": true, "threshold_cents": 1000, "target_cents": 5000, "currency": "usd"},
		"overage": {"amount_cents": 0, "currency": "usd"},
		"runs_paused": {"paused": false},
		"trial": {"state": "used_up", "currency": "usd"}
	}`))

	result, errFetch := fetch(t, metadataFetchReq())
	if errFetch != nil {
		t.Fatalf("zero balance must not be an error: %v", errFetch)
	}
	summary := result["summary"].([]any)
	balance := metricByKey(t, summary, metricCreditBalance)
	if balance["value"] != float64(0) {
		t.Fatalf("balance value = %v, want 0", balance["value"])
	}
	groups := result["groups"].([]any)
	credit := groups[0].(map[string]any)
	buckets := credit["buckets"].([]any)
	bucket := buckets[0].(map[string]any)
	if bucket["remainingFraction"] != float64(0) {
		t.Fatalf("remainingFraction = %v, want 0", bucket["remainingFraction"])
	}
	if !strings.Contains(bucket["description"].(string), "0 cents") {
		t.Fatalf("description must state zero explicitly: %v", bucket["description"])
	}
}

// TestZeroBalanceWithUnreadableTopUpFailsClosed checks the automatic top-up rule.
func TestZeroBalanceWithUnreadableTopUpFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 0, "currency": "usd"},
		"unavailable": ["automatic_top_up"],
		"overage": {"amount_cents": 0, "currency": "usd"},
		"runs_paused": {"paused": false}
	}`))

	_, errFetch := fetch(t, metadataFetchReq())
	if errFetch == nil {
		t.Fatal("unreadable automatic top-up with zero balance must fail closed")
	}
	if errFetch.Code != codeBillingUnreadable {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeBillingUnreadable)
	}
}

// TestTopUpNotRequiredAllowsZero verifies the opt-out of the top-up rule.
func TestTopUpNotRequiredAllowsZero(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken+"require-top-up-on-failure: false\n")
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 0, "currency": "usd"},
		"unavailable": ["automatic_top_up"],
		"overage": {"amount_cents": 0, "currency": "usd"},
		"runs_paused": {"paused": false}
	}`))

	result, errFetch := fetch(t, metadataFetchReq())
	if errFetch != nil {
		t.Fatalf("opt-out must allow a confirmed zero: %v", errFetch)
	}
	balance := metricByKey(t, result["summary"].([]any), metricCreditBalance)
	if balance["value"] != float64(0) {
		t.Fatalf("balance = %v, want 0", balance["value"])
	}
}

// TestUnknownStatesFailClosed covers unmodeled and non-spendable states.
func TestUnknownStatesFailClosed(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantError bool
	}{
		{"unmodeled state", `{"state":"quantum","balance":{"total_cents":5000,"currency":"usd"}}`, true},
		{"pending card", `{"state":"pending_card","trial":{"state":"awaiting_card","credit_total_cents":10000}}`, true},
		{"not started", `{"state":"not_started"}`, true},
		{"missing state", `{"balance":{"total_cents":5000,"currency":"usd"},"automatic_top_up":{"enabled":true,"threshold_cents":0,"target_cents":0},"overage":{"amount_cents":0},"runs_paused":{"paused":false}}`, true},
		{"unknown only", `{"state":"weird-value"}`, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetPluginState(t)
			registerWithConfig(t, configWithToken)
			setBillingClient(newStubClient(http.StatusOK, testCase.payload))
			_, errFetch := fetch(t, metadataFetchReq())
			if testCase.wantError && errFetch == nil {
				t.Fatalf("expected fail closed for %s", testCase.name)
			}
			if testCase.wantError && errFetch.Code != codeUnknownQuota {
				t.Fatalf("code = %s, want %s", errFetch.Code, codeUnknownQuota)
			}
		})
	}
}

// TestRunsPausedFailsClosed covers billing enforcement.
func TestRunsPausedFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 4200, "currency": "usd"},
		"automatic_top_up": {"enabled": true, "threshold_cents": 500, "target_cents": 2000},
		"overage": {"amount_cents": 900},
		"runs_paused": {"paused": true, "reason": "over_limit"}
	}`))

	_, errFetch := fetch(t, metadataFetchReq())
	if errFetch == nil {
		t.Fatal("paused runs must fail closed even with a positive balance")
	}
	if errFetch.Code != codeUnknownQuota {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeUnknownQuota)
	}
}

// TestMissingBalanceFailsClosed covers unreadable balance sections.
func TestMissingBalanceFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"unavailable": ["balance"],
		"automatic_top_up": {"enabled": true, "threshold_cents": 0, "target_cents": 0},
		"overage": {"amount_cents": 0},
		"runs_paused": {"paused": false}
	}`))

	_, errFetch := fetch(t, metadataFetchReq())
	if errFetch == nil || errFetch.Code != codeBillingUnreadable {
		t.Fatalf("unreadable balance must fail closed, got %v", errFetch)
	}
}

// TestFailedTopUpChargeFailsClosed covers last_charge_failed.
func TestFailedTopUpChargeFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 300, "currency": "usd"},
		"automatic_top_up": {"enabled": false, "last_charge_failed": true, "threshold_cents": 1000, "target_cents": 5000},
		"overage": {"amount_cents": 0},
		"runs_paused": {"paused": false}
	}`))

	_, errFetch := fetch(t, metadataFetchReq())
	if errFetch == nil || errFetch.Code != codeUnknownQuota {
		t.Fatalf("failed top-up charge must fail closed, got %v", errFetch)
	}
}

// TestMissingCredentialFailsClosed covers a credential that carries no token.
func TestMissingCredentialFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, "enabled: true\n")
	setBillingClient(newStubClient(http.StatusOK, `{"state":"active"}`))

	_, errFetch := fetch(t, quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		Metadata: map[string]any{"other": "value"},
	}})
	if errFetch == nil {
		t.Fatal("missing credential must fail closed")
	}
	if errFetch.Code != codeMissingCredential {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeMissingCredential)
	}
}

// TestMetadataFallbackToken verifies the documented fallback chain.
func TestMetadataFallbackToken(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, "enabled: true\n")
	client := newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 100, "currency": "usd"},
		"automatic_top_up": {"enabled": true, "threshold_cents": 0, "target_cents": 1000},
		"overage": {"amount_cents": 0},
		"runs_paused": {"paused": false}
	}`)
	setBillingClient(client)

	_, errFetch := fetch(t, quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		Metadata: map[string]any{"entire_token": "metadata-token-abcdefgh"},
	}})
	if errFetch != nil {
		t.Fatalf("metadata token fallback failed: %v", errFetch)
	}
	if got := client.calls[0].Headers.Get("Authorization"); got != "Bearer metadata-token-abcdefgh" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestUpstreamAuthErrorsAreMapped covers 401/403/421 and generic failures.
func TestUpstreamAuthErrorsAreMapped(t *testing.T) {
	cases := []struct {
		status     int
		wantCode   string
		wantStatus int
	}{
		{http.StatusUnauthorized, codeUnauthenticated, http.StatusUnauthorized},
		{http.StatusForbidden, codeForbidden, http.StatusForbidden},
		{http.StatusMisdirectedRequest, codeUpstreamStatus, http.StatusMisdirectedRequest},
		{http.StatusInternalServerError, codeUpstreamStatus, http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			resetPluginState(t)
			registerWithConfig(t, configWithToken)
			setBillingClient(newStubClient(testCase.status, `{"detail":"nope"}`))
			_, errFetch := fetch(t, metadataFetchReq())
			if errFetch == nil {
				t.Fatalf("status %d must fail closed", testCase.status)
			}
			if errFetch.Code != testCase.wantCode {
				t.Fatalf("code = %s, want %s", errFetch.Code, testCase.wantCode)
			}
			if errFetch.HTTPStatus != testCase.wantStatus {
				t.Fatalf("http status = %d, want %d", errFetch.HTTPStatus, testCase.wantStatus)
			}
		})
	}
}

// TestMalformedUpstreamFailsClosed covers non-JSON and schema-mismatched bodies.
func TestMalformedUpstreamFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `<html>oops</html>`},
		{"schema mismatch", `{"unexpected":{"a":1}}`},
		{"empty", ``},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetPluginState(t)
			registerWithConfig(t, configWithToken)
			setBillingClient(newStubClient(http.StatusOK, testCase.body))
			_, errFetch := fetch(t, metadataFetchReq())
			if errFetch == nil {
				t.Fatalf("malformed upstream (%s) must fail closed", testCase.name)
			}
			if errFetch.Code != codeUpstreamMalformed {
				t.Fatalf("code = %s, want %s", errFetch.Code, codeUpstreamMalformed)
			}
		})
	}
}

// TestResetIsRefused covers quota.reset.
func TestResetIsRefused(t *testing.T) {
	resetPluginState(t)
	raw, errReset := handleMethod("quota.reset", []byte(`{}`))
	if errReset != nil {
		t.Fatalf("quota.reset RPC failed: %v", errReset)
	}
	var decoded struct {
		OK     bool        `json:"ok"`
		Result resetResult `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode reset: %v", errUnmarshal)
	}
	if !decoded.OK || decoded.Result.Success {
		t.Fatalf("reset must report success=false: %s", raw)
	}
}

// TestTokenNeverAppearsInErrors is the credential-hygiene guarantee.
func TestTokenNeverAppearsInErrors(t *testing.T) {
	const token = "super-secret-entire-token-0001"
	resetPluginState(t)
	resetSecretRegistry()
	registerWithConfig(t, "enabled: true\n")

	// Register the per-credential token exactly as resolveCredential does so the
	// redaction registry covers it for the whole test.
	registerSecret(token)
	client := &stubBillingClient{err: newPluginError(codeUpstreamStatus, "boom "+token, 0)}
	setBillingClient(client)
	fetchReq := quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		Metadata: map[string]any{"entire_token": token},
	}}
	_, errFetch := fetch(t, fetchReq)
	if errFetch == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(errFetch.Message, token) {
		t.Fatalf("error message leaked the token: %s", errFetch.Message)
	}

	encoded := encodeErrorEnvelope(newPluginError(codePluginError, "token="+token, 0))
	if strings.Contains(string(encoded), token) {
		t.Fatalf("error envelope leaked the token: %s", encoded)
	}

	// Upstream bodies that echo the token must not reach an error message.
	setBillingClient(newStubClient(http.StatusInternalServerError, `{"detail":"invalid token `+token+`"}`))
	_, errStatus := fetch(t, fetchReq)
	if strings.Contains(errStatus.Message, token) {
		t.Fatalf("upstream error path leaked the token: %s", errStatus.Message)
	}
}

// TestRegistrationWithMalformedConfigFailsClosed covers YAML errors.
func TestRegistrationWithMalformedConfigFailsClosed(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, "token: [unterminated\n")
	if lastConfigError() == "" {
		t.Fatal("malformed YAML must be recorded")
	}
	setBillingClient(newStubClient(http.StatusOK, `{"state":"active"}`))
	_, errFetch := fetch(t, metadataFetchReq())
	if errFetch == nil {
		t.Fatal("a rejected configuration must fail closed")
	}
}

// TestOrgScopedBillingURL covers the org-id path.
func TestOrgScopedBillingURL(t *testing.T) {
	resetPluginState(t)
	registerWithConfig(t, configWithToken+"org-id: \"01JABCDEFGHJKMNPQRSTVWXYZ\"\n")
	setBillingClient(nil)
	client := newStubClient(http.StatusOK, `{
		"state": "active",
		"balance": {"total_cents": 500, "currency": "usd"},
		"automatic_top_up": {"enabled": true, "threshold_cents": 0, "target_cents": 1000},
		"overage": {"amount_cents": 0},
		"runs_paused": {"paused": false}
	}`)
	setBillingClient(client)

	if _, errFetch := fetch(t, quotaFetchRequest{quotaFetchContext: quotaFetchContext{
		Metadata: map[string]any{"entire_token": "org-scoped-token-abcdef"},
	}}); errFetch != nil {
		t.Fatalf("org fetch failed: %v", errFetch)
	}
	wantURL := "https://us.console.entire.io/api/v1/orgs/01JABCDEFGHJKMNPQRSTVWXYZ/billing"
	if got := client.calls[0].URL; got != wantURL {
		t.Fatalf("URL = %s, want %s", got, wantURL)
	}
}

// TestDirectClientHonoursAllowList proves the transport seam keeps the domain guard.
func TestDirectClientHonoursAllowList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer loopback-token-123456" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"state": "active",
			"balance": {"total_cents": 2500, "currency": "usd"},
			"automatic_top_up": {"enabled": true, "threshold_cents": 0, "target_cents": 10000},
			"overage": {"amount_cents": 0},
			"runs_paused": {"paused": false}
		}`))
	}))
	defer server.Close()

	// The direct client is only reachable for an allow-listed host; a loopback
	// fixture needs the explicit localhost entry.
	cfg := runtimeConfig{
		BaseURL:               server.URL,
		AllowedHosts:          []string{"localhost"},
		RequireTopUpOnFailure: true,
		TimeoutSeconds:        5,
	}
	client := directBillingClient{httpClient: server.Client(), allowedHosts: cfg.AllowedHosts}

	status, _, errRead := readBilling(context.Background(), cfg, credential{Token: "loopback-token-123456", Source: sourceMetadata}, client)
	if errRead != nil {
		t.Fatalf("loopback read failed: %v", errRead)
	}
	if status.Balance == nil || status.Balance.TotalCents == nil || *status.Balance.TotalCents != 2500 {
		t.Fatalf("unexpected balance: %+v", status.Balance)
	}

	// The same server without the loopback allow-list entry must be refused.
	blockedClient := directBillingClient{httpClient: server.Client(), allowedHosts: defaultAllowedHosts}
	if _, _, errBlocked := readBilling(context.Background(), runtimeConfig{
		BaseURL: server.URL, AllowedHosts: defaultAllowedHosts, TimeoutSeconds: 5,
	}, credential{Token: "loopback-token-123456"}, blockedClient); errBlocked == nil {
		t.Fatal("non-allow-listed host must be refused")
	}
}
