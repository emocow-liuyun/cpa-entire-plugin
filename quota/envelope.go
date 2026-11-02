package main

import (
	"encoding/json"
	"net/http"
)

// Plugin metadata constants.
const (
	pluginName        = "cpa-entire-quota"
	pluginVersion     = "0.1.0"
	pluginAuthor      = "local"
	pluginRepository  = "https://github.com/emocow-liuyun/cpa-entire-plugin"
	pluginDisplayName = "Entire Credits"
)

// schemaVersion is declared to the host at plugin.register. The quota RPC
// contract is stable from schema version 1; version 8 is the current schema.
const schemaVersion uint32 = 8

// Provider identifier handled by this quota provider.
const providerIdentifier = "entire"

// Plugin error codes surfaced through the ABI error envelope.
const (
	codeInvalidMethod  = "invalid_method"
	codeInvalidRequest = "invalid_request"
	codeUnknownMethod  = "unknown_method"
	codePluginError    = "plugin_error"

	codeHostUnavailable = "host_unavailable"
	codeHostCallFailed  = "host_callback_failed"

	codeMissingCredential = "entire_credential_missing"
	codeInvalidToken      = "entire_credential_invalid"
	codeBlockedHost       = "entire_host_not_allowed"
	codeUnauthenticated   = "entire_unauthenticated"
	codeForbidden         = "entire_forbidden"
	codeUpstreamStatus    = "entire_upstream_status"
	codeUpstreamMalformed = "entire_upstream_malformed"
	codeBillingUnreadable = "entire_billing_unreadable"
	codeUnknownQuota      = "entire_quota_unknown"
	codeResetUnsupported  = "entire_reset_unsupported"
)

// pluginEnvelope is the RPC envelope written back to the host.
type pluginEnvelope struct {
	OK     bool                 `json:"ok"`
	Result json.RawMessage      `json:"result,omitempty"`
	Error  *pluginEnvelopeError `json:"error,omitempty"`
}

// pluginEnvelopeError is the error arm of pluginEnvelope.
type pluginEnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// hostEnvelope is the RPC envelope returned by host callbacks.
type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// pluginError carries an ABI error code, a redacted message, and an optional HTTP status.
type pluginError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *pluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// StatusCode exposes the HTTP status the host should map onto the management response.
func (e *pluginError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// newPluginError builds a pluginError with a mandatory redaction pass.
func newPluginError(code, message string, httpStatus int) *pluginError {
	return &pluginError{
		Code:       code,
		Message:    redactSecrets(message),
		HTTPStatus: httpStatus,
	}
}

// encodeOKEnvelope marshals a successful RPC envelope around result.
func encodeOKEnvelope(result any) []byte {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return encodeErrorEnvelope(newPluginError(codePluginError, "encode result: "+errMarshal.Error(), 0))
	}
	out, errWrap := json.Marshal(pluginEnvelope{OK: true, Result: raw})
	if errWrap != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode envelope"}}`)
	}
	return out
}

// encodeErrorEnvelope marshals a failed RPC envelope.
func encodeErrorEnvelope(err *pluginError) []byte {
	if err == nil {
		err = newPluginError(codePluginError, "unspecified plugin error", 0)
	}
	raw, errMarshal := json.Marshal(pluginEnvelope{
		OK: false,
		Error: &pluginEnvelopeError{
			Code:       err.Code,
			Message:    redactSecrets(err.Message),
			HTTPStatus: err.HTTPStatus,
		},
	})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error envelope"}}`)
	}
	return raw
}

// okStatus reports whether an upstream status is a successful 2xx response.
func okStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// trimForMessage trims surrounding whitespace and caps the length of an
// untrusted string before it is placed into a message.
func trimForMessage(value string) string {
	const limit = 300
	trimmed := ""
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		trimmed += string(r)
		if len(trimmed) >= limit {
			break
		}
	}
	start, end := 0, len(trimmed)
	for start < end && (trimmed[start] == ' ' || trimmed[start] == '\t') {
		start++
	}
	for end > start && (trimmed[end-1] == ' ' || trimmed[end-1] == '\t') {
		end--
	}
	return trimmed[start:end]
}
