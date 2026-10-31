package main

// Tests for the automatic credential renewal path (renew.go).
//
// Nothing here touches a real auth file, a real identity provider or a real
// credential: host.auth.get / host.auth.save are served by an in-memory fake
// installed through installAuthDocOverride, the token endpoint by the authStubClient
// seam, and every token is a synthetic fixture.

import (
	"context"
	"encoding/base64"
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

const (
	// renewalExpiredAccess is a JWT-shaped token whose exp is in the past.
	renewalExpiredAccess = "entire_at_4444444444444444444444444444fixture"
	// renewalFreshAccess is the token the stub refresh hands back.
	renewalFreshAccess = "entire_at_5555555555555555555555555555fixture"
	// renewalRefresh is the single-use refresh token on disk.
	renewalRefresh = "entire_rt_6666666666666666666666666666fixture"
	// renewalRotatedRefresh is the rotated refresh token.
	renewalRotatedRefresh = "entire_rt_7777777777777777777777777777fixture"
	// renewalFileName is the auth file the host holds for the credential.
	renewalFileName = "entire-3ce7fab500d15cf7c278d8d5.json"
	// renewalAuthID is the host auth id (equal to the auth index by default).
	renewalAuthID = renewalFileName
)

// fixtureJWT renders a JWT-shaped token with the given exp.
func fixtureJWT(prefix string, exp time.Time) string {
	_ = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, errMarshal := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"iss": "https://us.auth.entire.io",
		"sub": "fixture-subject",
	})
	if errMarshal != nil {
		panic(errMarshal)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return prefix + "." + payload + ".signature"
}

// ---------------------------------------------------------------------------
// Host auth file fake
// ---------------------------------------------------------------------------

// docStore is the in-memory host auth file surface.
type docStore struct {
	mu      sync.Mutex
	docs    map[string]map[string]any // keyed by auth index
	name    string
	gets    int
	saves   int
	getErr  error
	saveErr error
	// saved records every document written, in order.
	saved []map[string]any
	// savesByName records the file name each save targeted.
	names []string
}

// newDocStore builds a store holding one document under renewalAuthID.
func newDocStore(doc map[string]any) *docStore {
	return &docStore{docs: map[string]map[string]any{renewalAuthID: doc}, name: renewalFileName}
}

func (s *docStore) Get(ctx context.Context, authIndex string) (hostAuthDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return hostAuthDocument{}, s.getErr
	}
	doc, exists := s.docs[authIndex]
	if !exists {
		return hostAuthDocument{}, newPluginError(codeAuthRenewHostFailure, "auth not found", 0)
	}
	raw, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		return hostAuthDocument{}, errMarshal
	}
	return hostAuthDocument{AuthIndex: authIndex, Name: s.name, JSON: raw}, nil
}

func (s *docStore) Save(ctx context.Context, name string, doc map[string]any) (hostAuthSaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.names = append(s.names, name)
	if s.saveErr != nil {
		return hostAuthSaveResult{}, s.saveErr
	}
	copied := make(map[string]any, len(doc))
	for key, value := range doc {
		copied[key] = value
	}
	s.saved = append(s.saved, copied)
	for index := range s.docs {
		s.docs[index] = copied
	}
	return hostAuthSaveResult{Name: name, Path: "/fixture/auths/" + name}, nil
}

func (s *docStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.saves
}

func (s *docStore) lastSaved() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saved) == 0 {
		return nil
	}
	return s.saved[len(s.saved)-1]
}

// installAuthClientStub routes the auth transport to an arbitrary
// authHTTPClient double and restores the previous one afterwards.
func installAuthClientStub(t *testing.T, client authHTTPClient) {
	t.Helper()
	previous := installAuthClientOverride(client)
	t.Cleanup(func() {
		installAuthClientOverride(previous)
		setAuthCallbackProbe(nil)
	})
}

// installDocStore routes host auth file IO to store for the current test.
func installDocStore(t *testing.T, store *docStore) {
	t.Helper()
	previous := installAuthDocOverride(store)
	t.Cleanup(func() {
		installAuthDocOverride(previous)
	})
}

// ---------------------------------------------------------------------------
// Credential fixtures
// ---------------------------------------------------------------------------

// expiredDoc is the auth file shape the reported failure produced: an expired
// access token and a refresh token, with operator fields that must survive.
func expiredDoc() map[string]any {
	expired := fixtureJWT(renewalExpiredAccess, time.Now().Add(-90*time.Minute))
	return map[string]any{
		"type":          "entire",
		"prefix":        "entire",
		"entire_token":  expired,
		"access_token":  expired,
		"refresh_token": renewalRefresh,
		"token_type":    "Bearer",
		"issuer":        entireAuthBaseURL,
		"scope":         "cli offline_access",
		"expires_at":    time.Now().Add(-90 * time.Minute).Unix(),
		"disabled":      false,
		"priority":      7,
		"proxy_url":     "http://127.0.0.1:8888",
		"note":          "operator note",
		"weight":        3,
		"label":         "us.auth.entire.io",
	}
}

// storageJSONOf encodes a credential document as the fetch payload would carry it.
func storageJSONOf(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		t.Fatalf("marshal doc: %v", errMarshal)
	}
	return raw
}

// renewalFetchReq builds a quota.fetch request for the fixture credential.
func renewalFetchReq(t *testing.T, doc map[string]any) quotaFetchRequest {
	t.Helper()
	return quotaFetchRequest{
		quotaFetchContext: quotaFetchContext{
			AuthIndex:   renewalAuthID,
			AuthID:      renewalAuthID,
			Provider:    providerIdentifier,
			StorageJSON: storageJSONOf(t, doc),
		},
		HostCallbackID: "callback-1",
	}
}

// rotatedTokenBody is the token endpoint answer for a refresh grant.
func rotatedTokenBody() string {
	return tokenBody(map[string]any{
		"access_token":  renewalFreshAccess,
		"refresh_token": renewalRotatedRefresh,
		"token_type":    "Bearer",
		"expires_in":    28800,
		"scope":         "cli offline_access",
	})
}

// billingOKBody is a usable billing document with a positive balance.
const billingOKBody = `{
	"state": "active",
	"currency": "usd",
	"balance": {"total_cents": 401, "trial_cents": 0, "purchased_cents": 401, "granted_cents": 0, "currency": "usd"},
	"automatic_top_up": {"enabled": true, "threshold_cents": 100, "target_cents": 1000, "currency": "usd"},
	"overage": {"amount_cents": 0, "currency": "usd"},
	"runs_paused": {"paused": false}
}`

// resetRenewState clears plugin state, the lock strip and both seams.
func resetRenewState(t *testing.T) {
	t.Helper()
	resetAuthState(t)
	renewLocks.reset()
	installAuthDocOverride(nil)
}

// ---------------------------------------------------------------------------
// Root cause: expiry inspection
// ---------------------------------------------------------------------------

// TestExpiryInspectionTakesTheEarlierStatement is the root-cause regression.
//
// The host schedules its own refresh from expires_at only
// (cpa-source sdk/cliproxy/auth/types.go expireKeys) and never looks at the JWT
// exp claim; a cached expires_at further in the future than the token's real exp
// therefore postpones every refresh until long after the 401 window opened.
// inspectAuthExpiry must not repeat that mistake: the earlier statement wins.
func TestExpiryInspectionTakesTheEarlierStatement(t *testing.T) {
	now := time.Now()
	deadToken := fixtureJWT(renewalExpiredAccess, now.Add(-2*time.Hour))

	cases := []struct {
		name      string
		doc       map[string]any
		wantSourc expirySource
		wantHas   bool
	}{
		{
			name: "stale expires_at hides a dead jwt",
			doc: map[string]any{
				"entire_token":  deadToken,
				"expires_at":    now.Add(6 * time.Hour).Unix(),
				"refresh_token": renewalRefresh,
			},
			wantSourc: expirySourceJWT,
			wantHas:   true,
		},
		{
			name: "dead expires_at with a live jwt",
			doc: map[string]any{
				"entire_token":  fixtureJWT(renewalFreshAccess, now.Add(8*time.Hour)),
				"expires_at":    now.Add(-time.Minute).Unix(),
				"refresh_token": renewalRefresh,
			},
			wantSourc: expirySourceMeta,
			wantHas:   true,
		},
		{
			name: "expires_at only",
			doc: map[string]any{
				"entire_token": fixtureJWT(renewalFreshAccess, now.Add(8*time.Hour)),
				// A non-JWT token shape forces the metadata path.
				"access_token": "opaque-access-token-0000fixture",
				"expires_at":   now.Add(-time.Minute).Unix(),
			},
			wantSourc: expirySourceMeta,
			wantHas:   true,
		},
		{
			name:      "no expiry at all",
			doc:       map[string]any{"entire_token": "opaque-access-token-0000fixture"},
			wantSourc: expirySourceNone,
			wantHas:   false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := inspectAuthExpiry(testCase.doc)
			if got.hasExpiry != testCase.wantHas {
				t.Fatalf("hasExpiry = %v, want %v", got.hasExpiry, testCase.wantHas)
			}
			if got.source != testCase.wantSourc {
				t.Fatalf("source = %q, want %q", got.source, testCase.wantSourc)
			}
		})
	}
}

// TestExpiryInspectionFlagsExpiredWithoutRefresh covers the unhealable class.
func TestExpiryInspectionFlagsExpiredWithoutRefresh(t *testing.T) {
	doc := map[string]any{
		"entire_token": fixtureJWT(renewalExpiredAccess, time.Now().Add(-time.Hour)),
	}
	if defect := inspectAuthExpiry(doc).defect(); defect == "" {
		t.Fatal("an expired credential without a refresh token must be reported as defective")
	}
	doc["refresh_token"] = renewalRefresh
	if defect := inspectAuthExpiry(doc).defect(); defect != "" {
		t.Fatalf("a renewable credential must not be flagged: %s", defect)
	}
}

// TestExpirySourcesKeepBothStatements checks the unix and RFC 3339 forms.
func TestExpirySourcesKeepBothStatements(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, value := range []any{
		expiredAt.Unix(),
		expiredAt.Format(time.RFC3339),
		json.Number("1700000000"),
	} {
		doc := map[string]any{"access_token": "opaque-access-token-0000fixture", "expires_at": value}
		got := inspectAuthExpiry(doc)
		if !got.hasExpiry {
			t.Fatalf("expires_at %v was not decoded", value)
		}
	}
}

// ---------------------------------------------------------------------------
// 401 renewal end to end
// ---------------------------------------------------------------------------

// TestFetchRenewsOn401AndPersistsRotation is the central requirement: a 401 from
// the billing endpoint rotates the credential once, writes it back with every
// original field, and serves the quota from the retry.
func TestFetchRenewsOn401AndPersistsRotation(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)

	store := newDocStore(expiredDoc())
	installDocStore(t, store)

	billing := &sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusOK, body: billingOKBody},
	}}
	setBillingClient(billing)

	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	result, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch != nil {
		t.Fatalf("fetch after renewal failed: %v", errFetch)
	}
	if _, okSummary := result["summary"].([]any); !okSummary {
		t.Fatalf("renewed fetch produced no summary: %v", result)
	}

	// One refresh grant, one save, exactly two billing reads (original + retry).
	if stub.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", stub.callCount())
	}
	if len(billing.calls) != 2 {
		t.Fatalf("billing calls = %d, want 2 (original + one retry)", len(billing.calls))
	}
	if got := billing.calls[1].Headers.Get("Authorization"); got != "Bearer "+renewalFreshAccess {
		t.Fatalf("retry used %q, want the rotated access token", got)
	}
	gets, saves := store.counts()
	if gets != 1 {
		t.Fatalf("host.auth.get calls = %d, want 1 (re-read under the lock)", gets)
	}
	if saves != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1", saves)
	}

	// The save keeps the original file name and every original field.
	if len(store.names) != 1 || store.names[0] != renewalFileName {
		t.Fatalf("saved file name = %v, want %s", store.names, renewalFileName)
	}
	saved := store.lastSaved()
	for _, key := range []string{"prefix", "disabled", "priority", "proxy_url", "note", "weight", "label", "issuer", "scope"} {
		if _, present := saved[key]; !present {
			t.Fatalf("field %q was dropped by the rotation: %v", key, saved)
		}
	}
	if saved["prefix"] != "entire" {
		t.Fatalf("prefix = %v, want entire", saved["prefix"])
	}
	if saved["disabled"] != false {
		t.Fatalf("disabled = %v, want false", saved["disabled"])
	}
	if saved["priority"] != float64(7) && saved["priority"] != 7 {
		t.Fatalf("priority = %v, want 7", saved["priority"])
	}
	if saved["entire_token"] != renewalFreshAccess || saved["access_token"] != renewalFreshAccess {
		t.Fatal("rotated access token was not written to both token keys")
	}
	if saved["refresh_token"] != renewalRotatedRefresh {
		t.Fatal("rotated refresh token was not written")
	}
	// The host schedules its own auto-refresh from expires_at, so it must now be
	// in the future again or the credential stays unscheduled.
	declared, okDeclared := saved["expires_at"].(int64)
	if !okDeclared {
		t.Fatalf("expires_at = %v (%T), want a unix second integer", saved["expires_at"], saved["expires_at"])
	}
	if declared <= time.Now().Unix() {
		t.Fatalf("expires_at = %d, want a future unix timestamp", declared)
	}
}

// TestFetchRenewsOnRequestExpiryWithout401 covers the case where the payload
// already shows the credential is dead: renewal happens before the 401 is even
// manufactured by the next host sweep.
func TestFetchRenewsOnLocallyExpiredCredential(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)

	store := newDocStore(expiredDoc())
	installDocStore(t, store)

	billing := &sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusOK, body: billingOKBody},
	}}
	setBillingClient(billing)

	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	if _, errFetch := fetch(t, renewalFetchReq(t, expiredDoc())); errFetch != nil {
		t.Fatalf("fetch failed: %v", errFetch)
	}
	if stub.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", stub.callCount())
	}
}

// TestFetchRenewsAtMostOnce covers the retry budget: a second 401 after a
// successful rotation is terminal and must not loop.
func TestFetchRenewsAtMostOnce(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)

	store := newDocStore(expiredDoc())
	installDocStore(t, store)

	billing := &sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusUnauthorized, body: `{"detail":"still invalid"}`},
		{status: http.StatusOK, body: billingOKBody},
	}}
	setBillingClient(billing)

	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a second 401 must fail")
	}
	if errFetch.Code != codeAuthReloginRequired {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeAuthReloginRequired)
	}
	if stub.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", stub.callCount())
	}
	if len(billing.calls) != 2 {
		t.Fatalf("billing calls = %d, want 2", len(billing.calls))
	}
}

// TestFetchReReadsHostAuthFileBeforeRefresh covers the rotating-token rule: the
// refresh token used is the one currently on disk, not the one in the payload.
func TestFetchReReadsHostAuthFileBeforeRefresh(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)

	// The disk file has already been rotated by a concurrent host refresh.
	diskDoc := expiredDoc()
	diskDoc["refresh_token"] = "entire_rt_8888888888888888888888888888fixture"
	store := newDocStore(diskDoc)
	installDocStore(t, store)

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusOK, body: billingOKBody},
	}})
	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	// The payload still carries the older, already-spent refresh token.
	payloadDoc := expiredDoc()
	if _, errFetch := fetch(t, renewalFetchReq(t, payloadDoc)); errFetch != nil {
		t.Fatalf("fetch failed: %v", errFetch)
	}
	call := stub.lastCall(t)
	form := parseForm(t, call.Body)
	if got := form.Get("refresh_token"); got != "entire_rt_8888888888888888888888888888fixture" {
		t.Fatalf("refresh_token = %q, want the on-disk value", got)
	}
	if form.Get("grant_type") != entireRefreshGrantType {
		t.Fatalf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("client_id") != entireDeviceClientID {
		t.Fatalf("client_id = %q", form.Get("client_id"))
	}
}

// TestFetchRenewalNeverPrintsTokens covers secret hygiene on the new path.
func TestFetchRenewalNeverPrintsTokens(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	store := newDocStore(expiredDoc())
	store.saveErr = newPluginError(codeAuthRenewHostFailure, "disk full", 0)
	installDocStore(t, store)

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a failed persistence must surface on the quota result")
	}
	for _, secret := range []string{renewalRefresh, renewalRotatedRefresh, renewalFreshAccess, renewalExpiredAccess} {
		if strings.Contains(errFetch.Message, secret) {
			t.Fatal("a token leaked into the renewal error message")
		}
	}
	if strings.Contains(errFetch.Message, "entire_rt_") {
		t.Fatal("a refresh token shape leaked into the renewal error message")
	}
}

// ---------------------------------------------------------------------------
// Failure classes
// ---------------------------------------------------------------------------

// TestFetchDeadRefreshTokenAsksForRelogin covers the terminal refresh failure.
func TestFetchDeadRefreshTokenAsksForRelogin(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	installDocStore(t, newDocStore(expiredDoc()))

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	stub := &authStubClient{responses: []authStubResponse{{
		Status: http.StatusBadRequest,
		Body:   tokenBody(map[string]any{"error": "invalid_grant", "error_description": "refresh token expired"}),
	}}}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a dead refresh token must fail")
	}
	if errFetch.Code != codeAuthReloginRequired {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeAuthReloginRequired)
	}
	if !strings.Contains(errFetch.Message, "sign in again") {
		t.Fatalf("message must tell the operator to sign in: %q", errFetch.Message)
	}
	if errFetch.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("http status = %d, want 401", errFetch.HTTPStatus)
	}
	if stub.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1 (no retry on a rejected grant)", stub.callCount())
	}
}

// TestFetchTransportErrorDoesNotRetryRefresh covers the "network errors are not
// retried" rule: a transport failure must not spend a second refresh attempt.
func TestFetchTransportErrorDoesNotRetryRefresh(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	installDocStore(t, newDocStore(expiredDoc()))

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	stub := &authStubClient{responses: []authStubResponse{{
		Err: newPluginError(codeAuthTransport, "dial tcp: i/o timeout", 0),
	}}}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a transport failure must fail")
	}
	if errFetch.Code != codeAuthRenewTransport {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeAuthRenewTransport)
	}
	if stub.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1 (network errors are not retried)", stub.callCount())
	}
}

// TestFetchNonStaleFailureIsNotRenewed covers the "do not refresh for unrelated
// failures" rule: 403 and unreadable billing keep their own answer.
func TestFetchNonStaleFailureIsNotRenewed(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetRenewState(t)
			registerWithConfig(t, configWithToken)
			// A credential that is NOT expired, so nothing suggests a refresh.
			liveDoc := expiredDoc()
			liveToken := fixtureJWT(renewalFreshAccess, time.Now().Add(6*time.Hour))
			liveDoc["entire_token"] = liveToken
			liveDoc["access_token"] = liveToken
			liveDoc["expires_at"] = time.Now().Add(6 * time.Hour).Unix()
			store := newDocStore(liveDoc)
			installDocStore(t, store)

			setBillingClient(&sequenceBillingClient{responses: []stubResponse{
				{status: status, body: `{"detail":"nope"}`},
			}})
			stub := &authStubClient{}
			installAuthStub(t, stub)

			if _, errFetch := fetch(t, renewalFetchReq(t, liveDoc)); errFetch == nil {
				t.Fatal("the upstream failure must be reported")
			}
			if stub.callCount() != 0 {
				t.Fatalf("refresh calls = %d, want 0 for a non-staleness failure", stub.callCount())
			}
			if gets, _ := store.counts(); gets != 0 {
				t.Fatalf("host.auth.get calls = %d, want 0", gets)
			}
		})
	}
}

// TestFetchUnauthorizedWithoutRefreshTokenIsTerminal covers a credential that
// cannot self-heal: the answer names the sign-in step.
func TestFetchUnauthorizedWithoutRefreshTokenIsTerminal(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	doc := expiredDoc()
	delete(doc, "refresh_token")
	installDocStore(t, newDocStore(doc))

	stub := &authStubClient{}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, doc))
	if errFetch == nil {
		t.Fatal("a 401 without a refresh token must fail")
	}
	if !strings.Contains(errFetch.Message, "sign in again") {
		t.Fatalf("message must instruct a sign-in: %q", errFetch.Message)
	}
	if errFetch.Code != codeUnauthenticated {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeUnauthenticated)
	}
	if stub.callCount() != 0 {
		t.Fatal("no refresh may be attempted without a refresh token")
	}
}

// TestFetchSaveFailureStillServesTheRead covers a host write failure: the read
// succeeds with the fresh token but the operator is told the file is stale.
func TestFetchSaveFailureStillServesTheRead(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	store := newDocStore(expiredDoc())
	store.saveErr = newPluginError(codeAuthRenewHostFailure, "permission denied", 0)
	installDocStore(t, store)

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusOK, body: billingOKBody},
	}})
	stub := &authStubClient{responses: []authStubResponse{{Status: http.StatusOK, Body: rotatedTokenBody()}}}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a failed save must be reported")
	}
	if errFetch.Code != codeAuthRenewHostFailure {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeAuthRenewHostFailure)
	}
	if len(store.saved) != 0 {
		t.Fatal("no document may be recorded when the save failed")
	}
}

// TestFetchHostGetFailureIsReported covers a host.auth.get failure with a
// refresh token present: the read must not proceed on a possibly spent token.
func TestFetchHostGetFailureIsReported(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	store := newDocStore(expiredDoc())
	store.getErr = newPluginError(codeAuthRenewHostFailure, "auth file not found", 0)
	installDocStore(t, store)

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	stub := &authStubClient{}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("an unreachable host auth file must fail the renewal")
	}
	if stub.callCount() != 0 {
		t.Fatal("no refresh may be issued without a verified on-disk refresh token")
	}
}

// TestFetchRenewalDisabledByConfig covers the operator switch.
func TestFetchRenewalDisabledByConfig(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, "enabled: true\nauto-renew: false\n")
	installDocStore(t, newDocStore(expiredDoc()))
	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
	}})
	stub := &authStubClient{}
	installAuthStub(t, stub)

	_, errFetch := fetch(t, renewalFetchReq(t, expiredDoc()))
	if errFetch == nil {
		t.Fatal("a disabled renewal must not silently succeed")
	}
	if errFetch.Code != codeAuthReloginRequired {
		t.Fatalf("code = %s, want %s", errFetch.Code, codeAuthReloginRequired)
	}
	if stub.callCount() != 0 {
		t.Fatal("no refresh may be issued when renewal is disabled")
	}
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// TestRenewalAndAuthRefreshShareTheAccountLock is the single-use-token guard: a
// concurrent host-driven auth.refresh and a quota.fetch renewal for the same
// credential must never both reach the token endpoint with the same value.
func TestRenewalAndAuthRefreshShareTheAccountLock(t *testing.T) {
	resetRenewState(t)
	registerWithConfig(t, configWithToken)
	installDocStore(t, newDocStore(expiredDoc()))

	setBillingClient(&sequenceBillingClient{responses: []stubResponse{
		{status: http.StatusUnauthorized, body: `{"detail":"invalid token"}`},
		{status: http.StatusOK, body: billingOKBody},
	}})

	stub := &gateStubClient{inner: &authStubClient{responses: []authStubResponse{
		{Status: http.StatusOK, Body: rotatedTokenBody()},
		{Status: http.StatusOK, Body: rotatedTokenBody()},
	}}}
	installAuthClientStub(t, stub)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		storage := storageJSONOf(t, expiredDoc())
		payload, errMarshal := json.Marshal(authRefreshRequest{
			AuthID:       renewalAuthID,
			AuthProvider: entireAuthIdentifier,
			StorageJSON:  storage,
		})
		if errMarshal != nil {
			return
		}
		_, _ = handleAuthMethod(methodAuthRefresh, payload)
	}()
	go func() {
		defer wg.Done()
		_, _ = fetch(t, renewalFetchReq(t, expiredDoc()))
	}()
	wg.Wait()

	if peak := stub.peak(); peak > 1 {
		t.Fatalf("concurrent grants reached the token endpoint (%d in flight); the account lock is not shared", peak)
	}
}

// ---------------------------------------------------------------------------
// Waits, budget and the lock strip
// ---------------------------------------------------------------------------

// TestAccountLockSerializesSameCredential covers the lock itself.
func TestAccountLockSerializesSameCredential(t *testing.T) {
	strip := &accountLockStrip{locks: map[string]*sync.Mutex{}}
	first, okFirst := strip.acquire("id:a", time.Second)
	if !okFirst {
		t.Fatal("the first acquire must succeed")
	}
	if _, okSecond := strip.acquire("id:a", 10*time.Millisecond); okSecond {
		t.Fatal("a second acquire for the same key must time out")
	}
	other, okOther := strip.acquire("id:b", 10*time.Millisecond)
	if !okOther {
		t.Fatal("a different credential must not be blocked")
	}
	other()
	first()
	if unlock, okAgain := strip.acquire("id:a", 10*time.Millisecond); !okAgain {
		t.Fatal("the lock must be reusable after release")
	} else {
		unlock()
	}
}

// TestCredentialLockKeyFallsBackToTheAuthIndex covers the key derivation.
func TestCredentialLockKeyFallsBackToTheAuthIndex(t *testing.T) {
	if got := credentialLockKey("auth-1", "idx-1"); got != "id:auth-1" {
		t.Fatalf("key = %q", got)
	}
	if got := credentialLockKey("", "idx-1"); got != "index:idx-1" {
		t.Fatalf("key = %q", got)
	}
	if got := credentialLockKey("", ""); got != "shared" {
		t.Fatalf("key = %q", got)
	}
}

// TestRotatedPairEqualSkipsNeedlessWrites covers the idempotent write guard.
func TestRotatedPairEqualSkipsNeedlessWrites(t *testing.T) {
	doc := expiredDoc()
	updated := authStorage{token: renewalFreshAccess, refresh: renewalRefresh}
	if rotatedPairEqual(doc, updated) {
		t.Fatal("a different access token must not compare equal")
	}
	updated.refresh = renewalRotatedRefresh
	doc["refresh_token"] = renewalRotatedRefresh
	doc["entire_token"] = renewalFreshAccess
	doc["access_token"] = renewalFreshAccess
	if !rotatedPairEqual(doc, updated) {
		t.Fatal("an identical token pair must compare equal")
	}
}

// TestMergeRotatedCredentialPreservesUnknownFields covers forward compatibility:
// a field this plugin does not know must survive a rotation.
func TestMergeRotatedCredentialPreservesUnknownFields(t *testing.T) {
	doc := expiredDoc()
	doc["operator_extension"] = map[string]any{"keep": true}
	merged := mergeRotatedCredential(doc, authStorage{
		token:     renewalFreshAccess,
		refresh:   renewalRotatedRefresh,
		expiresAt: time.Now().Add(time.Hour),
	})
	if _, present := merged["operator_extension"]; !present {
		t.Fatal("an unknown field was dropped")
	}
	if merged["refresh_token"] != renewalRotatedRefresh {
		t.Fatal("the rotated refresh token was not applied")
	}
	if doc["refresh_token"] != renewalRefresh {
		t.Fatal("the input document must not be mutated")
	}
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// parseForm decodes one x-www-form-urlencoded token request body.
func parseForm(t *testing.T, body string) url.Values {
	t.Helper()
	values, errParse := url.ParseQuery(body)
	if errParse != nil {
		t.Fatalf("parse form body: %v", errParse)
	}
	return values
}

// stubResponse is one canned billing answer.
type stubResponse struct {
	status int
	body   string
}

// sequenceBillingClient answers billing reads in order.
type sequenceBillingClient struct {
	mu        sync.Mutex
	responses []stubResponse
	calls     []stubCall
}

func (s *sequenceBillingClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{Method: method, URL: rawURL, Headers: headers.Clone(), Body: body})
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	if len(s.responses) == 0 {
		return http.StatusOK, hdr, []byte(billingOKBody), nil
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	return next.status, hdr, []byte(next.body), nil
}

// gateStubClient detects concurrent token-endpoint requests.
type gateStubClient struct {
	mu    sync.Mutex
	in    int
	maxIn int
	inner *authStubClient
}

func (g *gateStubClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	g.mu.Lock()
	g.in++
	if g.in > g.maxIn {
		g.maxIn = g.in
	}
	g.mu.Unlock()
	// Hold the window open long enough for a second, unguarded caller to enter.
	time.Sleep(20 * time.Millisecond)
	status, hdr, raw, err := g.inner.Do(ctx, method, rawURL, headers, body)
	g.mu.Lock()
	g.in--
	g.mu.Unlock()
	return status, hdr, raw, err
}

func (g *gateStubClient) peak() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxIn
}
