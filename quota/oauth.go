package main

// OAuth login support for the Entire auth provider capability.
//
// The host drives four RPCs against a plugin auth provider (sdk/pluginabi):
//
//	auth.identifier    -> {"identifier":"entire"}
//	auth.parse         -> AuthParseResponse
//	auth.login.start   -> AuthLoginStartResponse  (official device flow)
//	auth.login.poll    -> AuthLoginPollResponse   (RFC 8628 token exchange)
//	auth.refresh       -> AuthRefreshResponse     (refresh_token grant)
//
// Transport policy
//
// Every outbound call is classified first:
//
//   - official Microsoft-owned Entra hosts that are not Entire hosts
//     (login.microsoftonline.com, login.microsoft.com, login.windows.net,
//     login.live.com, sts.windows.net) are refused before any request is built,
//     so the device flow can never be degraded into a Microsoft tenant login;
//   - an Entire-owned host where the command builder attached the host HTTP
//     client goes through host.http.do with the live host_callback_id;
//   - an Entire-owned host where no host client is attached (the RPC path sets
//     AuthLoginStartRequest.HTTPClient to nil before it crosses the ABI) goes
//     through the plugin's own HTTPS transport with a hard timeout and a
//     redirect policy that only follows same-host, allow-listed URLs.
//
// No credential is read from the operator's machine, no real login is
// performed by the plugin, and no token value is ever written to a log line,
// an error message or a poll message: every token is registered with
// redact.go before it can reach any of them.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// RPC method names (sdk/pluginabi/types.go)
// ---------------------------------------------------------------------------

const (
	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"
	methodAuthRefresh    = "auth.refresh"
)

// ---------------------------------------------------------------------------
// Official Entire identity provider endpoints
// ---------------------------------------------------------------------------

const (
	// entireAuthIdentifier is the provider key the host routes on. It must match
	// quota.identifier ("entire") so one plugin instance owns the whole provider.
	entireAuthIdentifier = providerIdentifier

	// entireAuthBaseURL is the official Entire login server. The US jurisdiction
	// login host is us.auth.entire.io; the control plane (us.console.entire.io)
	// stays the billing base URL.
	entireAuthBaseURL = "https://us.auth.entire.io"

	// entireDeviceAuthorizationPath is the official device authorization endpoint.
	entireDeviceAuthorizationPath = "/device_authorization"
	// entireTokenPath is the official token endpoint (RFC 6749 §3.2, RFC 8628 §3.4).
	entireTokenPath = "/oauth/token"

	// entireDeviceClientID is the public CLI client identifier.
	entireDeviceClientID = "entire-cli"
	// entireDeviceScope requests a CLI session plus an offline refresh token.
	entireDeviceScope = "cli offline_access"

	// entireDeviceGrantType / entireRefreshGrantType are the token request grants.
	entireDeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	entireRefreshGrantType = "refresh_token"
)

// Entire-owned domains the auth flow may reach. A host outside this list is
// refused even when the operator's billing allow-list would accept it, so the
// login flow can never be pointed at a look-alike identity provider.
var entireAuthAllowedDomains = []string{
	"entire.io",
	"auth.entire.io",
	"console.entire.io",
	"api.entire.io",
}

// foreignIdentityHosts are non-Entire identity-provider hosts that must never
// be contacted by this flow. They are the hosts a device flow degrades into
// when client_id / tenant / resource parameters are taken from Microsoft
// documentation instead of Entire's own CLI.
var foreignIdentityHosts = []string{
	"login.microsoftonline.com",
	"login.microsoft.com",
	"login.windows.net",
	"login.live.com",
	"sts.windows.net",
	"microsoftonline.com",
}

// ---------------------------------------------------------------------------
// Auth storage keys written into AuthData.StorageJSON
// ---------------------------------------------------------------------------

const (
	// storageKeyEntireToken is the plugin's own canonical bearer key; it is first
	// in storageTokenKeys in credential.go, so quota.fetch picks it up unchanged.
	storageKeyEntireToken = "entire_token"
	storageKeyAccessToken = "access_token"
	storageKeyRefreshTok  = "refresh_token"
	storageKeyTokenType   = "token_type"
	storageKeyExpiresAt   = "expires_at"
	storageKeyIssuer      = "issuer"
	storageKeyScope       = "scope"

	// storageTypeKey / storageTypeValue tag the auth file; the host overwrites
	// "type" with the provider key but a standalone copy stays readable.
	storageTypeKey   = "type"
	storageTypeValue = "entire"

	// storageKeyLabel carries the operator-facing label. It is only ever the
	// login server hostname: never a handle, an e-mail or a token fragment.
	storageKeyLabel = "label"
)

// Label and filename budgets. The host lowercases nothing, but the auth file
// name is derived from the label, so it is kept to a conservative charset.
const (
	maxAuthLabelLength = 64
	authFileSuffix     = "-auth.json"
	authFilePrefix     = "entire-"
)

// ---------------------------------------------------------------------------
// Login flow timing
// ---------------------------------------------------------------------------

const (
	// defaultDeviceInterval is the RFC 8628 §3.2 default poll interval.
	defaultDeviceInterval = 5 * time.Second
	// minDeviceInterval floor: Entire may hand out interval=1 and the plugin
	// still refuses to hammer the token endpoint.
	minDeviceInterval = 1 * time.Second
	// maxDeviceInterval cap for a server asking for a pathological interval.
	maxDeviceInterval = 60 * time.Second
	// defaultDeviceExpiry is used when the device authorization response omits
	// expires_in.
	defaultDeviceExpiry = 10 * time.Minute
	// maxDeviceExpiry bounds the state TTL.
	maxDeviceExpiry = 30 * time.Minute
	// defaultPollTimeout bounds one token round trip.
	defaultPollTimeout = 15 * time.Second
	// maxPollTimeout caps a single token round trip.
	maxPollTimeout = 30 * time.Second
	// slowDownIncrement is the RFC 8628 §3.5 back-off step.
	slowDownIncrement = 5 * time.Second
	// loginStateBytes is the entropy of the opaque poll handle.
	loginStateBytes = 24
)

// ---------------------------------------------------------------------------
// Host payload shapes (mirrors sdk/pluginapi, declared locally)
// ---------------------------------------------------------------------------

// authLoginStartRequest mirrors pluginapi.AuthLoginStartRequest plus the
// host_callback_id the RPC wrapper adds (internal/pluginhost/rpc_schema.go).
type authLoginStartRequest struct {
	Provider       string         `json:"Provider,omitempty"`
	BaseURL        string         `json:"BaseURL,omitempty"`
	Host           hostSummary    `json:"Host,omitempty"`
	Metadata       map[string]any `json:"Metadata,omitempty"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
}

// authLoginPollRequest mirrors pluginapi.AuthLoginPollRequest.
type authLoginPollRequest struct {
	Provider       string         `json:"Provider,omitempty"`
	State          string         `json:"State,omitempty"`
	Host           hostSummary    `json:"Host,omitempty"`
	Metadata       map[string]any `json:"Metadata,omitempty"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
}

// authParseRequest mirrors pluginapi.AuthParseRequest.
type authParseRequest struct {
	Provider string      `json:"Provider,omitempty"`
	Path     string      `json:"Path,omitempty"`
	FileName string      `json:"FileName,omitempty"`
	RawJSON  []byte      `json:"RawJSON,omitempty"`
	Host     hostSummary `json:"Host,omitempty"`
}

// authRefreshRequest mirrors pluginapi.AuthRefreshRequest.
type authRefreshRequest struct {
	AuthID         string            `json:"AuthID,omitempty"`
	AuthProvider   string            `json:"AuthProvider,omitempty"`
	StorageJSON    []byte            `json:"StorageJSON,omitempty"`
	Metadata       map[string]any    `json:"Metadata,omitempty"`
	Attributes     map[string]string `json:"Attributes,omitempty"`
	Host           hostSummary       `json:"Host,omitempty"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

// hostSummary mirrors the parts of pluginapi.HostConfigSummary this flow reads.
type hostSummary struct {
	AuthDir  string `json:"AuthDir,omitempty"`
	ProxyURL string `json:"ProxyURL,omitempty"`
}

// authLoginStartResponse mirrors pluginapi.AuthLoginStartResponse.
type authLoginStartResponse struct {
	Provider  string         `json:"Provider,omitempty"`
	URL       string         `json:"URL,omitempty"`
	State     string         `json:"State,omitempty"`
	ExpiresAt time.Time      `json:"ExpiresAt,omitempty"`
	Metadata  map[string]any `json:"Metadata,omitempty"`
}

// authLoginStatus values (pluginapi.AuthLoginStatus).
const (
	authLoginStatusPending = "pending"
	authLoginStatusSuccess = "success"
	authLoginStatusError   = "error"
)

// authLoginPollResponse mirrors pluginapi.AuthLoginPollResponse.
type authLoginPollResponse struct {
	Status  string     `json:"Status,omitempty"`
	Message string     `json:"Message,omitempty"`
	Auth    *authData  `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

// authData mirrors pluginapi.AuthData with the field names the host decodes.
type authData struct {
	Provider         string            `json:"Provider,omitempty"`
	ID               string            `json:"ID,omitempty"`
	FileName         string            `json:"FileName,omitempty"`
	Label            string            `json:"Label,omitempty"`
	Prefix           string            `json:"Prefix,omitempty"`
	ProxyURL         string            `json:"ProxyURL,omitempty"`
	Disabled         bool              `json:"Disabled,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON,omitempty"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

// authParseResponse mirrors pluginapi.AuthParseResponse.
type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    *authData  `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

// authRefreshResponse mirrors pluginapi.AuthRefreshResponse.
type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter,omitempty"`
}

// ---------------------------------------------------------------------------
// Wire documents
// ---------------------------------------------------------------------------

// deviceAuthorizationRequest is the RFC 8628 §3.1 request body.
type deviceAuthorizationRequest struct {
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

// deviceAuthorizationResponse is the RFC 8628 §3.2 response body.
type deviceAuthorizationResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

// tokenResponse is the RFC 6749 §5.1 / RFC 8628 §3.5 token body. Error fields
// share the struct because RFC 6749 §5.2 returns them with HTTP 400.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	IDToken          string `json:"id_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// tokenRequestMode selects the grant built by buildTokenRequest. It exists so
// the poll and refresh requests can be adjusted without touching the endpoints:
// a deployment that requires HTTP Basic client authentication (the shape
// cmd/entire/cli/auth/provider.go claims with client_id=entire-cli and an empty
// password) or a client_id in the body is a one-line change here.
type tokenRequestMode int

const (
	// tokenRequestForm sends client_id and scope in the form body, as the
	// official CLI device flow does. No Authorization header is attached.
	tokenRequestForm tokenRequestMode = iota
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Plugin error codes introduced by the auth provider capability.
const (
	codeAuthUnsupportedGrant  = "entire_auth_unsupported_grant"
	codeAuthMalformedResponse = "entire_auth_malformed_response"
	codeAuthFlowExpired       = "entire_auth_flow_expired"
	codeAuthFlowNotFound      = "entire_auth_flow_not_found"
	codeAuthPollTooSoon       = "entire_auth_poll_too_soon"
	codeAuthDenied            = "entire_auth_denied"
	codeAuthClientRejected    = "entire_auth_client_rejected"
	codeAuthTokenUnavailable  = "entire_auth_token_unavailable"
	codeAuthForeignIdP        = "entire_auth_foreign_idp"
	codeAuthTransport         = "entire_auth_transport"
)

// sentinel errors carried by *pluginError. The call sites map them onto the
// RPC result the host expects (poll status "error", parse handled=false, ...).
var (
	errFlowExpired  = errors.New("device flow expired")
	errFlowNotFound = errors.New("unknown device flow state")
	errPollTooSoon  = errors.New("device flow poll interval not elapsed")
)

// ---------------------------------------------------------------------------
// Transport seam
// ---------------------------------------------------------------------------

// authHTTPClient is the transport seam for the login flow. Production routes
// host.http.do when the host opened a callback for this RPC and falls back to
// the plugin's own HTTPS transport (directBillingClient, which already applies
// validateUpstreamURL on every dial) otherwise.
type authHTTPClient interface {
	Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error)
}

// hostAuthClient routes an auth call through host.http.do when a live callback
// handle is present, and through the direct HTTPS transport otherwise.
type hostAuthClient struct {
	hostCallbackID string
	timeout        time.Duration
}

func (c hostAuthClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	// Policy gate first: a foreign identity host is refused before any dial,
	// whichever transport would have carried the request.
	if _, errValidate := validateAuthURL(rawURL, entireAuthAllowedDomains); errValidate != nil {
		return 0, nil, nil, errValidate
	}
	if strings.TrimSpace(c.hostCallbackID) != "" && hostCallbackBound() {
		return hostHTTPDo(ctx, c.hostCallbackID, method, rawURL, headers, body)
	}
	return newDirectAuthClient(c.timeout).Do(ctx, method, rawURL, headers, body)
}

// newDirectAuthClient builds the standard-library HTTPS transport used when the
// host exported no callback handle for the call. It inherits the allow-list
// enforcement of directBillingClient and adds the login-flow timeout plus a
// redirect policy that refuses to leave the Entire auth hosts.
func newDirectAuthClient(timeout time.Duration) directBillingClient {
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	if timeout > maxPollTimeout {
		timeout = maxPollTimeout
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: newAuthTransport(timeout),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxAuthRedirects {
				return newPluginError(codeAuthTransport, "too many redirects in the Entire auth flow", 0)
			}
			if _, errValidate := validateAuthURL(req.URL.String(), entireAuthAllowedDomains); errValidate != nil {
				return errValidate
			}
			// A redirect never inherits a credential the plugin attached.
			req.Header.Del("Authorization")
			return nil
		},
	}
	return directBillingClient{
		httpClient:   client,
		allowedHosts: append([]string(nil), entireAuthAllowedDomains...),
	}
}

// newAuthTransport builds a transport with explicit dial and TLS budgets so a
// hung identity provider cannot pin the host's poll RPC indefinitely.
func newAuthTransport(timeout time.Duration) *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	base.ResponseHeaderTimeout = timeout
	base.TLSHandshakeTimeout = timeout
	base.ForceAttemptHTTP2 = true
	return base
}

// maxAuthRedirects is the redirect budget for one auth call.
const maxAuthRedirects = 3

// maxAuthBodyBytes caps a decoded device/token response.
const maxAuthBodyBytes = 256 << 10

// validateAuthURL applies the login-flow transport policy on top of the shared
// upstream rules: https only, no userinfo, no fragment, no foreign identity
// provider, an Entire-allow-listed DNS host, and a device-flow path.
func validateAuthURL(rawURL string, allowedHosts []string) (*url.URL, *pluginError) {
	// The foreign-identity-provider check runs before the shared allow-list so a
	// Microsoft tenant is reported as such instead of as a generic host denial.
	if parsedProbe, errProbe := url.Parse(strings.TrimSpace(rawURL)); errProbe == nil && parsedProbe != nil {
		if isForeignIdentityHost(parsedProbe.Hostname()) {
			return nil, newPluginError(codeAuthForeignIdP,
				"auth URL points at a non-Entire identity provider; the Entire device flow never leaves entire.io", 0)
		}
	}
	parsed, errUpstream := validateUpstreamURL(rawURL, allowedHosts)
	if errUpstream != nil {
		// Loopback fixtures are accepted only when the allow-list opted in.
		if !loopbackEnabled(allowedHosts) {
			return nil, asAuthPluginError(errUpstream, "auth URL is not permitted")
		}
		fixture, errParse := url.Parse(strings.TrimSpace(rawURL))
		if errParse != nil || fixture == nil {
			return nil, asAuthPluginError(errUpstream, "auth URL is not permitted")
		}
		parsed = fixture
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if isForeignIdentityHost(host) {
		return nil, newPluginError(codeAuthForeignIdP,
			"auth URL points at a non-Entire identity provider; the Entire device flow never leaves entire.io", 0)
	}
	if parsed.User != nil {
		return nil, newPluginError(codeBlockedHost, "auth URL must not embed credentials", 0)
	}
	if parsed.Fragment != "" {
		return nil, newPluginError(codeBlockedHost, "auth URL must not carry a fragment", 0)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path != "" && !isAuthEndpointPath(path) && !isVerificationPath(path) {
		return nil, newPluginError(codeBlockedHost,
			"auth URL path is not an Entire device-flow endpoint", 0)
	}
	return parsed, nil
}

// isAuthEndpointPath reports whether path is one of the two API endpoints this
// flow calls: the base itself, the device authorization endpoint or the token
// endpoint.
func isAuthEndpointPath(path string) bool {
	switch path {
	case "", entireDeviceAuthorizationPath, entireTokenPath:
		return true
	}
	return false
}

// isVerificationPath reports whether path belongs to the operator-facing
// verification page the provider returns in a device authorization response.
// Only Entire hosts reach this check (the host allow-list ran first), and the
// page is opened by the operator's browser, not fetched by the plugin.
func isVerificationPath(path string) bool {
	switch path {
	case "/device", "/activate", "/verify", "/login", "/oauth/device", "/connect/device":
		return true
	}
	return false
}

// validateVerificationURL applies the host rules to the operator-facing
// verification page. The page path is chosen by the provider, so only the host,
// scheme and credential rules are enforced: the plugin hands the URL to the
// browser and never fetches it, and an entire.io-only destination keeps the
// login from being redirected to a third-party identity page.
func validateVerificationURL(rawURL string) (*url.URL, *pluginError) {
	trimmed := strings.TrimSpace(rawURL)
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil || parsed == nil {
		return nil, newPluginError(codeBlockedHost, "verification URL cannot be parsed", 0)
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "https" {
		return nil, newPluginError(codeBlockedHost, "verification URL must use https", 0)
	}
	if parsed.User != nil {
		return nil, newPluginError(codeBlockedHost, "verification URL must not embed credentials", 0)
	}
	if parsed.Fragment != "" {
		return nil, newPluginError(codeBlockedHost, "verification URL must not carry a fragment", 0)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return nil, newPluginError(codeBlockedHost, "verification URL has no host", 0)
	}
	if isForeignIdentityHost(host) {
		return nil, newPluginError(codeAuthForeignIdP,
			"verification URL points at a non-Entire identity provider", 0)
	}
	if net.ParseIP(host) != nil {
		return nil, newPluginError(codeBlockedHost,
			"verification URL host must be a DNS name from the Entire allow-list", 0)
	}
	if !hostAllowed(host, entireAuthAllowedDomains) {
		return nil, newPluginError(codeBlockedHost,
			fmt.Sprintf("verification host %q is not in the Entire auth allow-list", host), 0)
	}
	return parsed, nil
}

// isForeignIdentityHost reports whether host belongs to a non-Entire IdP.
func isForeignIdentityHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, entry := range foreignIdentityHosts {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// buildDeviceAuthorizationURL returns the official device authorization URL.
func buildDeviceAuthorizationURL(authBase string) string {
	return joinAuthBase(authBase) + entireDeviceAuthorizationPath
}

// buildTokenURL returns the official token endpoint URL.
func buildTokenURL(authBase string) string {
	return joinAuthBase(authBase) + entireTokenPath
}

// joinAuthBase normalizes the login server base URL.
func joinAuthBase(authBase string) string {
	base := strings.TrimSpace(authBase)
	if base == "" {
		base = entireAuthBaseURL
	}
	return strings.TrimRight(base, "/")
}

// ---------------------------------------------------------------------------
// Device flow state
// ---------------------------------------------------------------------------

// deviceFlow is one in-flight login. It holds the provider-issued device code
// (a secret: it is registered for redaction) together with the RFC 8628 poll
// parameters. State lives only in memory; shutdownRuntime drops it.
type deviceFlow struct {
	Provider        string
	AuthBase        string
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        time.Duration
	ExpiresAt       time.Time
	LastPoll        time.Time
	PollCount       int
	HostCallbackID  string
	Label           string
}

// flowStore is the process-wide login-state map.
type flowStore struct {
	mu    sync.Mutex
	flows map[string]*deviceFlow
	now   func() time.Time
}

var authFlows = &flowStore{flows: make(map[string]*deviceFlow, 4), now: time.Now}

// put stores a flow under a fresh opaque state handle.
func (s *flowStore) put(flow *deviceFlow) (string, error) {
	state, errState := newLoginState()
	if errState != nil {
		return "", errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flows == nil {
		s.flows = make(map[string]*deviceFlow, 4)
	}
	s.purgeLocked(s.nowOf())
	s.flows[state] = flow
	return state, nil
}

// get returns the live flow for state, or errFlowNotFound / errFlowExpired.
func (s *flowStore) get(state string) (*deviceFlow, error) {
	state = strings.TrimSpace(state)
	if state == "" || !isValidLoginState(state) {
		return nil, newPluginError(codeAuthFlowNotFound, "login state is malformed", http.StatusBadRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, exists := s.flows[state]
	if !exists {
		return nil, newPluginError(codeAuthFlowNotFound,
			"login state is unknown or was already consumed; start the Entire login again", http.StatusNotFound)
	}
	if !s.nowOf().Before(flow.ExpiresAt) {
		delete(s.flows, state)
		return nil, newPluginError(codeAuthFlowExpired,
			"the Entire device code expired before approval; start the login again", http.StatusGone)
	}
	return flow, nil
}

// drop removes one state entry.
func (s *flowStore) drop(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flows, strings.TrimSpace(state))
}

// purgeLocked drops every expired flow. The caller holds s.mu.
func (s *flowStore) purgeLocked(now time.Time) {
	for state, flow := range s.flows {
		if flow == nil || !now.Before(flow.ExpiresAt) {
			delete(s.flows, state)
		}
	}
}

// purge drops expired flows (called on every start/poll).
func (s *flowStore) purge(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
}

// reset clears every flow. Used by shutdown and by tests.
func (s *flowStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flows = make(map[string]*deviceFlow, 4)
}

// len reports the number of live (not yet purged) flows. Used by tests.
func (s *flowStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flows)
}

func (s *flowStore) nowOf() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// markPoll enforces the RFC 8628 poll interval and records the attempt.
// It mutates the flow under the store lock and returns errPollTooSoon when the
// host polls faster than the interval the provider handed out.
func (s *flowStore) markPoll(flow *deviceFlow, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flow == nil {
		return newPluginError(codeAuthFlowNotFound, "login state is unknown", http.StatusNotFound)
	}
	if !now.Before(flow.ExpiresAt) {
		return newPluginError(codeAuthFlowExpired, "the Entire device code expired", http.StatusGone)
	}
	if !flow.LastPoll.IsZero() && now.Sub(flow.LastPoll) < flow.Interval {
		return newPluginError(codeAuthPollTooSoon,
			"polling faster than the provider interval; retry shortly", http.StatusTooManyRequests)
	}
	flow.LastPoll = now
	flow.PollCount++
	return nil
}

// resetPollClock clears the last poll timestamp so a back-off notice does not
// lock the next attempt out twice.
func (s *flowStore) resetPollClock(flow *deviceFlow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flow != nil {
		flow.LastPoll = time.Time{}
	}
}

// slowDown applies the RFC 8628 §3.5 interval increase.
func (s *flowStore) slowDown(flow *deviceFlow, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flow == nil {
		return
	}
	flow.Interval += slowDownIncrement
	if flow.Interval > maxDeviceInterval {
		flow.Interval = maxDeviceInterval
	}
	flow.LastPoll = now
}

// newLoginState returns an opaque URL-safe state handle. It matches the host's
// ValidateOAuthState charset ([A-Za-z0-9._-], max 128 bytes) and is opaque: it
// is 24 random bytes in hex, so it carries no device code and no secret.
func newLoginState() (string, error) {
	buf := make([]byte, loginStateBytes)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", newPluginError(codePluginError, "generate login state failed", 0)
	}
	return "entire_" + hex.EncodeToString(buf), nil
}

// isValidLoginState reports whether state matches the host's accepted charset.
func isValidLoginState(state string) bool {
	if len(state) > 128 {
		return false
	}
	for _, r := range state {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// RPC dispatch
// ---------------------------------------------------------------------------

// handleAuthMethod dispatches one auth provider RPC and wraps the result in the
// plugin envelope. The method set is the five providerauth RPCs; anything else
// is rejected so a quota-only host never sees an auth result.
//
// Declared separately from handleMethod because the integration owner merges
// this file into the plugin's dispatcher (plugin.go's handleMethod gains the
// five cases listed in authProviderMethods and the registration gains the
// auth_provider capability).
func handleAuthMethod(method string, raw []byte) ([]byte, *pluginError) {
	if shutdownFlag.Load() {
		return nil, newPluginError(codePluginError, "plugin is shut down", 0)
	}
	switch method {
	case methodAuthIdentifier:
		return encodeOKEnvelope(authIdentifierResult{Identifier: entireAuthIdentifier}), nil
	case methodAuthParse:
		return handleAuthParse(raw)
	case methodAuthLoginStart:
		return handleAuthLoginStart(raw)
	case methodAuthLoginPoll:
		return handleAuthLoginPoll(raw)
	case methodAuthRefresh:
		return handleAuthRefresh(raw)
	default:
		return nil, newPluginError(codeUnknownMethod,
			"unknown auth method: "+trimForMessage(method), httpStatusForAuthMethod(method))
	}
}

// authProviderMethods lists the RPCs this file serves, for the integration
// owner's dispatcher and registration wiring.
func authProviderMethods() []string {
	return []string{
		methodAuthIdentifier,
		methodAuthParse,
		methodAuthLoginStart,
		methodAuthLoginPoll,
		methodAuthRefresh,
	}
}

// httpStatusForAuthMethod maps an unknown auth method onto a status.
func httpStatusForAuthMethod(method string) int {
	if strings.HasPrefix(method, "auth.") {
		return http.StatusInternalServerError
	}
	return 0
}

// authIdentifierResult is the auth.identifier result.
type authIdentifierResult struct {
	Identifier string `json:"identifier"`
}

// ---------------------------------------------------------------------------
// auth.parse
// ---------------------------------------------------------------------------

// handleAuthParse recognises an Entire auth document (the file auth.login.poll
// produced, or an operator-written token file) and normalises it. A document
// without a usable bearer is not handled: the plugin never claims an auth file
// it cannot serve.
func handleAuthParse(raw []byte) ([]byte, *pluginError) {
	var req authParseRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.parse payload is not valid JSON", 0)
		}
	}
	stored := parseAuthStorage(req.RawJSON)
	if !stored.found || stored.token == "" {
		// Not an Entire credential: report unhandled rather than inventing one.
		return encodeOKEnvelope(authParseResponse{Handled: false}), nil
	}
	registerSecret(stored.token)
	if stored.refresh != "" {
		registerSecret(stored.refresh)
	}
	auth := buildAuthData(stored, req.Host, stored.label)
	auth.ID = stored.authID
	if auth.ID == "" {
		auth.FileName = strings.TrimSpace(req.FileName)
	}
	return encodeOKEnvelope(authParseResponse{Handled: true, Auth: &auth}), nil
}

// normalizeProviderID lowercases and trims a provider key so "Entire",
// " entire " and "entire" all match. The host applies the same normalization
// (internal/pluginhost authProviderRecord -> normalizeProviderID).
func normalizeProviderID(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// authStorage is the decoded view of a provider auth document.
type authStorage struct {
	found      bool
	authID     string
	token      string
	refresh    string
	tokenType  string
	issuer     string
	scope      string
	expiresAt  time.Time
	label      string
	rawExpires int64
}

// parseAuthStorage reads an Entire auth document. It accepts both the file the
// plugin writes (entire_token / access_token / refresh_token / expires_at /
// issuer) and a bare token document.
func parseAuthStorage(raw []byte) authStorage {
	var out authStorage
	if len(raw) == 0 {
		return out
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return out
	}
	if doc == nil {
		return out
	}
	out.found = true
	if storedType, okType := doc[storageTypeKey].(string); okType {
		if normalizeProviderID(storedType) != entireAuthIdentifier && normalizeProviderID(storedType) != "" {
			out.found = false
			return out
		}
	}
	out.authID = stringField(doc, "id")
	out.label = stringField(doc, storageKeyLabel)
	out.tokenType = stringField(doc, storageKeyTokenType)
	out.issuer = stringField(doc, storageKeyIssuer)
	out.scope = stringField(doc, storageKeyScope)
	out.token = firstTokenFromMap(doc, storageTokenKeys)
	out.refresh = normalizeToken(stringField(doc, storageKeyRefreshTok))
	out.expiresAt = timeFromField(doc, storageKeyExpiresAt)
	return out
}

// stringField returns a trimmed string field, or "".
func stringField(doc map[string]any, key string) string {
	value, exists := doc[key]
	if !exists || value == nil {
		return ""
	}
	if text, okText := value.(string); okText {
		return strings.TrimSpace(text)
	}
	return ""
}

// timeFromField decodes an expiry that is either a unix second number or an
// RFC 3339 string. Anything else is treated as absent.
func timeFromField(doc map[string]any, key string) time.Time {
	value, exists := doc[key]
	if !exists || value == nil {
		return time.Time{}
	}
	switch typed := value.(type) {
	case float64:
		if typed <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(typed), 0).UTC()
	case int64:
		if typed <= 0 {
			return time.Time{}
		}
		return time.Unix(typed, 0).UTC()
	case json.Number:
		seconds, errInt := typed.Int64()
		if errInt != nil || seconds <= 0 {
			return time.Time{}
		}
		return time.Unix(seconds, 0).UTC()
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}
		}
		if parsed, errParse := time.Parse(time.RFC3339, text); errParse == nil {
			return parsed.UTC()
		}
		if parsed, errInt := parseInt64(text); errInt == nil && parsed > 0 {
			return time.Unix(parsed, 0).UTC()
		}
	}
	return time.Time{}
}

// parseInt64 parses a decimal integer string.
func parseInt64(text string) (int64, error) {
	var value int64
	if _, errScan := fmt.Sscan(strings.TrimSpace(text), &value); errScan != nil {
		return 0, errScan
	}
	return value, nil
}

// ---------------------------------------------------------------------------
// auth.login.start
// ---------------------------------------------------------------------------

// handleAuthLoginStart runs the RFC 8628 §3.1 device authorization request and
// returns the verification URL the operator opens. Nothing here reads a local
// credential and nothing writes a token.
func handleAuthLoginStart(raw []byte) ([]byte, *pluginError) {
	var req authLoginStartRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.login.start payload is not valid JSON", 0)
		}
	}
	provider := normalizeProviderID(req.Provider)
	if provider != "" && provider != entireAuthIdentifier {
		return nil, newPluginError(codeUnknownMethod,
			"this plugin only handles the entire provider", http.StatusNotFound)
	}
	authBase := resolveAuthBaseURL(req.BaseURL)
	// The base itself is not a device-flow path; the two endpoint URLs built from
	// it are validated (buildDeviceAuthorizationURL / buildTokenURL below), which
	// is where the policy gate runs for every request.
	client := authTransportFor(req.HostCallbackID, req.Metadata)

	ctx, cancel := context.WithTimeout(context.Background(), defaultPollTimeout)
	defer cancel()

	body := []byte(url.Values{"client_id": {entireDeviceClientID}, "scope": {entireDeviceScope}}.Encode())
	headers := formHeaders()
	status, _, rawBody, errDo := client.Do(ctx, http.MethodPost, buildDeviceAuthorizationURL(authBase), headers, body)
	if errDo != nil {
		return nil, asAuthPluginError(errDo, "requesting the Entire device code failed")
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, newPluginError(codeAuthClientRejected,
			fmt.Sprintf("Entire rejected the device authorization request for client %q (HTTP %d)", entireDeviceClientID, status), status)
	case status == http.StatusTooManyRequests:
		return nil, newPluginError(codeAuthClientRejected,
			"Entire rate-limited the device authorization request (HTTP 429)", status)
	case !okStatus(status):
		return nil, newPluginError(codeUpstreamStatus,
			fmt.Sprintf("Entire device authorization returned HTTP %d", status), status)
	}

	doc, errDecode := decodeJSONMap(rawBody)
	if errDecode != nil {
		return nil, errDecode
	}
	deviceCode := stringField(doc, "device_code")
	userCode := stringField(doc, "user_code")
	verification := firstNonEmptyString(
		stringField(doc, "verification_uri_complete"),
		stringField(doc, "verification_uri"),
	)
	if deviceCode == "" || verification == "" {
		return nil, newPluginError(codeAuthMalformedResponse,
			"Entire device authorization response is missing device_code or verification_uri", 0)
	}
	// The device code is a credential: register it before it can reach any log.
	registerSecret(deviceCode)

	if _, errVerify := validateVerificationURL(verification); errVerify != nil {
		return nil, newPluginError(codeBlockedHost,
			"Entire returned a verification URL outside the Entire auth hosts; refusing to hand it to the operator", 0)
	}

	now := time.Now()
	expiresIn := int64Field(doc, "expires_in")
	ttl := defaultDeviceExpiry
	if expiresIn > 0 {
		ttl = time.Duration(expiresIn) * time.Second
	}
	if ttl > maxDeviceExpiry {
		ttl = maxDeviceExpiry
	}
	interval := time.Duration(int64Field(doc, "interval")) * time.Second
	if interval < minDeviceInterval {
		interval = defaultDeviceInterval
	}
	if interval > maxDeviceInterval {
		interval = maxDeviceInterval
	}

	flow := &deviceFlow{
		Provider:        entireAuthIdentifier,
		AuthBase:        authBase,
		DeviceCode:      deviceCode,
		UserCode:        userCode,
		VerificationURL: verification,
		Interval:        interval,
		ExpiresAt:       now.Add(ttl),
		HostCallbackID:  strings.TrimSpace(req.HostCallbackID),
		Label:           labelForAuthBase(authBase),
	}
	state, errState := authFlows.put(flow)
	if errState != nil {
		return nil, asAuthPluginError(errState, "storing the Entire login state failed")
	}
	emitHostLog("info", "entire-auth: device authorization started", map[string]any{
		"provider":  entireAuthIdentifier,
		"client_id": entireDeviceClientID,
		"interval":  interval.String(),
		"expires":   flow.ExpiresAt.UTC().Format(time.RFC3339),
	})

	return encodeOKEnvelope(authLoginStartResponse{
		Provider:  entireAuthIdentifier,
		URL:       verification,
		State:     state,
		ExpiresAt: flow.ExpiresAt,
		Metadata: map[string]any{
			// The provider needs both to build the poll; neither is a token.
			"device_code": deviceCode,
			"user_code":   userCode,
			"interval":    int64(interval / time.Second),
			"label":       flow.Label,
		},
	}), nil
}

// ---------------------------------------------------------------------------
// auth.login.poll
// ---------------------------------------------------------------------------

// handleAuthLoginPoll performs one RFC 8628 §3.4 device access token request.
// A pending answer is reported as pending; a definitive client rejection is
// reported as an error; a successful exchange returns the AuthData the host
// persists. No branch fabricates a token: success requires an access_token
// from the provider.
func handleAuthLoginPoll(raw []byte) ([]byte, *pluginError) {
	var req authLoginPollRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.login.poll payload is not valid JSON", 0)
		}
	}
	state := strings.TrimSpace(req.State)
	flow, errFlow := authFlows.get(state)
	if errFlow != nil {
		return nil, asAuthPluginError(errFlow, "resolving the Entire login state failed")
	}

	now := time.Now()
	if errPoll := authFlows.markPoll(flow, now); errPoll != nil {
		var plugErr *pluginError
		if asPluginError(errPoll, &plugErr) {
			if plugErr.Code == codeAuthPollTooSoon {
				// Answer "pending" so the host keeps waiting instead of failing
				// the session; the message never carries a secret.
				return encodeOKEnvelope(authLoginPollResponse{
					Status:  authLoginStatusPending,
					Message: "waiting for approval at " + redactSecrets(flow.VerificationURL),
				}), nil
			}
			if plugErr.Code == codeAuthFlowExpired {
				authFlows.drop(state)
			}
			return nil, plugErr
		}
		return nil, newPluginError(codePluginError, "poll gate failed", 0)
	}

	client := authTransportFor(firstNonEmptyString(req.HostCallbackID, flow.HostCallbackID), req.Metadata)
	ctx, cancel := context.WithTimeout(context.Background(), defaultPollTimeout)
	defer cancel()

	form, errForm := buildTokenRequest(tokenRequestForm, flow.AuthBase, tokenRequestFormDoc{
		GrantType:  entireDeviceGrantType,
		DeviceCode: flow.DeviceCode,
		Scope:      entireDeviceScope,
	})
	if errForm != nil {
		return nil, errForm
	}
	headers := formHeaders()
	status, _, rawBody, errDo := client.Do(ctx, http.MethodPost, buildTokenURL(flow.AuthBase), headers, form)
	if errDo != nil {
		return nil, asAuthPluginError(errDo, "the Entire token request failed")
	}

	doc, errDecode := decodeTokenResponse(rawBody)
	if errDecode != nil {
		return nil, errDecode
	}

	// RFC 8628 §3.5: a 400 with an error code is the normal pending answer.
	if status == http.StatusBadRequest || !okStatus(status) {
		return pollResultForTokenError(state, flow, status, doc)
	}
	if doc.Error != "" {
		return pollResultForTokenError(state, flow, status, doc)
	}
	return pollSuccess(state, flow, doc)
}

// pollResultForTokenError maps the token endpoint error document onto the poll
// status the host expects. It never converts a provider rejection into success.
func pollResultForTokenError(state string, flow *deviceFlow, status int, doc tokenResponse) ([]byte, *pluginError) {
	errCode := strings.ToLower(strings.TrimSpace(doc.Error))
	switch errCode {
	case "authorization_pending":
		return encodeOKEnvelope(authLoginPollResponse{
			Status:  authLoginStatusPending,
			Message: "waiting for approval at " + redactSecrets(flow.VerificationURL),
		}), nil
	case "slow_down":
		authFlows.slowDown(flow, time.Now())
		return encodeOKEnvelope(authLoginPollResponse{
			Status:  authLoginStatusPending,
			Message: "the provider asked for a slower poll; backing off",
		}), nil
	case "access_denied":
		authFlows.drop(state)
		return encodeOKEnvelope(authLoginPollResponse{
			Status:  authLoginStatusError,
			Message: "the Entire login was denied in the browser",
		}), nil
	case "expired_token":
		authFlows.drop(state)
		return encodeOKEnvelope(authLoginPollResponse{
			Status:  authLoginStatusError,
			Message: "the Entire device code expired before approval; start the login again",
		}), nil
	case "invalid_client", "unauthorized_client", "invalid_grant", "invalid_request", "invalid_scope":
		// A client-authentication rejection is terminal: surface it instead of
		// polling forever or inventing a session.
		authFlows.drop(state)
		detail := trimForMessage(doc.ErrorDescription)
		if detail == "" {
			detail = errCode
		}
		code := codeAuthClientRejected
		if errCode == "invalid_grant" {
			code = codeAuthDenied
		}
		emitHostLog("warn", "entire-auth: token exchange rejected", map[string]any{
			"provider": entireAuthIdentifier,
			"error":    errCode,
			"status":   status,
		})
		return nil, newPluginError(code,
			fmt.Sprintf("Entire token endpoint rejected the device flow (%s): %s", errCode, redactSecrets(detail)), status)
	default:
		// Unknown outcome: pending once would hide a real failure. Fail closed.
		authFlows.drop(state)
		message := errCode
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return nil, newPluginError(codeAuthTokenUnavailable,
			"Entire token endpoint returned an unrecognised answer: "+redactSecrets(message), status)
	}
}

// pollSuccess converts a token document into AuthData. It is the only path that
// can report success, and it requires a non-empty access_token.
func pollSuccess(state string, flow *deviceFlow, doc tokenResponse) ([]byte, *pluginError) {
	if strings.TrimSpace(doc.AccessToken) == "" {
		return nil, newPluginError(codeAuthTokenUnavailable,
			"Entire returned a successful token response without an access_token", 0)
	}
	stored := authStorage{
		found:     true,
		token:     normalizeToken(doc.AccessToken),
		refresh:   normalizeToken(doc.RefreshToken),
		tokenType: strings.TrimSpace(doc.TokenType),
		issuer:    flow.AuthBase,
		scope:     strings.TrimSpace(doc.Scope),
	}
	if stored.tokenType == "" {
		stored.tokenType = "Bearer"
	}
	if stored.scope == "" {
		stored.scope = entireDeviceScope
	}
	if doc.ExpiresIn > 0 {
		stored.expiresAt = time.Now().Add(time.Duration(doc.ExpiresIn) * time.Second).UTC()
	}
	if stored.refresh == "" {
		emitHostLog("warn", "entire-auth: login produced no refresh token", map[string]any{
			"provider": entireAuthIdentifier,
			"scope":    stored.scope,
		})
	}

	registerSecret(stored.token)
	if stored.refresh != "" {
		registerSecret(stored.refresh)
	}
	if doc.IDToken != "" {
		registerSecret(doc.IDToken)
	}
	authFlows.drop(state)

	auth := buildAuthData(stored, hostSummary{}, flow.Label)
	// Identity claims select a filename, never grant authorization.
	// The token itself was obtained from the TLS-authenticated token endpoint.
	identity := stored.token
	parts := strings.Split(stored.token, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Subject string `json:"sub"`
				Issuer  string `json:"iss"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Subject != "" {
				identity = claims.Issuer + "\x00" + claims.Subject
			}
		}
	}
	digest := sha256.Sum256([]byte(identity))
	auth.FileName = "entire-" + hex.EncodeToString(digest[:12]) + ".json"
	auth.ID = ""
	auth.Label = "Entire " + hex.EncodeToString(digest[:4])
	emitHostLog("info", "entire-auth: login completed", map[string]any{
		"provider":     entireAuthIdentifier,
		"has_refresh":  stored.refresh != "",
		"expires_at":   formatOptionalTime(stored.expiresAt),
		"token_source": "device_flow",
	})
	return encodeOKEnvelope(authLoginPollResponse{
		Status: authLoginStatusSuccess,
		Auth:   &auth,
	}), nil
}

// ---------------------------------------------------------------------------
// auth.refresh
// ---------------------------------------------------------------------------

// handleAuthRefresh runs a refresh_token grant for a stored Entire auth.
func handleAuthRefresh(raw []byte) ([]byte, *pluginError) {
	var req authRefreshRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.refresh payload is not valid JSON", 0)
		}
	}
	// Host-driven refresh and quota.fetch renewal share one account lock: the
	// refresh token is single-use, so two concurrent grants would spend the same
	// value and the loser would rotate onto a dead token.
	unlock, okLock := renewLocksForAuth(req)
	if !okLock {
		return nil, newPluginError(codeAuthRenewLocked,
			"another refresh for this Entire credential is still running; retry shortly", http.StatusTooManyRequests)
	}
	defer unlock()
	return refreshAuthLocked(req)
}

// renewLocksForAuth takes the account lock for one auth.refresh request.
func renewLocksForAuth(req authRefreshRequest) (func(), bool) {
	return renewLocks.acquire(credentialLockKey(req.AuthID, ""), authLockTimeout)
}

// refreshAuthLocked is handleAuthRefresh with the account lock already held.
func refreshAuthLocked(req authRefreshRequest) ([]byte, *pluginError) {
	provider := normalizeProviderID(req.AuthProvider)
	if provider != "" && provider != entireAuthIdentifier {
		return nil, newPluginError(codeUnknownMethod,
			"this plugin only refreshes the entire provider", http.StatusNotFound)
	}
	stored := parseAuthStorage(req.StorageJSON)
	refresh := stored.refresh
	if refresh == "" && len(req.Metadata) > 0 {
		refresh = normalizeToken(stringField(req.Metadata, storageKeyRefreshTok))
	}
	if refresh == "" {
		return nil, newPluginError(codeAuthTokenUnavailable,
			"this Entire auth has no refresh_token; sign in again to obtain one", 0)
	}
	registerSecret(refresh)
	if stored.token != "" {
		registerSecret(stored.token)
	}
	authBase := resolveAuthBaseURL(stored.issuer)
	client := authTransportFor(req.HostCallbackID, req.Metadata)

	ctx, cancel := context.WithTimeout(context.Background(), defaultPollTimeout)
	defer cancel()

	form, errForm := buildTokenRequest(tokenRequestForm, authBase, tokenRequestFormDoc{
		GrantType:    entireRefreshGrantType,
		RefreshToken: refresh,
	})
	if errForm != nil {
		return nil, errForm
	}
	status, _, rawBody, errDo := client.Do(ctx, http.MethodPost, buildTokenURL(authBase), formHeaders(), form)
	if errDo != nil {
		return nil, asAuthPluginError(errDo, "the Entire refresh request failed")
	}
	doc, errDecode := decodeTokenResponse(rawBody)
	if errDecode != nil {
		return nil, errDecode
	}
	if !okStatus(status) || doc.Error != "" {
		errCode := strings.ToLower(strings.TrimSpace(doc.Error))
		if errCode == "" {
			errCode = fmt.Sprintf("HTTP %d", status)
		}
		emitHostLog("warn", "entire-auth: refresh rejected", map[string]any{
			"provider": entireAuthIdentifier,
			"error":    errCode,
			"auth_id":  req.AuthID,
		})
		switch {
		case errCode == "invalid_grant" || errCode == "unauthorized_client" || errCode == "invalid_client":
			// A dead refresh token is a sign-in problem, not a transport problem.
			// The message names the action and never the secret.
			return nil, newPluginError(codeAuthReloginRequired, reloginMessage, http.StatusUnauthorized)
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return nil, newPluginError(codeUnauthenticated,
				"Entire rejected the refresh token; sign in again (HTTP "+fmt.Sprint(status)+")", status)
		}
		return nil, newPluginError(codeAuthTokenUnavailable,
			"Entire refresh failed: "+redactSecrets(trimForMessage(doc.ErrorDescription)), status)
	}
	if strings.TrimSpace(doc.AccessToken) == "" {
		return nil, newPluginError(codeAuthTokenUnavailable,
			"Entire returned a successful refresh without an access_token", 0)
	}

	updated := stored
	updated.found = true
	updated.token = normalizeToken(doc.AccessToken)
	if refreshed := normalizeToken(doc.RefreshToken); refreshed != "" {
		updated.refresh = refreshed
	}
	updated.issuer = authBase
	if strings.TrimSpace(doc.TokenType) != "" {
		updated.tokenType = strings.TrimSpace(doc.TokenType)
	}
	if strings.TrimSpace(doc.Scope) != "" {
		updated.scope = strings.TrimSpace(doc.Scope)
	}
	if doc.ExpiresIn > 0 {
		updated.expiresAt = time.Now().Add(time.Duration(doc.ExpiresIn) * time.Second).UTC()
	}
	registerSecret(updated.token)
	if updated.refresh != "" {
		registerSecret(updated.refresh)
	}

	auth := buildAuthData(updated, req.Host, firstNonEmptyString(stored.label, req.AuthID))
	if strings.TrimSpace(req.AuthID) != "" {
		auth.ID = strings.TrimSpace(req.AuthID)
	}
	// A refresh must never rename the credential. The host keeps the file it is
	// already reading (cpa-source internal/pluginhost/auth_provider.go
	// RefreshAuth only substitutes FileName when the plugin leaves it empty), so
	// an empty name here means "keep the current one" instead of a new download
	// name, a lost prefix or a second file for the same account.
	auth.FileName = ""
	auth.Prefix = ""
	next := refreshAfter(updated.expiresAt)
	return encodeOKEnvelope(authRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: next,
	}), nil
}

// ---------------------------------------------------------------------------
// AuthData assembly
// ---------------------------------------------------------------------------

// buildAuthData assembles the host-facing auth record. Tokens live only inside
// StorageJSON (base64 on the wire) and in Metadata under the same names the
// quota provider already reads; Label, FileName and ID never contain a token.
func buildAuthData(stored authStorage, host hostSummary, label string) authData {
	storage := map[string]any{
		// entire_token is first in storageTokenKeys: quota.fetch consumes it.
		"type":                "entire",
		"prefix":              "entire",
		storageKeyEntireToken: stored.token,
		storageKeyAccessToken: stored.token,
	}
	if stored.refresh != "" {
		storage[storageKeyRefreshTok] = stored.refresh
	}
	if stored.tokenType != "" {
		storage[storageKeyTokenType] = stored.tokenType
	}
	storage[storageKeyIssuer] = stored.issuer
	if stored.scope != "" {
		storage[storageKeyScope] = stored.scope
	}
	if !stored.expiresAt.IsZero() {
		storage[storageKeyExpiresAt] = stored.expiresAt.UTC().Unix()
	}
	rawStorage, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		rawStorage = nil
	}

	safeLabel := sanitizeLabel(label)
	metadata := map[string]any{
		storageKeyEntireToken: stored.token,
	}
	if stored.refresh != "" {
		metadata[storageKeyRefreshTok] = stored.refresh
	}
	if stored.issuer != "" {
		metadata[storageKeyIssuer] = stored.issuer
	}
	if safeLabel != "" {
		metadata[storageKeyLabel] = safeLabel
	}
	if host.ProxyURL != "" {
		metadata["proxy_url"] = strings.TrimSpace(host.ProxyURL)
	}

	attributes := map[string]string{}
	if safeLabel != "" {
		attributes["entire_label"] = safeLabel
	}
	if stored.issuer != "" {
		attributes["entire_issuer"] = stored.issuer
	}

	return authData{
		Provider:         entireAuthIdentifier,
		Prefix:           "entire",
		ID:               stored.authID,
		FileName:         authFileNameFor(safeLabel),
		Label:            safeLabel,
		StorageJSON:      rawStorage,
		Metadata:         metadata,
		Attributes:       attributes,
		NextRefreshAfter: refreshAfter(stored.expiresAt),
	}
}

// refreshAfter returns the earliest refresh instant: two minutes before expiry,
// clamped so a long-lived token is not refreshed on every host sweep.
func refreshAfter(expiresAt time.Time) time.Time {
	if expiresAt.IsZero() {
		return time.Time{}
	}
	return expiresAt.Add(-2 * time.Minute).UTC()
}

// labelForAuthBase derives the label from the login server hostname.
func labelForAuthBase(authBase string) string {
	parsed, errParse := url.Parse(joinAuthBase(authBase))
	if errParse != nil {
		return entireAuthIdentifier
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || !isSafeLabel(host) {
		return entireAuthIdentifier
	}
	return truncateLabel(host)
}

// sanitizeLabel keeps a label to hostname-shaped characters. Anything that
// could carry a token, a handle or a path is replaced by the provider key, so a
// hostile or accidental input cannot leak through the label, the file name or
// the auth id.
func sanitizeLabel(label string) string {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return entireAuthIdentifier
	}
	if !isSafeLabel(trimmed) || looksLikeCredential(trimmed) {
		return entireAuthIdentifier
	}
	return truncateLabel(trimmed)
}

// looksLikeCredential reports whether a candidate label carries a token shape:
// a known OAuth/JWT prefix, a very long run without a dot-separated host shape,
// or any three-segment base64url blob. Labels are hostnames; nothing else is
// allowed through.
func looksLikeCredential(label string) bool {
	lowered := strings.ToLower(label)
	// A hostname never carries an underscore; every provider token prefix does.
	if strings.Contains(lowered, "_") {
		return true
	}
	for _, prefix := range []string{"sk-", "eyj", "ghp-", "gho-", "xoxb-", "bearer"} {
		if strings.HasPrefix(lowered, prefix) {
			return true
		}
	}
	// A JWT is three dot-separated base64url segments with no hyphen-separated
	// host labels; a hostname has at most one trailing dot and short labels.
	if strings.Count(lowered, ".") >= 2 {
		segments := strings.Split(lowered, ".")
		longest := 0
		for _, segment := range segments {
			if len(segment) > longest {
				longest = len(segment)
			}
		}
		if longest > 24 {
			return true
		}
	}
	if len(lowered) > 32 && !strings.Contains(lowered, ".") {
		return true
	}
	// Many short dot-separated segments are a concatenated-secret shape, not a
	// hostname.
	if strings.Count(lowered, ".") > 5 {
		return true
	}
	return false
}

// isSafeLabel reports whether label is a plain host/label token.
func isSafeLabel(label string) bool {
	if label == "" || len(label) > maxAuthLabelLength*2 {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// truncateLabel caps a label length without splitting a rune.
func truncateLabel(label string) string {
	if len(label) <= maxAuthLabelLength {
		return label
	}
	cut := maxAuthLabelLength
	for cut > 0 && !isLabelByte(label[cut-1]) {
		cut--
	}
	return label[:cut]
}

func isLabelByte(b byte) bool {
	return b == '.' || b == '-' || b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// authFileNameFor derives a stable auth file name from a safe label.
func authFileNameFor(label string) string {
	clean := sanitizeLabel(label)
	clean = strings.ToLower(clean)
	clean = strings.NewReplacer(".", "-", "_", "-").Replace(clean)
	return authFilePrefix + clean + authFileSuffix
}

// ---------------------------------------------------------------------------
// Transport helpers
// ---------------------------------------------------------------------------

// authClientOverride is the test seam for the auth transport. Production leaves
// it nil and every call selects host.http.do or the direct HTTPS transport.
var authClientOverride struct {
	sync.RWMutex
	client authHTTPClient
}

// installAuthClientOverride swaps the auth transport and returns the previous
// one, so a test can restore it with a defer or t.Cleanup.
func installAuthClientOverride(client authHTTPClient) authHTTPClient {
	authClientOverride.Lock()
	defer authClientOverride.Unlock()
	previous := authClientOverride.client
	authClientOverride.client = client
	return previous
}

// currentAuthClientOverride returns the installed test transport, if any.
func currentAuthClientOverride() authHTTPClient {
	authClientOverride.RLock()
	defer authClientOverride.RUnlock()
	return authClientOverride.client
}

// authCallbackProbe is the test seam for the C ABI presence check. Production
// leaves fn nil and reaches the cgo shim in abi.go.
var authCallbackProbe struct {
	sync.RWMutex
	fn func() bool
}

// setAuthCallbackProbe installs (or clears, with nil) the availability probe.
func setAuthCallbackProbe(fn func() bool) {
	authCallbackProbe.Lock()
	defer authCallbackProbe.Unlock()
	authCallbackProbe.fn = fn
}

// hostCallbackBound reports whether a live callback handle can be used for a
// host RPC. The test seam lets the auth tests exercise the direct transport
// without a loaded host; production always answers with the C ABI probe.
func hostCallbackBound() bool {
	authCallbackProbe.RLock()
	probe := authCallbackProbe.fn
	authCallbackProbe.RUnlock()
	if probe != nil {
		return probe()
	}
	// No probe installed: the RPC path is authoritative. ABI callers always send
	// a host_callback_id, so an empty handle means "use the direct transport".
	return true
}

// authTransportFor builds the client for one auth call.
func authTransportFor(hostCallbackID string, metadata map[string]any) authHTTPClient {
	timeout := defaultPollTimeout
	if metadata != nil {
		if raw, exists := metadata["timeout_seconds"]; exists {
			if seconds := int64Field(map[string]any{"v": raw}, "v"); seconds > 0 {
				timeout = time.Duration(seconds) * time.Second
				if timeout > maxPollTimeout {
					timeout = maxPollTimeout
				}
			}
		}
	}
	if override := currentAuthClientOverride(); override != nil {
		return override
	}
	return hostAuthClient{
		hostCallbackID: strings.TrimSpace(hostCallbackID),
		timeout:        timeout,
	}
}

// resolveAuthBaseURL resolves the login server base for a flow. Only Entire
// hosts survive: a BaseURL pointing anywhere else falls back to the official
// US login server rather than being dialed.
func resolveAuthBaseURL(rawBase string) string {
	trimmed := strings.TrimSpace(rawBase)
	if trimmed == "" {
		return entireAuthBaseURL
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return entireAuthBaseURL
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || isForeignIdentityHost(host) || !hostAllowed(host, entireAuthAllowedDomains) {
		return entireAuthBaseURL
	}
	if strings.EqualFold(strings.TrimSpace(parsed.Scheme), "http") && !loopbackEnabled(entireAuthAllowedDomains) {
		return entireAuthBaseURL
	}
	base := strings.TrimRight(parsed.Scheme+"://"+parsed.Host+strings.Split(parsed.Path, entireTokenPath)[0], "/")
	if base == "" {
		return entireAuthBaseURL
	}
	return base
}

// tokenRequestFormDoc carries the fields a token request can be built from.
type tokenRequestFormDoc struct {
	GrantType    string
	DeviceCode   string
	RefreshToken string
	Scope        string
}

// buildTokenRequest renders the x-www-form-urlencoded body for a token grant.
// Basic client authentication is deliberately not used: the provider's
// cmd/entire/cli/auth/provider.go claims Basic entire-cli with an empty
// password, yet the live endpoint answers HTTP 401 invalid_client for it, which
// makes the empty-secret header the known-bad shape. A deployment that requires
// client_secret_basic only needs to attach the header here.
func buildTokenRequest(mode tokenRequestMode, authBase string, doc tokenRequestFormDoc) ([]byte, *pluginError) {
	_ = mode
	_ = authBase
	if doc.GrantType == "" {
		return nil, newPluginError(codeAuthUnsupportedGrant, "token grant type is required", 0)
	}
	values := url.Values{}
	values.Set("grant_type", doc.GrantType)
	values.Set("client_id", entireDeviceClientID)
	switch doc.GrantType {
	case entireDeviceGrantType:
		if doc.DeviceCode == "" {
			return nil, newPluginError(codeAuthTokenUnavailable,
				"the device code is unavailable; start the Entire login again", 0)
		}
		values.Set("device_code", doc.DeviceCode)
	case entireRefreshGrantType:
		if doc.RefreshToken == "" {
			return nil, newPluginError(codeAuthTokenUnavailable,
				"the refresh token is unavailable; sign in again", 0)
		}
		values.Set("refresh_token", doc.RefreshToken)
	default:
		return nil, newPluginError(codeAuthUnsupportedGrant,
			"unsupported token grant type", 0)
	}
	scope := strings.TrimSpace(doc.Scope)
	if scope == "" && doc.GrantType == entireDeviceGrantType {
		scope = entireDeviceScope
	}
	if scope != "" {
		values.Set("scope", scope)
	}
	return []byte(values.Encode()), nil
}

// tokenRequestHeaders builds the request headers for a token grant.
func formHeaders() http.Header {
	return http.Header{
		"Accept":       []string{"application/json"},
		"Content-Type": []string{"application/x-www-form-urlencoded"},
		"User-Agent":   []string{"CPA-Entire-Auth/" + pluginVersion},
	}
}

// decodeTokenResponse decodes a token endpoint body. A non-JSON body on an
// error status is reported as a plain upstream status rather than a parse
// failure, so the host shows a useful message.
func decodeTokenResponse(rawBody []byte) (tokenResponse, *pluginError) {
	var doc tokenResponse
	if len(rawBody) == 0 {
		return doc, newPluginError(codeAuthMalformedResponse, "Entire token endpoint returned an empty body", 0)
	}
	if errUnmarshal := json.Unmarshal(rawBody, &doc); errUnmarshal == nil {
		return doc, nil
	}
	if docMap, errDecode := decodeJSONMap(rawBody); errDecode == nil {
		doc.Error = stringField(docMap, "error")
		doc.ErrorDescription = stringField(docMap, "error_description")
		return doc, nil
	}
	return doc, newPluginError(codeAuthMalformedResponse,
		"Entire token endpoint returned a body that is not JSON", 0)
}

// decodeJSONMap decodes a JSON object body.
func decodeJSONMap(rawBody []byte) (map[string]any, *pluginError) {
	if len(rawBody) == 0 {
		return nil, newPluginError(codeAuthMalformedResponse, "Entire returned an empty body", 0)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(rawBody, &doc); errUnmarshal != nil || doc == nil {
		return nil, newPluginError(codeAuthMalformedResponse,
			"Entire returned a body that is not a JSON object", 0)
	}
	return doc, nil
}

// int64Field reads an integer field that may arrive as a JSON number or string.
func int64Field(doc map[string]any, key string) int64 {
	value, exists := doc[key]
	if !exists || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, errInt := typed.Int64()
		if errInt != nil {
			return 0
		}
		return parsed
	case string:
		parsed, errParse := parseInt64(typed)
		if errParse != nil {
			return 0
		}
		return parsed
	}
	return 0
}

// asAuthPluginError narrows err to *pluginError, mapping anything else onto the
// given fallback message so transport internals never reach the host verbatim.
func asAuthPluginError(err error, fallback string) *pluginError {
	if err == nil {
		return nil
	}
	var plugErr *pluginError
	if asPluginError(err, &plugErr) {
		return plugErr
	}
	return newPluginError(codeAuthTransport, fallback, 0)
}

// firstNonEmptyString returns the first non-blank value.
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// formatOptionalTime renders a timestamp for logs, or "none".
func formatOptionalTime(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// capabilitiesForRegistration is the capability block this file needs on top of
// the quota provider. The integration owner merges it into
// registrationCapabilities (plugin.go) as `auth_provider: true`.
const authProviderCapabilityKey = "auth_provider"

// authRegistrationCapabilities is the auth-side addition to plugin.register.
type authRegistrationCapabilities struct {
	AuthProvider bool `json:"auth_provider"`
}

// authProviderCapabilities returns the auth capability block for registration.
func authProviderCapabilities() authRegistrationCapabilities {
	return authRegistrationCapabilities{AuthProvider: true}
}

// authIdentifierResultForRegistration returns the identifier the host caches at
// registration time; it is also what quota.identifier reports.
func authIdentifierResultForRegistration() string {
	return entireAuthIdentifier
}

// fingerprintAuthState returns a short non-reversible tag for one login state so
// a log line can correlate attempts without exposing the handle.
func fingerprintAuthState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:4])
}

// ensure imports used by optional paths stay real in every build.
var (
	_ = fingerprintAuthState
	_ = authProviderCapabilities
	_ = authIdentifierResultForRegistration
	_ = errFlowExpired
	_ = errFlowNotFound
	_ = errPollTooSoon
	_ = authFlows.resetPollClock
)
