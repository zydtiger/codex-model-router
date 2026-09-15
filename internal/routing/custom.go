package routing

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// customMapping belongs to one request. It remembers which function names the
// router derived from Responses custom-tool declarations, so response traffic
// for exactly those identities can be bridged back to custom tool calls.
// Ordinary function tools and unrelated names are never reinterpreted.
type customMapping struct {
	identities     map[toolIdentity]bool
	originalTools  json.RawMessage
	originalChoice json.RawMessage
}

// unwrapCustomInput validates the function-arguments wrapper the router itself
// produces for bridged custom tools: a JSON object whose only key is a string
// "input". Anything else is a protocol failure, not a best-effort string.
func unwrapCustomInput(arguments string) (string, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &wrapper); err != nil || wrapper == nil {
		return "", fmt.Errorf("bridged custom tool arguments are not a JSON object")
	}
	if len(wrapper) != 1 {
		return "", fmt.Errorf("bridged custom tool arguments must contain exactly the input key")
	}
	raw, ok := wrapper["input"]
	if !ok || bytesNull(raw) {
		return "", fmt.Errorf("bridged custom tool arguments are missing the input string")
	}
	var input string
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("bridged custom tool input is not a string")
	}
	return input, nil
}

// prepareCustomTools converts custom-tool declarations and replayed history to
// the function surface before any other translation runs. It runs only for
// routes that opted into the custom_to_functions adapter; the returned mapping
// is non-nil exactly when custom declarations exist, and drives response
// restoration for those identities.
func prepareCustomTools(fields map[string]json.RawMessage) (*customMapping, *requestError) {
	m := &customMapping{
		identities:     make(map[toolIdentity]bool),
		originalTools:  fields["tools"],
		originalChoice: fields["tool_choice"],
	}
	var tools []json.RawMessage
	if raw := fields["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_tool", "tools must be an array")
	}
	// Pre-scan the plain function declarations so a custom tool that reuses a
	// callable identity is refused regardless of declaration order. With both
	// traveling upstream as the same function name, response traffic could not
	// be attributed to one of them reliably.
	callable := make(map[toolIdentity]bool)
	for _, raw := range tools {
		f, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if f.string("type") == "function" && f.string("name") != "" {
			callable[toolIdentity{"", f.string("name")}] = true
			continue
		}
		if f.string("type") != "namespace" {
			continue
		}
		var children []toolFields
		if json.Unmarshal(f["tools"], &children) == nil {
			for _, child := range children {
				if child.string("type") == "function" && child.string("name") != "" {
					callable[toolIdentity{f.string("name"), child.string("name")}] = true
				}
			}
		}
	}
	declared := false
	for i, raw := range tools {
		f, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if f.string("type") == "custom" {
			converted, err2 := m.wrapCustomDeclaration("", f, callable)
			if err2 != nil {
				return nil, err2
			}
			tools[i] = converted
			declared = true
			continue
		}
		if f.string("type") != "namespace" {
			continue
		}
		var children []json.RawMessage
		namespace := f.string("name")
		if namespace == "" || json.Unmarshal(f["tools"], &children) != nil {
			return nil, clientError(http.StatusBadRequest, "invalid_namespace", "a namespace requires a name and tools array")
		}
		childChanged := false
		for j, child := range children {
			fn, err := decodeTool(child)
			if err != nil {
				return nil, err
			}
			if fn.string("type") != "custom" {
				continue
			}
			converted, err2 := m.wrapCustomDeclaration(namespace, fn, callable)
			if err2 != nil {
				return nil, err2
			}
			children[j] = converted
			childChanged = true
		}
		if childChanged {
			f["tools"], _ = json.Marshal(children)
			tools[i], _ = json.Marshal(f)
			declared = true
		}
	}
	if declared && fields["tools"] != nil {
		fields["tools"], _ = json.Marshal(tools)
	}
	// History conversion happens even without declarations, so replayed custom
	// traffic always reaches a function-only upstream in this adapter. It reuses
	// the shared history translation, which preserves namespaces for the
	// flattening pass that runs later.
	if isJSONArray(fields["input"]) {
		var input []json.RawMessage
		if err := json.Unmarshal(fields["input"], &input); err != nil {
			return nil, clientError(http.StatusBadRequest, "invalid_json", "the input field must be a JSON array")
		}
		changed := false
		for i, item := range input {
			f, err := decodeTool(item)
			if err != nil {
				return nil, err
			}
			if f.string("type") != "custom_tool_call" && f.string("type") != "custom_tool_call_output" {
				continue
			}
			converted, err2 := translateCustomTool(item, i)
			if err2 != nil {
				return nil, err2
			}
			input[i] = converted
			changed = true
		}
		if changed {
			fields["input"], _ = json.Marshal(input)
		}
	}
	if raw := fields["tool_choice"]; len(raw) > 0 && raw[0] == '{' {
		choice, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		converted, err2 := m.convertChoice(choice)
		if err2 != nil {
			return nil, err2
		}
		if converted {
			fields["tool_choice"], _ = json.Marshal(choice)
		}
	}
	if !declared {
		return nil, nil
	}
	return m, nil
}

// wrapCustomDeclaration turns one custom tool declaration into a function
// declaration whose single required string property carries the raw custom
// input, and records the identity for response restoration. The original
// description is retained; grammar formats travel verbatim as model-facing
// guidance, because the router performs no constrained decoding on the
// upstream's behalf.
func (m *customMapping) wrapCustomDeclaration(namespace string, f toolFields, callable map[toolIdentity]bool) (json.RawMessage, *requestError) {
	id := toolIdentity{namespace, f.string("name")}
	if id.Name == "" {
		return nil, clientError(http.StatusBadRequest, "invalid_custom_tool", "a custom tool requires a name")
	}
	if m.identities[id] {
		return nil, clientError(http.StatusBadRequest, "duplicate_custom_tool", "custom tool %q is declared more than once", id.Name)
	}
	if callable[id] {
		return nil, clientError(http.StatusBadRequest, "duplicate_custom_tool",
			"a function tool with the identity of custom tool %q is already declared in this request", id.Name)
	}
	var format struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	}
	if raw := f["format"]; raw != nil && !bytesNull(raw) {
		if err := json.Unmarshal(raw, &format); err != nil || format.Type == "" {
			return nil, clientError(http.StatusBadRequest, "unsupported_custom_tool", "a custom tool format must be an object with a type")
		}
		switch format.Type {
		case "text":
		case "grammar":
			if format.Definition == "" {
				return nil, clientError(http.StatusBadRequest, "invalid_custom_tool", "a grammar custom tool requires a definition")
			}
		default:
			return nil, clientError(http.StatusBadRequest, "unsupported_custom_tool", "custom tool format %q is not supported", format.Type)
		}
	}
	m.identities[id] = true

	description := "Custom tool. The single input string argument carries the tool's free-form text input."
	if text := f.string("description"); text != "" {
		description += "\n" + text
	}
	if format.Type == "grammar" {
		syntax := format.Syntax
		if syntax == "" {
			syntax = "unspecified"
		}
		description += "\nInput grammar (" + syntax + ", advisory guidance for the expected input shape; not enforced by decoding):\n" + format.Definition
	}
	fn := toolFields{}
	fn.setString("type", "function")
	fn.setString("name", id.Name)
	fn.setString("description", description)
	parameters, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"input": map[string]any{"type": "string"}},
		"required":             []string{"input"},
		"additionalProperties": false,
	})
	fn["parameters"] = parameters
	encoded, err := json.Marshal(fn)
	if err != nil {
		return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode a custom tool declaration")
	}
	return encoded, nil
}

// convertChoice rewrites custom tool_choice selectors to function selectors.
// none/auto/required and function selectors are left alone. An explicit custom
// selector that names no declared custom tool is refused instead of being
// ambiguously pointed at an unrelated function.
func (m *customMapping) convertChoice(choice toolFields) (bool, *requestError) {
	changed := false
	if choice.string("type") == "allowed_tools" {
		var allowed []toolFields
		if json.Unmarshal(choice["tools"], &allowed) != nil {
			return false, clientError(http.StatusBadRequest, "invalid_tool_choice", "allowed_tools requires a tools array")
		}
		for _, tool := range allowed {
			converted, err := m.convertChoice(tool)
			if err != nil {
				return false, err
			}
			changed = changed || converted
		}
		if changed {
			choice["tools"], _ = json.Marshal(allowed)
		}
		return changed, nil
	}
	if choice.string("type") != "custom" {
		return false, nil
	}
	id := toolIdentity{choice.string("namespace"), choice.string("name")}
	if !m.identities[id] {
		return false, clientError(http.StatusBadRequest, "invalid_tool_choice", "tool_choice references an undeclared custom tool")
	}
	choice.setString("type", "function")
	return true, nil
}

// restoreCustomFields rewrites protocol envelopes only, mirroring the namespace
// restoration: user content, ordinary function calls, and result payloads are
// never inspected for matching names.
func (m *customMapping) restoreCustomFields(fields toolFields) error {
	kind := fields.string("type")
	if kind == "function_call" {
		id := toolIdentity{fields.string("namespace"), fields.string("name")}
		if m.identities[id] {
			input, err := unwrapCustomInput(fields.string("arguments"))
			if err != nil {
				return err
			}
			fields.setString("type", "custom_tool_call")
			fields.setString("input", input)
			delete(fields, "arguments")
		}
	}
	for _, key := range []string{"item", "response"} {
		if raw, ok := fields[key]; ok && !bytesNull(raw) {
			restored, err := m.restoreCustomResponse(raw)
			if err != nil {
				return err
			}
			fields[key] = restored
		}
	}
	if raw, ok := fields["output"]; ok && isJSONArray(raw) {
		var output []json.RawMessage
		if err := json.Unmarshal(raw, &output); err != nil {
			return err
		}
		for i, item := range output {
			restored, err := m.restoreCustomResponse(item)
			if err != nil {
				return err
			}
			output[i] = restored
		}
		fields["output"], _ = json.Marshal(output)
	}
	// The custom mapping captured the tools before any adapter rewrote them,
	// so its restoration is authoritative when both adapters run.
	if fields.string("object") == "response" || fields["output"] != nil {
		if m.originalTools != nil {
			fields["tools"] = m.originalTools
		}
		if m.originalChoice != nil {
			fields["tool_choice"] = m.originalChoice
		}
	}
	return nil
}

func (m *customMapping) restoreCustomResponse(raw []byte) ([]byte, error) {
	var fields toolFields
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("invalid upstream response JSON")
	}
	if err := m.restoreCustomFields(fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
