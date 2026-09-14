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

const namespaceTools = `[
 {"type":"function","name":"exec_command","parameters":{"type":"object"}},
 {"type":"namespace","name":"mcp__node_repl","description":"Persistent JavaScript runtime","tools":[
  {"type":"function","name":"js","description":"Execute code","strict":false,"defer_loading":true,
   "parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}}
 ]}
]`

func namespaceRequest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	fields, err := decodeJSON([]byte(`{"model":"routed-model","tools":` + namespaceTools + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestNamespaceRequestAndReplay(t *testing.T) {
	fields := namespaceRequest(t)
	fields["input"] = json.RawMessage(`[
 {"type":"function_call","name":"js","namespace":"mcp__node_repl","call_id":"call_1","arguments":"{\"code\":\"1+1\"}"},
 {"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"2"}]},
 {"type":"function_call","namespace":"previous_tools","name":"read","call_id":"call_0","arguments":"{}"}
]`)
	fields["tool_choice"] = json.RawMessage(`{"type":"function","namespace":"mcp__node_repl","name":"js"}`)
	originalResult := []json.RawMessage{}
	_ = json.Unmarshal(fields["input"], &originalResult)
	m, err := flattenNamespaces(fields)
	if err != nil {
		t.Fatal(err)
	}
	var tools, input []toolFields
	_ = json.Unmarshal(fields["tools"], &tools)
	_ = json.Unmarshal(fields["input"], &input)
	if len(tools) != 2 || tools[0].string("name") != "exec_command" || tools[1].string("name") != "mcp__node_repl__js" {
		t.Fatalf("upstream tools: %s", fields["tools"])
	}
	if tools[1]["defer_loading"] != nil || string(tools[1]["strict"]) != "false" || !strings.Contains(tools[1].string("description"), "Persistent JavaScript runtime") {
		t.Fatalf("lost tool semantics: %s", fields["tools"])
	}
	if input[0].string("name") != "mcp__node_repl__js" || input[0]["namespace"] != nil || input[0].string("call_id") != "call_1" || input[0].string("arguments") != `{"code":"1+1"}` {
		t.Fatalf("replay call: %s", fields["input"])
	}
	var replay []json.RawMessage
	_ = json.Unmarshal(fields["input"], &replay)
	if !bytes.Equal(compactJSON(t, replay[1]), compactJSON(t, originalResult[1])) {
		t.Fatal("tool result changed")
	}
	if input[2].string("name") != "previous_tools__read" {
		t.Fatal("unadvertised history call was not mapped")
	}
	choice, _ := decodeTool(fields["tool_choice"])
	if choice.string("name") != "mcp__node_repl__js" || choice["namespace"] != nil {
		t.Fatalf("choice: %s", fields["tool_choice"])
	}
	if len(m.originals) != 2 {
		t.Fatal("missing reverse map")
	}
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNamespaceAliasesAvoidCollisions(t *testing.T) {
	fields, _ := decodeJSON([]byte(`{"tools":[
 {"type":"function","name":"a__read"},
 {"type":"function","name":"cmr_namespace_0"},
 {"type":"namespace","name":"a","tools":[{"type":"function","name":"read"}]},
 {"type":"namespace","name":"b","tools":[{"type":"function","name":"read"}]},
 {"type":"namespace","name":"` + strings.Repeat("long", 20) + `","tools":[{"type":"function","name":"read"}]}
],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"a","name":"read"}]}}`))
	m, err := flattenNamespaces(fields)
	if err != nil {
		t.Fatal(err)
	}
	var tools []toolFields
	_ = json.Unmarshal(fields["tools"], &tools)
	names := make(map[string]bool)
	for _, tool := range tools {
		name := tool.string("name")
		if names[name] || len(name) > 64 {
			t.Fatalf("ambiguous or long alias %q", name)
		}
		names[name] = true
	}
	if m.aliases[toolIdentity{"a", "read"}] == "a__read" {
		t.Fatal("alias collided with top-level tool")
	}
	if !strings.Contains(string(fields["tool_choice"]), `"name":"cmr_namespace_1"`) {
		t.Fatalf("allowed choice: %s", fields["tool_choice"])
	}
}

func TestNamespaceRejectsUnsupportedDefinitions(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"custom","name":"code"}]}]}`,
		`{"tools":[{"type":"namespace","tools":[]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f"},{"type":"function","name":"f"}]}]}`,
		`{"tools":[],"tool_choice":{"type":"function","namespace":"n","name":"f"}}`,
	} {
		fields, _ := decodeJSON([]byte(raw))
		if _, err := flattenNamespaces(fields); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestNamespaceResponseAndNextRequest(t *testing.T) {
	// The same request/replay flow must hold under every adapter combination:
	// namespace flattening with and without a reasoning adapter, and under both
	// schema loading modes.
	routes := []struct {
		name   string
		config func(t *testing.T, baseURL string) string
	}{
		{"reasoning and on_demand", namespaceRouteConfig},
		{"namespace only, schema_loading all", func(t *testing.T, baseURL string) string {
			return toolsOnlyRouteConfig(t, baseURL, "all")
		}},
		{"namespace only, on_demand", func(t *testing.T, baseURL string) string {
			return toolsOnlyRouteConfig(t, baseURL, "on_demand")
		}},
	}
	for _, route := range routes {
		for _, stream := range []bool{false, true} {
			t.Run(route.name+", stream="+fmt.Sprint(stream), func(t *testing.T) {
				call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp__node_repl__js","arguments":"{\"code\":\"1+1\"}"}`
				up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("ETag", "old")
						fmt.Fprintf(w, `{"object":"response","output":[%s],"tools":[]}`, call)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					fmt.Fprint(w, ": keepalive\r\n\r\n")
					fmt.Fprintf(w, "event: response.output_item.added\nid: 1\ndata: {\"type\":\"response.output_item.added\",\ndata: \"item\":%s}\n\n", call)
					fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"mcp__node_repl__js\",\"name\":\"mcp__node_repl__js\"}\n\n")
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", call)
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s],\"tools\":[]}}\n\n", call)
					fmt.Fprint(w, "data: [DONE]\n\n")
				})
				router, _ := newRouter(t, route.config(t, up.server.URL), nil)
				fields := namespaceRequest(t)
				fields["tool_choice"] = json.RawMessage(`{"type":"function","namespace":"mcp__node_repl","name":"js"}`)
				request, _ := json.Marshal(fields)
				result := doRequest(router, http.MethodPost, "/v1/responses", request, nil)
				if result.Code != 200 {
					t.Fatalf("response: %d %s", result.Code, result.Body.String())
				}
				if result.Header().Get("ETag") != "" {
					t.Fatal("stale ETag survived")
				}
				var final toolFields
				if stream {
					scanner := bufio.NewScanner(strings.NewReader(result.Body.String()))
					for scanner.Scan() {
						line := scanner.Text()
						if !strings.HasPrefix(line, "data: {") {
							continue
						}
						var event toolFields
						if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
							t.Fatal(err)
						}
						if event["item"] != nil {
							assertRestoredCall(t, event["item"])
						}
						if event.string("type") == "response.function_call_arguments.delta" && event.string("delta") != "mcp__node_repl__js" {
							t.Fatal("argument delta mutated")
						}
						if event["response"] != nil {
							_ = json.Unmarshal(event["response"], &final)
						}
					}
					if !strings.Contains(result.Body.String(), "id: 1\n") || !strings.Contains(result.Body.String(), "data: [DONE]\n\n") {
						t.Fatal("SSE metadata lost")
					}
				} else {
					_ = json.Unmarshal(result.Body.Bytes(), &final)
				}
				var output []json.RawMessage
				_ = json.Unmarshal(final["output"], &output)
				if len(output) != 1 {
					t.Fatalf("missing output: %s", result.Body.String())
				}
				assertRestoredCall(t, output[0])
				if !bytes.Equal(compactJSON(t, final["tools"]), compactJSON(t, []byte(namespaceTools))) {
					t.Fatal("response tool definitions were not restored")
				}
				// Replay exactly the restored call and its result through a second HTTP
				// request, as Codex does after executing the tool.
				fields["input"] = json.RawMessage("[" + string(output[0]) + `,{"type":"function_call_output","call_id":"call_1","output":"2"}]`)
				request, _ = json.Marshal(fields)
				result = doRequest(router, http.MethodPost, "/v1/responses", request, nil)
				if result.Code != 200 {
					t.Fatal(result.Body.String())
				}
				var replay struct {
					Tools      []toolFields
					Input      []toolFields
					ToolChoice toolFields `json:"tool_choice"`
				}
				_ = json.Unmarshal(up.requests()[1].Body, &replay)
				if replay.Tools[1].string("type") != "function" || replay.Input[0].string("name") != "mcp__node_repl__js" || replay.Input[0]["namespace"] != nil || replay.Input[1].string("output") != "2" {
					t.Fatalf("bad replay: %s", up.requests()[1].Body)
				}
				// The explicit selector keeps travelling upstream in flattened form.
				if replay.ToolChoice.string("name") != "mcp__node_repl__js" || replay.ToolChoice["namespace"] != nil {
					t.Fatalf("bad replay selector: %s", up.requests()[1].Body)
				}
			})
		}
	}
}

func assertRestoredCall(t *testing.T, raw []byte) {
	t.Helper()
	var call toolFields
	if err := json.Unmarshal(raw, &call); err != nil {
		t.Fatal(err)
	}
	if call.string("name") != "js" || call.string("namespace") != "mcp__node_repl" || call.string("call_id") != "call_1" || call.string("arguments") != `{"code":"1+1"}` {
		t.Fatalf("restored call: %s", raw)
	}
}

func TestNamespaceStreamFlushAndCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"mcp__node_repl__js\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	})
	router, _ := newRouter(t, namespaceRouteConfig(t, up.server.URL), nil)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(namespaceRequest(t))
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
	if scanner.Err() != nil || !strings.Contains(scanner.Text(), `"namespace":"mcp__node_repl"`) {
		t.Fatalf("first event did not arrive: %s %v", scanner.Text(), scanner.Err())
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not cancelled")
	}
}

func TestNamespaceResponseLimitsAndErrors(t *testing.T) {
	fields := namespaceRequest(t)
	m, _ := flattenNamespaces(fields)
	for _, tc := range []struct {
		content, body string
		status        int
		wantErr       bool
	}{
		{"application/json", strings.Repeat("x", 101), 200, true},
		{"application/json", `not JSON`, 200, true},
		{"text/event-stream", "data: " + strings.Repeat("x", 101) + "\n\n", 200, true},
		{"text/event-stream", "data: not JSON\n\n", 200, true},
		{"text/event-stream", ": comment\n\ndata: [DONE]\n\n", 200, false},
		{"application/json", `{"error":"unchanged"}`, 400, false},
	} {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.content}}, Body: io.NopCloser(strings.NewReader(tc.body))}
		err := adaptNamespaceResponse(resp, m, 100)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
		}
		_ = resp.Body.Close()
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: %v", tc.body, err)
		}
	}
}

func TestNamespaceNativeAndOtherAdaptersPassThrough(t *testing.T) {
	response := `{"output":[{"type":"function_call","namespace":"n","name":"f"}]}`
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, up.server.URL, up.server.URL, up.server.URL), nil)
	for _, model := range []string{"gpt-5", "qwen3-32b"} {
		body := []byte(`{"model":"` + model + `","tools":` + namespaceTools + `}`)
		result := doRequest(router, http.MethodPost, "/v1/responses", body, map[string]string{"Authorization": "Bearer test"})
		if result.Body.String() != response {
			t.Fatal("unrelated response changed")
		}
		requests := up.requests()
		var sent map[string]json.RawMessage
		_ = json.Unmarshal(requests[len(requests)-1].Body, &sent)
		if !bytes.Equal(compactJSON(t, sent["tools"]), compactJSON(t, []byte(namespaceTools))) {
			t.Fatal("unrelated tools changed")
		}
	}
}
