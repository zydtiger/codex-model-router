package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// inputStats records what a translation changed. It is logged as counts only:
// no item content is written anywhere.
type inputStats struct {
	MessagesKept       int
	DeveloperRewrites  int
	AgentMessages      int
	ReasoningDropped   int
	CompactionDropped  int
	CustomToolsMapped  int
	CustomToolsKept    int
	CustomToolsDropped int
	UnknownDropped     int
	IncludeStripped    int
}

// Changed reports whether the translated body differs from the decoded input.
func (s inputStats) Changed() bool {
	return s.DeveloperRewrites+s.AgentMessages+s.ReasoningDropped+s.CompactionDropped+
		s.CustomToolsMapped+s.CustomToolsKept+s.CustomToolsDropped+s.UnknownDropped+s.IncludeStripped > 0
}

// Warnings returns operator-visible notes about provider state that could not
// travel to a different provider. Conversation text is never in this list.
func (s inputStats) Warnings() []string {
	var warnings []string
	if s.ReasoningDropped > 0 {
		warnings = append(warnings, fmt.Sprintf("dropped %d encrypted reasoning item(s): provider reasoning state is not portable to route upstreams", s.ReasoningDropped))
	}
	if s.CompactionDropped > 0 {
		warnings = append(warnings, fmt.Sprintf("dropped %d encrypted compaction item(s): provider compaction state is not portable to route upstreams", s.CompactionDropped))
	}
	if s.UnknownDropped > 0 {
		warnings = append(warnings, fmt.Sprintf("dropped %d unmodelled input item(s)", s.UnknownDropped))
	}
	if s.CustomToolsDropped > 0 {
		warnings = append(warnings, fmt.Sprintf("dropped %d custom tool item(s)", s.CustomToolsDropped))
	}
	return warnings
}

// translateRemoteBody rewrites a decoded Codex request for a remote route.
//
// The rules are narrow on purpose:
//   - message and agent_message items are always kept, so conversation text
//     cannot be lost;
//   - function calls and function outputs are preserved as they are the tool
//     loop history;
//   - encrypted reasoning and compaction state is handled by the route policy
//     because another provider cannot decrypt it;
//   - anything the router does not model follows input.unknown_items.
func translateRemoteBody(fields map[string]json.RawMessage, route *config.Route) (map[string]json.RawMessage, inputStats, *requestError) {
	stats := inputStats{}
	if rawInput, ok := fields["input"]; ok && isJSONArray(rawInput) {
		translated, itemStats, err := translateInput(rawInput, route)
		if err != nil {
			return nil, stats, err
		}
		fields["input"] = translated
		stats.add(itemStats)
	}
	if rawInclude, ok := fields["include"]; ok && isJSONArray(rawInclude) {
		cleaned, stripped := stripUnusableInclude(rawInclude, route)
		if stripped > 0 {
			fields["include"] = cleaned
			stats.IncludeStripped = stripped
		}
	}
	return fields, stats, nil
}

func (s *inputStats) add(other inputStats) {
	s.MessagesKept += other.MessagesKept
	s.DeveloperRewrites += other.DeveloperRewrites
	s.AgentMessages += other.AgentMessages
	s.ReasoningDropped += other.ReasoningDropped
	s.CompactionDropped += other.CompactionDropped
	s.CustomToolsMapped += other.CustomToolsMapped
	s.CustomToolsKept += other.CustomToolsKept
	s.CustomToolsDropped += other.CustomToolsDropped
	s.UnknownDropped += other.UnknownDropped
	s.IncludeStripped += other.IncludeStripped
}

// translateInput rewrites the input array while preserving order.
func translateInput(rawInput json.RawMessage, route *config.Route) (json.RawMessage, inputStats, *requestError) {
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		return nil, inputStats{}, clientError(http.StatusBadRequest, "invalid_json", "the input field must be a JSON array")
	}
	stats := inputStats{}
	kept := make([]json.RawMessage, 0, len(items))
	for index, item := range items {
		converted, keep, itemStats, err := translateItem(item, route, index)
		if err != nil {
			return nil, stats, err
		}
		stats.add(itemStats)
		if keep {
			kept = append(kept, converted)
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, stats, clientError(http.StatusInternalServerError, "encode_failed", "could not encode the translated input")
	}
	return encoded, stats, nil
}

// translateItem maps one Codex input item onto what a generic OpenAI-compatible
// Responses server can accept.
func translateItem(item json.RawMessage, route *config.Route, index int) (json.RawMessage, bool, inputStats, *requestError) {
	var header struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(item, &header); err != nil {
		return nil, false, inputStats{}, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a JSON object", index)
	}
	itemType := strings.TrimSpace(header.Type)
	if itemType == "" && strings.TrimSpace(header.Role) != "" {
		// Codex also sends messages as {role, content} shorthand.
		itemType = "message"
	}
	stats := inputStats{}

	switch itemType {
	case "message":
		converted, rewrote, err := translateMessage(item, route, index)
		if err != nil {
			return nil, false, stats, err
		}
		stats.MessagesKept = 1
		if rewrote {
			stats.DeveloperRewrites = 1
		}
		return converted, true, stats, nil

	case "agent_message":
		converted, err := translateAgentMessage(item, index)
		if err != nil {
			return nil, false, stats, err
		}
		stats.AgentMessages = 1
		stats.MessagesKept = 1
		return converted, true, stats, nil

	case "function_call", "function_call_output":
		stats.MessagesKept = 0
		return item, true, stats, nil

	case "custom_tool_call", "custom_tool_call_output":
		switch route.Input.CustomTools {
		case config.PolicyKeep:
			stats.CustomToolsKept = 1
			return item, true, stats, nil
		case config.PolicyDrop:
			stats.CustomToolsDropped = 1
			return nil, false, stats, nil
		case config.PolicyReject:
			return nil, false, stats, clientError(http.StatusBadRequest, "unsupported_input_item",
				"input[%d] uses a custom tool item and input.custom_tools is reject", index)
		default:
			converted, err := translateCustomTool(item, index)
			if err != nil {
				return nil, false, stats, err
			}
			stats.CustomToolsMapped = 1
			return converted, true, stats, nil
		}

	case "reasoning":
		switch route.Input.ReasoningItems {
		case config.PolicyKeep:
			return item, true, stats, nil
		case config.PolicyReject:
			return nil, false, stats, clientError(http.StatusBadRequest, "unsupported_input_item",
				"input[%d] is a reasoning item and input.reasoning_items is reject", index)
		default:
			stats.ReasoningDropped = 1
			return nil, false, stats, nil
		}

	case "compaction":
		switch route.Input.CompactionItems {
		case config.PolicyReject:
			return nil, false, stats, clientError(http.StatusBadRequest, "unsupported_input_item",
				"input[%d] is a compaction item and input.compaction_items is reject", index)
		default:
			stats.CompactionDropped = 1
			return nil, false, stats, nil
		}

	default:
		switch route.Input.UnknownItems {
		case config.PolicyKeep:
			return item, true, stats, nil
		case config.PolicyReject:
			return nil, false, stats, clientError(http.StatusBadRequest, "unsupported_input_item",
				"input[%d] has unsupported type %q and input.unknown_items is reject", index, itemType)
		default:
			stats.UnknownDropped = 1
			return nil, false, stats, nil
		}
	}
}

// translateMessage keeps every message. A developer role becomes system when
// the route asks for it, because many chat-completion style servers reject
// developer.
func translateMessage(item json.RawMessage, route *config.Route, index int) (json.RawMessage, bool, *requestError) {
	if !route.DeveloperRoleAsSystem() {
		return item, false, nil
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(item, &message); err != nil {
		return nil, false, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a message object", index)
	}
	role, ok := message["role"]
	if !ok {
		return item, false, nil
	}
	var roleValue string
	if err := json.Unmarshal(role, &roleValue); err != nil {
		return item, false, nil
	}
	if roleValue != "developer" {
		return item, false, nil
	}
	message["role"] = json.RawMessage(`"system"`)
	converted, err := json.Marshal(message)
	if err != nil {
		return nil, false, clientError(http.StatusInternalServerError, "encode_failed", "could not encode input[%d]", index)
	}
	return converted, true, nil
}

// translateAgentMessage flattens Codex's multi-agent envelope into a plain user
// message. The visible text is preserved, including content parts that Codex
// labels encrypted_content but sends as plain text.
func translateAgentMessage(item json.RawMessage, index int) (json.RawMessage, *requestError) {
	var envelope struct {
		Author    string            `json:"author"`
		Recipient string            `json:"recipient"`
		Content   []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(item, &envelope); err != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a valid agent_message", index)
	}
	if strings.TrimSpace(envelope.Author) == "" || strings.TrimSpace(envelope.Recipient) == "" || len(envelope.Content) == 0 {
		return nil, clientError(http.StatusBadRequest, "invalid_input_item",
			"input[%d] is an agent_message without author, recipient, or content", index)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a JSON object", index)
	}

	parts := make([]any, 0, len(envelope.Content)+1)
	parts = append(parts, map[string]string{
		"type": "input_text",
		"text": fmt.Sprintf("Agent message from %q to %q:\n", envelope.Author, envelope.Recipient),
	})
	for partIndex, raw := range envelope.Content {
		var part struct {
			Type             string  `json:"type"`
			Text             *string `json:"text"`
			EncryptedContent *string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, clientError(http.StatusBadRequest, "invalid_json", "input[%d].content[%d] is not valid JSON", index, partIndex)
		}
		switch part.Type {
		case "input_text":
			if part.Text == nil {
				return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d].content[%d] input_text has no text", index, partIndex)
			}
			parts = append(parts, map[string]string{"type": "input_text", "text": *part.Text})
		case "encrypted_content":
			if part.EncryptedContent == nil {
				return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d].content[%d] encrypted_content has no value", index, partIndex)
			}
			parts = append(parts, map[string]string{"type": "input_text", "text": *part.EncryptedContent})
		default:
			return nil, clientError(http.StatusBadRequest, "unsupported_input_item",
				"input[%d].content[%d] has unsupported agent_message content type %q", index, partIndex, part.Type)
		}
	}

	fields["type"] = json.RawMessage(`"message"`)
	fields["role"] = json.RawMessage(`"user"`)
	delete(fields, "author")
	delete(fields, "recipient")
	encodedContent, err := json.Marshal(parts)
	if err != nil {
		return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode input[%d]", index)
	}
	fields["content"] = encodedContent
	converted, err := json.Marshal(fields)
	if err != nil {
		return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode input[%d]", index)
	}
	return converted, nil
}

// translateCustomTool converts Codex free-form tool traffic to function-call
// traffic, which generic Responses servers understand. The tool payload is kept
// verbatim inside an "input" argument.
func translateCustomTool(item json.RawMessage, index int) (json.RawMessage, *requestError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a JSON object", index)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(item, &kind); err != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_json", "input[%d] is not a JSON object", index)
	}

	converted := map[string]any{}
	if id, ok := fields["id"]; ok {
		var idValue string
		if json.Unmarshal(id, &idValue) == nil && idValue != "" {
			converted["id"] = idValue
		}
	}
	callID := ""
	if raw, ok := fields["call_id"]; ok {
		if err := json.Unmarshal(raw, &callID); err != nil {
			return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d] has no call_id", index)
		}
	}
	if callID == "" {
		return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d] has no call_id", index)
	}
	converted["call_id"] = callID

	if kind.Type == "custom_tool_call_output" {
		output := json.RawMessage(`""`)
		if raw, ok := fields["output"]; ok {
			output = raw
		}
		converted["type"] = "function_call_output"
		converted["output"] = output
	} else {
		name := ""
		if raw, ok := fields["name"]; ok {
			if err := json.Unmarshal(raw, &name); err != nil || name == "" {
				return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d] custom_tool_call has no name", index)
			}
		}
		input := ""
		if raw, ok := fields["input"]; ok {
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, clientError(http.StatusBadRequest, "invalid_input_item", "input[%d] custom_tool_call input must be a string", index)
			}
		}
		arguments, err := json.Marshal(map[string]string{"input": input})
		if err != nil {
			return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode input[%d] arguments", index)
		}
		converted["type"] = "function_call"
		converted["name"] = name
		converted["arguments"] = string(arguments)
	}

	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode input[%d]", index)
	}
	return encoded, nil
}

// stripUnusableInclude removes include values that ask a route upstream for
// encrypted provider state it cannot produce.
func stripUnusableInclude(rawInclude json.RawMessage, route *config.Route) (json.RawMessage, int) {
	var values []string
	if err := json.Unmarshal(rawInclude, &values); err != nil {
		return rawInclude, 0
	}
	keep := make([]string, 0, len(values))
	stripped := 0
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		asksForProviderState := strings.Contains(normalized, "encrypted_content") || strings.Contains(normalized, "reasoning")
		if !asksForProviderState || route.Input.ReasoningItems == config.PolicyKeep {
			keep = append(keep, value)
			continue
		}
		stripped++
	}
	if stripped == 0 {
		return rawInclude, 0
	}
	encoded, err := json.Marshal(keep)
	if err != nil {
		return rawInclude, 0
	}
	return encoded, stripped
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}
