package routing

import (
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

func compactConfig(t *testing.T, url string) string {
	base := strings.Replace(reasoningRouteConfig(t, url), `"reasoning":`, `"compaction":{"adapter":"text_summary"},"tools":{"namespace_adapter":"namespace_to_functions","schema_loading":"on_demand"},"reasoning":`, 1)
	return base
}
func compactRequest(t *testing.T, stream bool) map[string]json.RawMessage {
	f := namespaceRequest(t)
	f["input"] = json.RawMessage(`[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Remember cedar-742"}]},{"type":"compaction_trigger"}]`)
	f["stream"], _ = json.Marshal(stream)
	f["tool_choice"] = json.RawMessage(`{"type":"function","namespace":"mcp__node_repl","name":"js"}`)
	f["max_output_tokens"] = json.RawMessage(`9000`)
	return f
}
func summaryResponse() toolFields {
	var f toolFields
	_ = json.Unmarshal([]byte(`{"id":"resp_summary","object":"response","status":"completed","output":[{"type":"reasoning"},{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Checkpoint: cedar-742; continue the pending task."}]}],"usage":{"input_tokens":50100,"output_tokens":32,"total_tokens":50132}}`), &f)
	return f
}
func TestCompactionRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var received toolFields
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&received)
				f := summaryResponse()
				b, _ := json.Marshal(f)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", b)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(b)
				}
			}))
			defer up.Close()
			router, _ := newRouter(t, compactConfig(t, up.URL), nil)
			body, _ := json.Marshal(compactRequest(t, stream))
			r := doRequest(router, "POST", "/v1/responses", body, nil)
			if r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			if string(received["tools"]) != "[]" || string(received["tool_choice"]) != `"none"` || string(received["max_output_tokens"]) != "4096" || strings.Contains(string(received["input"]), "compaction_trigger") || !strings.Contains(string(received["input"]), checkpointPrompt) {
				t.Fatalf("bad summary request: %s", received)
			}
			var response toolFields
			if stream {
				if strings.Contains(r.Body.String(), "Checkpoint: cedar") {
					t.Fatal("raw summary leaked into stream")
				}
				for _, line := range strings.Split(r.Body.String(), "\n") {
					if strings.HasPrefix(line, "data: {") {
						var event toolFields
						_ = json.Unmarshal([]byte(line[6:]), &event)
						if event.string("type") == "response.completed" {
							_ = json.Unmarshal(event["response"], &response)
						}
					}
				}
			} else {
				_ = json.Unmarshal(r.Body.Bytes(), &response)
			}
			var items []toolFields
			_ = json.Unmarshal(response["output"], &items)
			if len(items) != 1 || items[0].string("type") != "compaction" {
				t.Fatalf("invalid output %s", response)
			}
			if !bytes.Equal(response["usage"], summaryResponse()["usage"]) { // JSON formatting may differ.
				var a, b any
				_ = json.Unmarshal(response["usage"], &a)
				_ = json.Unmarshal(summaryResponse()["usage"], &b)
				if fmt.Sprint(a) != fmt.Sprint(b) {
					t.Fatal("usage changed")
				}
			}
			input, _ := json.Marshal(items)
			next := map[string]json.RawMessage{"input": input}
			changed, err := expandCheckpoints(next, true)
			if err != nil || !changed || !strings.Contains(string(next["input"]), "cedar-742") || strings.Contains(string(next["input"]), "encrypted_content") {
				t.Fatal(changed, err, string(next["input"]))
			}
		})
	}
}

func TestCheckpointValidationAndNativeSwitch(t *testing.T) {
	for _, carrier := range []string{checkpointFamily + "v9:abc", checkpointPrefix + "!", checkpointPrefix + "e30", "opaque-native"} {
		f := map[string]json.RawMessage{}
		f["input"], _ = json.Marshal([]map[string]string{{"type": "compaction", "encrypted_content": carrier}})
		if _, err := expandCheckpoints(f, true); err == nil {
			t.Fatal("accepted", carrier)
		}
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "cedar-742") || strings.Contains(string(b), checkpointFamily) {
			t.Error("native checkpoint was not expanded", string(b))
		}
		w.Write([]byte(`{"output":[]}`))
	}))
	defer up.Close()
	configText := strings.ReplaceAll(compactConfig(t, up.URL), "http://127.0.0.1:1", up.URL)
	router, _ := newRouter(t, configText, nil)
	b, _ := json.Marshal(map[string]any{"model": "gpt-native", "input": []toolFields{checkpointItem("cedar-742")}})
	if r := doRequest(router, "POST", "/v1/responses", b, map[string]string{"Authorization": "Bearer test"}); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	f := map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"compaction","encrypted_content":"native"}]`)}
	if changed, err := expandCheckpoints(f, false); changed || err != nil {
		t.Fatal("native opaque checkpoint changed")
	}
}

func TestCompactionRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"position", "input", `[{"type":"compaction_trigger"},{"role":"user","content":"x"}]`},
		{"duplicate", "input", `[{"type":"compaction_trigger"},{"type":"compaction_trigger"}]`},
		{"stored", "previous_response_id", `"resp_old"`}, {"conversation", "conversation", `"conv"`},
		{"server compaction", "context_management", `[]`}, {"budget", "max_output_tokens", `0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _ := newRouter(t, compactConfig(t, "http://127.0.0.1:1"), nil)
			f := compactRequest(t, false)
			f[tc.key] = json.RawMessage(tc.value)
			b, _ := json.Marshal(f)
			if r := doRequest(router, "POST", "/v1/responses", b, nil); r.Code != 400 {
				t.Fatal(r.Code, r.Body.String())
			}
		})
	}
	router, _ := newRouter(t, reasoningRouteConfig(t, "http://127.0.0.1:1"), nil)
	b, _ := json.Marshal(compactRequest(t, false))
	if r := doRequest(router, "POST", "/v1/responses", b, nil); r.Code != 400 {
		t.Fatal(r.Code)
	}
}

func TestCompactionFailureNeverEmitsCheckpoint(t *testing.T) {
	for _, kind := range []string{"incomplete", "empty", "tool", "refusal", "broken_stream", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f := summaryResponse()
				switch kind {
				case "incomplete":
					f.setString("status", "incomplete")
				case "empty":
					f["output"] = json.RawMessage(`[]`)
				case "tool":
					f["output"] = json.RawMessage(`[{"type":"function_call","name":"exec_command"}]`)
				case "refusal":
					f["output"] = json.RawMessage(`[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`)
				case "broken_stream":
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
					return
				case "oversized":
					f["output"], _ = json.Marshal([]map[string]any{{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": strings.Repeat("x", maxCheckpointBytes+1)}}}})
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(f)
			}))
			defer up.Close()
			router, _ := newRouter(t, compactConfig(t, up.URL), nil)
			b, _ := json.Marshal(compactRequest(t, true))
			r := doRequest(router, "POST", "/v1/responses", b, nil)
			if r.Code != 502 || strings.Contains(r.Body.String(), checkpointPrefix) {
				t.Fatal(r.Code, r.Body.String())
			}
		})
	}
}

func TestCompactionCancellation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer up.Close()
	router, _ := newRouter(t, compactConfig(t, up.URL), nil)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _ := json.Marshal(compactRequest(t, true))
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/responses", bytes.NewReader(b))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := http.DefaultClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	<-done
}

func TestCompactionNamespacedHistoryAndBudget(t *testing.T) {
	var got toolFields
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summaryResponse())
	}))
	defer up.Close()
	router, _ := newRouter(t, compactConfig(t, up.URL), nil)
	f := compactRequest(t, false)
	f["max_output_tokens"] = json.RawMessage(`256`)
	f["input"] = json.RawMessage(`[{"type":"function_call","namespace":"mcp__node_repl","name":"js","call_id":"call_1","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"cedar-742"},{"type":"compaction_trigger"}]`)
	b, _ := json.Marshal(f)
	r := doRequest(router, "POST", "/v1/responses", b, nil)
	if r.Code != 200 || string(got["max_output_tokens"]) != "256" || !strings.Contains(string(got["input"]), "mcp__node_repl__js") || !strings.Contains(string(got["input"]), "cedar-742") {
		t.Fatal(r.Code, string(got["input"]))
	}
	// After compaction old calls are gone: discovery must still expose the
	// namespace directory so the next model turn can load tools again.
	f["input"], _ = json.Marshal([]toolFields{checkpointItem("Continue using JavaScript.")})
	delete(f, "tool_choice")
	b, _ = json.Marshal(f)
	r = doRequest(router, "POST", "/v1/responses", b, nil)
	if r.Code != 200 || !strings.Contains(string(got["tools"]), "router_load_tools") || strings.Contains(string(got["tools"]), `"name":"mcp__node_repl__js"`) {
		t.Fatal(r.Code, string(got["tools"]))
	}
}
