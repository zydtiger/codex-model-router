package routing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const customGrammar = "start: WORD+"

const customTools = `[
 {"type":"function","name":"exec_command","parameters":{"type":"object"}},
 {"type":"custom","name":"apply_patch","description":"Apply a file patch",
  "format":{"type":"grammar","syntax":"lark","definition":"` + customGrammar + `"}},
 {"type":"namespace","name":"mcp__node_repl","description":"JS runtime","tools":[
  {"type":"custom","name":"run","format":{"type":"text"}}
 ]}
]`

func customRequest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	fields, err := decodeJSON([]byte(`{"model":"routed-model","tools":` + customTools + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

// customRouteConfig builds a route with the requested tools block and optional
// extra top-level configuration text.
func customRouteConfig(t *testing.T, baseURL, tools, extraTop string) string {
	t.Helper()
	if tools == "" {
		tools = `"tools": {"custom_adapter": "custom_to_functions"}`
	}
	return fmt.Sprintf(`{
      %s
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],%s}]
    }`, extraTop, baseURL, tools)
}

// decodeRecordedRequest decodes the body newUpstream already consumed and
// recorded for the request currently being served.
func decodeRecordedRequest(t *testing.T, up *upstream) map[string]json.RawMessage {
	t.Helper()
	requests := up.requests()
	if len(requests) == 0 {
		t.Fatal("no recorded upstream request")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(requests[len(requests)-1].Body, &fields); err != nil {
		t.Fatalf("recorded upstream request is not JSON: %v", err)
	}
	return fields
}

func TestCustomPreparationWrapsDeclarationsAndHistory(t *testing.T) {
	fields := customRequest(t)
	fields["input"] = json.RawMessage(`[
	 {"type":"custom_tool_call","id":"cc_1","call_id":"call_1","name":"apply_patch","input":"*** AddFile"},
	 {"type":"custom_tool_call_output","id":"co_1","call_id":"call_1","output":"done"},
	 {"type":"custom_tool_call","id":"cc_2","call_id":"call_2","namespace":"mcp__node_repl","name":"run","input":"1+1"}
	]`)
	fields["tool_choice"] = json.RawMessage(`{"type":"allowed_tools","mode":"auto","tools":[{"type":"custom","name":"apply_patch"},{"type":"custom","namespace":"mcp__node_repl","name":"run"}]}`)
	m, err := prepareCustomTools(fields)
	if err != nil {
		t.Fatal(err)
	}
	var tools []toolFields
	_ = json.Unmarshal(fields["tools"], &tools)
	if len(tools) != 3 {
		t.Fatalf("tool count changed: %s", fields["tools"])
	}
	wrapped := tools[1]
	if wrapped.string("type") != "function" || wrapped.string("name") != "apply_patch" {
		t.Fatalf("wrapped declaration: %s", fields["tools"])
	}
	if !strings.Contains(wrapped.string("description"), "Apply a file patch") ||
		!strings.Contains(wrapped.string("description"), customGrammar) ||
		!strings.Contains(wrapped.string("description"), "advisory") {
		t.Fatalf("grammar guidance lost: %s", wrapped.string("description"))
	}
	var parameters struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Additional bool     `json:"additionalProperties"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(wrapped["parameters"], &parameters); err != nil {
		t.Fatal(err)
	}
	if parameters.Type != "object" || len(parameters.Required) != 1 || parameters.Required[0] != "input" ||
		parameters.Additional || len(parameters.Properties) != 1 || parameters.Properties["input"].Type != "string" {
		t.Fatalf("wrapped parameters: %s", wrapped["parameters"])
	}
	var children []toolFields
	if err := json.Unmarshal(tools[2]["tools"], &children); err != nil || len(children) != 1 {
		t.Fatalf("namespace children: %s", tools[2]["tools"])
	}
	if children[0].string("type") != "function" || children[0].string("name") != "run" {
		t.Fatalf("namespace child not wrapped: %s", tools[2]["tools"])
	}
	if !m.identities[toolIdentity{"", "apply_patch"}] || !m.identities[toolIdentity{"mcp__node_repl", "run"}] {
		t.Fatal("identities not registered")
	}

	var input []toolFields
	_ = json.Unmarshal(fields["input"], &input)
	if input[0].string("type") != "function_call" || input[0].string("name") != "apply_patch" ||
		input[0].string("arguments") != `{"input":"*** AddFile"}` || input[0].string("call_id") != "call_1" || input[0].string("id") != "cc_1" {
		t.Fatalf("history call: %s", fields["input"])
	}
	if input[1].string("type") != "function_call_output" || input[1].string("output") != "done" {
		t.Fatalf("history output: %s", fields["input"])
	}
	if input[2].string("namespace") != "mcp__node_repl" || input[2].string("name") != "run" ||
		input[2].string("arguments") != `{"input":"1+1"}` {
		t.Fatalf("namespaced history lost its identity: %s", fields["input"])
	}

	var choice toolFields
	_ = json.Unmarshal(fields["tool_choice"], &choice)
	var allowed []toolFields
	_ = json.Unmarshal(choice["tools"], &allowed)
	if choice.string("type") != "allowed_tools" || allowed[0].string("type") != "function" ||
		allowed[1].string("type") != "function" || allowed[1].string("namespace") != "mcp__node_repl" {
		t.Fatalf("converted choice: %s", fields["tool_choice"])
	}
}

func TestCustomPreparationRejectsInvalidInput(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[{"type":"custom","name":"a"},{"type":"custom","name":"a"}]}`,
		`{"tools":[{"type":"custom","name":"a","format":{"type":"regexp"}}]}`,
		`{"tools":[{"type":"custom","name":"a","format":{"type":"grammar","syntax":"lark"}}]}`,
		`{"tools":[{"type":"custom","format":{"type":"text"}}]}`,
		`{"tools":[{"type":"custom","name":"a"}],"tool_choice":{"type":"custom","name":"missing"}}`,
		`{"tools":[{"type":"custom","name":"a"}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"custom","name":"nope"}]}}`,
	} {
		fields, _ := decodeJSON([]byte(raw))
		if _, err := prepareCustomTools(fields); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

// A custom tool that reuses a callable identity would travel upstream as the
// same function name as an ordinary tool, so responses could not be attributed
// reliably. Both declaration orders are refused, top-level and namespaced.
func TestCustomPreparationRejectsCallableIdentityCollisions(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[{"type":"custom","name":"dup"},{"type":"function","name":"dup","parameters":{"type":"object"}}]}`,
		`{"tools":[{"type":"function","name":"dup","parameters":{"type":"object"}},{"type":"custom","name":"dup"}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"custom","name":"c"},{"type":"function","name":"c","parameters":{"type":"object"}}]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"c","parameters":{"type":"object"}},{"type":"custom","name":"c"}]}]}`,
	} {
		fields, _ := decodeJSON([]byte(raw))
		if _, err := prepareCustomTools(fields); err == nil {
			t.Fatalf("accepted colliding identities: %s", raw)
		}
	}
	// Distinct identities, including the same name in different namespaces,
	// keep working.
	fields, _ := decodeJSON([]byte(`{"tools":[
	 {"type":"function","name":"dup","parameters":{"type":"object"}},
	 {"type":"custom","name":"other"},
	 {"type":"namespace","name":"n","tools":[{"type":"custom","name":"dup"}]}
	]}`))
	if _, err := prepareCustomTools(fields); err != nil {
		t.Fatalf("distinct identities refused: %v", err)
	}
}

// The default policy still converts custom history without the adapter; that
// path must preserve namespaces now that a flattening pass can consume them.
func TestLegacyCustomHistoryPreservesNamespace(t *testing.T) {
	converted, err := translateCustomTool(json.RawMessage(
		`{"type":"custom_tool_call","call_id":"c1","namespace":"mcp__node_repl","name":"run","input":"x"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	var item toolFields
	_ = json.Unmarshal(converted, &item)
	if item.string("type") != "function_call" || item.string("namespace") != "mcp__node_repl" ||
		item.string("name") != "run" || item.string("arguments") != `{"input":"x"}` {
		t.Fatalf("legacy conversion: %s", converted)
	}
}

// bridgedTool finds the function declaration the custom adapter derived, by its
// single required input property, so mock upstreams can call it by whatever
// name reached the request (plain or namespace-flattened).
func bridgedTool(t *testing.T, fields map[string]json.RawMessage) (string, bool) {
	t.Helper()
	var tools []toolFields
	if err := json.Unmarshal(fields["tools"], &tools); err != nil {
		return "", false
	}
	for _, tool := range tools {
		if tool.string("type") != "function" {
			continue
		}
		if strings.Contains(string(tool["parameters"]), `"input"`) && strings.Contains(string(tool["parameters"]), `"required":["input"]`) {
			return tool.string("name"), true
		}
	}
	return "", false
}

// bridgedArguments is the escaped wrapper JSON: the newline and the astral
// emoji travel as JSON escapes so the split below can cut inside them.
const bridgedArguments = `{"input":"*** AddFile\nhello é \ud83d\ude00 done"}`

// decodedBridgedInput is what the client must see after unescaping.
const decodedBridgedInput = "*** AddFile\nhello é 😀 done"

// bridgedCall returns the upstream function_call item for the bridged tool.
func bridgedCall(t *testing.T, fields map[string]json.RawMessage) toolFields {
	t.Helper()
	name, ok := bridgedTool(t, fields)
	if !ok {
		t.Fatalf("no bridged custom tool in upstream request: %s", fields["tools"])
	}
	item := toolFields{}
	item.setString("type", "function_call")
	item.setString("id", "fc_1")
	item.setString("call_id", "call_1")
	item.setString("name", name)
	item.setString("arguments", bridgedArguments)
	item.setString("status", "completed")
	return item
}

// scanEvents parses every data event out of an SSE response body.
func scanEvents(t *testing.T, body string) []toolFields {
	t.Helper()
	var events []toolFields
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event toolFields
		if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func eventTypes(events []toolFields) []string {
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.string("type")
	}
	return types
}

func countEventType(events []toolFields, kind string) int {
	count := 0
	for _, event := range events {
		if event.string("type") == kind {
			count++
		}
	}
	return count
}

// assertContiguousSequence verifies the renumbered stream ordering.
func assertContiguousSequence(t *testing.T, events []toolFields) {
	t.Helper()
	last := int64(-1)
	for _, event := range events {
		var seq int64
		if err := json.Unmarshal(event["sequence_number"], &seq); err != nil {
			t.Fatalf("event without sequence number: %s", event["type"])
		}
		if seq != last+1 {
			t.Fatalf("noncontiguous sequence %d after %d", seq, last)
		}
		last = seq
	}
}

// mockInterleavedCustomStream emits an interleaved custom and ordinary call,
// splitting the custom wrapper inside the emoji's surrogate escape.
func mockInterleavedCustomStream(w http.ResponseWriter, customName string) {
	custom := toolFields{}
	custom.setString("type", "function_call")
	custom.setString("id", "fc_1")
	custom.setString("call_id", "call_1")
	custom.setString("name", customName)
	custom.setString("arguments", bridgedArguments)
	custom.setString("status", "completed")
	ordinary := toolFields{}
	ordinary.setString("type", "function_call")
	ordinary.setString("id", "fc_2")
	ordinary.setString("call_id", "call_2")
	ordinary.setString("name", "exec_command")
	ordinary.setString("arguments", `{"cmd":"true"}`)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	seq := 0
	emit := func(kind string, payload map[string]any) {
		payload["type"] = kind
		payload["sequence_number"] = seq
		seq++
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	emit("response.created", map[string]any{"response": map[string]any{"id": "resp_1", "object": "response", "output": []any{}, "status": "in_progress"}})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": customName}})
	emit("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "exec_command"}})
	split := strings.Index(bridgedArguments, `\ud83d`) + len(`\ud8`)
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc_1", "delta": bridgedArguments[:split]})
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_2", "delta": `{"cmd":"tr`})
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc_1", "delta": bridgedArguments[split:]})
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_2", "delta": `ue"}`})
	emit("response.function_call_arguments.done", map[string]any{"output_index": 1, "item_id": "fc_2", "arguments": `{"cmd":"true"}`})
	emit("response.output_item.done", map[string]any{"output_index": 1, "item": ordinary})
	emit("response.function_call_arguments.done", map[string]any{"output_index": 0, "item_id": "fc_1", "arguments": bridgedArguments})
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": custom})
	emit("response.completed", map[string]any{"response": map[string]any{"object": "response", "status": "completed", "output": []toolFields{custom, ordinary}, "tools": []any{}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestCustomHTTPRoundTrip(t *testing.T) {
	configs := []struct {
		name  string
		tools string
	}{
		{"custom only", ""},
		{"custom with namespace all", `"tools": {"custom_adapter": "custom_to_functions", "namespace_adapter": "namespace_to_functions"}`},
	}
	for _, routeConfig := range configs {
		for _, stream := range []bool{false, true} {
			t.Run(routeConfig.name+", stream="+fmt.Sprint(stream), func(t *testing.T) {
				var up *upstream
				up = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					fields := decodeRecordedRequest(t, up)
					if strings.Contains(string(fields["tools"]), `"type":"custom"`) {
						t.Error("custom declaration reached the upstream")
					}
					call := bridgedCall(t, fields)
					ordinary := toolFields{}
					ordinary.setString("type", "function_call")
					ordinary.setString("id", "fc_2")
					ordinary.setString("call_id", "call_2")
					ordinary.setString("name", "exec_command")
					ordinary.setString("arguments", `{"cmd":"true"}`)
					ordinaryRaw, _ := json.Marshal(ordinary)
					callRaw, _ := json.Marshal(call)
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprintf(w, `{"object":"response","status":"completed","output":[%s,%s],"tools":[]}`, callRaw, ordinaryRaw)
						return
					}
					name, _ := bridgedTool(t, fields)
					mockInterleavedCustomStream(w, name)
				})
				router, _ := newRouter(t, customRouteConfig(t, up.server.URL, routeConfig.tools, ""), nil)
				fields := customRequest(t)
				fields["input"] = json.RawMessage(`[
				 {"type":"custom_tool_call","call_id":"call_0","name":"apply_patch","input":"old"},
				 {"type":"custom_tool_call_output","call_id":"call_0","output":"ok"}]`)
				fields["tool_choice"] = json.RawMessage(`{"type":"custom","name":"apply_patch"}`)
				fields["stream"], _ = json.Marshal(stream)
				body, _ := json.Marshal(fields)
				result := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
				if result.Code != 200 {
					t.Fatalf("response: %d %s", result.Code, result.Body.String())
				}
				var sent map[string]json.RawMessage
				_ = json.Unmarshal(up.requests()[0].Body, &sent)
				var sentChoice toolFields
				_ = json.Unmarshal(sent["tool_choice"], &sentChoice)
				if sentChoice.string("type") != "function" || sentChoice.string("name") != "apply_patch" {
					t.Fatalf("upstream choice: %s", sent["tool_choice"])
				}
				if !strings.Contains(string(sent["input"]), `"type":"function_call"`) || strings.Contains(string(sent["input"]), `custom_tool_call`) {
					t.Fatalf("upstream history: %s", sent["input"])
				}

				assertCustomOutput := func(output []toolFields) {
					t.Helper()
					if len(output) != 2 {
						t.Fatalf("missing output: %v", output)
					}
					if output[0].string("type") != "custom_tool_call" || output[0].string("name") != "apply_patch" ||
						output[0].string("input") != decodedBridgedInput || output[0].string("call_id") != "call_1" {
						t.Fatalf("restored custom call: %s", output[0])
					}
					if output[1].string("type") != "function_call" || output[1].string("name") != "exec_command" {
						t.Fatalf("ordinary call changed: %s", output[1])
					}
				}
				var restoredTools json.RawMessage
				var customOutputItem json.RawMessage
				if stream {
					events := scanEvents(t, result.Body.String())
					assertContiguousSequence(t, events)
					want := []string{
						"response.created",
						"response.output_item.added",             // custom
						"response.output_item.added",             // ordinary
						"response.function_call_arguments.delta", // ordinary part 1
						"response.function_call_arguments.delta", // ordinary part 2
						"response.function_call_arguments.done",  // ordinary
						"response.output_item.done",              // ordinary
						"response.custom_tool_call_input.delta",  // custom, after completion
						"response.custom_tool_call_input.done",   // custom
						"response.output_item.done",              // custom
						"response.completed",
					}
					if got := eventTypes(events); !equalStrings(got, want) {
						t.Fatalf("event order:\n got  %v\n want %v\nbody:\n%s", got, want, result.Body.String())
					}
					for _, event := range events {
						if event.string("type") == "response.function_call_arguments.delta" && event.string("item_id") == "fc_1" {
							t.Fatal("wrapper delta for the custom call leaked")
						}
					}
					var added, done toolFields
					_ = json.Unmarshal(events[1]["item"], &added)
					_ = json.Unmarshal(events[9]["item"], &done)
					if added.string("type") != "custom_tool_call" || done.string("type") != "custom_tool_call" ||
						done.string("input") != decodedBridgedInput {
						t.Fatalf("custom items: added %s done %s", events[1]["item"], events[9]["item"])
					}
					var response toolFields
					_ = json.Unmarshal(events[len(events)-1]["response"], &response)
					var output []toolFields
					_ = json.Unmarshal(response["output"], &output)
					assertCustomOutput(output)
					restoredTools = response["tools"]
					for _, item := range output {
						if item.string("type") == "custom_tool_call" {
							raw, _ := json.Marshal(item)
							customOutputItem = raw
						}
					}
					if !strings.Contains(result.Body.String(), "event: response.custom_tool_call_input.done") {
						t.Fatal("event names not synchronized")
					}
					if !strings.Contains(result.Body.String(), "data: [DONE]\n\n") {
						t.Fatal("terminal marker lost")
					}
				} else {
					var response toolFields
					_ = json.Unmarshal(result.Body.Bytes(), &response)
					var output []toolFields
					_ = json.Unmarshal(response["output"], &output)
					assertCustomOutput(output)
					restoredTools = response["tools"]
					for _, item := range output {
						if item.string("type") == "custom_tool_call" {
							raw, _ := json.Marshal(item)
							customOutputItem = raw
						}
					}
				}
				if !bytes.Contains(compactJSON(t, restoredTools), compactJSON(t, []byte(customTools))) {
					t.Fatalf("original tools not restored: %s", restoredTools)
				}

				// Replay exactly the restored custom call and its result, as
				// Codex does after executing the tool.
				fields["input"] = json.RawMessage("[" + string(customOutputItem) + `,{"type":"custom_tool_call_output","call_id":"call_1","output":"patched"}]`)
				body, _ = json.Marshal(fields)
				result = doRequest(router, http.MethodPost, "/v1/responses", body, nil)
				if result.Code != 200 {
					t.Fatalf("replay response: %d %s", result.Code, result.Body.String())
				}
				var replay struct {
					Input []toolFields `json:"input"`
				}
				if err := json.Unmarshal(up.requests()[1].Body, &replay); err != nil {
					t.Fatal(err)
				}
				// The history conversion re-marshals the wrapper, which spells
				// the emoji as literal UTF-8 rather than a surrogate escape.
				wantArguments, _ := json.Marshal(map[string]string{"input": decodedBridgedInput})
				if len(replay.Input) != 2 || replay.Input[0].string("type") != "function_call" ||
					replay.Input[0].string("name") != "apply_patch" ||
					replay.Input[0].string("arguments") != string(wantArguments) ||
					replay.Input[0].string("call_id") != "call_1" {
					t.Fatalf("replayed custom call: %s", up.requests()[1].Body)
				}
				if replay.Input[1].string("type") != "function_call_output" || replay.Input[1].string("output") != "patched" {
					t.Fatalf("replayed custom output: %s", up.requests()[1].Body)
				}
			})
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// streamServer runs one SSE exchange through the custom-only adapter and
// returns the client-visible body.
func streamCustomExchange(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	up := newUpstream(t, handler)
	router, _ := newRouter(t, customRouteConfig(t, up.server.URL, "", ""), nil)
	body, _ := json.Marshal(customRequest(t))
	result := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	return result.Body.String()
}

func TestCustomStreamFallbackOrdering(t *testing.T) {
	customItemRaw := func() string {
		item := toolFields{}
		item.setString("type", "function_call")
		item.setString("id", "fc_1")
		item.setString("call_id", "call_1")
		item.setString("name", "apply_patch")
		item.setString("arguments", bridgedArguments)
		raw, _ := json.Marshal(item)
		return string(raw)
	}

	t.Run("item done fallback emits input before the item", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":%s}\n\n", customItemRaw())
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"object\":\"response\",\"output\":[%s]}}\n\n", customItemRaw())
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		events := scanEvents(t, body)
		assertContiguousSequence(t, events)
		want := []string{
			"response.output_item.added",
			"response.custom_tool_call_input.delta",
			"response.custom_tool_call_input.done",
			"response.output_item.done",
			"response.completed",
		}
		if got := eventTypes(events); !equalStrings(got, want) {
			t.Fatalf("event order:\n got  %v\n want %v\n%s", got, want, body)
		}
		if events[2].string("input") != decodedBridgedInput || events[2].string("item_id") != "fc_1" {
			t.Fatalf("input done event: %s", body)
		}
	})

	t.Run("completed-only fallback without added", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"object\":\"response\",\"output\":[]}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"object\":\"response\",\"output\":[%s]}}\n\n", customItemRaw())
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		events := scanEvents(t, body)
		assertContiguousSequence(t, events)
		want := []string{
			"response.created",
			"response.custom_tool_call_input.delta",
			"response.custom_tool_call_input.done",
			"response.completed",
		}
		if got := eventTypes(events); !equalStrings(got, want) {
			t.Fatalf("event order:\n got  %v\n want %v\n%s", got, want, body)
		}
		if events[2].string("input") != decodedBridgedInput {
			t.Fatalf("fallback input: %s", body)
		}
		var response toolFields
		_ = json.Unmarshal(events[3]["response"], &response)
		var output []toolFields
		_ = json.Unmarshal(response["output"], &output)
		if len(output) != 1 || output[0].string("type") != "custom_tool_call" || output[0].string("input") != decodedBridgedInput {
			t.Fatalf("completed output item: %s", response["output"])
		}
	})

	t.Run("completed fallback after added and deltas", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{\"}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"object\":\"response\",\"output\":[%s]}}\n\n", customItemRaw())
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		events := scanEvents(t, body)
		assertContiguousSequence(t, events)
		want := []string{
			"response.output_item.added",
			"response.custom_tool_call_input.delta",
			"response.custom_tool_call_input.done",
			"response.completed",
		}
		if got := eventTypes(events); !equalStrings(got, want) {
			t.Fatalf("event order:\n got  %v\n want %v\n%s", got, want, body)
		}
		if events[1].string("item_id") != "fc_1" {
			t.Fatalf("fallback lost item identity: %s", body)
		}
	})

	t.Run("completed payload contradicting emitted input fails", func(t *testing.T) {
		contradiction := toolFields{}
		contradiction.setString("type", "function_call")
		contradiction.setString("id", "fc_1")
		contradiction.setString("call_id", "call_1")
		contradiction.setString("name", "apply_patch")
		contradiction.setString("arguments", `{"input":"different"}`)
		contradicting, _ := json.Marshal(contradiction)
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":%q}\n\n", bridgedArguments)
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"object\":\"response\",\"output\":[%s]}}\n\n", contradicting)
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		if strings.Contains(body, "response.completed") {
			t.Fatalf("contradictory completion was forwarded:\n%s", body)
		}
	})
}

func TestCustomStreamLateAndDuplicateEvents(t *testing.T) {
	added := func(w http.ResponseWriter, seq int) {
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":%d,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n", seq)
	}
	deltasDone := func(w http.ResponseWriter, seq int) {
		fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":%d,\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":%q}\n\n", seq, bridgedArguments)
		fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":%d,\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":%q}\n\n", seq+1, bridgedArguments)
	}

	t.Run("duplicate consistent done is suppressed", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			added(w, 0)
			deltasDone(w, 1)
			// A repeated done with the same input must not re-emit it.
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":3,\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":%q}\n\n", bridgedArguments)
			item := toolFields{}
			item.setString("type", "function_call")
			item.setString("id", "fc_1")
			item.setString("call_id", "call_1")
			item.setString("name", "apply_patch")
			item.setString("arguments", bridgedArguments)
			raw, _ := json.Marshal(item)
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":4,\"output_index\":0,\"item\":%s}\n\n", raw)
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"object\":\"response\",\"output\":[%s]}}\n\n", raw)
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		if countEventType(scanEvents(t, body), "response.custom_tool_call_input.done") != 1 {
			t.Fatalf("duplicate done re-emitted input:\n%s", body)
		}
		if !strings.Contains(body, "response.completed") {
			t.Fatalf("consistent duplicate failed the stream:\n%s", body)
		}
	})

	t.Run("contradictory duplicate done fails", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			added(w, 0)
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":%q}\n\n", bridgedArguments)
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":2,\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":\"{\\\"input\\\":\\\"other\\\"}\"}\n\n")
		})
		if countEventType(scanEvents(t, body), "response.custom_tool_call_input.done") != 1 || strings.Contains(body, "other") || strings.Contains(body, "response.completed") {
			t.Fatalf("contradictory done produced duplicate or conflicting input:\n%s", body)
		}
	})

	t.Run("late delta after completion fails without leaking", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			added(w, 0)
			deltasDone(w, 1)
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":3,\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"leak\"}\n\n")
		})
		if strings.Contains(body, "leak") || strings.Contains(body, "response.completed") {
			t.Fatalf("late delta leaked or stream reported success:\n%s", body)
		}
	})

	t.Run("duplicate registration fails", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			added(w, 0)
			// Same output index again: the stream's item identities are
			// ambiguous, so nothing may be associated by guessing.
			added(w, 1)
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		if strings.Contains(body, "response.completed") {
			t.Fatalf("ambiguous registration was tolerated:\n%s", body)
		}
	})
}

func TestCustomRejections(t *testing.T) {
	for name, body := range map[string]string{
		"malformed wrapper":    `{"object":"response","output":[{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"{\"input\":1}"}]}`,
		"missing input":        `{"object":"response","output":[{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"{}"}]}`,
		"null input":           `{"object":"response","output":[{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"{\"input\":null}"}]}`,
		"extra wrapper key":    `{"object":"response","output":[{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"{\"input\":\"a\",\"x\":1}"}]}`,
		"non-object arguments": `{"object":"response","output":[{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"\"raw\""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			})
			router, _ := newRouter(t, customRouteConfig(t, up.server.URL, "", ""), nil)
			request, _ := json.Marshal(customRequest(t))
			result := doRequest(router, http.MethodPost, "/v1/responses", request, nil)
			if result.Code != 502 {
				t.Fatalf("status = %d, want 502: %s", result.Code, result.Body.String())
			}
			if strings.Contains(result.Body.String(), `"custom_tool_call"`) {
				t.Fatal("malformed call was emitted as custom traffic")
			}
		})
	}

	t.Run("incomplete stream fails", func(t *testing.T) {
		body := streamCustomExchange(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{\"}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		if strings.Contains(body, "response.completed") || strings.Contains(body, "custom_tool_call_input.done") {
			t.Fatalf("incomplete custom call was reported complete:\n%s", body)
		}
	})

	t.Run("buffered arguments are bounded", func(t *testing.T) {
		up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\"}}\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":%q}\n\n", strings.Repeat("x", 4096))
		})
		router, _ := newRouter(t, customRouteConfig(t, up.server.URL, "", `"max_request_bytes": 2048,`), nil)
		request, _ := json.Marshal(customRequest(t))
		result := doRequest(router, http.MethodPost, "/v1/responses", request, nil)
		if strings.Contains(result.Body.String(), "response.completed") {
			t.Fatalf("oversized buffered arguments were not refused:\n%s", result.Body.String())
		}
	})
}

// The disclosure loader must keep working with bridged custom tools, and the
// custom restoration must survive the disclosure layer's synthesized events.
func TestCustomWithOnDemandDisclosure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+fmt.Sprint(stream), func(t *testing.T) {
			round := 0
			var up *upstream
			up = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				round++
				fields := decodeRecordedRequest(t, up)
				if strings.Contains(string(fields["tools"]), `"type":"custom"`) {
					t.Error("custom declaration reached the upstream")
				}
				if round == 1 {
					mockDisclosureResponse(w, stream, round, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["mcp__node_repl"]}`, "load")})
					return
				}
				// The wrapped namespace child is now visible under its
				// flattened alias.
				name := "mcp__node_repl__run"
				var loaded []toolFields
				if err := json.Unmarshal(fields["tools"], &loaded); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, tool := range loaded {
					found = found || tool.string("name") == name
				}
				if !found {
					t.Fatalf("bridged custom tool not visible after load: %s", fields["tools"])
				}
				call := toolFields{}
				call.setString("type", "function_call")
				call.setString("id", "fc_9")
				call.setString("call_id", "call_9")
				call.setString("name", name)
				call.setString("arguments", `{"input":"console.log(1)"}`)
				raw, _ := json.Marshal(call)
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"object":"response","status":"completed","output":[%s],"tools":[]}`, raw)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				seq := 0
				emit := func(kind string, payload map[string]any) {
					payload["type"] = kind
					payload["sequence_number"] = seq
					seq++
					data, _ := json.Marshal(payload)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
				}
				emit("response.created", map[string]any{"response": map[string]any{"id": "resp_2", "object": "response", "output": []any{}, "status": "in_progress"}})
				emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_9", "call_id": "call_9", "name": name}})
				emit("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc_9", "delta": `{"input":"console.`})
				emit("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc_9", "delta": `log(1)"}`})
				emit("response.function_call_arguments.done", map[string]any{"output_index": 0, "item_id": "fc_9", "arguments": `{"input":"console.log(1)"}`})
				emit("response.output_item.done", map[string]any{"output_index": 0, "item": call})
				emit("response.completed", map[string]any{"response": map[string]any{"object": "response", "status": "completed", "output": []toolFields{call}, "tools": []any{}}})
				fmt.Fprint(w, "data: [DONE]\n\n")
			})
			router, _ := newRouter(t, customRouteConfig(t, up.server.URL,
				`"tools": {"custom_adapter": "custom_to_functions", "namespace_adapter": "namespace_to_functions", "schema_loading": "on_demand"}`, ""), nil)
			fields := customRequest(t)
			fields["stream"], _ = json.Marshal(stream)
			body, _ := json.Marshal(fields)
			result := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
			if result.Code != 200 || round != 2 {
				t.Fatalf("status %d, rounds %d: %s", result.Code, round, result.Body.String())
			}
			var response toolFields
			if stream {
				events := scanEvents(t, result.Body.String())
				assertContiguousSequence(t, events)
				if strings.Contains(result.Body.String(), "router_load_tools") || strings.Contains(result.Body.String(), `"call_id":"load"`) {
					t.Fatal("internal loader call leaked to Codex")
				}
				want := []string{
					"response.created",
					"response.output_item.added",
					"response.custom_tool_call_input.delta",
					"response.custom_tool_call_input.done",
					"response.output_item.done",
					"response.completed",
				}
				if got := eventTypes(events); !equalStrings(got, want) {
					t.Fatalf("event order:\n got  %v\n want %v\n%s", got, want, result.Body.String())
				}
				_ = json.Unmarshal(events[len(events)-1]["response"], &response)
			} else {
				_ = json.Unmarshal(result.Body.Bytes(), &response)
			}
			var output []toolFields
			_ = json.Unmarshal(response["output"], &output)
			if len(output) != 1 || output[0].string("type") != "custom_tool_call" || output[0].string("namespace") != "mcp__node_repl" ||
				output[0].string("name") != "run" || output[0].string("input") != "console.log(1)" {
				t.Fatalf("restored namespaced custom call: %s", response["output"])
			}
			if !bytes.Contains(compactJSON(t, response["tools"]), []byte(`"type":"custom"`)) {
				t.Fatalf("original custom declarations not restored: %s", response["tools"])
			}
		})
	}
}

// Compaction summary passes stay tool-free while custom history survives as
// function traffic for the summarizer.
func TestCustomCompactionStaysToolFree(t *testing.T) {
	var up *upstream
	up = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		fields := decodeRecordedRequest(t, up)
		if string(fields["tools"]) != "[]" || string(fields["tool_choice"]) != `"none"` {
			t.Fatalf("summary pass tools: %s %s", fields["tools"], fields["tool_choice"])
		}
		if !strings.Contains(string(fields["input"]), `"type":"function_call"`) || strings.Contains(string(fields["input"]), "custom_tool_call") {
			t.Fatalf("custom history did not survive as function traffic: %s", fields["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary of the custom tool session"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	})
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],
        "compaction": {"adapter": "text_summary"},
        "tools": {"custom_adapter": "custom_to_functions"}}]
    }`, up.server.URL), nil)
	fields := customRequest(t)
	fields["input"] = json.RawMessage(`[
	 {"type":"custom_tool_call","call_id":"call_0","name":"apply_patch","input":"do work"},
	 {"type":"custom_tool_call_output","call_id":"call_0","output":"worked"},
	 {"type":"compaction_trigger"}]`)
	body, _ := json.Marshal(fields)
	result := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if result.Code != 200 {
		t.Fatalf("status %d: %s", result.Code, result.Body.String())
	}
	if !strings.Contains(result.Body.String(), `"type":"compaction"`) {
		t.Fatalf("checkpoint missing: %s", result.Body.String())
	}
}

func TestCustomNativePassThroughUnchanged(t *testing.T) {
	response := `{"output":[{"type":"custom_tool_call","name":"apply_patch","input":"x"}]}`
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, up.server.URL, up.server.URL, up.server.URL), nil)
	body := []byte(`{"model":"gpt-5","tools":` + customTools + `}`)
	result := doRequest(router, http.MethodPost, "/v1/responses", body, map[string]string{"Authorization": "Bearer test"})
	if result.Body.String() != response {
		t.Fatal("native custom tool response changed")
	}
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(up.requests()[0].Body, &sent)
	if !bytes.Contains(compactJSON(t, sent["tools"]), compactJSON(t, []byte(customTools))) {
		t.Fatal("native custom tool declarations changed")
	}
}

func TestCustomStreamCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"name\":\"apply_patch\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	})
	router, _ := newRouter(t, customRouteConfig(t, up.server.URL, "", ""), nil)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(customRequest(t))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			break
		}
	}
	if scanner.Err() != nil || !strings.Contains(scanner.Text(), `"type":"custom_tool_call"`) {
		t.Fatalf("first event did not arrive as custom traffic: %s %v", scanner.Text(), scanner.Err())
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not cancelled")
	}
}

// The adapter must not touch function calls whose identity is not a declared
// custom tool, even when their arguments happen to use the input wrapper.
func TestCustomOrdinaryCallsWithWrapperArgumentsUnchanged(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"response","output":[{"type":"function_call","name":"exec_command","call_id":"c","arguments":"{\"input\":\"not custom\"}"}]}`)
	})
	router, _ := newRouter(t, customRouteConfig(t, up.server.URL, "", ""), nil)
	body, _ := json.Marshal(customRequest(t))
	result := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if !strings.Contains(result.Body.String(), `"type":"function_call"`) || strings.Contains(result.Body.String(), "custom_tool_call") {
		t.Fatalf("ordinary call was reinterpreted:\n%s", result.Body.String())
	}
}

// Exercise stream limits independently of request-body limits, so a rejected
// client request cannot accidentally make the upstream-state tests pass.
func readCustomStream(t *testing.T, frames string, limit int64) (string, error) {
	t.Helper()
	fields, _ := decodeJSON([]byte(`{"tools":[{"type":"custom","name":"record"}]}`))
	mapping, err := prepareCustomTools(fields)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}
	if err := adaptCustomResponse(resp, mapping, limit); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	return string(body), readErr
}

func TestCustomRetainedStateLimits(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint("empty=", empty), func(t *testing.T) {
			var frames strings.Builder
			for i := 0; i < 20; i++ {
				fmt.Fprintf(&frames, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":{\"type\":\"function_call\",\"id\":\"f%d\",\"call_id\":\"c%d\",\"name\":\"record\"}}\n\n", i, i, i)
				input := ""
				if !empty {
					input = strings.Repeat("x", 300)
				}
				args, _ := json.Marshal(map[string]string{"input": input})
				fmt.Fprintf(&frames, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":%d,\"item_id\":\"f%d\",\"delta\":%q}\n\n", i, i, args)
				fmt.Fprintf(&frames, "data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":%d,\"item_id\":\"f%d\",\"arguments\":%q}\n\n", i, i, args)
			}
			body, err := readCustomStream(t, frames.String(), 1024)
			if err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("limit error = %v", err)
			}
			completed := countEventType(scanEvents(t, body), "response.custom_tool_call_input.done")
			if empty && completed >= 4 || !empty && completed != 1 {
				t.Fatalf("completed %d calls before cap, empty=%v", completed, empty)
			}
		})
	}
}

func TestCustomTerminalInputFallback(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, omit := range []bool{false, true} {
			t.Run(fmt.Sprintf("prior=%v,omit=%v", prior, omit), func(t *testing.T) {
				var frames strings.Builder
				if prior {
					fmt.Fprint(&frames, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"f\",\"call_id\":\"c\",\"name\":\"record\"}}\n\n")
					fmt.Fprint(&frames, ": keep-comment\nid: keep-id\nretry: 200\ndata: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"f\",\"delta\":\"{\\\"input\\\":\\\"ok\\\"}\"}\n\n")
				}
				item := map[string]any{"type": "function_call", "id": "f", "call_id": "c", "name": "record"}
				if !prior {
					item["arguments"] = `{"input":"ok"}`
				} else if !omit {
					item["arguments"] = ""
				}
				raw, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 0, "response": map[string]any{"object": "response", "output": []any{item}}})
				fmt.Fprintf(&frames, "data: %s\n\ndata: [DONE]\n\n", raw)
				body, err := readCustomStream(t, frames.String(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				events := scanEvents(t, body)
				assertContiguousSequence(t, events)
				if countEventType(events, "response.custom_tool_call_input.done") != 1 || countEventType(events, "response.completed") != 1 {
					t.Fatal(body)
				}
				if prior && (!strings.Contains(body, ": keep-comment") || !strings.Contains(body, "id: keep-id") || !strings.Contains(body, "retry: 200")) {
					t.Fatalf("metadata lost: %s", body)
				}
				var response toolFields
				_ = json.Unmarshal(events[len(events)-1]["response"], &response)
				var output []toolFields
				_ = json.Unmarshal(response["output"], &output)
				if len(output) != 1 || output[0].string("type") != "custom_tool_call" || output[0].string("input") != "ok" {
					t.Fatalf("terminal output: %s", response["output"])
				}
			})
		}
	}
}

func TestCustomTerminalRegeneratedIDs(t *testing.T) {
	for _, input := range []string{"ok", "changed"} {
		t.Run(input, func(t *testing.T) {
			frames := "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"stream_id\",\"call_id\":\"stream_call\",\"name\":\"record\"}}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"stream_id\",\"arguments\":\"{\\\"input\\\":\\\"ok\\\"}\"}\n\n"
			args, _ := json.Marshal(map[string]string{"input": input})
			terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 2, "response": map[string]any{"object": "response", "output": []any{map[string]any{"type": "function_call", "name": "record", "id": "new_id", "call_id": "new_call", "arguments": string(args)}}}})
			body, err := readCustomStream(t, frames+"data: "+string(terminal)+"\n\n", 4096)
			if input != "ok" {
				if err == nil || strings.Contains(body, "response.completed") {
					t.Fatalf("contradictory input accepted: %v %s", err, body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			events := scanEvents(t, body)
			assertContiguousSequence(t, events)
			if countEventType(events, "response.custom_tool_call_input.done") != 1 {
				t.Fatal(body)
			}
			var response toolFields
			_ = json.Unmarshal(events[len(events)-1]["response"], &response)
			var output []toolFields
			_ = json.Unmarshal(response["output"], &output)
			if len(output) != 1 || output[0].string("id") != "stream_id" || output[0].string("call_id") != "stream_call" {
				t.Fatalf("terminal ids changed: %s", response["output"])
			}
		})
	}
}
