package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/zydtiger/codex-model-router/internal/config"
)

func developerFields(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertDeveloperJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestDeveloperMessagesKeepPositionsAndContent(t *testing.T) {
	fields := developerFields(t, `{"input":[
		{"type":"message","id":"dev-1","role":"developer","content":"first\nrule"},
		{"role":"developer","content":[{"type":"input_text","text":"second rule","extra":"keep"},{"type":"input_text","text":"more"}]},
		{"role":"user","content":"<developer>ordinary user text</developer>"},
		{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":"result"},
		{"role":"developer","content":"later update"},
		{"role":"assistant","content":"answer"}
	]}`)
	got, stats, err := translateRemoteBody(fields, &config.Route{Input: config.Input{DeveloperRoleAsUser: true}})
	if err != nil {
		t.Fatal(err)
	}
	assertDeveloperJSON(t, got["input"], []byte(`[
		{"type":"message","id":"dev-1","role":"user","content":"<developer>\nfirst\nrule\n</developer>"},
		{"role":"user","content":[{"type":"input_text","text":"<developer>\n"},{"type":"input_text","text":"second rule","extra":"keep"},{"type":"input_text","text":"more"},{"type":"input_text","text":"\n</developer>"}]},
		{"role":"user","content":"<developer>ordinary user text</developer>"},
		{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":"result"},
		{"role":"user","content":"<developer>\nlater update\n</developer>"},
		{"role":"assistant","content":"answer"}
	]`))
	if stats.DeveloperRewrites != 3 || !stats.Changed() {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestDeveloperConventionInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, body, prefix string
	}{
		{"missing", `{}`, ""},
		{"null", `{"instructions":null}`, ""},
		{"empty", `{"instructions":""}`, ""},
		{"existing", `{"instructions":"Original system instructions.\n"}`, "Original system instructions.\n\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, stats, err := translateRemoteBody(developerFields(t, tc.body), &config.Route{Input: config.Input{DeveloperRoleAsUser: true}})
			if err != nil {
				t.Fatal(err)
			}
			var instructions string
			if err := json.Unmarshal(got["instructions"], &instructions); err != nil {
				t.Fatal(err)
			}
			if instructions != tc.prefix+developerInstructionConvention {
				t.Fatalf("instructions = %q", instructions)
			}
			if !stats.Changed() || stats.InstructionsAppended != 1 {
				t.Fatalf("convention-only request stats = %+v", stats)
			}
		})
	}
}

func TestDeveloperConversionDisabledPreservesMessagesAndInstructions(t *testing.T) {
	for _, instructions := range []string{"", `,"instructions":null`, `,"instructions":"Keep exactly"`} {
		fields := developerFields(t, `{"input":[{"role":"developer","content":"rules"},{"role":"user","content":"question"}]`+instructions+`}`)
		before, _ := json.Marshal(fields)
		got, stats, err := translateRemoteBody(fields, &config.Route{})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(got)
		assertDeveloperJSON(t, after, before)
		if stats.Changed() {
			t.Fatalf("disabled conversion reported changes: %+v", stats)
		}
	}
}

func TestDeveloperAppendPreservesTranslatedPrefix(t *testing.T) {
	common := `{"role":"developer","content":"initial"},{"role":"user","content":"question"}`
	var prefix []json.RawMessage
	var previousInstructions json.RawMessage
	for i, tail := range []string{"", `,{"role":"developer","content":"new rule"}`} {
		fields := developerFields(t, `{"instructions":"Base rules","input":[`+common+tail+`]}`)
		got, _, err := translateRemoteBody(fields, &config.Route{Input: config.Input{DeveloperRoleAsUser: true}})
		if err != nil {
			t.Fatal(err)
		}
		var messages []json.RawMessage
		if err := json.Unmarshal(got["input"], &messages); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			prefix, previousInstructions = messages, got["instructions"]
			continue
		}
		if !bytes.Equal(previousInstructions, got["instructions"]) {
			t.Fatal("appending a developer message changed the fixed instructions")
		}
		for j := range prefix {
			if !bytes.Equal(prefix[j], messages[j]) {
				t.Fatalf("appending a developer message changed prefix item %d", j)
			}
		}
	}
}

func TestDeveloperConversionRejectsUnwrappableInput(t *testing.T) {
	for _, body := range []string{
		`{"instructions":42}`,
		`{"input":[{"role":"developer","content":null}]}`,
		`{"input":[{"role":"developer","content":42}]}`,
		`{"input":[{"role":"developer"}]}`,
	} {
		_, _, err := translateRemoteBody(developerFields(t, body), &config.Route{Input: config.Input{DeveloperRoleAsUser: true}})
		if err == nil || err.status != http.StatusBadRequest {
			t.Fatalf("body %s: error = %+v, want 400", body, err)
		}
	}
}

func TestDeveloperConversionIsRouteSpecificAndNeverNative(t *testing.T) {
	chatgpt, api, local := newUpstream(t, nil), newUpstream(t, nil), newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
		"native":{"chatgpt_base_url":%q,"api_base_url":%q,"models":["gpt-5"]},
		"routes":[
			{"name":"enabled","base_url":%q,"models":["local-enabled"],"input":{"developer_role_as_user":true}},
			{"name":"disabled","base_url":%q,"models":["local-disabled"],"input":{"developer_role_as_user":false}},
			{"name":"default","base_url":%q,"models":["local-default"]}
		]
	}`, chatgpt.server.URL, api.server.URL, local.server.URL, local.server.URL, local.server.URL), nil)
	for _, model := range []string{"local-enabled", "local-disabled", "local-default"} {
		body := jsonBody(t, map[string]any{"model": model, "instructions": "Base", "input": []any{map[string]any{"role": "developer", "content": "rules"}}})
		response := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("model %s: status %d: %s", model, response.Code, response.Body.String())
		}
		forwarded := local.requests()[local.count()-1].Body
		if model == "local-enabled" {
			fields := developerFields(t, string(forwarded))
			assertDeveloperJSON(t, fields["input"], []byte(`[{"role":"user","content":"<developer>\nrules\n</developer>"}]`))
			var instructions string
			_ = json.Unmarshal(fields["instructions"], &instructions)
			if instructions != "Base\n\n"+developerInstructionConvention {
				t.Fatalf("instructions = %q", instructions)
			}
		} else {
			assertDeveloperJSON(t, forwarded, body)
		}
	}
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		upstream *upstream
	}{
		{"chatgpt", map[string]string{"Authorization": "Bearer test", "ChatGPT-Account-ID": "account"}, chatgpt},
		{"api", map[string]string{"Authorization": "Bearer test"}, api},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{ "model": "gpt-5", "instructions": "Base", "input": [{"role":"developer","content":"keep native"}] }`)
			response := doRequest(router, http.MethodPost, "/v1/responses", body, tc.headers)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if !bytes.Equal(tc.upstream.requests()[0].Body, body) {
				t.Fatal("native body changed")
			}
		})
	}
}

func TestDeveloperConversionToolRoundTripAndStreaming(t *testing.T) {
	local := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"}]}`))
	})
	router, _ := newRouter(t, fmt.Sprintf(`{"routes":[{"name":"local","base_url":%q,"models":["local-model"],"input":{"developer_role_as_user":true}}]}`, local.server.URL), nil)
	initial := `{"role":"developer","content":"Use the read tool."},{"role":"user","content":"Read a file."}`
	call := `{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"}`
	result := `{"type":"function_call_output","call_id":"call-1","output":"file contents"}`
	first := []byte(`{"model":"local-model","input":[` + initial + `],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`)
	response := doRequest(router, http.MethodPost, "/v1/responses", first, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"call_id":"call-1"`) {
		t.Fatalf("first response: %d %s", response.Code, response.Body.String())
	}
	second := []byte(`{"model":"local-model","stream":true,"input":[` + initial + `,` + call + `,` + result + `,{"role":"developer","content":"Now answer briefly."}]}`)
	response = doRequest(router, http.MethodPost, "/v1/responses", second, map[string]string{"Accept": "text/event-stream"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: response.completed") || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("streaming response: %d %s", response.Code, response.Body.String())
	}
	fields := developerFields(t, string(local.requests()[1].Body))
	var input []json.RawMessage
	_ = json.Unmarshal(fields["input"], &input)
	assertDeveloperJSON(t, input[2], []byte(call))
	assertDeveloperJSON(t, input[3], []byte(result))
	assertDeveloperJSON(t, input[4], []byte(`{"role":"user","content":"<developer>\nNow answer briefly.\n</developer>"}`))
}
