package routing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenDiagnosticBody struct{ io.Reader }

func (b brokenDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (brokenDiagnosticBody) Close() error { return nil }

func TestDiagnosticErrorsUseConfiguredLoggerWithoutRequestLogging(t *testing.T) {
	router, logs := newRouter(t, strings.ReplaceAll(nativeChatGPTConfigTemplate, "%q", `"http://127.0.0.1:1"`), nil)
	router.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	router.api.proxy.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://secret-token@example.com/private", Err: &net.OpError{
			Op: "read", Net: "tcp", Err: syscall.ECONNRESET,
		}}
	})
	res := doRequest(router, http.MethodPost, "/v1/responses", []byte(`{"model":"gpt-5","input":"private conversation"}`), map[string]string{"Authorization": "Bearer test-secret"})
	if res.Code != http.StatusBadGateway {
		t.Fatal(res.Code)
	}
	if router.countUpstreamErrs.Load() != 1 {
		t.Fatal("transport failure missing from health counter")
	}
	for _, want := range []string{"level=WARN", `msg="upstream request failed"`, "status=502", "network_op=read", "errno=", "*url.Error:*net.OpError:syscall.Errno"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in %s", want, logs)
		}
	}
	for _, secret := range []string{"secret-token", "example.com", "private conversation"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

func TestDiagnosticNativeStreamFailureLogsWithoutRequestLogging(t *testing.T) {
	router, logs := newRouter(t, strings.ReplaceAll(nativeChatGPTConfigTemplate, "%q", `"http://127.0.0.1:1"`), nil)
	router.api.proxy.Transport = diagnosticTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Request: r, Proto: "HTTP/2.0",
			Header: http.Header{"X-Oai-Request-Id": {"req-truncated"}},
			Body:   brokenDiagnosticBody{Reader: strings.NewReader("private streamed content")}}, nil
	})
	doRequest(router, http.MethodPost, "/v1/responses", []byte(`{"model":"gpt-5"}`), map[string]string{"Authorization": "Bearer test-secret"})
	for _, want := range []string{"level=WARN", "result=stream_error", "error=unexpected_eof", "req-truncated", "upstream_eof=false"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q: %s", want, logs)
		}
	}
	if strings.Contains(logs.String(), "private streamed content") || router.countUpstreamErrs.Load() != 1 {
		t.Fatal("stream logging leaked content or failed to count the error")
	}
}

func TestDiagnosticCancellationIsNotAnUpstreamFailure(t *testing.T) {
	router, logs := newRouter(t, strings.ReplaceAll(nativeChatGPTConfigTemplate, "%q", `"http://127.0.0.1:1"`), nil)
	router.api.proxy.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	doRequest(router, http.MethodPost, "/v1/responses", []byte(`{"model":"gpt-5"}`), map[string]string{"Authorization": "Bearer test-secret"})
	if router.countUpstreamErrs.Load() != 0 || strings.Contains(logs.String(), "upstream request failed") {
		t.Fatal("client cancellation counted as upstream failure")
	}
}

func TestDiagnosticBodyRecordsTruncatedStreamWithoutChangingBytes(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		payload := "data: {\"type\":\"response.created\"}\n\n"
		var body io.ReadCloser = io.NopCloser(strings.NewReader(payload))
		if truncated {
			body = brokenDiagnosticBody{Reader: strings.NewReader(payload)}
		}
		d := responseDiagnostics{}
		resp := &http.Response{Body: body, Proto: "HTTP/2.0", Header: http.Header{
			"X-Oai-Request-Id": {"req-test"}, "Content-Type": {"text/event-stream"},
			"Set-Cookie": {"secret-cookie"},
		}}
		d.capture(resp)
		got, err := io.ReadAll(resp.Body)
		if string(got) != payload || d.bytes != int64(len(payload)) || d.eof == truncated {
			t.Fatalf("response changed or incorrect diagnostics: %q %+v", got, &d)
		}
		if truncated && (!errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(d.err, err)) {
			t.Fatalf("lost read error: %v %+v", err, &d)
		}
		logs := &bytes.Buffer{}
		router := &Router{log: slog.New(slog.NewTextHandler(logs, nil))}
		router.logActivity(&requestState{diagnostics: &d}, 200, "ok")
		for _, want := range []string{"req-test", "text/event-stream", "HTTP/2.0"} {
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("missing %q", want)
			}
		}
		if strings.Contains(logs.String(), "secret-cookie") || strings.Contains(logs.String(), "response.created") {
			t.Fatal("response content leaked")
		}
	}
}

func TestDiagnosticSnapshotWhileAdapterReads(t *testing.T) {
	r, w := io.Pipe()
	d := &responseDiagnostics{}
	resp := &http.Response{Body: r, Header: http.Header{}}
	d.capture(resp)
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()
	router := &Router{}
	state := &requestState{diagnostics: d}
	for range 100 {
		if _, err := w.Write([]byte("private stream")); err != nil {
			t.Fatal(err)
		}
		_ = router.diagnosticAttrs(state, 200, "cancelled")
		_ = d.failed()
	}
	_ = w.CloseWithError(io.ErrUnexpectedEOF)
	if err := <-done; !errors.Is(err, io.ErrUnexpectedEOF) || !d.failed() {
		t.Fatalf("missing adapter read failure: %v", err)
	}
	_ = resp.Body.Close()
}
