package main

import "encoding/json"

// Snapshot of the authenticated Entire Agent catalogue, checked 2026-10-30.
// Catalogue availability is not a claim of inference execution support.
var entireModelIDs = []string{
	"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-opus-5", "claude-sonnet-5",
	"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-astra", "gpt-6-luna", "gpt-6-sol", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.6-terra",
}

func handleModels(method string, raw []byte) ([]byte, *pluginError) {
	models := make([]map[string]any, 0, len(entireModelIDs))
	if method == "model.for_auth" {
		var req struct{ AuthProvider string }
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, newPluginError(codeInvalidRequest, "invalid model discovery request", 400)
		}
		if req.AuthProvider != "entire" {
			return encodeOKEnvelope(map[string]any{"Provider": "entire", "Models": models}), nil
		}
	}
	for _, id := range entireModelIDs {
		models = append(models, map[string]any{"ID": id, "Object": "model", "OwnedBy": "entire", "DisplayName": "Entire / " + id, "Description": "Entire Agent catalogue; inference adapter not connected", "UserDefined": true})
	}
	return encodeOKEnvelope(map[string]any{"Provider": "entire", "Models": models}), nil
}
