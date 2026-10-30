package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// billingPathFor builds the control-plane billing path for one credential target.
func billingPathFor(orgID string) string {
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return "/api/v1/me/billing"
	}
	return "/api/v1/orgs/" + orgID + "/billing"
}

// buildBillingURL joins the configured base URL with the billing path.
// No refresh query parameter is ever appended: refresh=all on the real
// /api/v1/me/billing endpoint returns HTTP 400, so the cache-refresh setting
// was removed.
func buildBillingURL(baseURL, orgID string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return base + billingPathFor(orgID)
}

// hostHTTPDo performs one non-streaming request through the host transport
// policy and returns the raw upstream body.
func hostHTTPDo(ctx context.Context, hostCallbackID string, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	payload := struct {
		HostCallbackID string      `json:"host_callback_id,omitempty"`
		Method         string      `json:"method,omitempty"`
		URL            string      `json:"url,omitempty"`
		Headers        http.Header `json:"headers,omitempty"`
		Body           []byte      `json:"body,omitempty"`
	}{
		HostCallbackID: hostCallbackID,
		Method:         method,
		URL:            rawURL,
		Headers:        headers,
		Body:           body,
	}
	rawRequest, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return 0, nil, nil, newPluginError(codePluginError, "encode host.http.do request: "+errMarshal.Error(), 0)
	}
	rawResult, errCall := callHost(methodHostHTTPDo, rawRequest)
	if errCall != nil {
		return 0, nil, nil, errCall
	}
	var response struct {
		StatusCode int         `json:"StatusCode"`
		Headers    http.Header `json:"Headers"`
		Body       []byte      `json:"Body"`
	}
	if errUnmarshal := json.Unmarshal(rawResult, &response); errUnmarshal != nil {
		// The host serializes pluginapi.HTTPResponse without JSON tags; accept
		// the snake_case form as well in case a future host adds them.
		var alt struct {
			StatusCode int         `json:"status_code"`
			Headers    http.Header `json:"headers"`
			Body       []byte      `json:"body"`
		}
		if errAlt := json.Unmarshal(rawResult, &alt); errAlt != nil {
			return 0, nil, nil, newPluginError(codeHostCallFailed,
				"decode host.http.do response failed", 0)
		}
		return alt.StatusCode, alt.Headers, alt.Body, nil
	}
	return response.StatusCode, response.Headers, response.Body, nil
}

// billingHTTPClient is the seam used by the billing reader. Production uses
// the host callback; tests substitute an in-process implementation.
type billingHTTPClient interface {
	Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error)
}

// hostBillingClient routes billing reads through the host (CPA) transport.
type hostBillingClient struct {
	hostCallbackID string
}

func (c hostBillingClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	return hostHTTPDo(ctx, c.hostCallbackID, method, rawURL, headers, body)
}

// directBillingClient is used by tests and by the CLI harness. It still applies
// the same allow-list check before dialing.
type directBillingClient struct {
	httpClient   *http.Client
	allowedHosts []string
}

func (c directBillingClient) Do(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	parsed, errValidate := validateUpstreamURL(rawURL, c.allowedHosts)
	if errValidate != nil {
		return 0, nil, nil, errValidate
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeoutSecond * time.Second}
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if errRequest != nil {
		return 0, nil, nil, newPluginError(codePluginError, "build upstream request failed", 0)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, nil, nil, newPluginError(codeUpstreamStatus, "upstream request failed", 0)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	rawBody, errRead := io.ReadAll(io.LimitReader(resp.Body, maxBillingBodyBytes))
	if errRead != nil {
		return resp.StatusCode, resp.Header, nil, newPluginError(codeUpstreamMalformed, "reading upstream body failed", 0)
	}
	return resp.StatusCode, resp.Header, rawBody, nil
}

// maxBillingBodyBytes caps how much of a billing response is buffered.
const maxBillingBodyBytes = 1 << 20

// readBilling fetches and decodes the billing status document for one credential.
func readBilling(ctx context.Context, cfg runtimeConfig, cred credential, client billingHTTPClient) (billingStatus, time.Duration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return billingStatus{}, 0, newPluginError(codeHostUnavailable, "billing transport is not available", 0)
	}
	if strings.TrimSpace(cred.Token) == "" {
		return billingStatus{}, 0, newPluginError(codeMissingCredential, "Entire bearer token is empty", 0)
	}
	registerSecret(cred.Token)

	rawURL := buildBillingURL(cfg.BaseURL, cfg.OrgID)
	if _, errValidate := validateUpstreamURL(rawURL, cfg.AllowedHosts); errValidate != nil {
		return billingStatus{}, 0, errValidate
	}

	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSecond
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	headers := http.Header{
		"Accept":        []string{"application/json"},
		"Authorization": []string{"Bearer " + cred.Token},
		"User-Agent":    []string{"CPA-Entire-Quota/" + pluginVersion},
	}

	started := time.Now()
	status, respHeaders, body, errCall := client.Do(callCtx, http.MethodGet, rawURL, headers, nil)
	elapsed := time.Since(started)
	if errCall != nil {
		return billingStatus{}, elapsed, errCall
	}

	switch {
	case status == http.StatusUnauthorized:
		return billingStatus{}, elapsed, newPluginError(codeUnauthenticated,
			"Entire rejected the bearer token (HTTP 401)", http.StatusUnauthorized)
	case status == http.StatusForbidden:
		return billingStatus{}, elapsed, newPluginError(codeForbidden,
			"Entire denied access to billing for this token (HTTP 403)", http.StatusForbidden)
	case status == http.StatusMisdirectedRequest:
		return billingStatus{}, elapsed, newPluginError(codeUpstreamStatus,
			fmt.Sprintf("Entire reports a misdirected request (HTTP %d): token belongs to another jurisdiction", status), status)
	case !okStatus(status):
		return billingStatus{}, elapsed, newPluginError(codeUpstreamStatus,
			fmt.Sprintf("Entire billing returned HTTP %d", status), status)
	}

	if len(body) == 0 {
		return billingStatus{}, elapsed, newPluginError(codeUpstreamMalformed,
			"Entire billing response body is empty", 0)
	}
	decoded, errDecode := decodeBillingStatus(body)
	if errDecode != nil {
		return billingStatus{}, elapsed, errDecode
	}
	decoded.serverOffsetMs = computeServerOffset(respHeaders, started).Milliseconds()
	return decoded, elapsed, nil
}

// computeServerOffset returns the upstream clock skew implied by the Date header.
func computeServerOffset(headers http.Header, localNow time.Time) time.Duration {
	if headers == nil {
		return 0
	}
	dateHeader := headers.Get("Date")
	if dateHeader == "" {
		return 0
	}
	parsed, errParse := http.ParseTime(dateHeader)
	if errParse != nil {
		return 0
	}
	return parsed.Sub(localNow).Round(time.Millisecond)
}
