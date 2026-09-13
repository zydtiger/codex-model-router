package routing

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// A summary is not exposed as a checkpoint until the upstream has completed
// successfully. Closing/cancelling the incoming request cancels its transport.
func readCompactionResponse(resp *http.Response, limit int64) (toolFields, error) {
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return readDisclosureJSON(resp, limit)
	}
	defer resp.Body.Close()
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("unsupported compaction encoding")
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, limit+1))
	scanner.Buffer(make([]byte, 4096), int(limit)+1)
	var consumed int64
	for {
		lines, data, err := readSSEFrame(scanner, limit)
		if err != nil {
			return nil, fmt.Errorf("compaction stream ended without completion: %w", err)
		}
		for _, line := range lines {
			consumed += int64(len(line) + 1)
		}
		if consumed > limit {
			return nil, fmt.Errorf("compaction response exceeds size limit")
		}
		if len(data) == 0 {
			continue
		}
		var event toolFields
		if json.Unmarshal(data, &event) != nil || event == nil {
			return nil, fmt.Errorf("invalid compaction stream event")
		}
		switch event.string("type") {
		case "response.failed", "response.incomplete", "error":
			return nil, fmt.Errorf("compaction upstream did not complete successfully")
		case "response.completed":
			var response toolFields
			if json.Unmarshal(event["response"], &response) != nil || response == nil {
				return nil, fmt.Errorf("invalid compaction completion")
			}
			return response, nil
		}
	}
}

func compactionSummary(response toolFields) (string, error) {
	if response.string("status") != "completed" {
		return "", fmt.Errorf("compaction upstream did not complete successfully")
	}
	var output []toolFields
	if json.Unmarshal(response["output"], &output) != nil {
		return "", fmt.Errorf("invalid summary output")
	}
	var texts []string
	for _, item := range output {
		switch item.string("type") {
		case "reasoning":
			continue
		case "message":
			if item.string("role") != "assistant" || (item.string("status") != "" && item.string("status") != "completed") {
				return "", fmt.Errorf("invalid summary message")
			}
			var content []toolFields
			if json.Unmarshal(item["content"], &content) != nil {
				return "", fmt.Errorf("invalid summary content")
			}
			for _, part := range content {
				if part.string("type") != "output_text" {
					return "", fmt.Errorf("compaction did not return only text")
				}
				texts = append(texts, part.string("text"))
			}
		default:
			return "", fmt.Errorf("compaction returned an unexpected output item")
		}
	}
	summary := strings.TrimSpace(strings.Join(texts, "\n"))
	raw, _ := json.Marshal(map[string]string{"summary": summary})
	if summary == "" || len(raw) > maxCheckpointBytes {
		return "", fmt.Errorf("empty or oversized checkpoint summary")
	}
	return summary, nil
}

func adaptCompactionResponse(resp *http.Response, c *compactionRequest, limit int64) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	response, err := readCompactionResponse(resp, limit)
	if err != nil {
		return err
	}
	summary, err := compactionSummary(response)
	if err != nil {
		return err
	}
	item := checkpointItem(summary)
	item.setString("id", fmt.Sprintf("cmp_%x", randomCheckpointID()))
	// Keep upstream usage and identity, but never echo the summarization prompt
	// or its internal request settings into Codex's response metadata.
	final := toolFields{}
	for _, key := range []string{"id", "object", "created_at", "model", "usage"} {
		if raw := response[key]; raw != nil {
			final[key] = raw
		}
	}
	final.setString("status", "completed")
	final["output"], _ = json.Marshal([]toolFields{item})
	var encoded []byte
	if c.stream {
		created := toolFields{}
		for k, v := range final {
			created[k] = v
		}
		created.setString("status", "in_progress")
		created["output"] = json.RawMessage("[]")
		var buffer bytes.Buffer
		var seq int64
		for _, event := range []map[string]any{
			{"type": "response.created", "response": created},
			{"type": "response.output_item.added", "output_index": 0, "item": item},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": final},
		} {
			raw, _ := json.Marshal(event)
			var fields toolFields
			_ = json.Unmarshal(raw, &fields)
			if err := writeDisclosureEvent(&buffer, fields, &seq, limit); err != nil {
				return err
			}
		}
		buffer.WriteString("data: [DONE]\n\n")
		encoded = buffer.Bytes()
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		encoded, _ = json.Marshal(final)
		resp.Header.Set("Content-Type", "application/json")
	}
	if int64(len(encoded)) > limit {
		return fmt.Errorf("checkpoint response exceeds size limit")
	}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("ETag")
	return nil
}

func randomCheckpointID() []byte {
	var id [16]byte
	// crypto/rand.Read always fills the buffer or terminates the process.
	_, _ = rand.Read(id[:])
	return id[:]
}
