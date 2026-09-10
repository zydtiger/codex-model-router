package serve

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsNonLoopbackHosts(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "192.168.1.10", "10.0.0.1", "::", "router.example", "localhost.localdomain"} {
		if _, err := New(http.NotFoundHandler(), Options{Host: host, Port: 0}); err == nil {
			t.Fatalf("host %q should be refused before any socket is opened", host)
		}
	}
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		server, err := New(http.NotFoundHandler(), Options{Host: host, Port: 0})
		if err != nil {
			t.Fatalf("host %q should be accepted: %v", host, err)
		}
		if !server.Addr().(*net.TCPAddr).IP.IsLoopback() {
			t.Fatalf("host %q bound to a non-loopback address: %s", host, server.Addr())
		}
		_ = server.Close()
	}
}

func TestPortRangeIsValidated(t *testing.T) {
	if _, err := New(http.NotFoundHandler(), Options{Host: "127.0.0.1", Port: 70000}); err == nil {
		t.Fatal("an out-of-range port should be refused")
	}
	if _, err := New(http.NotFoundHandler(), Options{Host: "127.0.0.1", Port: -1}); err == nil {
		t.Fatal("a negative port should be refused")
	}
}

func TestServeAndServeShutdown(t *testing.T) {
	release := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-release
		}
		_, _ = w.Write([]byte("handled"))
	})
	server, err := New(handler, Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := server.Addr().(*net.TCPAddr).Port
	if port <= 0 {
		t.Fatalf("port 0 should bind a real port, got %d", port)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address("127.0.0.1", port) + "/quick")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(response.Body)
	response.Body.Close()
	if string(body) != "handled" {
		t.Fatalf("body = %q", body)
	}

	// An in-flight request is given the grace window instead of being cut off.
	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		response, err := client.Get("http://" + address("127.0.0.1", port) + "/slow")
		if err != nil {
			return
		}
		readAll(response.Body)
		response.Body.Close()
	}()
	waitForHandler()

	// A context already past its deadline means the grace window is over: the
	// server is force-closed and Serve returns.
	deadline, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Shutdown(deadline); err != nil {
		t.Fatalf("forced shutdown: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v, want a clean stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	close(release)

	// The listener is closed, so a new connection fails.
	if _, err := client.Get("http://" + address("127.0.0.1", port) + "/quick"); err == nil {
		t.Fatal("the listener should be closed")
	}
}

func TestShutdownWaitsForActiveRequest(t *testing.T) {
	finished := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		close(finished)
		_, _ = w.Write([]byte("done"))
	})
	server, err := New(handler, Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := server.Addr().(*net.TCPAddr).Port
	go func() { _ = server.Serve() }()

	client := &http.Client{Timeout: 5 * time.Second}
	responseChan := make(chan *http.Response, 1)
	go func() {
		response, err := client.Get("http://" + address("127.0.0.1", port) + "/")
		if err == nil {
			responseChan <- response
		} else {
			responseChan <- nil
		}
	}()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	response := <-responseChan
	if response == nil {
		t.Fatal("the in-flight request was cut off")
	}
	body := readAll(response.Body)
	response.Body.Close()
	if string(body) != "done" {
		t.Fatalf("body = %q", body)
	}
	select {
	case <-finished:
	default:
		t.Fatal("the handler finished after shutdown returned")
	}
}

func TestGraceWindowExpiryClosesStreams(t *testing.T) {
	block := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-block
	})
	server, err := New(handler, Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := server.Addr().(*net.TCPAddr).Port
	go func() { _ = server.Serve() }()

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address("127.0.0.1", port) + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := server.Shutdown(ctx); err != nil && !strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("shutdown after an expired grace window: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("shutdown took %s; the expired window should force a close", time.Since(started))
	}
	close(block)
}

func TestHealthEndpointReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"version":"0.1.0"}`))
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`nope`))
		case "/not-json":
			_, _ = w.Write([]byte(`<html>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}

	body, err := Health(context.Background(), host, number, "/healthz")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("body = %v", body)
	}
	if _, err := Health(context.Background(), host, number, "/missing"); err == nil {
		t.Fatal("a 404 should be an error")
	}
	if _, err := Health(context.Background(), host, number, "/broken"); err == nil {
		t.Fatal("a 500 should be an error")
	}
	if _, err := Health(context.Background(), host, number, "/not-json"); err == nil {
		t.Fatal("a non-JSON body should be an error")
	}
	if _, err := Health(context.Background(), host, 0, "/healthz"); err == nil {
		t.Fatal("port 0 means nothing is known to probe")
	}
	if _, err := Health(context.Background(), host, 1, "/healthz"); err == nil {
		t.Fatal("an unreachable router should be an error")
	}

	// A cancelled context stops a probe that cannot complete.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Health(ctx, host, number, "/healthz"); err == nil {
		t.Fatal("a cancelled probe should fail")
	}
}

func TestNoWriteTimeoutIsConfigured(t *testing.T) {
	// A fixed write deadline would truncate a long generation. This test pins the
	// decision down.
	server, err := New(http.NotFoundHandler(), Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.http.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want unbounded for streaming responses", server.http.WriteTimeout)
	}
	if server.http.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %s, want unbounded for large request bodies", server.http.ReadTimeout)
	}
	if server.http.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout should be bounded to hold off slow clients")
	}
}

func TestStreamedResponseIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: first\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = w.Write([]byte("event: last\n\n"))
	})
	server, err := New(handler, Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	port := server.Addr().(*net.TCPAddr).Port
	go func() { _ = server.Serve() }()

	response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + address("127.0.0.1", port) + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := make([]byte, 64)
	read := 0
	for read < len(buffer) {
		n, err := response.Body.Read(buffer[read:])
		read += n
		if n > 0 && strings.Contains(string(buffer[:read]), "event: first") {
			break
		}
		if err != nil {
			t.Fatalf("read: %v (%q)", err, buffer[:read])
		}
	}
	if !strings.Contains(string(buffer[:read]), "event: first") {
		t.Fatal("the first event was not delivered while the stream stayed open")
	}
	close(release)
}

// waitForHandler gives the in-flight request a moment to reach its handler.
func waitForHandler() { time.Sleep(120 * time.Millisecond) }

func readAll(reader io.Reader) []byte {
	body, _ := io.ReadAll(reader)
	return body
}

func address(host string, port int) string { return host + ":" + strconv.Itoa(port) }
