// Package routing implements the loopback HTTP handler that routes Codex
// Responses requests to native upstreams or to configured self-hosted servers.
package routing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// Version is reported by the health endpoint and the version command.
const Version = "0.1.0"

// HealthPath is the only route outside the configured base path.
const HealthPath = "/healthz"

// responsesSuffix is the model endpoint namespace Codex appends to
// openai_base_url. Only this namespace is forwarded; every other path is
// refused so the process cannot be used as a general proxy.
const responsesSuffix = "/responses"

// accountIDHeader selects the native ChatGPT backend. Its presence, not a
// credential file, is what identifies an account session.
const accountIDHeader = "Chatgpt-Account-Id"

// Non-standard status codes used by the router.
const (
	// statusClientClosed records a request whose client went away.
	statusClientClosed = 499
	// statusMisusedHost rejects a Host header the listener cannot serve, which
	// is the DNS-rebinding case.
	statusMisusedHost = 421
)

// defaultResponseHeaderTimeoutSeconds bounds the wait for upstream response
// headers. Generation time is never bounded: headers arrive first.
const defaultResponseHeaderTimeoutSeconds = 120

// envLookup resolves an environment variable name to its value.
type envLookup func(string) (string, bool)

// Options wires the router to its surroundings.
type Options struct {
	Logger *slog.Logger
	Env    envLookup
	// Transport overrides the upstream transport, for tests. When set it is
	// shared by every upstream and per-route header timeouts are ignored.
	Transport http.RoundTripper
	// ChatGPTProxy overrides only the native ChatGPT transport proxy lookup.
	ChatGPTProxy func(*http.Request) (*url.URL, error)
	// BoundAddr is the address the listener actually opened.
	BoundAddr string
}

// Router is the http.Handler for the loopback listener.
type Router struct {
	cfg    *config.Config
	log    *slog.Logger
	env    envLookup
	bound  string
	target map[string]*target // keyed by route name
	chat   *target
	api    *target

	allowedHosts map[string]bool

	countRequests     atomic.Uint64
	countNative       atomic.Uint64
	countRemote       atomic.Uint64
	countRejected     atomic.Uint64
	countUpstreamErrs atomic.Uint64
	last              atomic.Value // routeSnapshot
	started           time.Time

	activityMu sync.Mutex
}

// target is one upstream plus the reverse proxy that reaches it.
type target struct {
	name      string
	base      *url.URL
	route     *config.Route // nil for the native targets
	proxy     *httputil.ReverseProxy
	transport http.RoundTripper
	kind      targetKind
	stripV1   bool
}

type targetKind int

const (
	kindRemote targetKind = iota
	kindNativeChatGPT
	kindNativeAPI
)

type routeSnapshot struct {
	Model          string `json:"model,omitempty"`
	Route          string `json:"route,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
}

// requestState carries the per-request routing decision through the reverse
// proxy hooks.
type requestState struct {
	started       time.Time
	method        string
	rest          string
	model         string
	route         string
	target        *target
	body          []byte
	headers       http.Header
	status        int
	result        string
	warnings      []string
	namespaces    *namespaceMapping
	disclosure    *toolDisclosure
	responseLimit int64
}

type stateKey struct{}

// New compiles the configuration into upstream targets and reverse proxies.
func New(cfg *config.Config, options Options) (*Router, error) {
	if cfg == nil {
		return nil, errors.New("routing: configuration is required")
	}
	if err := cfg.LoadRuntimeCatalog(); err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	env := options.Env
	if env == nil {
		env = os.LookupEnv
	}
	router := &Router{
		cfg:          cfg,
		log:          logger,
		env:          env,
		bound:        options.BoundAddr,
		target:       make(map[string]*target, len(cfg.Routes)),
		allowedHosts: loopbackHostNames(cfg.Listen.Host),
		started:      time.Now(),
	}
	if router.bound == "" {
		router.bound = cfg.Addr()
	}

	chat, err := newTarget("native-chatgpt", cfg.Native.ChatGPTBaseURL, kindNativeChatGPT, nil, options)
	if err != nil {
		return nil, err
	}
	api, err := newTarget("native-api", cfg.Native.APIBaseURL, kindNativeAPI, nil, options)
	if err != nil {
		return nil, err
	}
	router.chat, router.api = chat, api

	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		remote, err := newTarget(route.Name, route.BaseURL, kindRemote, route, options)
		if err != nil {
			return nil, err
		}
		router.target[route.Name] = remote
	}
	return router, nil
}

// discardHandler is used where a standard-library component insists on a
// *log.Logger: the router does its own logging.
func discardHandler() slog.Handler {
	return slog.NewTextHandler(io.Discard, nil)
}

func newTarget(name, rawBase string, kind targetKind, route *config.Route, options Options) (*target, error) {
	base, err := url.Parse(rawBase)
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", name, err)
	}
	upstream := &target{name: name, base: base, route: route, kind: kind, stripV1: kind == kindNativeChatGPT}
	transport := options.Transport
	if transport == nil {
		defaults, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("target " + name + ": the default transport has been replaced")
		}
		clone := defaults.Clone()
		if kind == kindNativeChatGPT && options.ChatGPTProxy != nil {
			clone.Proxy = options.ChatGPTProxy
		}
		clone.ResponseHeaderTimeout = time.Duration(headerTimeoutSeconds(route)) * time.Second
		clone.MaxIdleConnsPerHost = 4
		transport = clone
	}
	upstream.transport = transport
	upstream.proxy = &httputil.ReverseProxy{
		Rewrite:        upstream.rewrite,
		ModifyResponse: upstream.modifyResponse,
		ErrorHandler:   upstream.handleError,
		Transport:      transport,
		// Flush every chunk so upstream SSE events reach Codex as they are
		// produced instead of waiting for a buffer to fill.
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(discardHandler(), slog.LevelError),
	}
	return upstream, nil
}

// headerTimeoutSeconds resolves the per-route header timeout. A negative value
// disables the bound entirely.
func headerTimeoutSeconds(route *config.Route) int {
	if route == nil {
		return defaultResponseHeaderTimeoutSeconds
	}
	if route.ResponseHeaderTimeoutSeconds < 0 {
		return 0
	}
	if route.ResponseHeaderTimeoutSeconds == 0 {
		return defaultResponseHeaderTimeoutSeconds
	}
	return route.ResponseHeaderTimeoutSeconds
}

// ServeHTTP implements the router.
func (h *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if !isLoopbackPeer(r.RemoteAddr) {
		h.reject(w, http.StatusForbidden, "peer_not_loopback", "only loopback clients are accepted")
		return
	}
	if !h.hostAllowed(r.Host) {
		h.reject(w, statusMisusedHost, "host_not_loopback", "the Host header must name the loopback address")
		return
	}

	if r.URL.Path == HealthPath {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		h.writeHealth(w, r)
		return
	}

	rest, ok := trimBasePath(r.URL.Path, h.cfg.BasePath)
	if !ok || rest == "" {
		h.reject(w, http.StatusNotFound, "not_found", "no route matches this path")
		return
	}
	if isWebSocketUpgrade(r) {
		// Codex reads 426 as "stop offering WebSocket for this session" and
		// falls back to plain HTTP Responses, where per-request routing happens.
		// This router does not implement a WebSocket transport.
		h.log.Info("websocket upgrade refused", "path", rest, "result", "http_fallback")
		h.reject(w, http.StatusUpgradeRequired, "upgrade_required", "this router serves the HTTP Responses transport")
		return
	}
	if rest != responsesSuffix && !strings.HasPrefix(rest, responsesSuffix+"/") {
		h.reject(w, http.StatusNotFound, "not_found", "only the Responses endpoint is proxied")
		return
	}
	if r.Method != http.MethodPost {
		h.methodNotAllowed(w, http.MethodPost)
		return
	}

	body, err := readBody(r, h.cfg.MaxRequestBytes)
	if err != nil {
		h.rejectRequestError(w, err, started, r.Method, rest, "")
		return
	}
	// A Responses request is always a JSON object. Checking the shape here gives
	// Codex a specific error instead of a confusing one from the upstream.
	fields, shapeErr := decodeJSON(body.Decoded)
	if shapeErr != nil {
		h.rejectRequestError(w, shapeErr, started, r.Method, rest, "")
		return
	}
	model, hasModel := modelFromFields(fields)
	if !hasModel {
		h.rejectRequestError(w, clientError(http.StatusBadRequest, "model_required", "the request body must name a model"),
			started, r.Method, rest, "")
		return
	}

	state := &requestState{started: started, method: r.Method, rest: rest, model: model}
	upstream, decisionErr := h.selectTarget(r, model, body, fields, state)
	if decisionErr != nil {
		h.rejectRequestError(w, decisionErr, started, r.Method, rest, model)
		return
	}
	state.target = upstream

	proxyRequest := r.WithContext(context.WithValue(r.Context(), stateKey{}, state))
	recorder := &responseRecorder{ResponseWriter: w}
	aborted := h.serveProxy(upstream, recorder, proxyRequest)
	h.finish(recorder, proxyRequest, state, aborted)
}

// selectTarget applies the only two permitted destinations: a configured remote
// route for an exact model ID, or a trusted native upstream for a model ID that
// the configuration declares native. Anything else is refused, so a model that
// should stay self-hosted can never reach a native API.
func (h *Router) selectTarget(r *http.Request, model string, body *decodedBody, fields map[string]json.RawMessage, state *requestState) (*target, *requestError) {
	if route, ok := h.cfg.RouteFor(model); ok {
		upstream, exists := h.target[route.Name]
		if !exists {
			return nil, clientError(http.StatusServiceUnavailable, "route_unavailable", "route %q is not available", route.Name)
		}
		translated, stats, err := translateRemoteBody(fields, route)
		if err != nil {
			return nil, err
		}
		adapted, err := applyReasoningAdapter(translated, route)
		if err != nil {
			return nil, err
		}
		if route.Reasoning.Adapter == config.AdapterSGLangChatTemplate {
			mapping, err := flattenNamespaces(translated)
			if err != nil {
				return nil, err
			}
			state.namespaces = mapping
			state.disclosure = prepareToolDisclosure(translated, mapping)
			if state.disclosure != nil {
				state.disclosure.reasoningPolicy = route.Input.ReasoningItems
				for _, key := range []string{"previous_response_id", "conversation"} {
					if raw := translated[key]; raw != nil && !bytesNull(raw) {
						return nil, clientError(http.StatusBadRequest, "disclosure_requires_history", "namespace tool disclosure requires replayed input instead of %s", key)
					}
				}
			}
			state.responseLimit = h.cfg.MaxRequestBytes
		}
		encoded, err := encodeJSON(translated)
		if err != nil {
			return nil, err
		}
		if int64(len(encoded)) > h.cfg.MaxRequestBytes {
			return nil, clientError(http.StatusRequestEntityTooLarge, "request_too_large", "translated request body exceeds size limit")
		}
		headers, err := buildRemoteHeaders(r.Header, route, h.env)
		if err != nil {
			return nil, err
		}
		if state.namespaces != nil {
			headers.Set("Accept-Encoding", "identity")
		}
		state.body = encoded
		state.headers = headers
		state.route = route.Name
		state.warnings = stats.Warnings()
		if adapted || stats.Changed() {
			h.log.Debug("translated request", "route", route.Name, "model", model, "translated", true)
		}
		h.countRequests.Add(1)
		h.countRemote.Add(1)
		return upstream, nil
	}

	if !h.cfg.IsNativeModel(model) {
		// Refusing an unlisted ID is what keeps a self-hosted model ID from reaching
		// a native API. There is no configuration that sends unlisted IDs native.
		return nil, clientError(http.StatusBadRequest, "unknown_model",
			"model %q is not served by this router; add a route for it or list it under native.models", model)
	}

	isAccountSession := strings.TrimSpace(r.Header.Get(accountIDHeader)) != ""
	upstream, routeName := h.api, "native-api"
	if isAccountSession {
		upstream, routeName = h.chat, "native-chatgpt"
	}
	headers, err := applyNativeHeaders(r.Header, h.cfg.PreserveClientAuth(), h.cfg.Native.APIKeyEnv, h.env)
	if err != nil {
		return nil, err
	}
	state.headers = headers
	// Native upstreams receive the body byte-for-byte as Codex sent it.
	state.body = body.Raw
	state.route = routeName
	h.countRequests.Add(1)
	h.countNative.Add(1)
	return upstream, nil
}

// modelFromFields reads the exact model ID from decoded top-level fields.
func modelFromFields(fields map[string]json.RawMessage) (string, bool) {
	raw, ok := fields["model"]
	if !ok {
		return "", false
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return "", false
	}
	// Routing is by exact model ID, so the ID is used as the client sent it.
	return model, model != ""
}

// rewrite is the ReverseProxy hook that builds the outbound request.
func (t *target) rewrite(pr *httputil.ProxyRequest) {
	state, _ := pr.In.Context().Value(stateKey{}).(*requestState)
	if state == nil {
		// Unreachable: ServeHTTP always attaches state before proxying.
		pr.SetURL(t.base)
		return
	}
	pr.Out.URL = t.targetURL(state.rest, pr.In.URL.RawQuery)
	// An empty Host lets the outbound Host header follow the upstream URL.
	pr.Out.Host = ""
	pr.Out.Body = io.NopCloser(bytes.NewReader(state.body))
	pr.Out.ContentLength = int64(len(state.body))
	if state.headers != nil {
		pr.Out.Header = state.headers.Clone()
	}
	if t.kind == kindRemote {
		// The translated body is uncompressed JSON.
		pr.Out.Header.Del("Content-Encoding")
	}
	// X-Forwarded-* is deliberately unset: the upstreams are loopback-only or
	// configured services and never need the caller's loopback identity.
}

// targetURL joins the upstream base with the request remainder.
//
// Codex appends /responses to openai_base_url, so rest never contains the base
// path. OpenAI-compatible servers expose that endpoint under /v1, while the
// native ChatGPT backend exposes it directly under its own namespace. The
// helpers below keep the two shapes correct without ever doubling /v1.
func (t *target) targetURL(rest, rawQuery string) *url.URL {
	basePath := strings.TrimRight(t.base.Path, "/")
	suffix := rest
	switch t.kind {
	case kindNativeChatGPT:
		// No /v1 namespace on this upstream.
	case kindNativeAPI, kindRemote:
		if !strings.HasSuffix(basePath, "/v1") {
			suffix = "/v1" + rest
		}
	}
	if strings.HasPrefix(suffix, "/v1/") && strings.HasSuffix(basePath, "/v1") {
		suffix = strings.TrimPrefix(suffix, "/v1")
	}
	joined := *t.base
	joined.Path = basePath + suffix
	joined.RawPath = ""
	joined.RawQuery = rawQuery
	joined.Fragment = ""
	return &joined
}

// modifyResponse records the upstream status for logging.
func (t *target) modifyResponse(resp *http.Response) error {
	if state, ok := resp.Request.Context().Value(stateKey{}).(*requestState); ok && state != nil {
		state.status = resp.StatusCode
		if state.disclosure != nil {
			if err := adaptDisclosureResponse(resp, state.disclosure, t.transport, state.responseLimit); err != nil {
				return err
			}
		}
		if state.namespaces != nil {
			return adaptNamespaceResponse(resp, state.namespaces, state.responseLimit)
		}
	}
	return nil
}

func (t *target) handleError(w http.ResponseWriter, r *http.Request, err error) {
	state, _ := r.Context().Value(stateKey{}).(*requestState)
	status := http.StatusBadGateway
	message := "the upstream request failed"
	switch {
	case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
		status = statusClientClosed
		message = "the request was cancelled before the upstream responded"
	case isUnreachable(err):
		message = fmt.Sprintf("the upstream for %s could not be reached", t.name)
	}
	if state != nil {
		state.status = status
		state.result = "upstream_error"
	}
	// The underlying error is logged without the request body or headers.
	if status == http.StatusBadGateway {
		// Only the error class is recorded: transport errors can echo a URL,
		// never credentials, but model content is never included.
		slog.Default().Debug("upstream request failed", "target", t.name, "error", errorClass(err))
	}
	writeJSONError(w, status, "upstream_error", message)
}

// errorClass reduces a transport error to a stable, non-sensitive label.
func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case isUnreachable(err):
		return "host_unreachable"
	case errors.Is(err, io.EOF):
		return "connection_closed"
	default:
		return "transport_error"
	}
}

func isUnreachable(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// serveProxy runs the reverse proxy and reports a post-header abort.
// ReverseProxy panics with http.ErrAbortHandler when a streamed body fails after
// its headers were sent; recovering here keeps the terminal log line and avoids
// a misleading 500 from the server.
func (h *Router) serveProxy(upstream *target, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if recovered != http.ErrAbortHandler {
			panic(recovered)
		}
		aborted = true
	}()
	upstream.proxy.ServeHTTP(w, r)
	return
}

func (h *Router) finish(recorder *responseRecorder, r *http.Request, state *requestState, aborted bool) {
	status := state.status
	result := state.result
	if result == "" {
		switch {
		case r.Context().Err() != nil:
			result = "cancelled"
		case aborted, recorder.writeErr != nil:
			result = "stream_error"
			h.countUpstreamErrs.Add(1)
		case status == 0:
			status = http.StatusBadGateway
			result = "no_response"
			h.countUpstreamErrs.Add(1)
		case status >= http.StatusInternalServerError:
			result = "upstream_error"
			h.countUpstreamErrs.Add(1)
		default:
			result = "ok"
		}
	}
	h.last.Store(routeSnapshot{Model: state.model, Route: state.route, UpstreamStatus: status})
	for _, warning := range state.warnings {
		h.log.Warn("request translation dropped provider state", "route", state.route, "model", state.model, "detail", warning)
	}
	if recorder.writeErr != nil {
		h.log.Warn("response write failed", "route", state.route, "model", state.model, "error", recorder.writeErr)
	}
	if h.cfg.Log.Requests {
		h.logActivity(state, status, result)
	}
}

// logActivity writes one line per request, carrying routing fields only.
func (h *Router) logActivity(state *requestState, status int, result string) {
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	h.log.Info("routed",
		"route", state.route,
		"model", state.model,
		"method", state.method,
		"path", state.rest,
		"status", status,
		"duration_ms", time.Since(state.started).Milliseconds(),
		"result", result,
	)
}

func (h *Router) reject(w http.ResponseWriter, status int, code, message string) {
	h.countRejected.Add(1)
	writeJSONError(w, status, code, message)
}

func (h *Router) rejectRequestError(w http.ResponseWriter, err *requestError, started time.Time, method, rest, model string) {
	h.countRejected.Add(1)
	if h.cfg.Log.Requests {
		h.logActivity(&requestState{started: started, method: method, rest: rest, model: model}, err.status, "rejected")
	}
	writeJSONError(w, err.status, err.code, err.message)
}

func (h *Router) methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	h.countRejected.Add(1)
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint only accepts "+strings.Join(allowed, " or "))
}

func (h *Router) writeHealth(w http.ResponseWriter, r *http.Request) {
	encoded, err := json.Marshal(h.Health())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "health_failed", "health encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(append(encoded, '\n'))
	}
}

// Health is the /healthz document. It reports routing counts and never
// credentials, request content, or environment values.
func (h *Router) Health() map[string]any {
	routes := make([]string, 0, len(h.cfg.Routes))
	for _, route := range h.cfg.Routes {
		routes = append(routes, route.Name)
	}
	sort.Strings(routes)
	last := routeSnapshot{}
	if value := h.last.Load(); value != nil {
		last = value.(routeSnapshot)
	}
	return map[string]any{
		"ok":              true,
		"version":         Version,
		"bound":           h.bound,
		"base_path":       h.cfg.BasePath,
		"openai_base_url": h.cfg.BaseURL(),
		"routes":          routes,
		"remote_models":   len(h.cfg.RoutedModelIDs()),
		"native_models":   len(h.cfg.NativeModelIDs()),
		"go":              runtime.Version(),
		"uptime_seconds":  int64(time.Since(h.started).Seconds()),
		"requests":        h.countRequests.Load(),
		"native_requests": h.countNative.Load(),
		"remote_requests": h.countRemote.Load(),
		"rejected":        h.countRejected.Load(),
		"upstream_errors": h.countUpstreamErrs.Load(),
		"last_model":      last.Model,
		"last_route":      last.Route,
		"last_status":     last.UpstreamStatus,
	}
}

// responseRecorder captures the upstream status and write failures for logging.
type responseRecorder struct {
	http.ResponseWriter
	status   int
	writeErr error
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	n, err := r.ResponseWriter.Write(body)
	if err != nil {
		r.writeErr = err
	}
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

// writeJSONError uses a Responses-style error envelope so Codex reports the
// reason instead of an opaque transport failure.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": "error", "code": code, "message": message},
	})
	if err != nil {
		body = []byte(`{"error":{"type":"error","message":"error encoding failed"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func trimBasePath(path, base string) (string, bool) {
	if base == "" || base == "/" {
		return path, true
	}
	if path == base {
		return "", true
	}
	if !strings.HasPrefix(path, base+"/") {
		return "", false
	}
	return strings.TrimPrefix(path, base), true
}

func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") || strings.EqualFold(host, "ip6-localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackHostNames lists the Host values the listener accepts. Checking Host
// stops a browser on this machine from driving the router through a rebinding
// DNS name even though its connection is loopback.
func loopbackHostNames(listenHost string) map[string]bool {
	names := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "ip6-localhost": true}
	if listenHost != "" {
		names[strings.Trim(listenHost, "[]")] = true
	}
	return names
}

func (h *Router) hostAllowed(host string) bool {
	name := host
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		name = splitHost
	} else {
		name = strings.Trim(host, "[]")
	}
	if h.allowedHosts[name] {
		return true
	}
	if ip := net.ParseIP(strings.Trim(name, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, raw := range r.Header.Values("Connection") {
		for _, token := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// SetBoundAddr records the address the listener actually opened. The health
// endpoint and the generated openai_base_url both report it, which matters when
// the configuration asked for port 0.
func (h *Router) SetBoundAddr(address string) {
	if strings.TrimSpace(address) == "" {
		return
	}
	h.bound = strings.TrimSpace(address)
}
