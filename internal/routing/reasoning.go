package routing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// applyReasoningAdapter translates Codex's reasoning request into the shape the
// route's server expects.
//
// AdapterNone leaves the request untouched, which is correct for servers that
// implement the Responses reasoning field. AdapterSGLangChatTemplate converts
// reasoning.effort into explicitly configured chat_template_kwargs, so a
// thinking model can be driven from the Codex thinking selector, including
// turning thinking off.
func applyReasoningAdapter(fields map[string]json.RawMessage, route *config.Route) (bool, *requestError) {
	switch route.Reasoning.Adapter {
	case config.AdapterNone:
		return false, nil
	case config.AdapterSGLangChatTemplate:
		return applySGLangChatTemplate(fields, route)
	default:
		// config.Validate rejects unknown adapters, so this is unreachable.
		return false, clientError(http.StatusInternalServerError, "adapter_misconfigured", "reasoning adapter %q is not supported", route.Reasoning.Adapter)
	}
}

func applySGLangChatTemplate(fields map[string]json.RawMessage, route *config.Route) (bool, *requestError) {
	rawReasoning, present := fields["reasoning"]
	effort := ""
	if present {
		trimmed := bytes.TrimSpace(rawReasoning)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return false, clientError(http.StatusBadRequest, "invalid_reasoning", "the reasoning field must be a JSON object")
		}
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(trimmed, &reasoning); err != nil {
			return false, clientError(http.StatusBadRequest, "invalid_reasoning", "the reasoning field is not a valid object")
		}
		effort = strings.ToLower(strings.TrimSpace(reasoning.Effort))
	}

	// A request without an effort uses the server default. The reasoning object
	// is still removed unless the route forwards it, because an unrecognised
	// reasoning field makes some strict servers fail the whole request.
	kwargs := map[string]any{}
	if effort != "" && len(route.Reasoning.SupportedEfforts) > 0 && !containsString(route.Reasoning.SupportedEfforts, effort) {
		if route.Reasoning.UnknownEffort == config.PolicyError {
			return false, clientError(http.StatusBadRequest, "unsupported_reasoning_effort",
				"reasoning effort %q is not supported by this route", effort)
		}
		// Drop the effort so the server applies its own default instead of failing
		// the request or guessing a level.
		effort = ""
	}
	// A configured value that is not a per-effort map is a constant keyword, such
	// as preserve_thinking, and applies to every request on this route.
	for name, raw := range route.Reasoning.ChatTemplateKwargs {
		value, ok, err := resolveKwarg(raw, effort)
		if err != nil {
			return false, clientError(http.StatusBadRequest, "adapter_misconfigured", "%s", err.Error())
		}
		if ok {
			kwargs[name] = value
		}
	}

	changed := false
	if present && !route.Reasoning.ForwardReasoning {
		delete(fields, "reasoning")
		changed = true
	}
	if len(kwargs) == 0 {
		return changed, nil
	}

	// The router owns this field for configured routes, so a value from the client
	// is merged into a map this function controls. A JSON null decodes into a nil
	// map without an error, which must be replaced rather than written into.
	merged := map[string]any{}
	if existing, ok := fields["chat_template_kwargs"]; ok {
		if err := json.Unmarshal(existing, &merged); err != nil || merged == nil {
			// The client's value is unusable as a map. It is replaced by the kwargs this
			// route configures, which is a change to the forwarded body.
			delete(fields, "chat_template_kwargs")
			merged = map[string]any{}
			changed = true
		}
	}
	for name, value := range kwargs {
		if current, ok := merged[name]; ok && equalJSON(current, value) {
			continue
		}
		merged[name] = value
		changed = true
	}
	if !changed {
		return false, nil
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return false, clientError(http.StatusInternalServerError, "encode_failed", "could not encode chat_template_kwargs")
	}
	fields["chat_template_kwargs"] = encoded
	return true, nil
}

// resolveKwarg resolves one configured keyword for an effort. A configured
// object is an effort map: without an effort, or without a key for that effort,
// or with a null value, the keyword is omitted.
func resolveKwarg(raw json.RawMessage, effort string) (any, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		var literal any
		if err := json.Unmarshal(trimmed, &literal); err != nil {
			return nil, false, err
		}
		return literal, true, nil
	}
	if effort == "" {
		return nil, false, nil
	}
	var effortMap map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &effortMap); err != nil {
		return nil, false, err
	}
	value, ok := effortMap[effort]
	if !ok {
		return nil, false, nil
	}
	if string(bytes.TrimSpace(value)) == "null" {
		return nil, false, nil
	}
	var resolved any
	if err := json.Unmarshal(value, &resolved); err != nil {
		return nil, false, err
	}
	return resolved, true, nil
}

func equalJSON(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}
