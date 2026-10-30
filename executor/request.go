package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// chatMessage is one entry of the OpenAI chat-completions message array.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// Every other field a caller might send is captured so it can be refused
	// explicitly instead of being silently dropped.
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
	ToolCallID   string          `json:"tool_call_id"`
	Name         string          `json:"name"`
	Audio        json.RawMessage `json:"audio"`
	Refusal      json.RawMessage `json:"refusal"`
}

// chatRequest is the parsed subset of an OpenAI chat-completions request.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// Fields that are refused outright when present and non-default.
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Functions       json.RawMessage `json:"functions"`
	FunctionCall    json.RawMessage `json:"function_call"`
	ParallelCalls   json.RawMessage `json:"parallel_tool_calls"`
	ResponseFormat  json.RawMessage `json:"response_format"`
	Modalities      json.RawMessage `json:"modalities"`
	Audio           json.RawMessage `json:"audio"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	N               json.RawMessage `json:"n"`
	Logprobs        json.RawMessage `json:"logprobs"`
	TopLogprobs     json.RawMessage `json:"top_logprobs"`
}

// refusalError carries the refusal reason and the client status.
type refusalError struct {
	code    string
	message string
	status  int
}

func (r *refusalError) Error() string { return r.message }

// toPluginError converts a refusal into an ABI error with an HTTP status.
func (r *refusalError) toPluginError() *pluginError {
	return newPluginError(r.code, r.message, r.status)
}

func refuse(code, message string) *refusalError {
	return &refusalError{code: code, message: message, status: 400}
}

// parseChatRequest decodes a chat-completions payload. Malformed JSON is a
// refusal, never a "best effort" interpretation.
func parseChatRequest(payload []byte) (*chatRequest, *refusalError) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, refuse(codePayloadRejected, "request body is empty; an OpenAI chat-completions payload is required")
	}
	if trimmed[0] != '{' {
		return nil, refuse(codePayloadRejected, "request body is not a JSON object")
	}
	var req chatRequest
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&req); errDecode != nil {
		return nil, refuse(codePayloadRejected, "request body is not valid JSON: "+trimForMessage(errDecode.Error()))
	}
	if decoder.More() {
		return nil, refuse(codePayloadRejected, "request body carries trailing data after the JSON object")
	}
	return &req, nil
}

// rejectUnsupported enforces the strict input contract for this executor.
//
// The executor is text-only, non-streaming, and tool-free: a cloud review agent
// runs its own tools on its own branch and cannot continue a caller's tool loop.
// Anything the caller asks for that this executor cannot honour is refused up
// front with a concrete status instead of being dropped or silently ignored.
func rejectUnsupported(req *chatRequest) *refusalError {
	if req == nil {
		return refuse(codePayloadRejected, "request is nil")
	}

	// 1. Streaming.
	if req.Stream {
		return &refusalError{code: codeStreamRejected,
			message: "Entire review runs are non-streaming: this executor returns one complete response and will not fabricate an SSE stream. Omit stream/stream_options and use the non-streaming endpoint.",
			status:  400}
	}

	// 2. Tools in any of the four shapes the OpenAI schema permits.
	if present(req.Tools) {
		return &refusalError{code: codeToolsRejected,
			message: "tools are not supported: an Entire review agent executes its own tools inside its sandbox and cannot call back into the client's tool loop.",
			status:  400}
	}
	if present(req.ToolChoice) {
		return &refusalError{code: codeToolsRejected,
			message: "tool_choice is not supported for the same reason as tools.", status: 400}
	}
	if present(req.Functions) {
		return &refusalError{code: codeToolsRejected,
			message: "deprecated functions are not supported; no function-calling channel exists on this executor.", status: 400}
	}
	if present(req.FunctionCall) {
		return &refusalError{code: codeToolsRejected,
			message: "deprecated function_call is not supported; no function-calling channel exists on this executor.", status: 400}
	}
	if present(req.ParallelCalls) && !isFalseJSON(req.ParallelCalls) {
		return &refusalError{code: codeToolsRejected,
			message: "parallel_tool_calls is not supported.", status: 400}
	}

	// 3. Images and any non-text content part.
	if present(req.Modalities) && !isTextOnlyModalities(req.Modalities) {
		return &refusalError{code: codeImageRejected,
			message: "modalities other than text are not supported; this executor is text-in/text-out.", status: 400}
	}
	if present(req.Audio) {
		return &refusalError{code: codeContentRejected,
			message: "audio input/output is not supported.", status: 400}
	}
	if errContent := rejectContent(req.Messages); errContent != nil {
		return errContent
	}

	// 4. Parameters whose semantics a review run cannot honour.
	if present(req.ResponseFormat) && !isPlainResponseFormat(req.ResponseFormat) {
		return &refusalError{code: codeExtrasRejected,
			message: "response_format is not supported: a review run returns a fixed JSON body and cannot be constrained to a caller-supplied schema.",
			status:  400}
	}
	if present(req.N) && !isOneJSON(req.N) {
		return &refusalError{code: codeExtrasRejected,
			message: "n must be 1: one review run produces exactly one response.", status: 400}
	}
	if present(req.Logprobs) && !isFalseJSON(req.Logprobs) {
		return &refusalError{code: codeExtrasRejected,
			message: "logprobs is not supported.", status: 400}
	}
	if present(req.TopLogprobs) {
		return &refusalError{code: codeExtrasRejected,
			message: "top_logprobs is not supported.", status: 400}
	}
	if present(req.ReasoningEffort) {
		return &refusalError{code: codeExtrasRejected,
			message: "reasoning_effort is not supported; select the harness with the model name instead.", status: 400}
	}

	return nil
}

// rejectContent walks the message array and refuses tool traffic, images, and
// non-text parts while validating roles and structure.
func rejectContent(messages []chatMessage) *refusalError {
	if len(messages) == 0 {
		return refuse(codePayloadRejected, "messages must contain at least one message")
	}
	for index, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "user", "assistant":
		case "tool", "function":
			return &refusalError{code: codeToolsRejected,
				message: fmt.Sprintf("messages[%d]: role %q requires tool support, which this executor does not have.", index, message.Role),
				status:  400}
		default:
			return refuse(codeContentRejected, fmt.Sprintf("messages[%d]: unsupported role %q", index, trimForMessage(message.Role)))
		}

		if present(message.ToolCalls) {
			return &refusalError{code: codeToolsRejected,
				message: fmt.Sprintf("messages[%d]: tool_calls in history are not supported; a review run has no tool channel.", index),
				status:  400}
		}
		if present(message.FunctionCall) {
			return &refusalError{code: codeToolsRejected,
				message: fmt.Sprintf("messages[%d]: function_call in history is not supported.", index), status: 400}
		}
		if strings.TrimSpace(message.ToolCallID) != "" {
			return &refusalError{code: codeToolsRejected,
				message: fmt.Sprintf("messages[%d]: tool_call_id requires tool support.", index), status: 400}
		}
		if present(message.Audio) {
			return &refusalError{code: codeContentRejected,
				message: fmt.Sprintf("messages[%d]: audio content is not supported.", index), status: 400}
		}
		if present(message.Refusal) {
			return &refusalError{code: codeContentRejected,
				message: fmt.Sprintf("messages[%d]: refusal messages are not accepted as input.", index), status: 400}
		}
		if errPart := rejectContentParts(index, message.Content); errPart != nil {
			return errPart
		}
	}
	return nil
}

// rejectContentParts validates the content field: a plain string, or an array
// whose parts are all {"type":"text"}.
func rejectContentParts(index int, content json.RawMessage) *refusalError {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		return nil
	case '[':
		var parts []map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(trimmed, &parts); errUnmarshal != nil {
			return refuse(codePayloadRejected, fmt.Sprintf("messages[%d].content is not a valid content-part array", index))
		}
		for partIndex, part := range parts {
			var partType string
			if rawType, exists := part["type"]; exists {
				_ = json.Unmarshal(rawType, &partType)
			}
			switch strings.ToLower(strings.TrimSpace(partType)) {
			case "text", "input_text", "output_text":
				var text string
				if rawText, exists := part["text"]; exists {
					_ = json.Unmarshal(rawText, &text)
				} else {
					return refuse(codePayloadRejected,
						fmt.Sprintf("messages[%d].content[%d]: text part has no text field", index, partIndex))
				}
				// A text part carrying extra fields may smuggle an image/document
				// reference; only text is permitted.
				for key := range part {
					if key != "type" && key != "text" {
						return refuse(codeContentRejected,
							fmt.Sprintf("messages[%d].content[%d]: text parts may only carry type and text", index, partIndex))
					}
				}
			case "image_url", "image", "input_image":
				return &refusalError{code: codeImageRejected,
					message: fmt.Sprintf("messages[%d].content[%d]: image input is not supported; this executor is text-only.", index, partIndex),
					status:  400}
			case "input_file", "file", "document":
				return &refusalError{code: codeContentRejected,
					message: fmt.Sprintf("messages[%d].content[%d]: file/document input is not supported.", index, partIndex),
					status:  400}
			case "audio", "input_audio":
				return &refusalError{code: codeContentRejected,
					message: fmt.Sprintf("messages[%d].content[%d]: audio input is not supported.", index, partIndex),
					status:  400}
			case "":
				return refuse(codePayloadRejected,
					fmt.Sprintf("messages[%d].content[%d]: content part has no type", index, partIndex))
			default:
				return &refusalError{code: codeContentRejected,
					message: fmt.Sprintf("messages[%d].content[%d]: unsupported content type %q", index, partIndex, trimForMessage(partType)),
					status:  400}
			}
		}
		return nil
	default:
		return refuse(codePayloadRejected, fmt.Sprintf("messages[%d].content must be a string or a content-part array", index))
	}
}

// flattenPrompt renders the accepted messages as one plain-text prompt. Only
// text survives here; rejectUnsupported has already proven nothing else is present.
func flattenPrompt(messages []chatMessage) string {
	var builder strings.Builder
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := flattenContent(message.Content)
		if text == "" {
			continue
		}
		if role == "user" && builder.Len() == 0 {
			builder.WriteString(text)
			builder.WriteString("\n")
			continue
		}
		builder.WriteString(role)
		builder.WriteString(": ")
		builder.WriteString(text)
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// flattenContent renders a validated content field as text.
func flattenContent(content json.RawMessage) string {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if errUnmarshal := json.Unmarshal(trimmed, &text); errUnmarshal == nil {
			return text
		}
		return ""
	}
	var parts []map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(trimmed, &parts); errUnmarshal != nil {
		return ""
	}
	chunks := make([]string, 0, len(parts))
	for _, part := range parts {
		var text string
		if rawText, exists := part["text"]; exists {
			if errUnmarshal := json.Unmarshal(rawText, &text); errUnmarshal == nil && text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	return strings.Join(chunks, "\n")
}

// present reports whether a raw JSON field carries a meaningful value.
func present(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return true
}

// isFalseJSON reports whether raw is the literal false.
func isFalseJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

// isOneJSON reports whether raw is the number 1.
func isOneJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("1")) ||
		bytes.Equal(bytes.TrimSpace(raw), []byte("1.0"))
}

// isTextOnlyModalities accepts ["text"] (or its singular spelling).
func isTextOnlyModalities(raw json.RawMessage) bool {
	var modalities []string
	if errUnmarshal := json.Unmarshal(raw, &modalities); errUnmarshal != nil {
		return false
	}
	if len(modalities) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(modalities[0]), "text")
}

// isPlainResponseFormat accepts only the "text" response format.
func isPlainResponseFormat(raw json.RawMessage) bool {
	var format struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(raw, &format); errUnmarshal != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(format.Type), "text")
}
