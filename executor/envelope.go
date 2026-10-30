package main

import (
	"encoding/json"
	"net/http"
)

// Plugin identity reported at plugin.register.
const (
	pluginName       = "cpa-entire-executor"
	pluginVersion    = "0.1.0"
	pluginAuthor     = "local"
	pluginRepository = ""
	pluginDisplay    = "Entire Agent (fail-closed)"
)

// schemaVersion is the RPC JSON schema this plugin was written against. The
// executor RPC family is unchanged across 1..8; 8 is the current host schema.
const schemaVersion uint32 = 8

// providerIdentifier is the provider key the host registers this executor
// under. quota.identifier in the sibling ../quota module also answers "entire";
// that is a different capability family (quota_provider vs executor) and the
// two do not collide — this executor never implements quota.fetch/quota.reset.
const providerIdentifier = "entire-agent"

// modelPrefix is the model-name namespace this executor claims. Only models
// under this prefix are served; everything else is refused locally so the host
// can fall back to a native provider instead of silently misrouting.
const modelPrefix = "entire-review-"

// Plugin error codes surfaced through the ABI error envelope.
const (
	codeInvalidMethod  = "invalid_method"
	codeInvalidRequest = "invalid_request"
	codeUnknownMethod  = "unknown_method"
	codePluginError    = "plugin_error"

	codeHostUnavailable = "host_unavailable"
	codeHostCallFailed  = "host_callback_failed"

	// Execution-path refusals. Each carries an explicit HTTP status so the host
	// surfaces a real client error instead of a 200 with fabricated content.
	codeExecutionDisabled  = "entire_execution_disabled"
	codeExecutionInsecure  = "entire_execution_insecure_transport"
	codeModelUnsupported   = "entire_model_unsupported"
	codeToolsRejected      = "entire_tools_unsupported"
	codeImageRejected      = "entire_image_unsupported"
	codeStreamRejected     = "entire_stream_unsupported"
	codeContentRejected    = "entire_content_unsupported"
	codePayloadRejected    = "entire_payload_invalid"
	codeExtrasRejected     = "entire_params_unsupported"
	codeCredentialMissing  = "entire_credential_missing"
	codeBillingUnavailable = "entire_billing_limit_unavailable"
	codeNotImplemented     = "entire_real_execution_not_implemented"

	// Auth-path refusals.
	codeAuthParseFailed = "entire_auth_parse_failed"
)

// pluginEnvelope is the RPC envelope written back to the host.
type pluginEnvelope struct {
	OK     bool                 `json:"ok"`
	Result json.RawMessage      `json:"result,omitempty"`
	Error  *pluginEnvelopeError `json:"error,omitempty"`
}

// pluginEnvelopeError is the error arm of pluginEnvelope. Field names follow
// pluginabi.Error exactly: http_status makes the host map the failure onto the
// client response instead of defaulting to HTTP 500.
type pluginEnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
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

// pluginError carries an ABI error code, a redacted message, and an HTTP status.
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

// StatusCode exposes the HTTP status the host maps onto the client response.
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

// okStatus reports whether a status is a successful 2xx response.
func okStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// trimForMessage flattens and caps an untrusted string before it reaches a message.
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
