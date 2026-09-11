package routing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

func readDisclosureJSON(resp *http.Response, limit int64) (toolFields, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tool disclosure upstream returned HTTP %d", resp.StatusCode)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("unsupported disclosure response encoding")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("tool disclosure response exceeds size limit")
	}
	var fields toolFields
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("invalid tool disclosure response")
	}
	return fields, nil
}

func (d *toolDisclosure) nextResponse(ctx context.Context, request *http.Request, transport http.RoundTripper, limit int64) (*http.Response, error) {
	body, err := json.Marshal(d.fields)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("tool disclosure request exceeds size limit")
	}
	req := request.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.Header.Del("Content-Length")
	return transport.RoundTrip(req)
}

func (d *toolDisclosure) splitOutput(output []json.RawMessage) (visible []json.RawMessage, hasLoad, hasRealCall bool) {
	for _, raw := range output {
		var item toolFields
		_ = json.Unmarshal(raw, &item)
		if d.isLoad(item) {
			hasLoad = true
			continue
		}
		if item.string("type") == "function_call" || item.string("type") == "custom_tool_call" {
			hasRealCall = true
		}
		visible = append(visible, raw)
	}
	return
}

// The input usage of the last upstream pass describes the current context.
// Summing repeated input usage would falsely trigger Codex auto-compaction.
// Generated output usage includes the hidden schema-loading passes.
type disclosureUsage struct{ output, reasoning int64 }

func (u *disclosureUsage) add(response toolFields) {
	var usage struct {
		Output  int64 `json:"output_tokens"`
		Details struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	_ = json.Unmarshal(response["usage"], &usage)
	u.output += usage.Output
	u.reasoning += usage.Details.Reasoning
}

func (u *disclosureUsage) apply(response toolFields) {
	var usage toolFields
	if json.Unmarshal(response["usage"], &usage) != nil || usage == nil {
		return
	}
	var input int64
	_ = json.Unmarshal(usage["input_tokens"], &input)
	usage["output_tokens"], _ = json.Marshal(u.output)
	usage["total_tokens"], _ = json.Marshal(input + u.output)
	var details toolFields
	_ = json.Unmarshal(usage["output_tokens_details"], &details)
	if details != nil {
		details["reasoning_tokens"], _ = json.Marshal(u.reasoning)
		usage["output_tokens_details"], _ = json.Marshal(details)
	}
	response["usage"], _ = json.Marshal(usage)
}

// adaptDisclosureResponse handles only the synthetic loader. All real calls
// are returned to Codex and retain its normal execution/approval boundary.
func adaptDisclosureResponse(resp *http.Response, d *toolDisclosure, transport http.RoundTripper, limit int64) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		ctx, cancel := context.WithCancel(resp.Request.Context())
		reader, writer := io.Pipe()
		initial := resp.Body
		first := *resp
		first.Header = resp.Header.Clone()
		resp.Body = &disclosureReader{PipeReader: reader, cancel: cancel, initial: initial}
		go func() {
			defer cancel()
			err := d.stream(ctx, &first, transport, limit, writer)
			_ = writer.CloseWithError(err)
		}()
	} else {
		request := resp.Request
		current := resp
		var visible []json.RawMessage
		var usage disclosureUsage
		for {
			response, err := readDisclosureJSON(current, limit)
			if err != nil {
				return err
			}
			var output []json.RawMessage
			if err := json.Unmarshal(response["output"], &output); err != nil {
				return fmt.Errorf("invalid disclosure output array")
			}
			part, load, real := d.splitOutput(output)
			visible = append(visible, part...)
			usage.add(response)
			if !load || real || response.string("status") != "completed" {
				response["output"], _ = json.Marshal(visible)
				usage.apply(response)
				body, _ := json.Marshal(response)
				if int64(len(body)) > limit {
					return fmt.Errorf("tool disclosure response exceeds size limit")
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				break
			}
			if err := d.continueAfterLoad(output, response); err != nil {
				return err
			}
			current, err = d.nextResponse(request.Context(), request, transport, limit)
			if err != nil {
				return err
			}
		}
	}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Del("ETag")
	return nil
}

type disclosureReader struct {
	*io.PipeReader
	cancel  context.CancelFunc
	initial io.ReadCloser
}

func (r *disclosureReader) Close() error {
	r.cancel()
	_ = r.initial.Close()
	return r.PipeReader.Close()
}

// readSSEFrame bounds each frame, including multi-line data and comments.
func readSSEFrame(scanner *bufio.Scanner, limit int64) ([]string, []byte, error) {
	var lines, data []string
	var size int64
	for scanner.Scan() {
		line := scanner.Text()
		size += int64(len(line)) + 1
		if size > limit {
			return nil, nil, fmt.Errorf("tool disclosure SSE event exceeds size limit")
		}
		lines = append(lines, line)
		if line == "data" {
			data = append(data, "")
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
		if line == "" {
			return lines, []byte(strings.Join(data, "\n")), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if len(lines) != 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return nil, nil, io.EOF
}

func writeDisclosureEvent(w io.Writer, event toolFields, sequence *int64, limit int64) error {
	event["sequence_number"], _ = json.Marshal(*sequence)
	*sequence++
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("tool disclosure SSE event exceeds size limit")
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.string("type"), data)
	return err
}

func (d *toolDisclosure) stream(ctx context.Context, first *http.Response, transport http.RoundTripper, limit int64, w io.Writer) error {
	request := first.Request
	current := first
	var sequence int64
	var visible []json.RawMessage
	var usage disclosureUsage
	responseID := ""
	created := false
	for {
		if current.StatusCode < 200 || current.StatusCode >= 300 {
			_ = current.Body.Close()
			return fmt.Errorf("tool disclosure upstream returned HTTP %d", current.StatusCode)
		}
		mediaType, _, _ := mime.ParseMediaType(current.Header.Get("Content-Type"))
		if mediaType != "text/event-stream" || (current.Header.Get("Content-Encoding") != "" && current.Header.Get("Content-Encoding") != "identity") {
			_ = current.Body.Close()
			return fmt.Errorf("tool disclosure expected an identity SSE response")
		}
		scanner := bufio.NewScanner(current.Body)
		scanner.Buffer(make([]byte, 4096), int(limit)+1)
		indices := make(map[int]int)
		hidden := make(map[int]bool)
		nextIndex := len(visible)
		var completion toolFields
		var output []json.RawMessage
		err := func() error {
			defer current.Body.Close()
			for {
				lines, data, err := readSSEFrame(scanner, limit)
				if err != nil {
					if err == io.EOF {
						return io.ErrUnexpectedEOF
					}
					return err
				}
				if len(data) == 0 {
					if _, err := io.WriteString(w, strings.Join(lines, "\n")+"\n"); err != nil {
						return err
					}
					continue
				}
				if string(data) == "[DONE]" {
					return io.ErrUnexpectedEOF
				}
				var event toolFields
				if json.Unmarshal(data, &event) != nil || event == nil {
					return fmt.Errorf("invalid tool disclosure SSE JSON")
				}
				kind := event.string("type")
				if kind == "response.created" || kind == "response.in_progress" {
					if created {
						continue
					}
					if kind == "response.created" {
						created = true
						var response toolFields
						_ = json.Unmarshal(event["response"], &response)
						responseID = response.string("id")
					}
				}
				if kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
					completion = event
					var response toolFields
					if json.Unmarshal(event["response"], &response) != nil || response == nil {
						return fmt.Errorf("invalid disclosure completion")
					}
					if raw := response["output"]; raw != nil && !bytesNull(raw) {
						if err := json.Unmarshal(raw, &output); err != nil {
							return err
						}
					}
					return nil
				}
				if raw, ok := event["output_index"]; ok {
					var index int
					if err := json.Unmarshal(raw, &index); err != nil {
						return err
					}
					if kind == "response.output_item.added" {
						var item toolFields
						if err := json.Unmarshal(event["item"], &item); err != nil {
							return err
						}
						hidden[index] = d.isLoad(item)
						if !hidden[index] {
							indices[index] = nextIndex
							nextIndex++
						}
					}
					if hidden[index] {
						continue
					}
					mapped, ok := indices[index]
					if !ok {
						return fmt.Errorf("disclosure event references an unknown output index")
					}
					event["output_index"], _ = json.Marshal(mapped)
				}
				for _, line := range lines {
					if line != "" && line != "data" && !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "event:") {
						if _, err := io.WriteString(w, line+"\n"); err != nil {
							return err
						}
					}
				}
				if err := writeDisclosureEvent(w, event, &sequence, limit); err != nil {
					return err
				}
			}
		}()
		if err != nil {
			return err
		}
		part, load, real := d.splitOutput(output)
		visible = append(visible, part...)
		var response toolFields
		_ = json.Unmarshal(completion["response"], &response)
		usage.add(response)
		if !load || real || completion.string("type") != "response.completed" {
			response["output"], _ = json.Marshal(visible)
			if responseID != "" {
				response.setString("id", responseID)
			}
			usage.apply(response)
			completion["response"], _ = json.Marshal(response)
			if err := writeDisclosureEvent(w, completion, &sequence, limit); err != nil {
				return err
			}
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		}
		if err := d.continueAfterLoad(output, response); err != nil {
			return err
		}
		current, err = d.nextResponse(ctx, request, transport, limit)
		if err != nil {
			return err
		}
	}
}
