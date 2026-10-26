package routing

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLargeAdaptedResponse(t *testing.T) {
	// Both a JSON response and one SSE event may exceed the former 32 MiB cap.
	text := strings.Repeat("x", 33<<20)
	fields := namespaceRequest(t)
	mapping, err := flattenNamespaces(fields)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		body := `{"output":[],"text":"` + text + `"}`
		contentType := "application/json"
		if stream {
			body = "data: " + body + "\n\ndata: [DONE]\n\n"
			contentType = "text/event-stream"
		}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
		if err := adaptNamespaceResponse(resp, mapping); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), text) {
			t.Fatal("large response truncated")
		}
	}
}

func TestLargeCheckpointSummaryRoundTrip(t *testing.T) {
	summary := strings.Repeat("summary ", 40000) // Above the former 256 KiB cap.
	response := summaryResponse()
	response["output"], _ = json.Marshal([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": summary}}}})
	got, err := compactionSummary(response)
	if err != nil {
		t.Fatal(err)
	}
	fields := toolFields{}
	fields["input"], _ = json.Marshal([]toolFields{checkpointItem(got)})
	changed, requestErr := expandCheckpoints(fields, true)
	if requestErr != nil || !changed || !strings.Contains(string(fields["input"]), strings.TrimSpace(summary)) {
		t.Fatal("large checkpoint did not round-trip", requestErr)
	}
}
