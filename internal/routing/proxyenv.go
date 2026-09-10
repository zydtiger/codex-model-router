package routing

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"golang.org/x/net/http/httpproxy"
)

// LoadChatGPTProxy reads optional Codex proxy settings without changing process
// environment. A nil result preserves the transport's existing proxy behavior.
func LoadChatGPTProxy(env func(string) (string, bool)) (func(*http.Request) (*url.URL, error), error) {
	home, _ := env("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("cannot locate Codex environment directory")
		}
		home = filepath.Join(userHome, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, ".env"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read optional Codex .env file")
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, errors.New("cannot parse optional Codex .env file")
	}
	return proxyFromValues(values, env), nil
}

func proxyFromValues(values map[string]string, env func(string) (string, bool)) func(*http.Request) (*url.URL, error) {
	names := []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"}
	found := false
	merged := make(map[string]string, len(names))
	for _, name := range names {
		if value, ok := values[name]; ok {
			found = true
			merged[name] = value
		}
		if value, ok := env(name); ok {
			merged[name] = value
		}
	}
	if !found {
		return nil
	}
	first := func(names ...string) string {
		for _, name := range names {
			if merged[name] != "" {
				return merged[name]
			}
		}
		return ""
	}
	settings := &httpproxy.Config{
		HTTPProxy:  first("HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"),
		HTTPSProxy: first("HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"),
		NoProxy:    first("NO_PROXY", "no_proxy"),
	}
	lookup := settings.ProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		proxy, err := lookup(req.URL)
		if err != nil {
			return nil, errors.New("invalid ChatGPT proxy configuration")
		}
		return proxy, nil
	}
}
