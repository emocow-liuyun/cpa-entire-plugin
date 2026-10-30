package main

import (
	"sort"
	"strings"
	"sync"
)

// secretRegistry holds credential values seen at runtime so every message
// leaving the plugin can be scrubbed without relying on call-site discipline.
const (
	minSecretLength   = 8
	secretPlaceholder = "[REDACTED]"
	maxRegisteredKeys = 64
)

var (
	secretMu      sync.RWMutex
	secretValues  = make(map[string]struct{}, 8)
	secretOrder   []string
	secretDropped int
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
	if len(secretValues) >= maxRegisteredKeys {
		oldest := secretOrder[0]
		secretOrder = secretOrder[1:]
		delete(secretValues, oldest)
		secretDropped++
	}
	secretValues[trimmed] = struct{}{}
	secretOrder = append(secretOrder, trimmed)
}

// redactSecrets replaces every registered credential value with a placeholder.
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

	// Longest first, so a token containing a shorter registered value is replaced whole.
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
	secretDropped = 0
}

// registeredSecretCount reports how many distinct values are tracked.
func registeredSecretCount() int {
	secretMu.RLock()
	defer secretMu.RUnlock()
	return len(secretValues)
}
