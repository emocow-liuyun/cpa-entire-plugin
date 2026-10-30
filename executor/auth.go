package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authParseRequest mirrors pluginapi.AuthParseRequest field names.
type authParseRequest struct {
	Provider string         `json:"Provider"`
	Path     string         `json:"Path"`
	FileName string         `json:"FileName"`
	RawJSON  []byte         `json:"RawJSON"`
	Host     map[string]any `json:"Host"`
}

// authData mirrors pluginapi.AuthData field names.
type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	Prefix           string            `json:"Prefix"`
	ProxyURL         string            `json:"ProxyURL"`
	Disabled         bool              `json:"Disabled"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata"`
	Attributes       map[string]string `json:"Attributes"`
	NextRefreshAfter string            `json:"NextRefreshAfter"`
}

// authParseResponse mirrors pluginapi.AuthParseResponse field names.
type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

// authRefreshResponse mirrors pluginapi.AuthRefreshResponse field names.
type authRefreshResponse struct {
	Auth             authData `json:"Auth"`
	NextRefreshAfter string   `json:"NextRefreshAfter,omitempty"`
}

// handleAuthParse serves auth.parse for the Entire provider.
//
// This is the auth contract, not an execution path: it recognizes an Entire
// bearer token stored in an auth file, normalizes it into StorageJSON, and
// refuses anything it does not own. It never contacts the network, so it cannot
// consume credit and cannot confirm the token is live — a wrong token is only
// discovered at dispatch time, which is exactly what the fail-closed gate
// prevents.
func handleAuthParse(rawRequest []byte) ([]byte, *pluginError) {
	var req authParseRequest
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.parse payload is not valid JSON", http.StatusBadRequest)
		}
	}

	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider != "" && provider != providerIdentifier && provider != "entire" {
		// Not ours: let another provider claim the material.
		return encodeOKEnvelope(authParseResponse{Handled: false}), nil
	}
	if len(req.RawJSON) == 0 {
		return nil, newPluginError(codeAuthParseFailed, "auth.parse received no auth material", http.StatusBadRequest)
	}

	stored, token, owned := parseAuthMaterial(req.RawJSON)
	if !owned {
		// Unrecognized material is not an error; report it unhandled so the host
		// can try the next parser.
		return encodeOKEnvelope(authParseResponse{Handled: false}), nil
	}
	registerSecret(token)

	storage, errMarshal := json.Marshal(stored)
	if errMarshal != nil {
		return nil, newPluginError(codeAuthParseFailed, "auth.parse could not encode storage JSON", http.StatusInternalServerError)
	}

	label := strings.TrimSpace(req.FileName)
	if label == "" {
		label = "Entire"
	}
	// Attributes carried back to the host. The path/source attributes are filled
	// in by the host from the parse request; provider and boundary markers are set
	// here so an operator can see which gate state an auth was parsed under.
	data := authData{
		Provider:    providerIdentifier,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: storage,
		Metadata: map[string]any{
			"entire_auth_parsed_at": time.Now().UTC().Format(time.RFC3339),
		},
		Attributes: map[string]string{
			"entire_origin":   "file",
			"entire_provider": providerIdentifier,
		},
	}
	return encodeOKEnvelope(authParseResponse{Handled: true, Auth: data}), nil
}

// handleAuthRefresh serves auth.refresh.
//
// Refresh is deliberately a no-op copy: the Entire bearer is a long-lived
// operator-issued token, this plugin holds no refresh secret, and there is no
// documented refresh endpoint. Returning the auth unchanged with no
// NextRefreshAfter stops the host from polling a refresh that cannot exist,
// and never invents a rotated credential.
func handleAuthRefresh(rawRequest []byte) ([]byte, *pluginError) {
	var req struct {
		AuthID       string            `json:"AuthID"`
		AuthProvider string            `json:"AuthProvider"`
		StorageJSON  []byte            `json:"StorageJSON"`
		Metadata     map[string]any    `json:"Metadata"`
		Attributes   map[string]string `json:"Attributes"`
	}
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &req); errUnmarshal != nil {
			return nil, newPluginError(codeInvalidRequest, "auth.refresh payload is not valid JSON", http.StatusBadRequest)
		}
	}
	attributes := make(map[string]string, len(req.Attributes)+1)
	for key, value := range req.Attributes {
		attributes[key] = value
	}
	attributes["entire_refresh"] = "unsupported"
	return encodeOKEnvelope(authRefreshResponse{
		Auth: authData{
			Provider:    firstNonEmptyString(req.AuthProvider, providerIdentifier),
			ID:          req.AuthID,
			StorageJSON: req.StorageJSON,
			Metadata:    req.Metadata,
			Attributes:  attributes,
		},
	}), nil
}

// firstNonEmptyString returns the first non-empty trimmed value.
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// describeRefusal renders a refusal for the host log.
func describeRefusal(err *pluginError) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s (http %d): %s", err.Code, err.HTTPStatus, err.Message)
}
