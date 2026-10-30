package main

import (
	"sort"
	"strings"
	"sync"
)

// secretRegistry holds credential values seen at runtime so that every
// message leaving the plugin can be scrubbed without relying on call-site
// discipline. Values shorter than minSecretLength are ignored to avoid
// destroying ordinary text.
const (
	minSecretLength   = 8
	secretPlaceholder = "[REDACTED]"
)

var (
	secretMu      sync.RWMutex
	secretValues = make(map[string]struct{}, 8)
	secretOrder  []string
)

// registerSecret records a credential value for redaction.
func registerSecret(value string) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < minSecretLength {
		return
	}
	secretMu.Lock()
	defer secretMu.Unlock()
	if _, exists := secretValues[trimmed]; exists {
		return
	}
	// No eviction: an evicted token could later appear unredacted in a message.
	// The registry stays small in practice (one entry per resolved credential).
	secretValues[trimmed] = struct{}{}
	secretOrder = append(secretOrder, trimmed)
}

// redactSecrets replaces every registered credential value with a placeholder.
// Callers must run it over anything that can reach a log line or an error message.
func redactSecrets(message string) string {
	if message == "" {
		return message
	}
	secretMu.RLock()
	if len(secretValues) == 0 {
		secretMu.RUnlock()
		return message
	}
	candidates := make([]string, 0, len(secretValues))
	for value := range secretValues {
		candidates = append(candidates, value)
	}
	secretMu.RUnlock()

	// Longest first so a token that contains a shorter registered value is
	// replaced as a whole.
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i]) == len(candidates[j]) {
			return candidates[i] < candidates[j]
		}
		return len(candidates[i]) > len(candidates[j])
	})
	out := message
	for _, value := range candidates {
		if strings.Contains(out, value) {
			out = strings.ReplaceAll(out, value, secretPlaceholder)
		}
	}
	return out
}

// resetSecretRegistry clears the redaction registry. Used by tests.
func resetSecretRegistry() {
	secretMu.Lock()
	defer secretMu.Unlock()
	secretValues = make(map[string]struct{}, 8)
	secretOrder = nil
}

// registeredSecretCount reports how many distinct values are tracked.
func registeredSecretCount() int {
	secretMu.RLock()
	defer secretMu.RUnlock()
	return len(secretValues)
}
