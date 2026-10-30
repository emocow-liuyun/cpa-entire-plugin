package main

import (
	"strings"
)

// supportedModelSuffixes is the closed set of Entire review harnesses this
// executor will ever address. The wire model name is modelPrefix + suffix.
//
// "review" is the trail-review harness. the other entries are reserved names for
// per-harness review variants; a name not in this list is refused locally rather
// than forwarded to an unvalidated upstream.
var supportedModelSuffixes = map[string]string{
	"review":       "trail-review",
	"review-fast":  "trail-review",
	"review-deep":  "trail-review",
	"review-light": "trail-review",
}

// thinkingSuffixes are the reasoning-effort suffixes the host appends to a model
// name ("entire-review-review(high)"). They are part of the host model grammar,
// not part of the provider model id, so they are stripped before matching.
const maxThinkingSuffixLength = 24

// resolveModel maps a requested model name onto the Entire harness it addresses.
//
// It returns the base model name (prefix intact), the matching key, and the
// harness identifier. A name outside the closed set is refused.
func resolveModel(requested string) (base, key, harness string, err *pluginError) {
	base = stripThinkingSuffix(strings.TrimSpace(requested))
	if base == "" {
		return "", "", "", newPluginError(codeModelUnsupported,
			"Entire executor requires a model name (expected "+modelPrefix+"<variant>)", 400)
	}
	if !strings.HasPrefix(base, modelPrefix) {
		return "", "", "", newPluginError(codeModelUnsupported,
			"Entire executor only serves models under the "+modelPrefix+"* namespace; got "+trimForMessage(base), 404)
	}
	key = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(base, modelPrefix)))
	if key == "" {
		return "", "", "", newPluginError(codeModelUnsupported,
			"model name carries no variant after "+modelPrefix+"; supported: "+supportedModelList(), 400)
	}
	harness, okHarness := supportedModelSuffixes[key]
	if !okHarness {
		return "", "", "", newPluginError(codeModelUnsupported,
			"unsupported Entire review variant "+trimForMessage(modelPrefix+key)+"; supported: "+supportedModelList(), 400)
	}
	return base, key, harness, nil
}

// stripThinkingSuffix removes a trailing "(...)" reasoning-effort suffix.
func stripThinkingSuffix(model string) string {
	if !strings.HasSuffix(model, ")") {
		return model
	}
	open := strings.LastIndex(model, "(")
	if open < 0 {
		return model
	}
	// A trailing empty "()" is still the host's effort grammar, not part of the
	// provider model id, so it is stripped rather than matched literally.
	if len(model)-open-1 > maxThinkingSuffixLength {
		return model
	}
	return strings.TrimSpace(model[:open])
}

// supportedModelList renders the sorted supported model names for error messages.
func supportedModelList() string {
	keys := make([]string, 0, len(supportedModelSuffixes))
	for key := range supportedModelSuffixes {
		keys = append(keys, modelPrefix+key)
	}
	// Insertion sort keeps the package stdlib-only and the list deterministic.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ", ")
}

// modelCatalog renders the ModelInfo entries published to the host.
func modelCatalog() []modelInfo {
	entries := make([]modelInfo, 0, len(supportedModelSuffixes))
	for key := range supportedModelSuffixes {
		entries = append(entries, modelInfo{
			ID:            modelPrefix + key,
			Object:        "model",
			OwnedBy:       providerIdentifier,
			Type:          "entire-review",
			DisplayName:   "Entire " + key,
			Description:   "Entire cloud review agent (" + supportedModelSuffixes[key] + "). Non-streaming, text-only; execution is fail-closed.",
			ContextLength: 0,
			SupportedParameters: []string{
				"messages",
			},
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
			UserDefined:               false,
		})
	}
	sortModelInfos(entries)
	return entries
}

// sortModelInfos orders catalog entries by ID for deterministic registration.
func sortModelInfos(entries []modelInfo) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].ID < entries[j-1].ID; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
