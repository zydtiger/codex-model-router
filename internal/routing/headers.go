package routing

import (
	"net/http"
	"strings"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// buildRemoteHeaders creates the header set for a remote route.
//
// The incoming request's credentials, account identifiers, cookies, and
// forwarding headers are deliberately absent: a self-hosted server must never
// learn about a ChatGPT session or receive an OpenAI key. Only a small
// transport allow list is copied, then the route's own configured credential is
// applied.
func buildRemoteHeaders(in http.Header, route *config.Route, env envLookup) (http.Header, *requestError) {
	out := make(http.Header, len(config.AllowedRequestHeaders())+len(route.Headers))
	for _, key := range config.AllowedRequestHeaders() {
		for _, value := range in.Values(key) {
			out.Add(key, value)
		}
	}
	for name, value := range route.Headers {
		out.Set(name, value)
	}
	if route.Auth == nil {
		return out, nil
	}
	key, ok := env(route.Auth.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" {
		return nil, clientError(http.StatusServiceUnavailable, "route_credentials_missing",
			"route %q expects its credential in $%s, which is not set", route.Name, route.Auth.APIKeyEnv)
	}
	if route.Auth.Scheme == "bearer" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "bearer ") {
		out.Set(route.Auth.Header, "Bearer "+strings.TrimSpace(key))
	} else {
		out.Set(route.Auth.Header, strings.TrimSpace(key))
	}
	return out, nil
}

// applyNativeHeaders decides which headers and which credential the trusted native
// upstream receives.
//
// With preservation on, everything is forwarded unchanged so native sessions keep their
// normal behaviour, and a request with no credential at all is refused here rather than
// sent upstream to fail with an opaque provider error.
//
// With preservation off, the incoming header set cannot be used at all. Cloning it and
// deleting a few names would leave X-Api-Key, Proxy-Authorization, and whatever
// provider-specific credential the client happened to send, which is the opposite of
// what turning preservation off means. So only the same transport allow list the remote
// routes copy is carried over, and the configured native credential is the only secret
// that reaches the upstream.
func applyNativeHeaders(in http.Header, preserveClientAuth bool, envKey string, env envLookup) (http.Header, *requestError) {
	var out http.Header
	if preserveClientAuth {
		out = in.Clone()
		if out == nil {
			out = http.Header{}
		}
	} else {
		allowed := config.AllowedRequestHeaders()
		out = make(http.Header, len(allowed))
		for _, name := range allowed {
			for _, value := range in.Values(name) {
				out.Add(name, value)
			}
		}
	}
	if out.Get("Authorization") != "" {
		return out, nil
	}
	if preserveClientAuth && out.Get("Cookie") != "" {
		// A cookie session is still client authentication, and it only reaches the
		// configured trusted upstream.
		return out, nil
	}
	if envKey == "" {
		return nil, clientError(http.StatusUnauthorized, "authentication_required",
			"native requests need client credentials or native.api_key_env")
	}
	key, ok := env(envKey)
	if !ok || strings.TrimSpace(key) == "" {
		return nil, clientError(http.StatusServiceUnavailable, "native_credentials_missing",
			"native.api_key_env names $%s, which is not set", envKey)
	}
	value := strings.TrimSpace(key)
	if !strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = "Bearer " + value
	}
	out.Set("Authorization", value)
	return out, nil
}
