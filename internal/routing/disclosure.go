package routing

import (
	"encoding/json"
	"fmt"
	"strings"
)

const maxDisclosureRounds = 4

// toolDisclosure retains the complete request inventory locally. Only the
// directory, core tools and explicitly loaded namespaces travel to the model.
// Its lifetime is one Responses request; replayed calls rehydrate used groups.
type toolDisclosure struct {
	name            string
	mapping         *namespaceMapping
	all             []json.RawMessage
	groups          map[string][]string
	loaded          map[string]bool
	directory       string
	loader          json.RawMessage
	fields          map[string]json.RawMessage
	rounds          int
	reasoningPolicy string
}

func prepareToolDisclosure(fields map[string]json.RawMessage, mapping *namespaceMapping) *toolDisclosure {
	if mapping == nil {
		return nil
	}
	d := &toolDisclosure{mapping: mapping, fields: fields, groups: make(map[string][]string), loaded: make(map[string]bool)}
	var original []toolFields
	_ = json.Unmarshal(mapping.originalTools, &original)
	var directory []string
	for _, group := range original {
		if group.string("type") != "namespace" {
			continue
		}
		name := group.string("name")
		if _, ok := d.groups[name]; !ok {
			directory = append(directory, name+": "+group.string("description"))
		}
		var children []toolFields
		_ = json.Unmarshal(group["tools"], &children)
		for _, child := range children {
			d.groups[name] = append(d.groups[name], child.string("name"))
		}
	}
	if len(d.groups) == 0 {
		return nil
	}
	d.directory = strings.Join(directory, "\n")
	_ = json.Unmarshal(fields["tools"], &d.all)
	var input []toolFields
	if isJSONArray(fields["input"]) {
		_ = json.Unmarshal(fields["input"], &input)
	}
	for _, item := range input {
		if item.string("type") == "function_call" {
			if id, ok := mapping.originals[item.string("name")]; ok {
				d.loaded[id.Namespace] = true
			}
		}
	}
	// Explicit selectors must remain immediately callable, even on the first
	// turn, and tool_choice=none must not gain an internal tool call.
	var choice toolFields
	_ = json.Unmarshal(fields["tool_choice"], &choice)
	var loadChoice func(toolFields)
	loadChoice = func(c toolFields) {
		if id, ok := mapping.originals[c.string("name")]; ok {
			d.loaded[id.Namespace] = true
		}
		var allowed []toolFields
		_ = json.Unmarshal(c["tools"], &allowed)
		for _, child := range allowed {
			loadChoice(child)
		}
	}
	loadChoice(choice)
	d.name = "router_load_tools"
	for n := 0; mapping.used[d.name]; n++ {
		d.name = fmt.Sprintf("router_load_tools_%d", n)
	}
	d.loader, _ = json.Marshal(map[string]any{
		"type": "function", "name": d.name,
		"description": "Load the function schemas for the namespaces needed for the current task. Call this before using an unloaded namespace; then call the newly available functions. This only reveals schemas, it does not execute tools. Available namespaces:\n" + d.directory,
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{"namespaces": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "minItems": 1, "maxItems": 8}},
			"required": []string{"namespaces"}, "additionalProperties": false,
		}, "strict": false,
	})
	d.updateTools()
	return d
}

func (d *toolDisclosure) updateTools() {
	visible := make([]json.RawMessage, 0, len(d.all))
	for _, raw := range d.all {
		var tool toolFields
		_ = json.Unmarshal(raw, &tool)
		id, namespaced := d.mapping.originals[tool.string("name")]
		if !namespaced || d.loaded[id.Namespace] {
			visible = append(visible, raw)
		}
	}
	var mode string
	_ = json.Unmarshal(d.fields["tool_choice"], &mode)
	if mode != "none" {
		visible = append(visible, d.loader)
	}
	d.fields["tools"], _ = json.Marshal(visible)
}

func (d *toolDisclosure) isLoad(item toolFields) bool {
	return item.string("type") == "function_call" && item.string("name") == d.name
}

// load validates the entire selection before changing visibility. Invalid
// selections produce a local result so the model can correct them, never an
// accidental fallback to exposing every schema.
func (d *toolDisclosure) load(arguments string) string {
	var args struct {
		Namespaces []string `json:"namespaces"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.Namespaces) == 0 || len(args.Namespaces) > 8 {
		return `{"error":"Select between one and eight namespace names from the directory."}`
	}
	for _, name := range args.Namespaces {
		if _, ok := d.groups[name]; !ok {
			return `{"error":"Unknown namespace; use exact names from the directory."}`
		}
	}
	var names []string
	for _, name := range args.Namespaces {
		d.loaded[name] = true
		for _, fn := range d.groups[name] {
			names = append(names, d.mapping.aliases[toolIdentity{name, fn}])
		}
	}
	d.updateTools()
	result, _ := json.Marshal(map[string]any{"available_functions": names, "instruction": "The selected function schemas are now in tools. Call the needed function; loading does not execute it."})
	return string(result)
}

// continueAfterLoad appends only the internal function call and its local
// result. No real tool runs here; a response with real calls returns to Codex.
func (d *toolDisclosure) continueAfterLoad(output []json.RawMessage, response toolFields) error {
	if d.rounds >= maxDisclosureRounds {
		return fmt.Errorf("tool disclosure exceeded round limit")
	}
	d.rounds++
	if raw, ok := d.fields["max_output_tokens"]; ok && !bytesNull(raw) {
		var remaining int64
		var usage struct {
			Output int64 `json:"output_tokens"`
		}
		if err := json.Unmarshal(raw, &remaining); err != nil {
			return err
		}
		if json.Unmarshal(response["usage"], &usage) != nil || usage.Output <= 0 {
			return fmt.Errorf("tool disclosure cannot account for max_output_tokens")
		}
		remaining -= usage.Output
		if remaining <= 0 {
			return fmt.Errorf("tool disclosure exhausted max_output_tokens")
		}
		d.fields["max_output_tokens"], _ = json.Marshal(remaining)
	}
	var input []json.RawMessage
	if raw := d.fields["input"]; isJSONArray(raw) {
		if err := json.Unmarshal(raw, &input); err != nil {
			return err
		}
	} else if len(raw) > 0 && !bytesNull(raw) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		message, _ := json.Marshal(map[string]any{"role": "user", "content": text})
		input = append(input, message)
	}
	var results []json.RawMessage
	for _, raw := range output {
		var item toolFields
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		switch item.string("type") {
		case "reasoning":
			if d.reasoningPolicy == "reject" {
				return fmt.Errorf("tool disclosure reasoning replay rejected by route policy")
			}
			if d.reasoningPolicy != "keep" {
				continue
			}
		case "function_call":
			if !d.isLoad(item) {
				return fmt.Errorf("real tool call cannot execute inside disclosure")
			}
			if item.string("call_id") == "" {
				return fmt.Errorf("tool disclosure call has no call_id")
			}
			result, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": item.string("call_id"), "output": d.load(item.string("arguments"))})
			results = append(results, result)
		}
		input = append(input, raw)
	}
	input = append(input, results...)
	d.fields["input"], _ = json.Marshal(input)
	return nil
}
