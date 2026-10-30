package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

// credentialSource records where a usable bearer token came from.
type credentialSource string

const (
	sourcePluginConfig credentialSource = "plugin_config_token"
	sourcePluginEnv    credentialSource = "plugin_config_token_env"
	sourceAuthStorage  credentialSource = "auth_storage_token"
	sourceAuthMetadata credentialSource = "auth_metadata_token"
)

// credential is a resolved bearer token plus its provenance.
type credential struct {
	Token  string
	Source credentialSource
}

// storageTokenKeys lists the auth storage JSON keys consulted, in priority order.
var storageTokenKeys = []string{
	"entire_token",
	"access_token",
	"token",
	"bearer_token",
}

// metadataTokenKeys lists the auth-metadata keys consulted, in priority order.
// Only explicitly supplied bearer tokens are accepted; OAuth-incompatible
// cookie sessions are ignored.
var metadataTokenKeys = []string{
	"entire_token",
	"token",
	"bearer_token",
}

var bearerPrefixPattern = regexp.MustCompile(`(?i)^bearer\s+`)

// resolveCredential picks the bearer token for one call. Precedence: plugin
// config token, plugin config env var, auth storage JSON, auth metadata. An
// empty result is a refusal, never an anonymous call.
func resolveCredential(cfg runtimeConfig, storageJSON []byte, metadata map[string]any) (credential, error) {
	if token := normalizeToken(cfg.Token); token != "" {
		registerSecret(token)
		return credential{Token: token, Source: sourcePluginConfig}, nil
	}
	if envName := strings.TrimSpace(cfg.TokenEnv); envName != "" {
		if token := normalizeToken(os.Getenv(envName)); token != "" {
			registerSecret(token)
			return credential{Token: token, Source: sourcePluginEnv}, nil
		}
	}
	if len(storageJSON) > 0 {
		var storage map[string]any
		if errUnmarshal := json.Unmarshal(storageJSON, &storage); errUnmarshal == nil {
			if token := firstTokenFromMap(storage, storageTokenKeys); token != "" {
				registerSecret(token)
				return credential{Token: token, Source: sourceAuthStorage}, nil
			}
		}
	}
	if len(metadata) > 0 {
		if token := firstTokenFromMap(metadata, metadataTokenKeys); token != "" {
			registerSecret(token)
			return credential{Token: token, Source: sourceAuthMetadata}, nil
		}
	}
	return credential{}, newPluginError(codeCredentialMissing,
		"no Entire bearer token available: set plugins.configs."+pluginName+".token (preferred) or token-env, or bind an Entire auth file", 0)
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
// rejects values carrying control characters or embedded spaces.
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
// token is not a JWT or carries no numeric exp. No other claim is read.
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

// authMaterialKeys are the JSON keys recognized in an Entire auth file.
var authMaterialKeys = []string{"entire_token", "access_token", "token", "bearer_token"}

// parseAuthMaterial converts an Entire auth file into storage JSON.
//
// It accepts a bare JSON string, a {"token": "..."} object, or an object with
// one of authMaterialKeys. Nothing else is treated as owned by this provider, so
// an unrelated auth file is left to whichever provider understands it.
func parseAuthMaterial(raw []byte) (map[string]any, string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, "", false
	}

	// A bare JSON string is accepted as the token itself.
	var bare string
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &bare); errUnmarshal == nil {
		if token := normalizeToken(bare); token != "" {
			return map[string]any{"entire_token": token}, token, true
		}
		return nil, "", false
	}

	var obj map[string]any
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &obj); errUnmarshal != nil {
		return nil, "", false
	}
	token := firstTokenFromMap(obj, authMaterialKeys)
	if token == "" {
		return nil, "", false
	}
	// Persist a normalized copy so a stored auth never keeps a "Bearer " prefix
	// or stray whitespace.
	stored := make(map[string]any, len(obj)+1)
	for key, value := range obj {
		stored[key] = value
	}
	stored["entire_token"] = token
	for _, alias := range authMaterialKeys {
		if alias == "entire_token" {
			continue
		}
		delete(stored, alias)
	}
	return stored, token, true
}
