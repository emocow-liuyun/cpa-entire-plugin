package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

const inferenceUnavailable = "Entire currently supports OAuth and quota queries in this plugin, but its inference executor is not connected. This is not an API key error. No upstream model request was sent."

// Stop explicit Entire routes before credential selection or fallback execution.
func interceptEntire(raw []byte) ([]byte, *pluginError) {
	var req struct {
		Model, RequestedModel string
		Headers               http.Header
		Body                  []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newPluginError(codeInvalidRequest, "invalid interceptor request", 400)
	}
	isEntire := func(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "entire/") }
	if !isEntire(req.Model) && !isEntire(req.RequestedModel) {
		return encodeOKEnvelope(map[string]any{"Headers": req.Headers, "Body": req.Body}), nil
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "entire_inference_unavailable", "message": inferenceUnavailable}})
	return encodeOKEnvelope(map[string]any{"Terminate": true, "StatusCode": 501, "ResponseHeaders": http.Header{"Content-Type": {"application/json"}}, "ResponseBody": body}), nil
}
