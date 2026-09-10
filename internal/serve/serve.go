// Package serve owns the loopback listener: binding, verifying that it really is
// loopback, and shutting down in-flight requests.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Options control the listener.
type Options struct {
	// Host must be a loopback name or address. Anything else is refused.
	Host string
	Port int
	// ReadHeaderTimeout bounds the request header phase. It is intentionally the
	// only deadline applied to a connection: request bodies can be large and
	// streamed responses must not be cut off.
	ReadHeaderTimeout time.Duration
	// IdleTimeout closes keep-alive connections that go quiet.
	IdleTimeout time.Duration
	// OnReady receives the bound address once the listener is open.
	OnReady func(net.Addr)
}

// Server is a running loopback HTTP server.
type Server struct {
	listener net.Listener
	http     *http.Server
	addr     net.Addr
}

// ErrNotLoopback is returned before any socket is opened when the requested host
// is not loopback, and again if a bound address turns out to be reachable from
// outside the machine.
var ErrNotLoopback = errors.New("the router must listen on a loopback address")

// New binds the listener and builds the server without accepting connections.
func New(handler http.Handler, options Options) (*Server, error) {
	host := options.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopbackName(host) {
		return nil, fmt.Errorf("%w: %q", ErrNotLoopback, host)
	}
	if options.Port < 0 || options.Port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", options.Port)
	}

	address := net.JoinHostPort(host, fmt.Sprint(options.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	if !isLoopbackAddr(listener.Addr()) {
		_ = listener.Close()
		return nil, fmt.Errorf("%w: bound to %s", ErrNotLoopback, listener.Addr())
	}
	readHeaderTimeout := options.ReadHeaderTimeout
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 15 * time.Second
	}
	idleTimeout := options.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// ReadTimeout and WriteTimeout stay unset. A request body may be tens of
		// megabytes and a Responses stream stays open for the whole generation;
		// a fixed deadline would cut off legitimate work.
		ErrorLog: nil,
	}
	return &Server{listener: listener, http: server, addr: listener.Addr()}, nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.addr }

// Serve accepts connections until the server is shut down.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting new connections and waits for in-flight requests,
// including open streams, until ctx is done. When the grace window expires the
// remaining connections are force-closed; that is reported as a completed
// shutdown because the caller chose the window.
//
// http.Server owns the listener once Serve has started, so this method does not
// close it as well.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return s.Close()
	}
	err := s.http.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, http.ErrServerClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("shutdown failed: %w", err)
}

// Close stops the server without waiting for active requests. It is safe to call
// before Serve has started, which is what releases a socket after a failed
// startup check.
func (s *Server) Close() error {
	err := s.http.Close()
	if closeErr := s.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return closeErr
	}
	return err
}

// Health fetches /healthz from a bound router.
func Health(ctx context.Context, host string, port int, path string) (map[string]any, error) {
	if port <= 0 {
		return nil, errors.New("a running router port is required")
	}
	if path == "" {
		path = "/healthz"
	}
	requestURL := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(port)), Path: path}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			// No proxy: a health probe must reach the local process directly.
			Proxy: nil,
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("health request to %s failed: %w", requestURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", requestURL, response.Status)
	}
	body := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode health response from %s: %w", requestURL, err)
	}
	return body, nil
}

func isLoopbackName(host string) bool {
	switch host {
	case "", "localhost", "ip6-localhost", "localhost.":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackAddr(addr net.Addr) bool {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcpAddr.IP.IsLoopback()
}
