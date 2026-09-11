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
	"sync"
	"testing"
	"time"

	"github.com/zydtiger/codex-model-router/internal/config"
)

func disclosureRequest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	fields := namespaceRequest(t)
	var tools []json.RawMessage
	_ = json.Unmarshal(fields["tools"], &tools)
	other := `{"type":"namespace","name":"unrelated_mail","description":"Send and read mail","tools":[{"type":"function","name":"send","parameters":{"type":"object","properties":{"body":{"type":"string","description":"` + strings.Repeat("mail schema detail ", 2000) + `"}}}}]}`
	tools = append(tools, json.RawMessage(other))
	fields["tools"], _ = json.Marshal(tools)
	return fields
}

func visibleNames(t *testing.T, fields map[string]json.RawMessage) []string {
	t.Helper()
	var tools []toolFields
	if err := json.Unmarshal(fields["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	var result []string
	for _, tool := range tools {
		result = append(result, tool.string("name"))
	}
	return result
}

func TestDisclosureOnlyLoadsSelectedSchemas(t *testing.T) {
	fields := disclosureRequest(t)
	m, err := flattenNamespaces(fields)
	if err != nil {
		t.Fatal(err)
	}
	eagerBytes := len(fields["tools"])
	d := prepareToolDisclosure(fields, m)
	if got := strings.Join(visibleNames(t, fields), ","); got != "exec_command,router_load_tools" {
		t.Fatal(got)
	}
	if len(fields["tools"])*10 >= eagerBytes {
		t.Fatalf("directory did not reduce schemas: %d versus %d", len(fields["tools"]), eagerBytes)
	}
	before := string(fields["tools"])
	for _, input := range []string{`{}`, `not JSON`, `{"namespaces":["mcp__node_repl","not_a_namespace"]}`} {
		if !strings.Contains(d.load(input), "error") || string(fields["tools"]) != before {
			t.Fatal("invalid selection changed visibility")
		}
	}
	if result := d.load(`{"namespaces":["mcp__node_repl"]}`); !strings.Contains(result, "mcp__node_repl__js") {
		t.Fatal(result)
	}
	if strings.Contains(string(fields["tools"]), "mail schema detail") {
		t.Fatal("unselected schema was sent")
	}
	if got := strings.Join(visibleNames(t, fields), ","); got != "exec_command,mcp__node_repl__js,router_load_tools" {
		t.Fatal(got)
	}
	if !strings.Contains(d.load(`{"namespaces":["unrelated_mail"]}`), "unrelated_mail__send") {
		t.Fatal("second group inaccessible")
	}
}

func TestDisclosureRehydratesReplayAndSelectors(t *testing.T) {
	for _, tc := range []struct {
		choice, input  string
		loaded, loader bool
	}{
		{`"auto"`, `[{"type":"function_call","namespace":"mcp__node_repl","name":"js","call_id":"c","arguments":"{}"}]`, true, true},
		{`{"type":"function","namespace":"mcp__node_repl","name":"js"}`, `[]`, true, true},
		{`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"mcp__node_repl","name":"js"}]}`, `[]`, true, true},
		{`"none"`, `[]`, false, false},
	} {
		fields := disclosureRequest(t)
		fields["tool_choice"] = json.RawMessage(tc.choice)
		fields["input"] = json.RawMessage(tc.input)
		m, err := flattenNamespaces(fields)
		if err != nil {
			t.Fatal(err)
		}
		prepareToolDisclosure(fields, m)
		names := strings.Join(visibleNames(t, fields), ",")
		if strings.Contains(names, "mcp__node_repl__js") != tc.loaded || strings.Contains(names, "router_load_tools") != tc.loader {
			t.Fatal(names)
		}
	}
}

func disclosureCall(name, args, id string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": args, "status": "completed"})
	return b
}

func mockDisclosureResponse(w http.ResponseWriter, stream bool, round int, output []json.RawMessage) {
	r := map[string]any{"object": "response", "id": fmt.Sprintf("resp_%d", round), "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 100 + round, "output_tokens": 7, "total_tokens": 107 + round}}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	seq := 0
	emit := func(kind string, fields map[string]any) {
		fields["type"] = kind
		fields["sequence_number"] = seq
		seq++
		data, _ := json.Marshal(fields)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		w.(http.Flusher).Flush()
	}
	emit("response.created", map[string]any{"response": map[string]any{"id": r["id"], "object": "response", "output": []any{}, "status": "in_progress"}})
	for i, raw := range output {
		var item toolFields
		_ = json.Unmarshal(raw, &item)
		args := item.string("arguments")
		item.setString("arguments", "")
		emit("response.output_item.added", map[string]any{"output_index": i, "item": item})
		emit("response.function_call_arguments.delta", map[string]any{"output_index": i, "item_id": item.string("id"), "delta": args})
		emit("response.function_call_arguments.done", map[string]any{"output_index": i, "item_id": item.string("id"), "arguments": args, "name": item.string("name")})
		emit("response.output_item.done", map[string]any{"output_index": i, "item": raw})
	}
	emit("response.completed", map[string]any{"response": r})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestDisclosureHTTPRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			round := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				round++
				var fields map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&fields)
				names := strings.Join(visibleNames(t, fields), ",")
				if strings.Contains(string(fields["tools"]), "mail schema detail") {
					t.Error("unrelated schemas sent upstream")
				}
				if round == 1 {
					if strings.Contains(names, "mcp__node_repl__js") {
						t.Error("schema eagerly exposed")
					}
					mockDisclosureResponse(w, stream, round, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["mcp__node_repl"]}`, "load")})
					return
				}
				if !strings.Contains(names, "mcp__node_repl__js") || !strings.Contains(string(fields["input"]), "available_functions") {
					t.Error("missing schemas or local result")
				}
				mockDisclosureResponse(w, stream, round, []json.RawMessage{disclosureCall("mcp__node_repl__js", `{"code":"6*7"}`, "real")})
			}))
			defer up.Close()
			router, _ := newRouter(t, sglangRouteConfig(t, up.URL), nil)
			fields := disclosureRequest(t)
			fields["stream"], _ = json.Marshal(stream)
			body, _ := json.Marshal(fields)
			res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
			if res.Code != 200 || round != 2 {
				t.Fatalf("%d, %d, %s", res.Code, round, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "router_load_tools") || strings.Contains(res.Body.String(), `"call_id":"load"`) {
				t.Fatal("internal call leaked to Codex")
			}
			var final toolFields
			if stream {
				scanner := bufio.NewScanner(strings.NewReader(res.Body.String()))
				lastSequence := int64(-1)
				for scanner.Scan() {
					line := scanner.Text()
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					var event toolFields
					_ = json.Unmarshal([]byte(line[6:]), &event)
					var seq int64
					_ = json.Unmarshal(event["sequence_number"], &seq)
					if seq != lastSequence+1 {
						t.Error("noncontiguous sequence")
					}
					lastSequence = seq
					if event["output_index"] != nil && string(event["output_index"]) != "0" {
						t.Error("hidden index counted")
					}
					if event.string("type") == "response.completed" {
						_ = json.Unmarshal(event["response"], &final)
					}
				}
			} else {
				_ = json.Unmarshal(res.Body.Bytes(), &final)
			}
			var output []toolFields
			_ = json.Unmarshal(final["output"], &output)
			if len(output) != 1 || output[0].string("namespace") != "mcp__node_repl" || output[0].string("name") != "js" {
				t.Fatalf("output: %s", final["output"])
			}
			var usage map[string]int
			_ = json.Unmarshal(final["usage"], &usage)
			if usage["input_tokens"] != 102 || usage["output_tokens"] != 14 {
				t.Fatalf("usage: %s", final["usage"])
			}
		})
	}
}

func TestDisclosureMixedCallsReturnToClient(t *testing.T) {
	for _, stream := range []bool{false, true} {
		up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mockDisclosureResponse(w, stream, 1, []json.RawMessage{
				disclosureCall("router_load_tools", `{"namespaces":["mcp__node_repl"]}`, "load"),
				disclosureCall("exec_command", `{"cmd":"true"}`, "real"),
			})
		})
		router, _ := newRouter(t, sglangRouteConfig(t, up.server.URL), nil)
		body, _ := json.Marshal(disclosureRequest(t))
		res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
		if up.count() != 1 || strings.Contains(res.Body.String(), `"call_id":"load"`) || !strings.Contains(res.Body.String(), `"call_id":"real"`) {
			t.Fatal(res.Body.String())
		}
	}
}

func TestDisclosureBoundedInvalidSelections(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mockDisclosureResponse(w, false, 1, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["wrong"]}`, "load")})
	})
	router, _ := newRouter(t, sglangRouteConfig(t, up.server.URL), nil)
	body, _ := json.Marshal(disclosureRequest(t))
	res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if res.Code != 502 || up.count() != maxDisclosureRounds+1 {
		t.Fatalf("%d requests, status %d", up.count(), res.Code)
	}
}

func TestDisclosureCancellationDuringFollowup(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	round := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		round++
		if round == 1 {
			mockDisclosureResponse(w, true, round, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["mcp__node_repl"]}`, "load")})
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer up.Close()
	cfg, _ := config.Parse([]byte(sglangRouteConfig(t, up.URL)))
	router, _ := New(cfg, Options{})
	srv := httptest.NewServer(router)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(disclosureRequest(t))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("followup not started")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("followup not cancelled")
	}
}

func TestDisclosureConcurrentIsolation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&fields)
		var tag string
		_ = json.Unmarshal(fields["instructions"], &tag)
		if !strings.Contains(string(fields["input"]), "function_call_output") {
			mockDisclosureResponse(w, false, 1, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["`+tag+`"]}`, "load")})
			return
		}
		names := strings.Join(visibleNames(t, fields), ",")
		if tag == "mcp__node_repl" && strings.Contains(names, "unrelated_mail__send") {
			t.Error("other request's group leaked")
		}
		if tag == "unrelated_mail" && strings.Contains(names, "mcp__node_repl__js") {
			t.Error("other request's group leaked")
		}
		mockDisclosureResponse(w, false, 2, []json.RawMessage{})
	}))
	defer up.Close()
	cfg, _ := config.Parse([]byte(sglangRouteConfig(t, up.URL)))
	router, _ := New(cfg, Options{})
	var wg sync.WaitGroup
	for _, tag := range []string{"mcp__node_repl", "unrelated_mail", "mcp__node_repl", "unrelated_mail"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fields := disclosureRequest(t)
			fields["instructions"], _ = json.Marshal(tag)
			body, _ := json.Marshal(fields)
			res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
			if res.Code != 200 {
				t.Error(res.Body.String())
			}
		}()
	}
	wg.Wait()
}

func TestDisclosureHonorsOutputBudgetAndReasoningPolicy(t *testing.T) {
	for _, policy := range []string{"drop", "keep", "reject"} {
		fields := disclosureRequest(t)
		fields["max_output_tokens"] = json.RawMessage(`20`)
		mapping, _ := flattenNamespaces(fields)
		d := prepareToolDisclosure(fields, mapping)
		d.reasoningPolicy = policy
		output := []json.RawMessage{json.RawMessage(`{"type":"reasoning","summary":[]}`), disclosureCall(d.name, `{"namespaces":["mcp__node_repl"]}`, "c")}
		response := toolFields{"usage": json.RawMessage(`{"output_tokens":7}`)}
		err := d.continueAfterLoad(output, response)
		if policy == "reject" {
			if err == nil {
				t.Fatal("reasoning policy ignored")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(fields["max_output_tokens"]) != "13" {
			t.Fatal("output budget was not reduced")
		}
		if strings.Contains(string(fields["input"]), `"type":"reasoning"`) != (policy == "keep") {
			t.Fatal("reasoning replay policy ignored")
		}
		if err := d.continueAfterLoad(output, toolFields{}); err == nil {
			t.Fatal("missing usage ignored under explicit budget")
		}
	}
}

func TestDisclosureRejectsManagedHistoryBeforeForwarding(t *testing.T) {
	up := newUpstream(t, nil)
	router, _ := newRouter(t, sglangRouteConfig(t, up.server.URL), nil)
	for _, key := range []string{"previous_response_id", "conversation"} {
		fields := disclosureRequest(t)
		fields[key] = json.RawMessage(`"provider_id"`)
		body, _ := json.Marshal(fields)
		res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
		if res.Code != 400 || up.count() != 0 {
			t.Fatal("provider-managed history reached disclosure loop")
		}
	}
}

func TestDisclosureFollowupFailureAndRequestLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		round := 0
		up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			round++
			if round == 1 {
				mockDisclosureResponse(w, stream, round, []json.RawMessage{disclosureCall("router_load_tools", `{"namespaces":["mcp__node_repl"]}`, "load")})
				return
			}
			w.WriteHeader(503)
			_, _ = w.Write([]byte("unavailable"))
		})
		router, _ := newRouter(t, sglangRouteConfig(t, up.server.URL), nil)
		body, _ := json.Marshal(disclosureRequest(t))
		res := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
		if round != 2 {
			t.Fatal("followup not attempted")
		}
		if !stream && res.Code != 502 {
			t.Fatal("failed JSON followup did not fail the response")
		}
		if stream && strings.Contains(res.Body.String(), "response.completed") {
			t.Fatal("failed stream was reported completed")
		}
	}
	fields := disclosureRequest(t)
	mapping, _ := flattenNamespaces(fields)
	d := prepareToolDisclosure(fields, mapping)
	_ = d.load(`{"namespaces":["unrelated_mail"]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/responses", nil)
	if _, err := d.nextResponse(context.Background(), req, http.DefaultTransport, 100); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("expanded request was not bounded")
	}
}
