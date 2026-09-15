package routing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// adaptCustomResponse restores bridged custom-tool traffic in upstream
// responses. JSON bodies are rewritten whole; SSE streams are rewritten event
// by event with per-call state, bounded buffering, and monotonic sequence
// numbering. It must run after the namespace restoration, because custom
// identities are keyed by (namespace, name).
func adaptCustomResponse(resp *http.Response, mapping *customMapping, limit int64) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("unsupported custom tool response content encoding")
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), int(limit)+1)
		resp.Body = &customStream{source: resp.Body, scanner: scanner, mapping: mapping, limit: limit,
			calls: make(map[string]*customCall), byIndex: make(map[int]*customCall)}
	} else {
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if int64(len(body)) > limit {
			return fmt.Errorf("custom tool response exceeds size limit")
		}
		restored, err := mapping.restoreCustomResponse(body)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(restored))
	}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Del("ETag")
	return nil
}

// customCall tracks one in-flight or completed bridged custom tool call in an
// SSE stream. Completed calls keep only their input, counted against the same
// aggregate bound as pending argument buffers.
type customCall struct {
	identity  toolIdentity
	itemID    string
	callID    string
	index     int
	buffered  string
	input     string
	hasInput  bool
	emitted   bool
	finalized bool
}

type customStream struct {
	source   io.ReadCloser
	scanner  *bufio.Scanner
	mapping  *customMapping
	limit    int64
	pending  []byte
	finished bool

	calls      map[string]*customCall // keyed by upstream item id
	byIndex    map[int]*customCall
	bufferedSz int64
	retained   int64
	sequence   int64
	numbered   bool
}

func (s *customStream) Close() error { return s.source.Close() }

// lookup resolves the call a delta/done event belongs to. Deltas often carry
// only item_id and output_index, so both are registered from the added item.
func (s *customStream) lookup(event toolFields) *customCall {
	if call, ok := s.calls[event.string("item_id")]; ok {
		return call
	}
	var index int
	if err := json.Unmarshal(event["output_index"], &index); err == nil {
		return s.byIndex[index]
	}
	return nil
}

// findCall matches a completed output item to its call by item id, then by
// call id, for the completion-only fallback path.
func (s *customStream) findCall(item toolFields) *customCall {
	if call, ok := s.calls[item.string("id")]; ok {
		return call
	}
	callID := item.string("call_id")
	if callID != "" {
		for _, call := range s.byIndex {
			if call.callID == callID {
				return call
			}
		}
	}
	return nil
}

// register records a call for an added item. A second registration for the
// same slot, or an item id reused by another call, means the stream's item
// identities are ambiguous; refusing keeps interleaved calls correctly
// associated instead of silently overwriting state.
func (s *customStream) register(identity toolIdentity, itemID, callID string, index int) (*customCall, error) {
	if _, ok := s.byIndex[index]; ok {
		return nil, fmt.Errorf("custom tool call output index %d was registered twice", index)
	}
	if itemID != "" {
		if _, ok := s.calls[itemID]; ok {
			return nil, fmt.Errorf("custom tool call item id %q was registered twice", itemID)
		}
	}
	// Charge each tracked call, including empty inputs, against a bounded
	// state budget. The fixed allowance covers the struct and map entries.
	cost := int64(256 + len(itemID) + len(callID) + len(identity.Namespace) + len(identity.Name))
	if s.retained+s.bufferedSz+cost > s.limit {
		return nil, fmt.Errorf("retained custom tool call state exceeds size limit")
	}
	s.retained += cost
	call := &customCall{identity: identity, itemID: itemID, callID: callID, index: index}
	if itemID != "" {
		s.calls[itemID] = call
	}
	s.byIndex[index] = call
	return call, nil
}

// Read mirrors the namespace stream: one SSE frame at a time, never buffering
// the whole response. Suppressed and injected events are compensated by
// renumbering every emitted JSON event's sequence_number monotonically.
func (s *customStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.finished {
			return 0, io.EOF
		}
		lines, data, err := readSSEFrame(s.scanner, s.limit)
		if err != nil {
			if err == io.EOF {
				// A known custom call that never completed cannot be silently
				// dropped from a successful stream.
				if s.unfinalized() {
					return 0, fmt.Errorf("custom tool call stream ended before its input completed")
				}
				s.finished = true
				return 0, io.EOF
			}
			return 0, err
		}
		if len(data) == 0 {
			s.pending = []byte(strings.Join(lines, "\n") + "\n")
			continue
		}
		if string(data) == "[DONE]" {
			if s.unfinalized() {
				return 0, fmt.Errorf("custom tool call stream ended before its input completed")
			}
			s.pending = []byte(strings.Join(lines, "\n") + "\n")
			continue
		}
		var event toolFields
		if json.Unmarshal(data, &event) != nil || event == nil {
			return 0, fmt.Errorf("invalid custom tool SSE JSON")
		}
		// Seed numbering before transformation: a fallback may prepend events
		// to the first numbered upstream frame.
		if !s.numbered && event["sequence_number"] != nil {
			if err := json.Unmarshal(event["sequence_number"], &s.sequence); err != nil {
				return 0, fmt.Errorf("invalid custom tool SSE sequence number")
			}
			s.numbered = true
		}
		events, err := s.transform(event)
		if err != nil {
			return 0, err
		}
		if len(events) == 0 {
			// Keep transport metadata and comments even when the argument
			// payload itself is suppressed.
			var metadata []string
			for _, line := range lines {
				if line != "" && line != "data" && line != "event" && !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "event:") {
					metadata = append(metadata, line)
				}
			}
			if len(metadata) > 0 {
				s.pending = []byte(strings.Join(metadata, "\n") + "\n\n")
			}
			continue
		}
		var out bytes.Buffer
		s.writeFrame(&out, lines, events[0])
		for _, extra := range events[1:] {
			s.writeInjected(&out, extra)
		}
		s.pending = out.Bytes()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// writeFrame rewrites one upstream frame into an output frame for the
// transformed event, synchronizing any event: line with the new data type.
func (s *customStream) writeFrame(out *bytes.Buffer, lines []string, event toolFields) {
	kind := event.string("type")
	data, _ := json.Marshal(event)
	data = s.number(data, event)
	written := false
	for _, line := range lines {
		switch {
		case line == "data" || strings.HasPrefix(line, "data:"):
			if written {
				continue
			}
			out.WriteString("data: ")
			out.Write(data)
			out.WriteByte('\n')
			written = true
		case strings.HasPrefix(line, "event:"):
			out.WriteString("event: " + kind + "\n")
		default:
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
}

// inputDelta derives the matching custom input delta event for a done event.
// The complete validated input travels as one delta, followed by the done
// event; token-by-token custom input streaming is not attempted.
func (s *customStream) inputDelta(done toolFields, input string) toolFields {
	delta := toolFields{}
	delta.setString("type", "response.custom_tool_call_input.delta")
	delta.setString("delta", input)
	if raw := done["item_id"]; raw != nil {
		delta["item_id"] = raw
	}
	if raw := done["output_index"]; raw != nil {
		delta["output_index"] = raw
	}
	if raw := done["sequence_number"]; raw != nil {
		delta["sequence_number"] = raw
	}
	return delta
}

// writeInjected emits a synthesized event, such as the input delta/done pair
// produced by the fallback paths where only a done item or the terminal
// response carried full arguments.
func (s *customStream) writeInjected(out *bytes.Buffer, event toolFields) {
	kind := event.string("type")
	if event["sequence_number"] == nil && s.numbered {
		// Keep the client-visible sequence contiguous with the numbering the
		// upstream started.
		event["sequence_number"] = json.RawMessage("0")
	}
	data, _ := json.Marshal(event)
	data = s.number(data, event)
	out.WriteString("event: " + kind + "\ndata: ")
	out.Write(data)
	out.WriteString("\n\n")
}

// number assigns the next sequence number to a serialized event. Numbering
// starts from the first observed value and increments per emitted event, so
// suppressing deltas or injecting done events can never duplicate or reverse
// the sequence the client observes.
func (s *customStream) number(data []byte, event toolFields) []byte {
	if event["sequence_number"] == nil {
		if !s.numbered && !strings.HasPrefix(event.string("type"), "response.custom_tool_call_input.") {
			return data
		}
		event["sequence_number"] = json.RawMessage("0")
	}
	if !s.numbered {
		var original int64
		_ = json.Unmarshal(event["sequence_number"], &original)
		s.sequence = original
		s.numbered = true
	}
	event["sequence_number"], _ = json.Marshal(s.sequence)
	s.sequence++
	encoded, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return encoded
}

func (s *customStream) unfinalized() bool {
	for _, call := range s.byIndex {
		if !call.finalized {
			return true
		}
	}
	return false
}

// resolveInput validates the arguments source for a completing call, falling
// back to a previously validated input or the accumulated deltas.
func (s *customStream) resolveInput(call *customCall, arguments string) (string, error) {
	switch {
	case arguments != "":
		return unwrapCustomInput(arguments)
	case call.hasInput:
		return call.input, nil
	case call.buffered != "":
		return unwrapCustomInput(call.buffered)
	default:
		return "", fmt.Errorf("custom tool call completed without arguments")
	}
}

// finalize completes a call. The validated input is retained so late duplicate
// events can be compared against it, and its size is charged to the same
// aggregate bound as pending argument buffers.
func (s *customStream) finalize(call *customCall, input string) error {
	s.bufferedSz -= int64(len(call.buffered))
	call.buffered = ""
	call.input = input
	call.hasInput = true
	call.emitted = true
	call.finalized = true
	s.retained += int64(len(input))
	if s.bufferedSz+s.retained > s.limit {
		return fmt.Errorf("retained custom tool call state exceeds size limit")
	}
	return nil
}

// transform rewrites one parsed event into the ordered list of events the
// client should observe: an empty list suppresses the event entirely.
func (s *customStream) transform(event toolFields) ([]toolFields, error) {
	kind := event.string("type")
	switch kind {
	case "response.output_item.added":
		var item toolFields
		if err := json.Unmarshal(event["item"], &item); err != nil {
			return nil, err
		}
		if item.string("type") != "function_call" {
			return []toolFields{event}, nil
		}
		identity := toolIdentity{item.string("namespace"), item.string("name")}
		if !s.mapping.identities[identity] {
			return []toolFields{event}, nil
		}
		var index int
		_ = json.Unmarshal(event["output_index"], &index)
		if _, err := s.register(identity, item.string("id"), item.string("call_id"), index); err != nil {
			return nil, err
		}
		item.setString("type", "custom_tool_call")
		item.setString("input", "")
		delete(item, "arguments")
		event["item"], _ = json.Marshal(item)
		return []toolFields{event}, nil

	case "response.function_call_arguments.delta":
		call := s.lookup(event)
		if call == nil {
			return []toolFields{event}, nil
		}
		if call.finalized {
			// A delta after the arguments completed is malformed for a known
			// custom call; passing it through would leak the wrapper.
			return nil, fmt.Errorf("custom tool call received arguments after completion")
		}
		delta := event.string("delta")
		if s.retained+s.bufferedSz+int64(len(delta)) > s.limit {
			return nil, fmt.Errorf("buffered custom tool arguments exceed size limit")
		}
		call.buffered += delta
		s.bufferedSz += int64(len(delta))
		return nil, nil

	case "response.function_call_arguments.done":
		call := s.lookup(event)
		if call == nil {
			return []toolFields{event}, nil
		}
		arguments := event.string("arguments")
		if call.finalized {
			// Duplicate done: accept it only when it repeats the same input,
			// and never emit a second delta/done pair.
			if arguments == "" {
				return nil, nil
			}
			input, err := unwrapCustomInput(arguments)
			if err != nil || input != call.input {
				return nil, fmt.Errorf("custom tool call arguments contradict the completed input")
			}
			return nil, nil
		}
		if arguments == "" {
			arguments = call.buffered
		}
		input, err := unwrapCustomInput(arguments)
		if err != nil {
			return nil, err
		}
		if err := s.finalize(call, input); err != nil {
			return nil, err
		}
		event.setString("type", "response.custom_tool_call_input.done")
		event.setString("input", input)
		delete(event, "arguments")
		delete(event, "name")
		return []toolFields{s.inputDelta(event, input), event}, nil

	case "response.output_item.done":
		var item toolFields
		if err := json.Unmarshal(event["item"], &item); err != nil {
			return nil, err
		}
		if item.string("type") != "function_call" {
			return []toolFields{event}, nil
		}
		identity := toolIdentity{item.string("namespace"), item.string("name")}
		if !s.mapping.identities[identity] {
			return []toolFields{event}, nil
		}
		var index int
		_ = json.Unmarshal(event["output_index"], &index)
		call := s.byIndex[index]
		if call == nil {
			registered, regErr := s.register(identity, item.string("id"), item.string("call_id"), index)
			if regErr != nil {
				return nil, regErr
			}
			call = registered
		} else if call.identity != identity {
			return nil, fmt.Errorf("custom tool call output index %d changed identity", index)
		}
		input, err := s.resolveInput(call, item.string("arguments"))
		if err != nil {
			return nil, err
		}
		if call.finalized && input != call.input {
			return nil, fmt.Errorf("custom tool call done item contradicts its emitted input")
		}
		item.setString("type", "custom_tool_call")
		item.setString("input", input)
		delete(item, "arguments")
		event["item"], _ = json.Marshal(item)
		if call.emitted {
			return []toolFields{event}, nil
		}
		// Fallback: the input events precede the completed item they belong to.
		done := toolFields{}
		done.setString("type", "response.custom_tool_call_input.done")
		done.setString("input", input)
		if item.string("id") != "" {
			done.setString("item_id", item.string("id"))
		}
		if event["output_index"] != nil {
			done["output_index"] = event["output_index"]
		}
		if err := s.finalize(call, input); err != nil {
			return nil, err
		}
		return []toolFields{s.inputDelta(done, input), done, event}, nil

	case "response.completed":
		events, err := s.completeFallback(event)
		if err != nil {
			return nil, err
		}
		if err := s.mapping.restoreCustomFields(event); err != nil {
			return nil, err
		}
		if s.unfinalized() {
			return nil, fmt.Errorf("custom tool call missing from completed response")
		}
		return append(events, event), nil

	case "response.failed", "response.incomplete":
		if err := s.mapping.restoreCustomFields(event); err != nil {
			return nil, err
		}
		return []toolFields{event}, nil

	default:
		if err := s.mapping.restoreCustomFields(event); err != nil {
			return nil, err
		}
		return []toolFields{event}, nil
	}
}

// completeFallback resolves bridged custom calls that only the terminal
// response's output mentions: either no added event identified them, or their
// per-call events never arrived. Synthesized input events are emitted before
// the terminal event, and a payload contradicting an already emitted input is
// an error rather than a second, conflicting input.
func (s *customStream) completeFallback(event toolFields) ([]toolFields, error) {
	var response toolFields
	if err := json.Unmarshal(event["response"], &response); err != nil || response == nil {
		return nil, fmt.Errorf("invalid completion envelope")
	}
	var output []json.RawMessage
	if raw := response["output"]; raw != nil && !bytesNull(raw) {
		if err := json.Unmarshal(raw, &output); err != nil {
			return nil, err
		}
	}
	var events []toolFields
	for i, raw := range output {
		var item toolFields
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		if item.string("type") != "function_call" {
			continue
		}
		identity := toolIdentity{item.string("namespace"), item.string("name")}
		if !s.mapping.identities[identity] {
			continue
		}
		call := s.findCall(item)
		if call == nil {
			// Some Responses servers regenerate both ids in the terminal
			// envelope. The output position still identifies the streamed
			// item; require matching tool identity and validated input below.
			call = s.byIndex[i]
		}
		if call != nil && call.identity != identity {
			return nil, fmt.Errorf("completed custom tool call changed identity")
		}
		input, err := s.resolveInputFor(call, item)
		if err != nil {
			return nil, err
		}
		// Persist the resolved input before generic envelope restoration.
		// Some upstreams omit final arguments after streaming them earlier.
		item.setString("type", "custom_tool_call")
		item.setString("input", input)
		delete(item, "arguments")
		if call != nil {
			if call.itemID != "" {
				item.setString("id", call.itemID)
			}
			if call.callID != "" {
				item.setString("call_id", call.callID)
			}
		}
		output[i], _ = json.Marshal(item)
		if call != nil && call.finalized {
			if call.input != input {
				return nil, fmt.Errorf("completed custom tool call contradicts its emitted input")
			}
			continue
		}
		if call == nil {
			// Completion-only fallback without a previous added item. The
			// item's output position stands in for its output index.
			call, err = s.register(identity, item.string("id"), item.string("call_id"), i)
			if err != nil {
				return nil, err
			}
		}
		done := toolFields{}
		done.setString("type", "response.custom_tool_call_input.done")
		done.setString("input", input)
		if item.string("id") != "" {
			done.setString("item_id", item.string("id"))
		}
		done["output_index"], _ = json.Marshal(call.index)
		if err := s.finalize(call, input); err != nil {
			return nil, err
		}
		events = append(events, s.inputDelta(done, input), done)
	}
	response["output"], _ = json.Marshal(output)
	event["response"], _ = json.Marshal(response)
	return events, nil
}

// resolveInputFor validates the completion item's arguments. An item whose
// arguments are empty falls back to the call's own buffered or emitted state,
// because some servers stream arguments only in per-call events.
func (s *customStream) resolveInputFor(call *customCall, item toolFields) (string, error) {
	arguments := item.string("arguments")
	if arguments != "" {
		return unwrapCustomInput(arguments)
	}
	if call != nil {
		return s.resolveInput(call, "")
	}
	return "", fmt.Errorf("custom tool call completed without arguments")
}
