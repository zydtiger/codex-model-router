package routing

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// adaptNamespaceResponse buffers only one SSE event at a time. Closing the
// wrapper closes the upstream body, preserving cancellation and backpressure.
func adaptNamespaceResponse(resp *http.Response, mapping *namespaceMapping, limit int64) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("unsupported namespace response content encoding")
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), int(limit)+1)
		resp.Body = &namespaceStream{source: resp.Body, scanner: scanner, mapping: mapping, limit: limit}
	} else {
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if int64(len(body)) > limit {
			return fmt.Errorf("namespace response exceeds size limit")
		}
		restored, err := mapping.restoreResponse(body)
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

type namespaceStream struct {
	source   io.ReadCloser
	scanner  *bufio.Scanner
	mapping  *namespaceMapping
	limit    int64
	pending  []byte
	finished bool
}

func (s *namespaceStream) Close() error { return s.source.Close() }

func (s *namespaceStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.finished {
			return 0, io.EOF
		}
		var lines []string
		var size int64
		for s.scanner.Scan() {
			line := s.scanner.Text()
			size += int64(len(line)) + 1
			if size > s.limit {
				return 0, fmt.Errorf("namespace SSE event exceeds size limit")
			}
			lines = append(lines, line)
			if line == "" {
				break
			}
		}
		if err := s.scanner.Err(); err != nil {
			return 0, err
		}
		if len(lines) == 0 {
			s.finished = true
			return 0, io.EOF
		}
		if lines[len(lines)-1] != "" {
			s.finished = true
		}
		var data []string
		for _, line := range lines {
			if line == "data" {
				data = append(data, "")
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(line[5:], " "))
			}
		}
		payload := strings.Join(data, "\n")
		if len(data) == 0 || strings.TrimSpace(payload) == "[DONE]" || strings.TrimSpace(payload) == "" {
			s.pending = []byte(strings.Join(lines, "\n") + "\n")
			continue
		}
		restored, err := s.mapping.restoreResponse([]byte(payload))
		if err != nil {
			return 0, err
		}
		var out strings.Builder
		written := false
		for _, line := range lines {
			if line == "data" || strings.HasPrefix(line, "data:") {
				if !written {
					out.WriteString("data: ")
					out.Write(restored)
					out.WriteByte('\n')
					written = true
				}
			} else {
				out.WriteString(line)
				out.WriteByte('\n')
			}
		}
		s.pending = []byte(out.String())
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
