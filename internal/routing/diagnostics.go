package routing

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Observe reads without buffering or changing the response. In particular, a
// clean HTTP EOF does not imply that an application-level stream completed.
type responseDiagnostics struct {
	mu                               sync.Mutex
	requestID, contentType, protocol string
	bytes                            int64
	eof                              bool
	err                              error
}

type diagnosticBody struct {
	io.ReadCloser
	d *responseDiagnostics
}

func (d *responseDiagnostics) capture(resp *http.Response) {
	d.requestID = resp.Header.Get("x-oai-request-id")
	if d.requestID == "" {
		d.requestID = resp.Header.Get("x-request-id")
	}
	d.contentType = resp.Header.Get("Content-Type")
	d.protocol = resp.Proto
	resp.Body = &diagnosticBody{ReadCloser: resp.Body, d: d}
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.d.mu.Lock()
	defer b.d.mu.Unlock()
	b.d.bytes += int64(n)
	if err == io.EOF {
		b.d.eof = true
	} else if err != nil {
		b.d.err = err
	}
	return n, err
}

func (d *responseDiagnostics) failed() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err != nil
}

func (h *Router) diagnosticAttrs(s *requestState, status int, result string) []any {
	d := s.diagnostics
	if d == nil {
		d = &responseDiagnostics{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	attrs := []any{
		"route", s.route, "model", s.model, "method", s.method,
		"path", s.rest, "status", status,
		"duration_ms", time.Since(s.started).Milliseconds(), "result", result,
		"upstream_request_id", boundedMetadata(d.requestID),
		"upstream_content_type", boundedMetadata(d.contentType),
		"upstream_protocol", d.protocol, "upstream_bytes", d.bytes,
		"upstream_eof", d.eof,
	}
	if d.err != nil {
		attrs = append(attrs, "error", errorClass(d.err), "error_types", errorTypes(d.err))
		for cause := d.err; cause != nil; cause = errors.Unwrap(cause) {
			// crypto/tls's private alert type formats only a fixed protocol alert,
			// unlike arbitrary provider or transport errors.
			if fmt.Sprintf("%T", cause) == "tls.alert" {
				attrs = append(attrs, "tls_alert", cause.Error())
			}
		}
		var op *net.OpError
		if errors.As(d.err, &op) {
			attrs = append(attrs, "network_op", op.Op, "network", op.Net)
		}
		var call *os.SyscallError
		if errors.As(d.err, &call) {
			attrs = append(attrs, "syscall", call.Syscall)
		}
		var errno syscall.Errno
		if errors.As(d.err, &errno) {
			attrs = append(attrs, "errno", int(errno), "os_error", errno.Error())
		}
		var timeout net.Error
		if errors.As(d.err, &timeout) {
			attrs = append(attrs, "timeout", timeout.Timeout())
		}
	}
	return attrs
}

func boundedMetadata(value string) string {
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

// Error strings can contain request URLs, credentials or provider content.
// Preserve concrete types and safe OS details instead of logging Error().
func errorTypes(err error) string {
	var types []string
	for err != nil {
		types = append(types, fmt.Sprintf("%T", err))
		err = errors.Unwrap(err)
	}
	return strings.Join(types, ":")
}
