package routing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zydtiger/codex-model-router/internal/config"
)

// blockedUpstream is an address that cannot answer: a test that leaves a native
// upstream unset must never be able to reach a real provider endpoint.
const blockedUpstream = "http://127.0.0.1:1"

// upstream records what the router forwarded so a test can assert on the exact
// request an upstream server would see.
type upstream struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []recordedRequest
	// handler overrides the default response.
	handler http.HandlerFunc
	// receivedPath is the last path the upstream saw.
	receivedPath string
}

type recordedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *upstream {
	t.Helper()
	record := &upstream{handler: handler}
	record.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record.mu.Lock()
		record.seen = append(record.seen, recordedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     body,
		})
		if r.URL.Path != "" {
			record.receivedPath = r.URL.Path
		}
		record.mu.Unlock()
		if record.handler != nil {
			record.handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_mock"}`))
	}))
	t.Cleanup(record.server.Close)
	return record
}

func (u *upstream) requests() []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedRequest(nil), u.seen...)
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

func (u *upstream) lastPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.receivedPath
}

// newRouter starts the router against mock upstreams.
func newRouter(t *testing.T, configText string, env map[string]string) (*Router, *bytes.Buffer) {
	t.Helper()
	cfg, err := config.Parse([]byte(configText))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, configText)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	envLookup := func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
	router, err := New(cfg, Options{Logger: logger, Env: envLookup})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return router, logs
}

// doRequest sends a request through the handler as a loopback client would.
func doRequest(handler http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:52123"
	if _, ok := headers["Host"]; !ok {
		request.Host = "127.0.0.1:4317"
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func jsonBody(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

const nativeChatGPTConfigTemplate = `{
  "listen": {"host": "127.0.0.1", "port": 4317},
  "native": {
    "chatgpt_base_url": %q,
    "api_base_url": %q,
    "models": ["gpt-5"]
  },
  "routes": [{
    "name": "local",
    "base_url": %q,
    "models": ["qwen3-32b"]
  }]
}`

func TestNativeChatGPTSessionKeepsAuth(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "gpt-5", "input": "hello"}),
		map[string]string{
			"Authorization":      "Bearer chatgpt-access-token",
			"ChatGPT-Account-ID": "acct-123",
			"Content-Type":       "application/json",
		})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if chatgpt.count() != 1 {
		t.Fatalf("chatgpt upstream calls = %d, want 1", chatgpt.count())
	}
	if api.count()+local.count() != 0 {
		t.Fatal("an account session reached the wrong upstream")
	}
	recorded := chatgpt.requests()[0]
	if recorded.Path != "/responses" {
		t.Fatalf("chatgpt path = %q, want /responses (the backend has no /v1 namespace)", recorded.Path)
	}
	if got := recorded.Header.Get("Authorization"); got != "Bearer chatgpt-access-token" {
		t.Fatalf("native Authorization = %q, want the client credential preserved", got)
	}
	if got := recorded.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
		t.Fatalf("ChatGPT-Account-ID = %q, want it preserved for the trusted native upstream", got)
	}
	if !bytes.Contains(recorded.Body, []byte(`"model":"gpt-5"`)) {
		t.Fatalf("native body was modified: %s", recorded.Body)
	}
}

func TestNativeAPIKeySessionKeepsAuth(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "gpt-5"}),
		map[string]string{"Authorization": "Bearer sk-user-key"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if api.count() != 1 || chatgpt.count() != 0 {
		t.Fatalf("expected only the API upstream, got api=%d chatgpt=%d", api.count(), chatgpt.count())
	}
	recorded := api.requests()[0]
	if recorded.Path != "/v1/responses" {
		t.Fatalf("api path = %q, want /v1/responses", recorded.Path)
	}
	if got := recorded.Header.Get("Authorization"); got != "Bearer sk-user-key" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestNativeAPIWithoutCredentialIsRefused(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}), nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	if api.count()+chatgpt.count() != 0 {
		t.Fatal("an unauthenticated native request reached an upstream")
	}
	if !strings.Contains(recorder.Body.String(), "authentication_required") {
		t.Fatalf("body = %s, want an actionable error code", recorder.Body.String())
	}
}

func TestNativeAPIKeyFromConfiguredEnvironment(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"], "api_key_env": "ROUTER_NATIVE_KEY"},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, chatgpt.server.URL, api.server.URL, local.server.URL), map[string]string{"ROUTER_NATIVE_KEY": "env-native-key"})

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := api.requests()[0].Header.Get("Authorization"); got != "Bearer env-native-key" {
		t.Fatalf("Authorization = %q, want the configured environment key", got)
	}
}

func TestPreserveClientAuthOffStripsClientCredentials(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	configText := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {
        "chatgpt_base_url": %q,
        "api_base_url": %q,
        "models": ["gpt-5"],
        "preserve_client_auth": false,
        "api_key_env": "NATIVE_KEY"
      },
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, chatgpt.server.URL, api.server.URL, local.server.URL)
	router, _ := newRouter(t, configText, map[string]string{"NATIVE_KEY": "configured-only"})

	// Turning client credential preservation off means the router authenticates
	// native requests with its own configured key. The upstream is still chosen by
	// the incoming ChatGPT-Account-ID header.
	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}),
		map[string]string{
			"Authorization":       "Bearer should-be-dropped",
			"ChatGPT-Account-ID":  "acct-1",
			"Cookie":              "session=drop-me",
			"X-Api-Key":           "client-openai-key",
			"Proxy-Authorization": "Basic pr0xy",
			"X-Provider-Token":    "custom-credential",
			"OpenAI-Organization": "org-secret",
			"X-Session-Id":        "session-secret",
			"X-Client-Request-Id": "request-secret",
			"Content-Type":        "application/json",
			"Accept":              "text/event-stream",
			"Openai-Beta":         "responses=experimental",
			"User-Agent":          "codex/0.1",
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if chatgpt.count() != 1 || api.count() != 0 {
		t.Fatalf("account session should still select the account backend: chatgpt=%d api=%d", chatgpt.count(), api.count())
	}
	recorded := chatgpt.requests()[0]
	if got := recorded.Header.Get("Authorization"); got != "Bearer configured-only" {
		t.Fatalf("Authorization = %q, want only the configured key", got)
	}
	// Deleting only Authorization, ChatGPT-Account-ID and Cookie would leave every
	// other credential the client sent pointing at the native upstream, so the
	// forwarded set is built from an allow list instead of a block list.
	for _, name := range []string{
		"ChatGPT-Account-ID", "Cookie", "X-Api-Key", "Proxy-Authorization",
		"X-Provider-Token", "OpenAI-Organization", "X-Session-Id", "X-Client-Request-Id",
	} {
		if got := recorded.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want it withheld when credential preservation is off", name, got)
		}
	}
	for _, name := range []string{"Content-Type", "Accept", "Openai-Beta", "User-Agent"} {
		if got := recorded.Header.Get(name); got == "" {
			t.Fatalf("%s = %q, want the safe transport header preserved", name, got)
		}
	}

	// Without an account header the same configuration uses the API upstream.
	recorder = doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}),
		map[string]string{"Authorization": "Bearer should-be-dropped"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if api.count() != 1 {
		t.Fatalf("api calls = %d, want 1", api.count())
	}
	if got := api.requests()[0].Header.Get("Authorization"); got != "Bearer configured-only" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestPreserveClientAuthOnForwardsNativeHeadersUnchanged keeps the other half of the
// credential policy pinned: with preservation on, a native request is passed through to
// the client's own upstream as-is. A future tightening of the allow list to this branch
// would break real ChatGPT sessions, so it has to be a deliberate change.
func TestPreserveClientAuthOnForwardsNativeHeadersUnchanged(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	configText := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"], "preserve_client_auth": true},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, chatgpt.server.URL, api.server.URL, newUpstream(t, nil).server.URL)
	router, _ := newRouter(t, configText, nil)

	sent := map[string]string{
		"Authorization":       "Bearer client-token",
		"ChatGPT-Account-ID":  "acct-1",
		"Cookie":              "session=keep-me",
		"X-Api-Key":           "client-openai-key",
		"OpenAI-Organization": "org-1",
		"X-Session-Id":        "session-1",
		"Content-Type":        "application/json",
		"User-Agent":          "codex/0.1",
	}
	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}), sent)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if chatgpt.count() != 1 {
		t.Fatalf("chatgpt calls = %d, want 1", chatgpt.count())
	}
	recorded := chatgpt.requests()[0]
	for name, want := range sent {
		if got := recorded.Header.Get(name); got != want {
			t.Fatalf("%s = %q, want the client's value %q", name, got, want)
		}
	}
}

func TestNativeRequestWithoutAnyCredentialIsRefused(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}),
		map[string]string{"ChatGPT-Account-ID": "acct-1"})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	if chatgpt.count()+api.count() != 0 {
		t.Fatal("a credential-less native request reached a native upstream")
	}
}

func TestRemoteRouteNeverSeesClientCredentials(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"]},
      "routes": [{
        "name": "local",
        "base_url": %q,
        "models": ["qwen3-32b"],
        "auth": {"api_key_env": "LOCAL_KEY", "scheme": "bearer"},
        "extra_headers": {"X-Tenant": "demo"}
      }]
    }`, chatgpt.server.URL, api.server.URL, local.server.URL), map[string]string{"LOCAL_KEY": "local-server-key"})

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), map[string]string{
		"Authorization":       "Bearer sk-secret-openai-key",
		"ChatGPT-Account-ID":  "acct-123",
		"Cookie":              "session=leak-me",
		"X-Api-Key":           "sk-another-secret",
		"X-Forwarded-For":     "203.0.113.9",
		"Content-Type":        "application/json",
		"Accept":              "application/json",
		"User-Agent":          "codex-shell/1.0",
		"X-Client-Request-Id": "req-1",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if local.count() != 1 {
		t.Fatalf("local upstream calls = %d, want 1", local.count())
	}
	if chatgpt.count()+api.count() != 0 {
		t.Fatal("a routed model also reached a native upstream")
	}
	header := local.requests()[0].Header
	if got := header.Get("Authorization"); got != "Bearer local-server-key" {
		t.Fatalf("Authorization = %q, want the route's own configured key", got)
	}
	for name, value := range map[string]string{
		"ChatGPT-Account-ID":  "acct-123",
		"Cookie":              "session=leak-me",
		"X-Api-Key":           "sk-another-secret",
		"X-Forwarded-For":     "203.0.113.9",
		"X-Client-Request-Id": "req-1",
	} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q was forwarded to a self-hosted server", name, got)
		}
		if strings.Contains(strings.Join(header.Values(name), ","), value) {
			t.Fatalf("%s leaked %q", name, value)
		}
	}
	if got := header.Get("X-Tenant"); got != "demo" {
		t.Fatalf("X-Tenant = %q, want the configured extra header", got)
	}
	if got := header.Get("User-Agent"); got != "codex-shell/1.0" {
		t.Fatalf("User-Agent = %q, want the allow-listed client header", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestMissingRouteCredentialFailsClosed(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"],"auth":{"api_key_env":"ABSENT_KEY"}}]
    }`, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	if local.count() != 0 {
		t.Fatal("the route was called without its credential")
	}
	if !strings.Contains(recorder.Body.String(), "route_credentials_missing") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestUnknownModelIsRejectedWithoutForwarding(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL),
		map[string]string{"ChatGPT-Account-ID": "unused"})

	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "not-configured"}),
		map[string]string{"Authorization": "Bearer token", "ChatGPT-Account-ID": "acct-1"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unknown_model") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if chatgpt.count()+api.count()+local.count() != 0 {
		t.Fatal("an unknown model ID reached an upstream")
	}
}

func TestUnlistedModelNeverReachesNative(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	config := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, chatgpt.server.URL, api.server.URL, local.server.URL)
	router, _ := newRouter(t, config, nil)

	// A model ID that is neither routed nor listed is refused even when the request
	// carries a usable native credential: an unlisted self-hosted ID must not be
	// able to reach a native API.
	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "brand-new-model"}),
		map[string]string{"Authorization": "Bearer token", "ChatGPT-Account-ID": "acct-1"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unknown_model") {
		t.Fatalf("error body = %s", recorder.Body.String())
	}
	if chatgpt.count()+api.count()+local.count() != 0 {
		t.Fatalf("an unlisted model reached an upstream: chatgpt=%d api=%d local=%d",
			chatgpt.count(), api.count(), local.count())
	}
}

func TestModelIDsMatchExactly(t *testing.T) {
	native := newUpstream(t, nil)
	local := newUpstream(t, nil)
	config := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"]}]
    }`, native.server.URL, native.server.URL, local.server.URL)
	router, _ := newRouter(t, config, nil)

	// Routing is by exact model ID. A padded value is a different ID and is refused,
	// so it cannot borrow a registered model's upstream.
	for _, model := range []string{
		" routed-model", "routed-model ", "\trouted-model", " routed-model ",
		" gpt-native", "gpt-native\n", "Routed-Model", "routed-model-2",
	} {
		recorder := doRequest(router, http.MethodPost, "/v1/responses",
			jsonBody(t, map[string]any{"model": model}),
			map[string]string{"Authorization": "Bearer token", "ChatGPT-Account-ID": "acct-1"})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("model %q: status = %d, want 400: %s", model, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "unknown_model") {
			t.Fatalf("model %q: body = %s", model, recorder.Body.String())
		}
		if native.count() != 0 || local.count() != 0 {
			t.Fatalf("model %q reached an upstream: native=%d local=%d", model, native.count(), local.count())
		}
	}

	// The exact IDs still work, which keeps the check from being a blanket refusal.
	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "routed-model"}), nil)
	if recorder.Code != http.StatusOK || local.count() != 1 {
		t.Fatalf("exact routed id: status=%d calls=%d", recorder.Code, local.count())
	}
	recorder = doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "gpt-native"}),
		map[string]string{"Authorization": "Bearer token", "ChatGPT-Account-ID": "acct-1"})
	if recorder.Code != http.StatusOK || native.count() != 1 {
		t.Fatalf("exact native id: status=%d calls=%d", recorder.Code, native.count())
	}
}

func TestRequestBodyBoundsAndShapes(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "max_request_bytes": 4096,
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, local.server.URL), nil)

	cases := []struct {
		name       string
		body       []byte
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{"empty body", nil, nil, http.StatusBadRequest, "empty_request_body"},
		{"not json", []byte("hello"), nil, http.StatusBadRequest, "invalid_json"},
		{"json array", []byte(`[{"model":"qwen3-32b"}]`), nil, http.StatusBadRequest, "invalid_json"},
		{"trailing content", []byte(`{"model":"qwen3-32b"} {}`), nil, http.StatusBadRequest, "invalid_json"},
		{"no model", []byte(`{"input":"hi"}`), nil, http.StatusBadRequest, "model_required"},
		{"oversized", jsonBody(t, map[string]any{"model": "qwen3-32b", "padding": strings.Repeat("x", 9000)}), nil, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"unsupported encoding", []byte("binary"), map[string]string{"Content-Encoding": "br"}, http.StatusUnsupportedMediaType, "unsupported_content_encoding"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			for name, value := range testCase.headers {
				headers[name] = value
			}
			recorder := doRequest(router, http.MethodPost, "/v1/responses", testCase.body, headers)
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Fatalf("body = %s, want code %s", recorder.Body.String(), testCase.wantCode)
			}
		})
	}
	if local.count() != 0 {
		t.Fatalf("%d rejected requests reached the upstream", local.count())
	}
}

func TestCompressedBodiesAreInspectedAndRouted(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	payload := jsonBody(t, map[string]any{"model": "qwen3-32b", "input": "hello"})

	gzipped := &bytes.Buffer{}
	writer := gzip.NewWriter(gzipped)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := doRequest(router, http.MethodPost, "/v1/responses", gzipped.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if local.count() != 1 {
		t.Fatalf("the gzip body was not routed by its model: local=%d", local.count())
	}
	if !bytes.Contains(local.requests()[0].Body, []byte(`"model":"qwen3-32b"`)) {
		t.Fatalf("routed body was not decoded JSON: %s", local.requests()[0].Body)
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	// A zstd body naming an unknown model must still be refused, which proves the
	// router reads the body rather than forwarding an opaque payload.
	unknownPayload := jsonBody(t, map[string]any{"model": "nope-not-here", "input": "hello"})
	unknown := encoder.EncodeAll(unknownPayload, nil)
	recorder = doRequest(router, http.MethodPost, "/v1/responses", unknown, map[string]string{"Content-Encoding": "zstd"})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown_model") {
		t.Fatalf("zstd unknown model: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestNativeBodyIsForwardedByteForByte(t *testing.T) {
	chatgpt := newUpstream(t, nil)
	api := newUpstream(t, nil)
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, api.server.URL, local.server.URL), nil)

	payload := jsonBody(t, map[string]any{"model": "gpt-5", "input": "keep me exact"})
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(payload, nil)
	_ = encoder.Close()

	recorder := doRequest(router, http.MethodPost, "/v1/responses", compressed,
		map[string]string{"Content-Encoding": "zstd", "Authorization": "Bearer token", "ChatGPT-Account-ID": "acct-1"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	recorded := chatgpt.requests()[0]
	if !bytes.Equal(recorded.Body, compressed) {
		t.Fatal("the native body was rewritten instead of passed through")
	}
	if got := recorded.Header.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding = %q, want the original encoding kept", got)
	}
}

func TestReasoningAdapterMapsConfiguredEfforts(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{
        "name": "local",
        "base_url": %q,
        "models": ["qwen3-32b"],
	        "reasoning": {
	          "adapter": "reasoning_to_chat_template",
	          "supported_efforts": ["none", "low", "medium", "high"],
	          "chat_template_kwargs": {
	            "enable_thinking": {"none": false, "low": true, "medium": true, "high": true},
	            "reasoning_effort": {"low": "low", "medium": "medium", "high": "high"},
	            "preserve_thinking": true
          }
        }
      }]
    }`, local.server.URL), nil)

	cases := []struct {
		name   string
		effort string
		want   map[string]any
	}{
		{"high", "high", map[string]any{"enable_thinking": true, "reasoning_effort": "high", "preserve_thinking": true}},
		{"none disables thinking", "none", map[string]any{"enable_thinking": false, "preserve_thinking": true}},
		{"unsupported effort is dropped", "ultra", map[string]any{"preserve_thinking": true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := jsonBody(t, map[string]any{
				"model":     "qwen3-32b",
				"reasoning": map[string]any{"effort": testCase.effort, "summary": "auto"},
				"input":     []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
			})
			recorder := doRequest(router, http.MethodPost, "/v1/responses", body, map[string]string{"Content-Type": "application/json"})
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			requests := local.requests()
			var forwarded map[string]any
			if err := json.Unmarshal(requests[len(requests)-1].Body, &forwarded); err != nil {
				t.Fatalf("forwarded body is not JSON: %v", err)
			}
			if _, stillThere := forwarded["reasoning"]; stillThere {
				t.Fatalf("the reasoning object was forwarded to a self-hosted server: %s", requests[len(requests)-1].Body)
			}
			kwargs, _ := forwarded["chat_template_kwargs"].(map[string]any)
			for key, want := range testCase.want {
				if got, ok := kwargs[key]; !ok || fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
					t.Fatalf("chat_template_kwargs[%s] = %v (ok=%v), want %v; full=%s", key, got, ok, want, requests[len(requests)-1].Body)
				}
			}
			for key := range kwargs {
				if _, expected := testCase.want[key]; !expected {
					t.Fatalf("unexpected chat_template_kwargs[%s] = %v", key, kwargs[key])
				}
			}
			// The conversation text must survive every reasoning shape.
			if !bytes.Contains(requests[len(requests)-1].Body, []byte("hi")) {
				t.Fatal("the message content was lost")
			}
		})
	}
}

func TestReasoningAdapterCanRejectUnknownEffort(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{
        "name": "local", "base_url": %q, "models": ["qwen3-32b"],
	        "reasoning": {"adapter": "reasoning_to_chat_template", "unknown_effort": "error",
          "supported_efforts": ["low", "medium"],
          "chat_template_kwargs": {"enable_thinking": {"low": true, "medium": true}}}
      }]
    }`, local.server.URL), nil)

	body := jsonBody(t, map[string]any{"model": "qwen3-32b", "reasoning": map[string]any{"effort": "ultra"}})
	recorder := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unsupported_reasoning_effort") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if local.count() != 0 {
		t.Fatal("a rejected request reached the upstream")
	}
}

func TestToolHistorySurvivesAndProviderStateIsDropped(t *testing.T) {
	local := newUpstream(t, nil)
	router, logs := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, local.server.URL), nil)

	body := jsonBody(t, map[string]any{
		"model":   "qwen3-32b",
		"include": []string{"reasoning.encrypted_content"},
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "instructions"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "please fix the bug"}}},
			map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "gAAAA-obtained-from-native"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file list"},
			map[string]any{"type": "custom_tool_call", "call_id": "call_2", "name": "exec", "input": "echo hi"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_2", "output": "hi"},
			map[string]any{"type": "compaction", "encrypted_content": `{"type":"provider_compaction","state":"opaque"}`},
			map[string]any{"type": "mystery_item", "data": "unmodelled"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}},
		},
	})
	recorder := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	forwarded := local.requests()[0].Body
	var payload struct {
		Input   []map[string]json.RawMessage `json:"input"`
		Include []string                     `json:"include"`
	}
	if err := json.Unmarshal(forwarded, &payload); err != nil {
		t.Fatalf("forwarded body: %v (%s)", err, forwarded)
	}

	var types []string
	for _, item := range payload.Input {
		types = append(types, strings.Trim(strings.TrimSpace(string(item["type"])), `"`))
	}
	joined := strings.Join(types, ",")
	wantTypes := "message,message,function_call,function_call_output,function_call,function_call_output,message"
	if joined != wantTypes {
		t.Fatalf("translated item types =\n  %s\nwant\n  %s\nbody: %s", joined, wantTypes, forwarded)
	}
	// Custom tool traffic becomes function traffic, with the payload kept.
	if !strings.Contains(string(forwarded), `"echo hi"`) && !strings.Contains(string(forwarded), "echo hi") {
		t.Fatalf("the custom tool input was not carried into the function call: %s", forwarded)
	}
	// Conversation text is never dropped, in either role.
	for _, text := range []string{"please fix the bug", "done", "instructions", "file list"} {
		if !strings.Contains(string(forwarded), text) {
			t.Fatalf("content %q was lost: %s", text, forwarded)
		}
	}
	// developer became system so a chat-style server accepts the history.
	var developerKept bool
	for _, item := range payload.Input {
		if strings.Contains(string(item["type"]), "message") && strings.Contains(string(item["role"]), "developer") {
			developerKept = true
		}
	}
	if developerKept {
		t.Fatalf("developer role survived: %s", forwarded)
	}
	for _, include := range payload.Include {
		if strings.Contains(include, "encrypted_content") {
			t.Fatalf("include still asks for encrypted reasoning: %v", payload.Include)
		}
	}
	warnings := logs.String()
	for _, want := range []string{"encrypted reasoning", "encrypted compaction"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("the log did not report dropped %s:\n%s", want, warnings)
		}
	}
}

func TestAgentMessageTextIsPreserved(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, local.server.URL), nil)

	body := jsonBody(t, map[string]any{
		"model": "qwen3-32b",
		"input": []any{map[string]any{
			"type":      "agent_message",
			"author":    "agent_a",
			"recipient": "agent_b",
			"content":   []any{map[string]any{"type": "encrypted_content", "encrypted_content": "plain but labelled"}},
		}},
	})
	recorder := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	forwarded := string(local.requests()[0].Body)
	for _, want := range []string{"plain but labelled", "Agent message from", `"role":"user"`} {
		if !strings.Contains(forwarded, want) {
			t.Fatalf("agent_message text was not preserved as %q in: %s", want, forwarded)
		}
	}
}

func TestSSEFirstEventReachesTheClientBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.Once{}
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	started := make(chan struct{})
	upstreamServer := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.created\ndata: {\"id\":\"first\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-release
		fmt.Fprint(w, "event: response.completed\ndata: {\"id\":\"last\"}\n\n")
		_ = w
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, upstreamServer.server.URL, upstreamServer.server.URL, upstreamServer.server.URL), nil)

	server := httptest.NewServer(router)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(jsonBody(t, map[string]any{"model": "qwen3-32b"})))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	// The first event must arrive while the upstream is still holding the stream
	// open, which is what a flush-on-write proxy guarantees.
	firstEvent := make(chan string, 1)
	buffer := make([]byte, 512)
	go func() {
		total := 0
		for total < len(buffer) {
			n, err := response.Body.Read(buffer[total:])
			total += n
			if n > 0 && bytes.Contains(buffer[:total], []byte("response.created")) {
				firstEvent <- string(buffer[:total])
				return
			}
			if err != nil {
				firstEvent <- ""
				return
			}
		}
	}()
	select {
	case seen := <-firstEvent:
		if !strings.Contains(seen, "response.created") {
			t.Fatalf("first event did not arrive: %q", seen)
		}
	case <-time.After(3 * time.Second):
		releaseStream()
		t.Fatal("the first SSE event was buffered until the upstream finished")
	}
	<-started
	releaseStream()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestClientCancellationCancelsTheUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstreamServer := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.created\ndata: {}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
		case <-time.After(10 * time.Second):
		}
	})

	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, upstreamServer.server.URL, upstreamServer.server.URL, upstreamServer.server.URL), nil)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses",
		bytes.NewReader(jsonBody(t, map[string]any{"model": "qwen3-32b"})))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	// Read the flushed header so the proxy is mid-stream, then go away.
	buf := make([]byte, 1)
	_, _ = response.Body.Read(buf)
	cancel()
	_ = response.Body.Close()

	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled when the client went away")
	}
}

func TestUpstreamStatusAndFailures(t *testing.T) {
	failing := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream said no"}}`))
	})
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, failing.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the upstream status preserved", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "upstream said no") {
		t.Fatalf("body = %s, want the upstream error body", recorder.Body.String())
	}

	// An unreachable upstream must produce a router error, not a redirect or a
	// leaked credential.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	unreachable, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, closedURL), nil)
	recorder = doRequest(unreachable, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "upstream_error") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestURLJoining(t *testing.T) {
	var captured []string
	recorderServer := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	cases := []struct {
		name     string
		routeURL string
		want     string
	}{
		{"base without /v1", recorderServer.server.URL, "/v1/responses?source=codex"},
		{"base with /v1", recorderServer.server.URL + "/v1", "/v1/responses?source=codex"},
		{"base with trailing slash", recorderServer.server.URL + "/", "/v1/responses?source=codex"},
		{"base under a prefix", recorderServer.server.URL + "/inference", "/inference/v1/responses?source=codex"},
		{"base prefix with /v1", recorderServer.server.URL + "/inference/v1", "/inference/v1/responses?source=codex"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			captured = nil
			router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, testCase.routeURL), nil)
			recorder := doRequest(router, http.MethodPost, "/v1/responses?source=codex", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			if len(captured) != 1 {
				t.Fatalf("captured = %v", captured)
			}
			if captured[0] != testCase.want {
				t.Fatalf("upstream path = %q, want %q", captured[0], testCase.want)
			}
		})
	}

	// The native ChatGPT backend has no /v1 namespace, while the API upstream
	// does, and neither may end up with /v1 doubled.
	t.Run("native chatgpt has no v1", func(t *testing.T) {
		captured = nil
		router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, recorderServer.server.URL+"/backend-api/codex", recorderServer.server.URL+"/v1", recorderServer.server.URL), nil)
		recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}),
			map[string]string{"Authorization": "Bearer t", "ChatGPT-Account-ID": "acct"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		if len(captured) != 1 || captured[0] != "/backend-api/codex/responses?" {
			t.Fatalf("native chatgpt path = %v", captured)
		}
	})
	t.Run("native api keeps one v1", func(t *testing.T) {
		captured = nil
		router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": %q, "api_base_url": %q, "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, recorderServer.server.URL+"/backend-api/codex", recorderServer.server.URL+"/v1", recorderServer.server.URL), nil)
		recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "gpt-5"}),
			map[string]string{"Authorization": "Bearer t"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		if len(captured) != 1 || captured[0] != "/v1/responses?" {
			t.Fatalf("native api path = %v, want exactly one /v1", captured)
		}
	})
}

func TestPathAndMethodPolicy(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, local.server.URL), nil)

	t.Run("health", func(t *testing.T) {
		recorder := doRequest(router, http.MethodGet, "/healthz", nil, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("health body is not JSON: %s", recorder.Body.String())
		}
		if body["ok"] != true {
			t.Fatalf("health body = %v", body)
		}
	})
	t.Run("health rejects post", func(t *testing.T) {
		recorder := doRequest(router, http.MethodPost, "/healthz", []byte("{}"), nil)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
	t.Run("models endpoint is not proxied", func(t *testing.T) {
		recorder := doRequest(router, http.MethodPost, "/v1/models", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for a path outside the Responses namespace", recorder.Code)
		}
	})
	t.Run("arbitrary proxy path is refused", func(t *testing.T) {
		recorder := doRequest(router, http.MethodGet, "/http://example.invalid/secret", nil, nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
	t.Run("responses subpath routes by model", func(t *testing.T) {
		// A subpath such as the compaction endpoint still has to be routed by the
		// exact model ID rather than assumed native.
		recorder := doRequest(router, http.MethodPost, "/v1/responses/compact", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("a routed subpath request should succeed: %d %s", recorder.Code, recorder.Body.String())
		}
		if got := local.lastPath(); got != "/v1/responses/compact" {
			t.Fatalf("subpath was not forwarded intact: %q", got)
		}
		t.Run("unknown model on a subpath is refused", func(t *testing.T) {
			recorder := doRequest(router, http.MethodPost, "/v1/responses/compact", jsonBody(t, map[string]any{"model": "nope"}), nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
		})
	})
	t.Run("get on the responses endpoint is refused", func(t *testing.T) {
		recorder := doRequest(router, http.MethodGet, "/v1/responses", nil, nil)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != "POST" {
			t.Fatalf("Allow = %q", got)
		}
	})
	t.Run("unsupported method", func(t *testing.T) {
		recorder := doRequest(router, http.MethodPut, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
	t.Run("websocket upgrade gets 426", func(t *testing.T) {
		before := local.count()
		recorder := doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), map[string]string{
			"Upgrade":    "websocket",
			"Connection": "Upgrade",
		})
		if recorder.Code != http.StatusUpgradeRequired {
			t.Fatalf("status = %d, want 426 so Codex falls back to HTTP", recorder.Code)
		}
		if local.count() != before {
			t.Fatal("a refused WebSocket upgrade reached an upstream")
		}
	})
	t.Run("non loopback peer is refused", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.RemoteAddr = "203.0.113.7:41000"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", recorder.Code)
		}
	})
	t.Run("rebinding host is refused", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.RemoteAddr = "127.0.0.1:41000"
		request.Host = "attacker.example:4317"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != statusMisusedHost {
			t.Fatalf("status = %d, want %d", recorder.Code, statusMisusedHost)
		}
	})
}

func TestBasePathIsHonoured(t *testing.T) {
	local := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "base_path": "/router/v1",
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"]}]
    }`, local.server.URL), nil)

	recorder := doRequest(router, http.MethodPost, "/router/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := local.lastPath(); got != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got)
	}
	recorder = doRequest(router, http.MethodPost, "/v1/responses", jsonBody(t, map[string]any{"model": "qwen3-32b"}), nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 outside the configured base path", recorder.Code)
	}
}

func TestLogsNeverContainBodiesOrCredentials(t *testing.T) {
	local := newUpstream(t, nil)
	router, logs := newRouter(t, fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "log": {"level": "debug", "requests": true},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1/backend-api/codex", "api_base_url": "http://127.0.0.1:1/v1", "models": ["gpt-5"]},
      "routes": [{"name":"local","base_url":%q,"models":["qwen3-32b"],"auth":{"api_key_env":"LOCAL_KEY"}}]
    }`, local.server.URL), map[string]string{"LOCAL_KEY": "route-secret-value"})

	doRequest(router, http.MethodPost, "/v1/responses",
		jsonBody(t, map[string]any{"model": "qwen3-32b", "input": "private conversation text", "instructions": "also private"}),
		map[string]string{"Authorization": "Bearer user-secret-token", "Cookie": "session=cookie-secret"})

	text := logs.String()
	for _, secret := range []string{"user-secret-token", "route-secret-value", "private conversation text", "cookie-secret", "also private"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the log leaked %q:\n%s", secret, text)
		}
	}
	for _, want := range []string{"route=local", "model=qwen3-32b", "status=200"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the request log is missing %q:\n%s", want, text)
		}
	}
}

func TestHealthReportsRoutingWithoutSecrets(t *testing.T) {
	local := newUpstream(t, nil)
	chatgpt := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chatgpt.server.URL, chatgpt.server.URL, local.server.URL), nil)
	health := router.Health()
	if health["ok"] != true {
		t.Fatalf("health = %v", health)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Bearer", "sk-", "api_key", "Authorization"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("health output contains %q: %s", forbidden, encoded)
		}
	}
	if fmt.Sprint(health["remote_models"]) != "1" || fmt.Sprint(health["native_models"]) != "1" {
		t.Fatalf("health counts = %v", health)
	}
}

// TestChatTemplateKwargsNullDoesNotPanic covers the one request shape that used to
// abort the process: a JSON null decodes into a nil map, and writing the configured
// kwargs into that nil map panicked.
func TestChatTemplateKwargsNullDoesNotPanic(t *testing.T) {
	for _, payload := range []string{
		`{"model":"routed-model","input":"x","reasoning":{"effort":"high"},"chat_template_kwargs":null}`,
		`{"model":"routed-model","input":"x","reasoning":{"effort":"high"},"chat_template_kwargs":{"sending":null}}`,
		`{"model":"routed-model","input":"x","reasoning":{"effort":"high"},"chat_template_kwargs":"not-a-map"}`,
		`{"model":"routed-model","input":"x","reasoning":{"effort":"high"},"chat_template_kwargs":[1,2]}`,
		`{"model":"routed-model","input":"x","reasoning":{"effort":"high"},"chat_template_kwargs":{"thinking":"yes"}}`,
	} {
		local := newUpstream(t, nil)
		config := reasoningRouteConfig(t, local.server.URL)
		router, _ := newRouter(t, config, nil)
		recorder := doRequest(router, http.MethodPost, "/v1/responses", json.RawMessage(payload), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("payload %s: status = %d: %s", payload, recorder.Code, recorder.Body.String())
		}
		var forwarded string
		for _, seen := range local.requests() {
			forwarded += string(seen.Body)
		}
		if !strings.Contains(forwarded, "\"thinking\":true") {
			t.Fatalf("payload %s: the configured kwarg was not injected: %s", payload, forwarded)
		}
		// The client's unusable value is replaced, never forwarded as it arrived.
		if strings.Contains(forwarded, "\"chat_template_kwargs\":null") ||
			strings.Contains(forwarded, "\"chat_template_kwargs\":\"not-a-map\"") ||
			strings.Contains(forwarded, "\"chat_template_kwargs\":[") {
			t.Fatalf("payload %s: an unusable client value survived: %s", payload, forwarded)
		}
	}
}

// TestClientNullFieldsSurviveWhenNothingIsConfigured keeps the flip side of the
// guard: with no adapter and no configured kwargs the router has no reason to touch
// the body, so a null from the client is forwarded as the client sent it.
func TestClientNullFieldsSurviveWhenNothingIsConfigured(t *testing.T) {
	local := newUpstream(t, nil)
	config := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"]}]
    }`, local.server.URL)
	router, _ := newRouter(t, config, nil)
	recorder := doRequest(router, http.MethodPost, "/v1/responses",
		json.RawMessage(`{"model":"routed-model","input":"x","chat_template_kwargs":null}`), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if forwarded := string(local.requests()[0].Body); !strings.Contains(forwarded, "\"chat_template_kwargs\":null") {
		t.Fatalf("an unconfigured route should not rewrite the field: %s", forwarded)
	}
}

// reasoningRouteConfig routes one model through the reasoning adapter with a
// kwarg that is always injected.
func reasoningRouteConfig(t *testing.T, baseURL string) string {
	t.Helper()
	return fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],
        "reasoning":{"adapter":"reasoning_to_chat_template","supported_efforts":["high"],
          "chat_template_kwargs":{"thinking":{"none":false,"high":true}}}}]
    }`, baseURL)
}

// namespaceRouteConfig combines the reasoning adapter with explicit namespace
// flattening and on-demand schema loading, which is what the former
// sglang_chat_template adapter enabled implicitly.
func namespaceRouteConfig(t *testing.T, baseURL string) string {
	t.Helper()
	return fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],
        "reasoning":{"adapter":"reasoning_to_chat_template","supported_efforts":["high"],
          "chat_template_kwargs":{"thinking":{"none":false,"high":true}}},
        "tools":{"namespace_adapter":"namespace_to_functions","schema_loading":"on_demand"}}]
    }`, baseURL)
}

// toolsOnlyRouteConfig enables a tools adapter without any reasoning adapter,
// with the requested schema loading mode.
func toolsOnlyRouteConfig(t *testing.T, baseURL, schemaLoading string) string {
	t.Helper()
	return fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],
        "tools":{"namespace_adapter":"namespace_to_functions","schema_loading":%q}}]
    }`, baseURL, schemaLoading)
}

// TestReasoningAndToolsAdaptersAreIndependent proves through the forwarded
// upstream payload that the two adapters are configured separately: a
// reasoning adapter never flattens namespace tools, and a tools adapter never
// injects chat_template_kwargs.
func TestReasoningAndToolsAdaptersAreIndependent(t *testing.T) {
	reasoning := `"reasoning":{"adapter":"reasoning_to_chat_template","supported_efforts":["high"],` +
		`"chat_template_kwargs":{"thinking":{"none":false,"high":true}}}`
	namespaced := `"tools":{"namespace_adapter":"namespace_to_functions"}`

	cases := []struct {
		name              string
		routeExtra        string
		wantKwargs        bool
		wantReasoningGone bool
		wantFlattened     bool
		wantLoader        bool
	}{
		{"neither", ``, false, false, false, false},
		{"reasoning only", reasoning, true, true, false, false},
		{"namespace only", namespaced, false, false, true, false},
		{"namespace only on_demand", `"tools":{"namespace_adapter":"namespace_to_functions","schema_loading":"on_demand"}`, false, false, true, true},
		{"reasoning and namespace", reasoning + "," + namespaced, true, true, true, false},
		{"reasoning and namespace on_demand", reasoning + "," + `"tools":{"namespace_adapter":"namespace_to_functions","schema_loading":"on_demand"}`, true, true, true, true},
	}

	// One request carrying both a reasoning effort and a namespace tool group;
	// each case must observe only the effects its route enables. The upstream
	// answers with a valid empty Responses document, which on_demand disclosure
	// requires before it can hand a response back to Codex.
	payload := `{"model":"routed-model","reasoning":{"effort":"high"},"tools":` + namespaceTools + `,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			local := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_mock","object":"response","status":"completed","output":[]}`))
			})
			routeFields := `"name":"local","base_url":` + fmt.Sprintf("%q", local.server.URL) + `,"models":["routed-model"]`
			if testCase.routeExtra != "" {
				routeFields += "," + testCase.routeExtra
			}
			configText := `{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{` + routeFields + `}]
    }`
			router, _ := newRouter(t, configText, nil)
			recorder := doRequest(router, http.MethodPost, "/v1/responses", json.RawMessage(payload), nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			forwarded := string(local.requests()[0].Body)

			_, kwargsPresent := containsField(t, forwarded, "chat_template_kwargs")
			if kwargsPresent != testCase.wantKwargs {
				t.Fatalf("chat_template_kwargs present = %v, want %v: %s", kwargsPresent, testCase.wantKwargs, forwarded)
			}
			if kwargsPresent && !strings.Contains(forwarded, `"thinking":true`) {
				t.Fatalf("the configured kwarg was not injected: %s", forwarded)
			}
			_, reasoningPresent := containsField(t, forwarded, "reasoning")
			if reasoningPresent == testCase.wantReasoningGone {
				t.Fatalf("reasoning present = %v, want gone = %v: %s", reasoningPresent, testCase.wantReasoningGone, forwarded)
			}
			namespaceKept := strings.Contains(forwarded, `"type":"namespace"`)
			flattened := !namespaceKept
			if flattened != testCase.wantFlattened {
				t.Fatalf("flattened = %v, want %v: %s", flattened, testCase.wantFlattened, forwarded)
			}
			hasLoader := strings.Contains(forwarded, `"router_load_tools"`)
			if hasLoader != testCase.wantLoader {
				t.Fatalf("loader tool present = %v, want %v: %s", hasLoader, testCase.wantLoader, forwarded)
			}
			// With the eager (or no) tools adapter the flattened schema is visible;
			// with on_demand the child schema stays hidden behind the loader.
			schemaVisible := strings.Contains(forwarded, `"mcp__node_repl__js"`)
			if schemaVisible != (testCase.wantFlattened && !testCase.wantLoader) {
				t.Fatalf("namespace child schema visible = %v: %s", schemaVisible, forwarded)
			}
			// The conversation always survives.
			if !strings.Contains(forwarded, "hi") {
				t.Fatalf("the message content was lost: %s", forwarded)
			}
		})
	}
}

// containsField reports whether the top-level JSON object carries key, and
// returns its raw value.
func containsField(t *testing.T, body, key string) (string, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("forwarded body is not JSON: %v (%s)", err, body)
	}
	raw, ok := fields[key]
	return string(raw), ok
}

// TestNamespaceSchemaLoadingAllSendsFullInventory pins the eager half of
// tools.schema_loading: with all, the flattened namespace schemas travel with
// the request and no loader tool exists.
func TestNamespaceSchemaLoadingAllSendsFullInventory(t *testing.T) {
	local := newUpstream(t, nil)
	configText := fmt.Sprintf(`{
      "listen": {"host": "127.0.0.1", "port": 4317},
      "native": {"chatgpt_base_url": "http://127.0.0.1:1", "api_base_url": "http://127.0.0.1:1", "models": ["gpt-native"]},
      "routes": [{"name":"local","base_url":%q,"models":["routed-model"],
        "tools":{"namespace_adapter":"namespace_to_functions"}}]
    }`, local.server.URL)
	router, _ := newRouter(t, configText, nil)
	body := jsonBody(t, map[string]any{
		"model": "routed-model",
		"tools": json.RawMessage(namespaceTools),
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	recorder := doRequest(router, http.MethodPost, "/v1/responses", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	forwarded := string(local.requests()[0].Body)
	// The full flattened inventory: both the core tool and the namespace child.
	for _, want := range []string{`"exec_command"`, `"mcp__node_repl__js"`, `"code"`} {
		if !strings.Contains(forwarded, want) {
			t.Fatalf("the full schema inventory was not sent (missing %s): %s", want, forwarded)
		}
	}
	if strings.Contains(forwarded, "router_load_tools") {
		t.Fatalf("schema_loading=all must not add a loader tool: %s", forwarded)
	}
}
