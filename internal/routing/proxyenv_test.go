package routing

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestOptionalCodexProxyFile(t *testing.T) {
	for _, tc := range []struct {
		name, content               string
		missing, wantNil, wantError bool
	}{
		{name: "missing", missing: true, wantNil: true},
		{name: "empty", wantNil: true},
		{name: "unrelated", content: "UNRELATED_SECRET=private\n", wantNil: true},
		{name: "dotenv", content: "# comment\nexport HTTPS_PROXY='http://proxy.example:8080' # comment\n"},
		{name: "malformed", content: "HTTPS_PROXY='secret-not-terminated", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.missing {
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := os.Getenv("HTTPS_PROXY")
			proxy, err := LoadChatGPTProxy(envMap(map[string]string{"CODEX_HOME": dir}))
			if (err != nil) != tc.wantError {
				t.Fatalf("error presence=%v", err != nil)
			}
			if err != nil {
				if strings.Contains(err.Error(), "secret-not-terminated") {
					t.Fatal("error leaked input")
				}
				return
			}
			if (proxy == nil) != tc.wantNil {
				t.Fatal("unexpected optional proxy result")
			}
			if proxy != nil {
				req, _ := http.NewRequest("GET", "https://chatgpt.com/responses", nil)
				got, err := proxy(req)
				if err != nil || got == nil || got.Host != "proxy.example:8080" {
					t.Fatal("dotenv proxy not selected")
				}
			}
			if os.Getenv("HTTPS_PROXY") != before {
				t.Fatal("process environment changed")
			}
		})
	}
}

func TestDefaultCodexProxyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", ".env"), []byte("HTTPS_PROXY=http://proxy.example:8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	proxy, err := LoadChatGPTProxy(envMap(nil))
	if err != nil || proxy == nil {
		t.Fatal("default Codex directory not loaded")
	}
}

func TestCodexProxySelection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		file, env    map[string]string
		target, want string
	}{
		{"https", map[string]string{"HTTPS_PROXY": "http://tls-proxy:80"}, nil, "https://chatgpt.com", "http://tls-proxy:80"},
		{"http", map[string]string{"HTTP_PROXY": "http://plain-proxy:80"}, nil, "http://chatgpt.com", "http://plain-proxy:80"},
		{"lowercase", map[string]string{"https_proxy": "http://lower:80"}, nil, "https://chatgpt.com", "http://lower:80"},
		{"all", map[string]string{"ALL_PROXY": "socks5://fallback:1080"}, nil, "https://chatgpt.com", "socks5://fallback:1080"},
		{"specific-over-all", map[string]string{"ALL_PROXY": "http://fallback:80", "HTTPS_PROXY": "http://specific:80"}, nil, "https://chatgpt.com", "http://specific:80"},
		{"environment-wins", map[string]string{"HTTPS_PROXY": "http://file:80"}, map[string]string{"HTTPS_PROXY": "http://process:80"}, "https://chatgpt.com", "http://process:80"},
		{"no-proxy", map[string]string{"HTTPS_PROXY": "http://file:80", "NO_PROXY": "chatgpt.com"}, nil, "https://chatgpt.com", ""},
		{"only-no-proxy", map[string]string{"NO_PROXY": "chatgpt.com"}, map[string]string{"HTTPS_PROXY": "http://process:80"}, "https://chatgpt.com", ""},
		{"loopback", map[string]string{"HTTP_PROXY": "http://file:80"}, nil, "http://127.0.0.1:4011", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := proxyFromValues(tc.file, envMap(tc.env))
			req, _ := http.NewRequest("GET", tc.target, nil)
			got, err := proxy(req)
			if err != nil {
				t.Fatal(err)
			}
			actual := ""
			if got != nil {
				actual = got.String()
			}
			if actual != tc.want {
				t.Fatalf("got %q want %q", actual, tc.want)
			}
		})
	}
}

func TestCodexFileProxyOnlyAffectsChatGPTTransport(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "chatgpt.example" {
			t.Error("wrong proxy destination")
		}
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxyServer.Close()
	lookup := proxyFromValues(map[string]string{"HTTP_PROXY": proxyServer.URL}, envMap(nil))
	called := 0
	options := Options{ChatGPTProxy: func(r *http.Request) (*url.URL, error) { called++; return lookup(r) }}
	req, _ := http.NewRequest("GET", "http://chatgpt.example/responses", nil)
	for _, kind := range []targetKind{kindNativeChatGPT, kindNativeAPI, kindRemote} {
		target, err := newTarget("test", "http://chatgpt.example", kind, nil, options)
		if err != nil {
			t.Fatal(err)
		}
		transport := target.proxy.Transport.(*http.Transport)
		defer transport.CloseIdleConnections()
		before := called
		if kind == kindNativeChatGPT {
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != "proxied" {
				t.Fatal("mock proxy was not used")
			}
			if called == before {
				t.Fatal("ChatGPT proxy callback not used")
			}
		} else {
			_, _ = transport.Proxy(req)
			if called != before {
				t.Fatal("file proxy leaked into other upstream")
			}
		}
	}
}
