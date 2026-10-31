package main

// Automatic auth renewal for quota.fetch.
//
// The host calls quota.fetch with the credential's auth storage and metadata.
// When the bearer inside that storage is expired (own expires_at, or the JWT
// exp claim) or the control plane answers HTTP 401, this file renews it:
//
//	1. take the per-credential account lock (serializes auth.refresh and
//	   quota.fetch so a rotating, single-use refresh token is never spent twice);
//	2. re-read the credential from the host auth file through host.auth.get, so
//	   the refresh token rotated by a concurrent host refresh is the one used;
//	3. run the refresh_token grant through handleAuthRefresh;
//	4. persist the result atomically through host.auth.save, keeping the
//	   original file name, every original field, prefix, disabled and priority;
//	5. retry the billing read at most once with the fresh access token.
//
// Nothing here writes a token to a log line, an error message or a result: every
// value goes through the redaction registry before it can be formatted.
//
// Host surface (cpa-source sdk/pluginabi + internal/pluginhost/auth_callbacks.go):
//
//	host.auth.get   {"auth_index":"..."} ->
//	                {"auth_index","name","path","json"}   raw auth file bytes
//	host.auth.save  {"name":"...json","json":{...}}      -> {"name","path"}
//	host.auth.list  {}                                   -> {"files":[...]}

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Host callback method names (sdk/pluginabi/types.go)
// ---------------------------------------------------------------------------

const (
	methodHostAuthGet  = "host.auth.get"
	methodHostAuthSave = "host.auth.save"
)

// ---------------------------------------------------------------------------
// Error codes introduced by the renewal path
// ---------------------------------------------------------------------------

const (
	// codeAuthReloginRequired is the terminal state: the refresh token the
	// provider holds is no longer usable and only an operator sign-in fixes it.
	codeAuthReloginRequired = "entire_auth_relogin_required"
	// codeAuthRenewTransport marks a network/transport failure during renewal.
	// It is deliberately not retried: a flaky network must not burn a rotating
	// refresh token.
	codeAuthRenewTransport = "entire_auth_renew_transport"
	// codeAuthRenewHostFailure marks a host.auth.get / host.auth.save failure.
	codeAuthRenewHostFailure = "entire_auth_renew_host_failure"
	// codeAuthRenewLocked marks a renewal that could not take the account lock.
	codeAuthRenewLocked = "entire_auth_renew_locked"
	// codeAuthRenewNoGrant marks a renewal attempt on a credential that carries
	// no refresh token, so renewal is structurally impossible.
	codeAuthRenewNoGrant = "entire_auth_renew_no_grant"
)

// renewLockTimeout bounds how long one fetch waits for the account lock before
// giving up instead of queueing behind a hung refresh.
const renewLockTimeout = 20 * time.Second

// authLockTimeout bounds a host-driven auth.refresh waiting for the lock.
const authLockTimeout = 25 * time.Second

// retryBudget is the number of extra billing reads allowed after a renewal.
// The requirement is "at most one retry"; a second 401 is terminal.
const retryBudget = 1

// ---------------------------------------------------------------------------
// Account lock
// ---------------------------------------------------------------------------

// accountLockStrip serializes every renewal path that can spend a refresh
// token for one credential. The key is the credential identity (auth id, else
// auth index); an empty key falls back to a single shared lock, which is
// conservative but never permits two concurrent refreshes.
type accountLockStrip struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var renewLocks = &accountLockStrip{locks: make(map[string]*sync.Mutex, 8)}

// lockFor returns the mutex for one credential key.
func (s *accountLockStrip) lockFor(key string) *sync.Mutex {
	if s == nil {
		return &sync.Mutex{}
	}
	key = strings.TrimSpace(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex, 8)
	}
	if existing, ok := s.locks[key]; ok && existing != nil {
		return existing
	}
	created := &sync.Mutex{}
	s.locks[key] = created
	return created
}

// acquire takes the lock for key, giving up after timeout.
func (s *accountLockStrip) acquire(key string, timeout time.Duration) (func(), bool) {
	lock := s.lockFor(key)
	if lock.TryLock() {
		return lock.Unlock, true
	}
	if timeout <= 0 {
		return nil, false
	}
	deadline := time.Now().Add(timeout)
	for {
		if lock.TryLock() {
			return lock.Unlock, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// reset clears the strip. Used by tests.
func (s *accountLockStrip) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locks = make(map[string]*sync.Mutex, 8)
}

// credentialLockKey derives the lock key for one fetch/refresh request.
func credentialLockKey(authID, authIndex string) string {
	if id := strings.TrimSpace(authID); id != "" {
		return "id:" + id
	}
	if index := strings.TrimSpace(authIndex); index != "" {
		return "index:" + index
	}
	return "shared"
}

// ---------------------------------------------------------------------------
// Host auth document seam
// ---------------------------------------------------------------------------

// hostAuthDocument is the decoded host.auth.get result.
type hostAuthDocument struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthSaveRequest is the host.auth.save payload. Only these two fields are
// read by the host (pluginapi.HostAuthSaveRequest): the file name and the raw
// credential JSON. Everything else in the file is carried inside JSON.
type hostAuthSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

// hostAuthSaveResult is the host.auth.save result, kept for logging only.
type hostAuthSaveResult struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// authDocClient is the seam that reads and writes the host auth file. The
// production implementation is hostAuthFiles; tests install a fake through
// installAuthDocOverride and never touch a real file.
type authDocClient interface {
	// Get returns the on-disk credential document for an auth index.
	Get(ctx context.Context, authIndex string) (hostAuthDocument, error)
	// Save writes the credential document back under name.
	Save(ctx context.Context, name string, doc map[string]any) (hostAuthSaveResult, error)
}

// authDocOverride is the test seam for the host auth file callbacks.
var authDocOverride struct {
	sync.RWMutex
	client authDocClient
}

// installAuthDocOverride swaps the host auth document transport.
func installAuthDocOverride(client authDocClient) authDocClient {
	authDocOverride.Lock()
	defer authDocOverride.Unlock()
	previous := authDocOverride.client
	authDocOverride.client = client
	return previous
}

// currentAuthDocOverride returns the installed fake, if any.
func currentAuthDocOverride() authDocClient {
	authDocOverride.RLock()
	defer authDocOverride.RUnlock()
	return authDocOverride.client
}

// authDocTransport returns the client used for host auth file IO.
func authDocTransport() authDocClient {
	if override := currentAuthDocOverride(); override != nil {
		return override
	}
	return hostAuthFiles{}
}

// hostAuthFiles is the production host.auth.get / host.auth.save transport.
type hostAuthFiles struct{}

// Get reads one credential document through host.auth.get.
func (hostAuthFiles) Get(ctx context.Context, authIndex string) (hostAuthDocument, error) {
	var out hostAuthDocument
	index := strings.TrimSpace(authIndex)
	if index == "" {
		return out, newPluginError(codeAuthRenewHostFailure,
			"cannot re-read the credential: the host passed no auth index", 0)
	}
	payload, errMarshal := json.Marshal(map[string]string{"auth_index": index})
	if errMarshal != nil {
		return out, newPluginError(codeAuthRenewHostFailure, "encode host.auth.get request failed", 0)
	}
	rawResult, errCall := callHostCtx(ctx, methodHostAuthGet, payload)
	if errCall != nil {
		return out, asRenewHostError(errCall, "re-reading the credential from the host failed")
	}
	if errUnmarshal := json.Unmarshal(rawResult, &out); errUnmarshal != nil {
		return out, newPluginError(codeAuthRenewHostFailure,
			"host.auth.get returned a document this plugin cannot decode", 0)
	}
	if len(strings.TrimSpace(string(out.JSON))) == 0 {
		return out, newPluginError(codeAuthRenewHostFailure,
			"host.auth.get returned no credential document", 0)
	}
	return out, nil
}

// Save writes one credential document through host.auth.save.
func (hostAuthFiles) Save(ctx context.Context, name string, doc map[string]any) (hostAuthSaveResult, error) {
	var out hostAuthSaveResult
	fileName := strings.TrimSpace(name)
	if fileName == "" {
		fileName = authFileNameFor(entireAuthIdentifier)
	}
	rawDoc, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		return out, newPluginError(codeAuthRenewHostFailure, "encode the rotated credential failed", 0)
	}
	payload, errPayload := json.Marshal(hostAuthSaveRequest{Name: fileName, JSON: rawDoc})
	if errPayload != nil {
		return out, newPluginError(codeAuthRenewHostFailure, "encode host.auth.save request failed", 0)
	}
	rawResult, errCall := callHostCtx(ctx, methodHostAuthSave, payload)
	if errCall != nil {
		return out, asRenewHostError(errCall, "saving the rotated credential through the host failed")
	}
	if len(rawResult) > 0 {
		_ = json.Unmarshal(rawResult, &out)
	}
	return out, nil
}

// asRenewHostError narrows a host callback failure onto the renewal code space.
func asRenewHostError(err error, fallback string) *pluginError {
	if err == nil {
		return nil
	}
	var plugErr *pluginError
	if asPluginError(err, &plugErr) {
		return newPluginError(codeAuthRenewHostFailure, fallback+": "+plugErr.Message, 0)
	}
	return newPluginError(codeAuthRenewHostFailure, fallback, 0)
}

// ---------------------------------------------------------------------------
// Expiry inspection
// ---------------------------------------------------------------------------

// expirySource marks which document field produced the expiry.
type expirySource string

const (
	expirySourceJWT  expirySource = "jwt_exp"
	expirySourceMeta expirySource = "expires_at"
	expirySourceNone expirySource = ""
)

// expired reports whether at is in the past relative to now.
func (s expirySource) expired(at time.Time) bool {
	return !at.IsZero() && !time.Now().Before(at)
}

// authExpiry is the decoded view of what a credential document says about its
// own lifetime, plus where that statement came from.
type authExpiry struct {
	expiresAt  time.Time
	hasExpiry  bool
	source     expirySource
	hasRefresh bool
}

// defect is non-empty when the document is expired but cannot self-heal,
// which is the class of failure that must surface a sign-in instruction.
func (e authExpiry) defect() string {
	if e.hasExpiry && !e.source.expired(e.expiresAt) {
		return ""
	}
	if e.hasRefresh {
		return ""
	}
	return "the credential has no refresh_token"
}

// inspectAuthExpiry reads the expiry of one credential document.
//
// Both statements of lifetime are inspected and the earlier one wins:
//
//   - the JWT exp claim of the access token is what the control plane actually
//     validates;
//   - expires_at is the locally cached copy the host itself uses for its
//     auto-refresh schedule (cpa-source sdk/cliproxy/auth/types.go: expireKeys
//     = expired, expire, expires_at, expiresAt, expiry, expires).
//
// They disagree in two ways that both end in a silent HTTP 401: a cached
// expires_at that is still in the future hides an already dead JWT, and a JWT
// exp that is later than the cached value means the cache is stale. Taking the
// earliest value is the only reading that cannot wrongly claim "still valid".
func inspectAuthExpiry(doc map[string]any) authExpiry {
	out := authExpiry{}
	if doc == nil {
		return out
	}
	if jwtExp, okJWT := firstJWTExpiryFromDocument(doc); okJWT {
		out.expiresAt = jwtExp
		out.hasExpiry = true
		out.source = expirySourceJWT
	}
	if declared := timeFromField(doc, storageKeyExpiresAt); !declared.IsZero() {
		if !out.hasExpiry || declared.Before(out.expiresAt) {
			out.expiresAt = declared
			out.hasExpiry = true
			out.source = expirySourceMeta
		}
	}
	out.hasRefresh = normalizeToken(stringField(doc, storageKeyRefreshTok)) != ""
	return out
}

// firstJWTExpiryFromDocument returns the exp claim of the first JWT-shaped
// token in the document, using the same key precedence quota.fetch uses.
func firstJWTExpiryFromDocument(doc map[string]any) (time.Time, bool) {
	for _, key := range storageTokenKeys {
		token := normalizeToken(stringField(doc, key))
		if token == "" {
			continue
		}
		if exp, okExp := jwtExpiryFromToken(token); okExp {
			return exp, true
		}
	}
	return time.Time{}, false
}

// jwtExpiryFromToken exposes the JWT exp of a single token as a time.
func jwtExpiryFromToken(token string) (time.Time, bool) {
	if seconds := decodeJWTExpiry(token); seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// Credential document merge
// ---------------------------------------------------------------------------

// preservedDocFields documents the operator-controlled keys mergeRotatedCredential
// copies through untouched. They are never rewritten, only carried.
var preservedDocFields = []string{
	"prefix",
	"disabled",
	"priority",
	"weight",
	"proxy_url",
	"base_url",
	"headers",
	"email",
	"note",
	"excluded_models",
	"request_retry",
	storageKeyLabel,
}

// mergeRotatedCredential returns a copy of the on-disk document with the
// rotated token pair applied, every other field preserved verbatim.
//
// Atomicity is the host's job: host.auth.save writes to a temp file and renames
// (cpa-source internal/pluginhost/auth_provider.go atomicWriteFile), so the same
// file name is reused deliberately. Renaming the file would orphan the host's
// auth manager record, its ID, its priority and the operator's prefix.
func mergeRotatedCredential(doc map[string]any, updated authStorage) map[string]any {
	out := make(map[string]any, len(doc)+8)
	for key, value := range doc {
		out[key] = value
	}
	access := normalizeToken(updated.token)
	out[storageKeyEntireToken] = access
	out[storageKeyAccessToken] = access
	if refresh := normalizeToken(updated.refresh); refresh != "" {
		out[storageKeyRefreshTok] = refresh
	}
	if updated.tokenType != "" {
		out[storageKeyTokenType] = updated.tokenType
	}
	if updated.issuer != "" {
		out[storageKeyIssuer] = updated.issuer
	}
	if updated.scope != "" {
		out[storageKeyScope] = updated.scope
	}
	if !updated.expiresAt.IsZero() {
		// The host schedules its own auto-refresh from this field, so it is kept
		// in unix seconds and kept truthful.
		out[storageKeyExpiresAt] = updated.expiresAt.UTC().Unix()
	}
	return out
}

// rotatedPairEqual reports whether the on-disk document already carries the
// token pair a re-read produced, so a needless write can be skipped.
func rotatedPairEqual(doc map[string]any, updated authStorage) bool {
	if doc == nil {
		return false
	}
	current := normalizeToken(firstTokenFromMap(doc, storageTokenKeys))
	if current != normalizeToken(updated.token) {
		return false
	}
	diskRefresh := normalizeToken(stringField(doc, storageKeyRefreshTok))
	newRefresh := normalizeToken(updated.refresh)
	return newRefresh == "" || diskRefresh == newRefresh
}

// ---------------------------------------------------------------------------
// Renewal
// ---------------------------------------------------------------------------

// renewOutcome is what the fetch path needs after a renewal attempt.
type renewOutcome struct {
	// credential is the token to retry with, empty when no retry is possible.
	credential credential
	// err is set when the renewal failed and the fetch must report it.
	err *pluginError
	// saved reports whether a rotated pair reached the host auth file.
	saved bool
	// name is the auth file name the host now holds.
	name string
}

// renewCredential re-reads the credential under the account lock, refreshes it,
// and persists the result. It never retries on its own: the caller owns the
// single retry budget.
func renewCredential(ctx context.Context, cfg runtimeConfig, req quotaFetchRequest, hostCallbackID string) renewOutcome {
	key := credentialLockKey(req.AuthID, req.AuthIndex)
	unlock, okLock := renewLocks.acquire(key, renewLockTimeout)
	if !okLock {
		return renewOutcome{err: newPluginError(codeAuthRenewLocked,
			"another renewal for this credential is still running; retry the quota read shortly", 0)}
	}
	defer unlock()

	if cfg.renewalDisabled() {
		return renewOutcome{err: newPluginError(codeAuthReloginRequired,
			"automatic credential renewal is disabled by configuration; sign in again when the token expires", 0)}
	}

	// Step 1: re-read the credential from the host auth file. The fetch payload
	// can be stale by the time the lock is taken, and a rotating refresh token
	// means a stale copy is a spent copy. Without host.auth.get the payload is
	// still usable, but then it is never persisted: the host owns that file.
	doc, name, live, errRead := readCredentialDocument(ctx, req)
	if errRead != nil {
		// Nothing to renew with means the caller's own reason is the accurate
		// one: a credential without a refresh token is not a host failure.
		if !hasAnyRefreshToken(documentFromRequest(req)) {
			return renewOutcome{err: newPluginError(codeAuthRenewNoGrant,
				"this Entire credential has no refresh_token to renew with", 0)}
		}
		return renewOutcome{err: errRead}
	}

	if !live || name == "" {
		return renewOutcome{err: newPluginError(codeAuthRenewHostFailure, "a persistent credential is required for token renewal", 0)}
	}
	fresh := credential{Token: firstTokenFromMap(doc, storageTokenKeys), Source: sourceStorageJSON}
	previous, _ := resolveCredential(ctx, cfg, req.quotaFetchContext)
	if fresh.Token != "" && fresh.Token != previous.Token && decodeJWTExpiry(fresh.Token) > time.Now().Unix()+30 {
		return renewOutcome{credential: fresh, saved: true, name: name}
	}
	// Step 2: refresh under the same lock, using the fresh document.
	// Re-encode under the underscore key names the grant actually reads. The host
	// normalizes camelCase to snake_case on save, but a payload that only ever
	// carried "refreshToken" must still work here.
	storageRaw, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		return renewOutcome{err: newPluginError(codeAuthRenewHostFailure,
			"the re-read credential document could not be encoded", 0)}
	}
	refreshReq := authRefreshRequest{
		AuthID:         strings.TrimSpace(req.AuthID),
		AuthProvider:   entireAuthIdentifier,
		StorageJSON:    storageRaw,
		Metadata:       req.Metadata,
		Attributes:     req.Attributes,
		Host:           hostSummary{},
		HostCallbackID: strings.TrimSpace(hostCallbackID),
	}
	updated, errRefresh := refreshUnderLock(ctx, refreshReq)
	if errRefresh != nil {
		return renewOutcome{err: renewalError(errRefresh)}
	}

	// Step 3: persist the rotated pair atomically, preserving every field.
	// A payload-only document has no verified file target, so it is not written.
	var saved bool
	var errSave *pluginError
	if live {
		saved, errSave = persistRotated(ctx, doc, name, updated)
	} else {
		name = ""
	}
	if errSave != nil {
		// The in-memory token is still usable for this one read; the failure is
		// reported on the quota result so the operator can see the staleness.
		return renewOutcome{
			credential: credential{Token: normalizeToken(updated.token), Source: sourceStorageJSON},
			err:        errSave,
			saved:      false,
			name:       name,
		}
	}
	return renewOutcome{
		credential: credential{Token: normalizeToken(updated.token), Source: sourceStorageJSON},
		saved:      saved,
		name:       name,
	}
}

// readCredentialDocument returns the credential document, the file name to save
// back to, and whether the document came from the host auth file (the only
// case in which writing it back is safe). A host without host.auth.get degrades
// to the fetch payload with live=false.
func readCredentialDocument(ctx context.Context, req quotaFetchRequest) (map[string]any, string, bool, *pluginError) {
	name := strings.TrimSpace(req.AuthFileName)
	if name == "" && len(req.StorageJSON) > 0 {
		// The payload document may name its own file.
		if payloadDoc, errPayload := decodeCredentialDocument(req.StorageJSON); errPayload == nil {
			if declared := strings.TrimSpace(stringField(payloadDoc, "file_name")); declared != "" {
				name = declared
			}
		}
	}
	// host.auth.get is authoritative whenever the host named a credential. The
	// refresh token is single-use: renewing from the payload while the file has
	// already moved on would spend a value the provider no longer accepts, so a
	// failed read here aborts the renewal instead of falling back.
	client := authDocTransport()
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex != "" {
		if client == nil {
			return nil, name, false, newPluginError(codeAuthRenewHostFailure,
				"the host auth file transport is not available; the credential cannot be re-read", 0)
		}
		doc, errGet := client.Get(ctx, authIndex)
		if errGet != nil {
			var plugErr *pluginError
			if asPluginError(errGet, &plugErr) {
				return nil, name, false, plugErr
			}
			return nil, name, false, newPluginError(codeAuthRenewHostFailure,
				"re-reading the credential from the host failed", 0)
		}
		parsed, errParse := decodeCredentialDocument(doc.JSON)
		if errParse != nil {
			return nil, name, false, errParse
		}
		if declared := strings.TrimSpace(doc.Name); declared != "" {
			name = declared
		}
		return parsed, name, true, nil
	}
	// No credential index (a CLI harness or a pure payload read): renew from the
	// payload, but never write back because the host owns that file.
	payload, errPayload := decodeCredentialDocument(req.StorageJSON)
	if errPayload != nil {
		return nil, name, false, errPayload
	}
	return payload, name, false, nil
}

// decodeCredentialDocument decodes a raw auth document.
func decodeCredentialDocument(raw []byte) (map[string]any, *pluginError) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, newPluginError(codeAuthRenewHostFailure,
			"the credential document is empty; sign in again", 0)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil || doc == nil {
		return nil, newPluginError(codeAuthRenewHostFailure,
			"the credential document is not a JSON object", 0)
	}
	return doc, nil
}

// refreshUnderLock runs the refresh_token grant for one document. The caller
// already holds the account lock, so it calls refreshAuthLocked directly:
// handleAuthRefresh would try to take the same (non-reentrant) lock and stall
// until authLockTimeout.
func refreshUnderLock(ctx context.Context, req authRefreshRequest) (authStorage, *pluginError) {
	raw, errHandle := refreshAuthLocked(req)
	if errHandle != nil {
		return authStorage{}, errHandle
	}
	var envelope struct {
		OK     bool                `json:"ok"`
		Result authRefreshResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil || !envelope.OK {
		return authStorage{}, newPluginError(codeAuthRenewHostFailure,
			"the refresh result could not be decoded", 0)
	}
	if normalizeToken(envelope.Result.Auth.StorageJSONBase64Token()) == "" {
		return authStorage{}, newPluginError(codeAuthRenewHostFailure,
			"the refresh returned no access token", 0)
	}
	_ = ctx
	return storageFromAuthData(envelope.Result.Auth), nil
}

// StorageJSONBase64Token is a guard helper: it returns the token carried by an
// AuthData with the same key precedence quota.fetch uses.
func (a authData) StorageJSONBase64Token() string {
	if len(a.StorageJSON) == 0 {
		return ""
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(a.StorageJSON, &doc); errUnmarshal != nil {
		return ""
	}
	return normalizeToken(firstTokenFromMap(doc, storageTokenKeys))
}

// storageFromAuthData rebuilds the refreshed authStorage from AuthData.
func storageFromAuthData(a authData) authStorage {
	out := authStorage{found: true}
	if len(a.StorageJSON) == 0 {
		return out
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(a.StorageJSON, &doc); errUnmarshal != nil {
		return out
	}
	out.token = normalizeToken(firstTokenFromMap(doc, storageTokenKeys))
	out.refresh = normalizeToken(stringField(doc, storageKeyRefreshTok))
	out.tokenType = stringField(doc, storageKeyTokenType)
	out.issuer = stringField(doc, storageKeyIssuer)
	out.scope = stringField(doc, storageKeyScope)
	out.expiresAt = timeFromField(doc, storageKeyExpiresAt)
	return out
}

// renewalError turns a refresh failure into the operator-facing error. A dead
// refresh token yields the sign-in instruction; a transport failure is
// reported as transport and is never retried by this plugin.
func renewalError(errRefresh *pluginError) *pluginError {
	if errRefresh == nil {
		return nil
	}
	switch errRefresh.Code {
	case codeUnauthenticated, codeAuthDenied, codeAuthClientRejected, codeAuthTokenUnavailable:
		return newPluginError(codeAuthReloginRequired, reloginMessage, http.StatusUnauthorized)
	case codeAuthTransport, codeUpstreamStatus, codeHostUnavailable, codeHostCallFailed:
		return newPluginError(codeAuthRenewTransport,
			"renewing the Entire credential failed on the network; the stored tokens were left untouched", 0)
	}
	if isInvalidGrantRefresh(errRefresh) {
		return newPluginError(codeAuthReloginRequired, reloginMessage, http.StatusUnauthorized)
	}
	return errRefresh
}

// codeAuthUnauthenticatedNoRefresh is the 401 for a credential that has no
// refresh token at all. It stays on the historical code so an operator alerting
// rule keyed on it keeps firing; the message carries the sign-in instruction.
const codeAuthUnauthenticatedNoRefresh = codeUnauthenticated

// reloginMessage is the single operator-facing instruction for a dead refresh
// token. It names the action, never the secret.
const reloginMessage = "the Entire refresh token is no longer accepted; sign in again from the CPA auth page " +
	"(Entire OAuth card) to issue a new access and refresh token"

// noRefreshMessage is the instruction when a 401 arrives on a credential that
// holds no refresh token, so there is nothing to renew.
const noRefreshMessage = "the Entire access token was rejected and this credential holds no refresh_token; " +
	"sign in again from the CPA auth page (Entire OAuth card)"

// isInvalidGrantRefresh reports whether an error is an OAuth invalid_grant.
func isInvalidGrantRefresh(err *pluginError) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(err.Message)
	return strings.Contains(lowered, "invalid_grant") || strings.Contains(lowered, "sign in again")
}

// persistRotated writes the rotated pair back through host.auth.save.
func persistRotated(ctx context.Context, doc map[string]any, name string, updated authStorage) (bool, *pluginError) {
	if rotatedPairEqual(doc, updated) {
		return false, nil
	}
	client := authDocTransport()
	if client == nil {
		return false, newPluginError(codeAuthRenewHostFailure,
			"the host auth file transport is not available; the rotated token was not persisted", 0)
	}
	merged := mergeRotatedCredential(doc, updated)
	if _, errSave := client.Save(ctx, name, merged); errSave != nil {
		var plugErr *pluginError
		if asPluginError(errSave, &plugErr) {
			return false, plugErr
		}
		return false, newPluginError(codeAuthRenewHostFailure,
			"saving the rotated credential failed", 0)
	}
	emitHostLog("info", "entire-quota: credential renewed", map[string]any{
		"provider":    entireAuthIdentifier,
		"auth_id":     strings.TrimSpace(name),
		"has_refresh": normalizeToken(updated.refresh) != "",
		"expires_at":  formatOptionalTime(updated.expiresAt),
	})
	return true, nil
}

// renewalDisabled reports whether the operator turned automatic renewal off.
func (c runtimeConfig) renewalDisabled() bool {
	return c.AutoRenew != nil && !*c.AutoRenew
}

// ---------------------------------------------------------------------------
// Fetch integration
// ---------------------------------------------------------------------------

// fetchWithRenewal resolves the credential, reads billing, and renews the
// credential once when the read says it is stale. The host contract is that a
// quota read either returns a value or a reason; a stale token is never
// reported as an empty balance.
func fetchWithRenewal(ctx context.Context, cfg runtimeConfig, req quotaFetchRequest, hostCallbackID string) (billingStatus, credential, *pluginError) {
	cred, errCredential := resolveCredential(ctx, cfg, req.quotaFetchContext)
	if errCredential != nil {
		// A payload with no usable bearer may still carry a refresh token:
		// renewing is the only way to serve it instead of reporting it missing.
		outcome := renewCredential(ctx, cfg, req, hostCallbackID)
		if outcome.credential.Token == "" {
			// Renewal could not produce a token. A structurally impossible
			// renewal (no refresh token) leaves the original reason intact;
			// anything else is the actionable failure.
			if outcome.err != nil && outcome.err.Code != codeAuthRenewNoGrant {
				return billingStatus{}, credential{}, outcome.err
			}
			var plugErr *pluginError
			if asPluginError(errCredential, &plugErr) {
				return billingStatus{}, credential{}, plugErr
			}
			return billingStatus{}, credential{}, newPluginError(codeMissingCredential, errCredential.Error(), 0)
		}
		cred = outcome.credential
	}

	status, _, errRead := readBilling(ctx, cfg, cred, billingTransport(hostCallbackID))
	if errRead == nil {
		return status, cred, nil
	}
	var plugErr *pluginError
	asPluginError(errRead, &plugErr)

	localDoc := documentFromRequest(req)
	expiry := inspectAuthExpiry(localDoc)
	unauthorized := plugErr != nil && plugErr.Code == codeUnauthenticated
	locallyExpired := expiry.hasExpiry && expiry.source.expired(expiry.expiresAt)
	if !unauthorized && !locallyExpired {
		// Not a staleness signal: report the original failure unchanged so the
		// host sees the real reason (403, unreadable billing, transport, ...).
		return billingStatus{}, cred, plugErr
	}
	if unauthorized && !hasAnyRefreshToken(localDoc) {
		// Nothing to refresh with. When the token did not even come from the auth
		// storage (metadata/config/env holder) the original 401 is the accurate
		// answer; when it did, the credential needs a sign-in to gain a refresh
		// token.
		if cred.Source != sourceStorageJSON {
			return billingStatus{}, cred, plugErr
		}
		return billingStatus{}, cred, newPluginError(codeAuthUnauthenticatedNoRefresh, noRefreshMessage, http.StatusUnauthorized)
	}

	for attempt := 0; attempt < retryBudget; attempt++ {
		outcome := renewCredential(ctx, cfg, req, hostCallbackID)
		if outcome.credential.Token == "" {
			if outcome.err != nil {
				return billingStatus{}, cred, outcome.err
			}
			return billingStatus{}, cred, plugErr
		}
		status, _, errRetry := readBilling(ctx, cfg, outcome.credential, billingTransport(hostCallbackID))
		if errRetry == nil {
			// The read succeeded with the fresh token. A persistence failure is
			// still reported: the host file is stale, and the next sweep would
			// spend an already-rotated token.
			if outcome.err != nil {
				return billingStatus{}, outcome.credential, outcome.err
			}
			return status, outcome.credential, nil
		}
		var retryErr *pluginError
		asPluginError(errRetry, &retryErr)
		if retryErr != nil && retryErr.Code == codeUnauthenticated {
			// A second 401 against a freshly rotated token is terminal: the
			// refresh-token chain itself is dead.
			return billingStatus{}, outcome.credential, newPluginError(codeAuthReloginRequired, reloginMessage, http.StatusUnauthorized)
		}
		if outcome.err != nil {
			return billingStatus{}, outcome.credential, outcome.err
		}
		if retryErr != nil {
			return billingStatus{}, outcome.credential, retryErr
		}
		return billingStatus{}, outcome.credential, plugErr
	}
	return billingStatus{}, cred, plugErr
}

// documentFromRequest decodes the credential document carried by a fetch.
func documentFromRequest(req quotaFetchRequest) map[string]any {
	doc, errDecode := decodeCredentialDocument(req.StorageJSON)
	if errDecode != nil {
		return nil
	}
	return doc
}

// hasAnyRefreshToken reports whether a document names a refresh token at all,
// under either the canonical key or the camelCase alias the host normalizes.
func hasAnyRefreshToken(doc map[string]any) bool {
	if doc == nil {
		return false
	}
	for _, key := range []string{storageKeyRefreshTok, "refreshToken"} {
		if normalizeToken(stringField(doc, key)) != "" {
			return true
		}
	}
	return false
}
