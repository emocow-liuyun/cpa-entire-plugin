package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Agent is the read-only view of the Agent schema that this bridge uses. The
// ETag field is authoritative: the spec says to copy the JSON etag value into
// If-Match rather than the HTTP ETag header, which intermediaries may
// transform.
type Agent struct {
	ID              string   `json:"id"`
	ProjectID       string   `json:"project_id"`
	Version         int64    `json:"version"`
	ETag            string   `json:"etag"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Harness         string   `json:"harness"`
	Model           *string  `json:"model"`
	ReasoningEffort *string  `json:"reasoning_effort"`
	Instructions    string   `json:"instructions"`
	IsEnabled       bool     `json:"is_enabled"`
	EnvironmentID   string   `json:"environment_id"`
	RepositoryIDs   []string `json:"repository_ids"`
	TimeoutMS       int64    `json:"timeout_ms"`
	Signal          struct {
		ValueType string `json:"value_type"`
		Polarity  string `json:"polarity"`
	} `json:"signal"`
	Preset       json.RawMessage `json:"preset"`
	ArchivedAt   json.RawMessage `json:"archived_at"`
	StaleRepoIDs []string        `json:"stale_repository_ids"`
}

// AgentSignalInput narrows signal: value_type is boolean, percent or size and
// polarity which readings are good.
type AgentSignalInput struct {
	ValueType string  `json:"value_type"`
	Polarity  *string `json:"polarity,omitempty"`
}

// AgentTriggerInput is the at-most-one push trigger.
type AgentTriggerInput struct {
	ID       string   `json:"id,omitempty"`
	Kind     string   `json:"kind"`
	Branches []string `json:"branches,omitempty"`
	Paths    []string `json:"paths,omitempty"`
}

// AgentReplace is the exact AgentReplace request body: every editable field,
// with additionalProperties false. environment_id and repository_ids are the
// only required members.
//
// Omitted optional fields take their defaults on the server, so a replace that
// leaves a field out resets it; build one through AgentFrom or set every field
// deliberately.
type AgentReplace struct {
	Schema          string              `json:"$schema,omitempty"`
	Name            string              `json:"name,omitempty"`
	Description     *string             `json:"description,omitempty"`
	Harness         string              `json:"harness,omitempty"`
	Model           *NullableString     `json:"model,omitempty"`
	ReasoningEffort *NullableString     `json:"reasoning_effort,omitempty"`
	Instructions    string              `json:"instructions,omitempty"`
	IsEnabled       *bool               `json:"is_enabled,omitempty"`
	Preset          *string             `json:"preset,omitempty"`
	Signal          *AgentSignalInput   `json:"signal,omitempty"`
	TimeoutMS       *int64              `json:"timeout_ms,omitempty"`
	Triggers        []AgentTriggerInput `json:"triggers,omitempty"`
	RepositoryIDs   []string            `json:"repository_ids"`
	EnvironmentID   string              `json:"environment_id"`
}

// Validate mirrors the published constraints that a client can check without
// the server: the 16384 ceiling on instructions, the name/description/model
// maxima, the harness enum, the reasoning_effort enum and the required
// environment and repository selection.
func (a *AgentReplace) Validate() error {
	if a == nil {
		return fmt.Errorf("%w: nil AgentReplace", ErrInvalidConfig)
	}
	if a.EnvironmentID == "" {
		return fmt.Errorf("%w: environment_id is required", ErrInvalidConfig)
	}
	if len(a.RepositoryIDs) == 0 {
		return fmt.Errorf("%w: repository_ids requires 1-100 ids", ErrInvalidConfig)
	}
	if len(a.RepositoryIDs) > 100 {
		return fmt.Errorf("%w: %d repository_ids exceeds the 100 maximum", ErrInvalidConfig, len(a.RepositoryIDs))
	}
	seen := make(map[string]struct{}, len(a.RepositoryIDs))
	for _, id := range a.RepositoryIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: repository_ids contains an empty id", ErrInvalidConfig)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: repository_ids repeats %q", ErrInvalidConfig, id)
		}
		seen[id] = struct{}{}
	}
	if err := ValidateInstructions(a.Instructions); err != nil {
		return err
	}
	if len(a.Name) > MaxAgentNameLength {
		return fmt.Errorf("%w: name is %d chars, maximum %d", ErrInvalidConfig, len(a.Name), MaxAgentNameLength)
	}
	if a.Description != nil && len(*a.Description) > MaxDescriptionLength {
		return fmt.Errorf("%w: description is %d chars, maximum %d", ErrInvalidConfig, len(*a.Description), MaxDescriptionLength)
	}
	if a.Model != nil && !a.Model.IsNull() && (len(a.Model.Value) > MaxModelIDLength || a.Model.Value == "") {
		return fmt.Errorf("%w: model must be 1-%d chars", ErrInvalidConfig, MaxModelIDLength)
	}
	if a.Harness != "" && a.Harness != "claude" && a.Harness != "codex" {
		return fmt.Errorf("%w: harness %q is not claude or codex", ErrInvalidConfig, a.Harness)
	}
	if a.ReasoningEffort != nil && !a.ReasoningEffort.IsNull() && !validReasoningEffort(a.ReasoningEffort.Value) {
		return fmt.Errorf("%w: reasoning_effort %q is not one of low, medium, high, xhigh, max, ultra", ErrInvalidConfig, a.ReasoningEffort.Value)
	}
	if a.Signal != nil {
		switch a.Signal.ValueType {
		case "boolean", "percent", "size":
		case "":
			return fmt.Errorf("%w: signal.value_type is required", ErrInvalidConfig)
		default:
			return fmt.Errorf("%w: signal.value_type %q is not boolean, percent or size", ErrInvalidConfig, a.Signal.ValueType)
		}
		if a.Signal.Polarity != nil && !validPolarity(*a.Signal.Polarity) {
			return fmt.Errorf("%w: signal.polarity %q is invalid", ErrInvalidConfig, *a.Signal.Polarity)
		}
	}
	if a.TimeoutMS != nil && (*a.TimeoutMS < 1 || *a.TimeoutMS > 1800000) {
		return fmt.Errorf("%w: timeout_ms must be 1-1800000", ErrInvalidConfig)
	}
	if len(a.Triggers) > 1 {
		return fmt.Errorf("%w: at most one trigger is allowed", ErrInvalidConfig)
	}
	for _, t := range a.Triggers {
		if t.Kind != "push" {
			return fmt.Errorf("%w: trigger kind %q is not push", ErrInvalidConfig, t.Kind)
		}
	}
	return nil
}

// ValidateInstructions enforces the published 16384-character ceiling on
// AgentReplace.instructions. It is measured in runes, which is the stricter
// reading for non-ASCII prompts; a UTF-8 byte count is also checked so a
// multi-byte prompt cannot pass here and fail on the server.
func ValidateInstructions(instructions string) error {
	if n := len([]rune(instructions)); n > MaxInstructions {
		return fmt.Errorf("%w: instructions are %d characters, maximum %d", ErrInvalidConfig, n, MaxInstructions)
	}
	if len(instructions) > MaxInstructions {
		return fmt.Errorf("%w: instructions are %d bytes, maximum %d", ErrInvalidConfig, len(instructions), MaxInstructions)
	}
	return nil
}

func validReasoningEffort(v string) bool {
	switch v {
	case "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}

func validPolarity(v string) bool {
	switch v {
	case "higher_is_better", "lower_is_better", "neutral":
		return true
	}
	return false
}

// AgentFrom builds a full AgentReplace from a read Agent, which is how a
// replace stays a full replace instead of dropping fields to their defaults.
func AgentFrom(a *Agent) *AgentReplace {
	if a == nil {
		return nil
	}
	enabled := a.IsEnabled
	// model and reasoning_effort are nullable in the schema; null explicitly
	// selects the harness default/its default effort, which is what a read
	// Agent reported. Copy them as-is rather than dropping the fields.
	model := nullable(a.Model)
	effort := nullable(a.ReasoningEffort)
	out := &AgentReplace{
		Name:            a.Name,
		Harness:         a.Harness,
		Model:           model,
		ReasoningEffort: effort,
		Instructions:    a.Instructions,
		IsEnabled:       &enabled,
		EnvironmentID:   a.EnvironmentID,
		RepositoryIDs:   append([]string(nil), a.RepositoryIDs...),
	}
	desc := a.Description
	out.Description = &desc
	timeout := a.TimeoutMS
	if timeout > 0 {
		out.TimeoutMS = &timeout
	}
	if a.Signal.ValueType != "" {
		pol := a.Signal.Polarity
		out.Signal = &AgentSignalInput{ValueType: a.Signal.ValueType}
		if pol != "" {
			out.Signal.Polarity = &pol
		}
	}
	return out
}

// NullableString is a string that can be sent as an explicit JSON null.
//
// The schema types model and reasoning_effort as ["string","null"] and null is
// meaningful: it selects the harness default model or its default reasoning
// effort, which is what a read Agent reports with a nil value.
//
// encoding/json drops a nil *T field that carries omitempty, so "not sent" and
// "sent as null" would collapse into the same wire form. Two states, two
// constructors:
//
//	nil                               -> {}          (field not replaced)
//	&NullableString{Present: true}    -> {"f": null}  (explicit null)
//	NullableStringOf("gpt-5-codex")   -> {"f": "..."} (explicit value)
type NullableString struct {
	// Value is the string when Present is true and Value != "".
	Value string
	// Present marks the field as set. MarshalJSON always emits either the value
	// or null, so a non-nil pointer without Present is a null.
	Present bool
}

// NullableStringOf returns a NullableString carrying v.
func NullableStringOf(v string) *NullableString { return &NullableString{Value: v, Present: true} }

// NullableStringNull returns a NullableString that encodes as null.
func NullableStringNull() *NullableString { return &NullableString{Present: true} }

// IsNull reports whether the value encodes as JSON null, which includes the
// zero NullableString.
func (n *NullableString) IsNull() bool { return n == nil || n.Value == "" }

// StringValue returns the carried value and whether one is set.
func (n *NullableString) StringValue() (string, bool) {
	if n.IsNull() {
		return "", false
	}
	return n.Value, true
}

// MarshalJSON encodes the carried value, or null when it is empty.
func (n *NullableString) MarshalJSON() ([]byte, error) {
	if n.IsNull() {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// UnmarshalJSON accepts a string or null.
func (n *NullableString) UnmarshalJSON(raw []byte) error {
	n.Present = true
	if string(raw) == "null" {
		n.Value = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	n.Value = s
	return nil
}

// nullable converts a plain pointer into the wire form, preserving null.
func nullable(p *string) *NullableString {
	if p == nil {
		return NullableStringNull()
	}
	if *p == "" {
		// An empty string is not a valid model id; send null instead, which the
		// schema accepts and which means "harness default".
		return NullableStringNull()
	}
	return NullableStringOf(*p)
}
