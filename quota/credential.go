package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

// credentialSource records where the usable bearer token came from so the
// management API and logs can explain a resolution result without the value.
type credentialSource string

const (
	sourceStorageJSON credentialSource = "auth_storage_token"
	sourceMetadata    credentialSource = "auth_metadata_token"
)

// credential is a resolved bearer token plus its provenance.
type credential struct {
	Token  string
	Source credentialSource
}

// quotaFetchContext is the subset of the host request this plugin depends on.
// It mirrors pluginapi.QuotaFetchRequest without importing the host module.
type quotaFetchContext struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json"`
	Metadata    map[string]any    `json:"metadata"`
	Attributes  map[string]string `json:"attributes"`
}

// metadataTokenKeys lists the auth-metadata keys consulted, in priority order.
// Only explicitly user-supplied bearer tokens are accepted here; OAuth access
// tokens and cookie sessions are deliberately ignored.
var metadataTokenKeys = []string{
	"entire_token",
	"token",
	"bearer_token",
}

// storageTokenKeys lists the auth storage JSON keys consulted, in priority order.
var storageTokenKeys = []string{
	"entire_token",
	"access_token",
	"token",
	"bearer_token",
}

var bearerPrefixPattern = regexp.MustCompile(`(?i)^bearer\s+`)

// resolveCredential picks the bearer token for one credential request.
// Precedence: the per-credential auth storage JSON, then the per-credential
// auth metadata. There is deliberately no plugin-global token and no
// token-env fallback: one CPA install may hold several Entire credentials and
// a global token would silently override each credential's own token. It
// returns an error when nothing usable is present; the caller fails closed.
func resolveCredential(ctx context.Context, cfg runtimeConfig, req quotaFetchContext) (credential, error) {
	if errCtx := ctx.Err(); errCtx != nil {
		return credential{}, newPluginError(codeMissingCredential, "credential resolution cancelled: "+errCtx.Error(), 0)
	}

	if len(req.StorageJSON) > 0 {
		var storage map[string]any
		if errUnmarshal := json.Unmarshal(req.StorageJSON, &storage); errUnmarshal == nil {
			if token := firstTokenFromMap(storage, storageTokenKeys); token != "" {
				registerSecret(token)
				return credential{Token: token, Source: sourceStorageJSON}, nil
			}
		}
	}

	if len(req.Metadata) > 0 {
		if token := firstTokenFromMap(req.Metadata, metadataTokenKeys); token != "" {
			registerSecret(token)
			return credential{Token: token, Source: sourceMetadata}, nil
		}
	}

	return credential{}, newPluginError(codeMissingCredential,
		"no Entire bearer token found for this credential: set entire_token / access_token / token / bearer_token in the credential auth storage or metadata", 0)
}

// firstTokenFromMap returns the first non-empty normalized token for keys.
func firstTokenFromMap(source map[string]any, keys []string) string {
	for _, key := range keys {
		raw, exists := source[key]
		if !exists || raw == nil {
			continue
		}
		value, okValue := raw.(string)
		if !okValue {
			continue
		}
		if token := normalizeToken(value); token != "" {
			return token
		}
	}
	return ""
}

// normalizeToken strips a "Bearer " prefix and surrounding whitespace, and
// rejects obviously invalid values (control characters, spaces inside the token).
func normalizeToken(raw string) string {
	value := strings.TrimSpace(raw)
	value = bearerPrefixPattern.ReplaceAllString(value, "")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

// decodeJWTExpiry returns the exp claim of a JWT-shaped token, or 0 when the
// token is not a JWT or carries no numeric exp. It never parses claims other
// than exp and never logs them.
func decodeJWTExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		if payload, errDecode = base64.URLEncoding.DecodeString(parts[1]); errDecode != nil {
			return 0
		}
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return 0
	}
	return claims.ExpiresAt
}
