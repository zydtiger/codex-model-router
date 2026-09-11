package routing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type toolIdentity struct {
	Namespace string
	Name      string
}

// namespaceMapping belongs to one request, including its replayed history.
// No shared state is needed to route concurrent conversations independently.
type namespaceMapping struct {
	aliases        map[toolIdentity]string
	originals      map[string]toolIdentity
	used           map[string]bool
	originalTools  json.RawMessage
	originalChoice json.RawMessage
}

type toolFields map[string]json.RawMessage

func (f toolFields) string(key string) string {
	var value string
	_ = json.Unmarshal(f[key], &value)
	return value
}

func (f toolFields) setString(key, value string) {
	f[key], _ = json.Marshal(value)
}

func decodeTool(raw json.RawMessage) (toolFields, *requestError) {
	var f toolFields
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return nil, clientError(http.StatusBadRequest, "invalid_tool", "tool definitions and calls must be objects")
	}
	return f, nil
}

func (m *namespaceMapping) alias(id toolIdentity) string {
	if name, ok := m.aliases[id]; ok {
		return name
	}
	name := id.Namespace + "__" + id.Name
	// Keep ordinary MCP names readable. Longer names or collisions use a short
	// request-local alias; the description still contains the original identity.
	if len(name) > 64 || m.used[name] || strings.ContainsAny(name, ". /") {
		for n := 0; ; n++ {
			name = fmt.Sprintf("cmr_namespace_%d", n)
			if !m.used[name] {
				break
			}
		}
	}
	m.used[name] = true
	m.aliases[id] = name
	m.originals[name] = id
	return name
}

// flattenNamespaces bridges Responses namespaces to the function-only tool
// surface used by SGLang. Unrelated top-level tools are left as received.
func flattenNamespaces(fields map[string]json.RawMessage) (*namespaceMapping, *requestError) {
	m := &namespaceMapping{
		aliases: make(map[toolIdentity]string), originals: make(map[string]toolIdentity),
		used: make(map[string]bool), originalTools: fields["tools"], originalChoice: fields["tool_choice"],
	}
	var tools, input []json.RawMessage
	if raw := fields["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_tool", "tools must be an array")
	}
	if isJSONArray(fields["input"]) {
		_ = json.Unmarshal(fields["input"], &input)
	}
	// Reserve top-level names before allocating aliases, including old calls to
	// tools that are no longer advertised in this request.
	for _, raw := range append(append([]json.RawMessage{}, tools...), input...) {
		f, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if f.string("type") != "namespace" && f.string("namespace") == "" {
			m.used[f.string("name")] = true
		}
	}
	flat := make([]json.RawMessage, 0, len(tools))
	seen := make(map[toolIdentity]bool)
	for _, raw := range tools {
		f, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if f.string("type") != "namespace" {
			flat = append(flat, raw)
			continue
		}
		namespace := f.string("name")
		var children []json.RawMessage
		if namespace == "" || json.Unmarshal(f["tools"], &children) != nil {
			return nil, clientError(http.StatusBadRequest, "invalid_namespace", "a namespace requires a name and tools array")
		}
		for _, child := range children {
			fn, err := decodeTool(child)
			if err != nil {
				return nil, err
			}
			if fn.string("type") != "function" || fn.string("name") == "" {
				return nil, clientError(http.StatusBadRequest, "unsupported_namespace_tool", "SGLang namespaces require named function tools")
			}
			id := toolIdentity{namespace, fn.string("name")}
			if seen[id] {
				return nil, clientError(http.StatusBadRequest, "duplicate_namespace_tool", "a namespace function is declared more than once")
			}
			seen[id] = true
			fn.setString("name", m.alias(id))
			description := "Namespace " + namespace + ", function " + id.Name + "."
			if text := f.string("description"); text != "" {
				description += "\n" + text
			}
			if text := fn.string("description"); text != "" {
				description += "\n" + text
			}
			fn.setString("description", description)
			// These definitions are now eagerly visible to the upstream model.
			delete(fn, "defer_loading")
			encoded, _ := json.Marshal(fn)
			flat = append(flat, encoded)
		}
	}
	for i, raw := range input {
		f, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if f.string("type") == "function_call" && f.string("namespace") != "" {
			if f.string("name") == "" {
				return nil, clientError(http.StatusBadRequest, "invalid_tool_call", "namespaced calls require a name")
			}
			f.setString("name", m.alias(toolIdentity{f.string("namespace"), f.string("name")}))
			delete(f, "namespace")
			input[i], _ = json.Marshal(f)
		}
	}
	if raw := fields["tool_choice"]; len(raw) > 0 && raw[0] == '{' {
		choice, err := decodeTool(raw)
		if err != nil {
			return nil, err
		}
		if err := m.flattenChoice(choice); err != nil {
			return nil, err
		}
		fields["tool_choice"], _ = json.Marshal(choice)
	}
	if len(m.originals) == 0 && len(flat) == len(tools) {
		return nil, nil
	}
	if fields["tools"] != nil {
		fields["tools"], _ = json.Marshal(flat)
	}
	if input != nil {
		fields["input"], _ = json.Marshal(input)
	}
	return m, nil
}

func (m *namespaceMapping) flattenChoice(choice toolFields) *requestError {
	if choice.string("type") == "allowed_tools" {
		var allowed []toolFields
		if json.Unmarshal(choice["tools"], &allowed) != nil {
			return clientError(http.StatusBadRequest, "invalid_tool_choice", "allowed_tools requires a tools array")
		}
		for _, tool := range allowed {
			if err := m.flattenChoice(tool); err != nil {
				return err
			}
		}
		choice["tools"], _ = json.Marshal(allowed)
	} else if namespace := choice.string("namespace"); namespace != "" {
		id := toolIdentity{namespace, choice.string("name")}
		alias, ok := m.aliases[id]
		if !ok {
			return clientError(http.StatusBadRequest, "invalid_tool_choice", "tool_choice references an undeclared namespace function")
		}
		choice.setString("name", alias)
		delete(choice, "namespace")
	}
	return nil
}

// restoreResponse only visits protocol envelopes, never user arguments, text,
// schemas or tool-result payloads that happen to contain a matching name.
func (m *namespaceMapping) restoreResponse(raw []byte) ([]byte, error) {
	var fields toolFields
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("invalid upstream response JSON")
	}
	if err := m.restoreFields(fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (m *namespaceMapping) restoreFields(fields toolFields) error {
	kind := fields.string("type")
	if kind == "function_call" || strings.HasPrefix(kind, "response.function_call_arguments.") {
		if id, ok := m.originals[fields.string("name")]; ok {
			fields.setString("name", id.Name)
			fields.setString("namespace", id.Namespace)
		}
	}
	for _, key := range []string{"item", "response"} {
		if raw, ok := fields[key]; ok && !bytesNull(raw) {
			restored, err := m.restoreResponse(raw)
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
			restored, err := m.restoreResponse(item)
			if err != nil {
				return err
			}
			output[i] = restored
		}
		fields["output"], _ = json.Marshal(output)
	}
	// SGLang may echo the request's flattened tool definitions in response
	// metadata. Return the original definitions and selector to Codex.
	if fields.string("object") == "response" || fields["output"] != nil {
		if fields["tools"] != nil && m.originalTools != nil {
			fields["tools"] = m.originalTools
		}
		if fields["tool_choice"] != nil && m.originalChoice != nil {
			fields["tool_choice"] = m.originalChoice
		}
	}
	return nil
}

func bytesNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }
