package routing

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWebSearchNativeForwarding(t *testing.T) {
	for _, account := range []bool{true, false} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("account=%v/gzip=%v", account, compressed), func(t *testing.T) {
				reply := `{"results":[{"title":"example"}]}`
				handler := func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(202)
					_, _ = w.Write([]byte(reply))
				}
				chat := newUpstream(t, handler)
				api := newUpstream(t, handler)
				remote := newUpstream(t, nil)
				router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chat.server.URL+"/backend-api/codex", api.server.URL+"/v1", remote.server.URL), nil)
				body := []byte(`{"search_query":[{"q":"example"}],"model":"qwen3-32b"}`)
				headers := map[string]string{"Authorization": "Bearer test-token", "Content-Type": "application/json"}
				if account {
					headers[accountIDHeader] = "test-account"
				}
				if compressed {
					var buf bytes.Buffer
					zw := gzip.NewWriter(&buf)
					_, _ = zw.Write(body)
					_ = zw.Close()
					body = buf.Bytes()
					headers["Content-Encoding"] = "gzip"
				}
				got := doRequest(router, http.MethodPost, "/v1/alpha/search?mode=live", body, headers)
				if got.Code != 202 || got.Body.String() != reply {
					t.Fatalf("response: %d %s", got.Code, got.Body.String())
				}
				selected, other, path := api, chat, "/v1/alpha/search"
				if account {
					selected, other, path = chat, api, "/backend-api/codex/alpha/search"
				}
				if selected.count() != 1 || other.count() != 0 || remote.count() != 0 {
					t.Fatal("wrong destination")
				}
				req := selected.requests()[0]
				if req.Path != path || req.RawQuery != "mode=live" || req.Method != http.MethodPost || !bytes.Equal(req.Body, body) {
					t.Fatalf("request not preserved: %#v", req)
				}
				if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("Content-Encoding") != headers["Content-Encoding"] || req.Header.Get(accountIDHeader) != headers[accountIDHeader] {
					t.Fatal("headers not preserved")
				}
			})
		}
	}
}

func TestWebSearchGuards(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		auth                     bool
		limit                    int64
		want                     int
	}{
		{"no model", "POST", "/v1/alpha/search", `{}`, true, 0, 200},
		{"missing auth", "POST", "/v1/alpha/search", `{}`, false, 0, 401},
		{"wrong method", "GET", "/v1/alpha/search", `{}`, true, 0, 405},
		{"subpath", "POST", "/v1/alpha/search/extra", `{}`, true, 0, 404},
		{"other endpoint", "POST", "/v1/alpha/other", `{}`, true, 0, 404},
		{"too large", "POST", "/v1/alpha/search", strings.Repeat("x", 17), true, 16, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := newUpstream(t, nil)
			api := newUpstream(t, nil)
			remote := newUpstream(t, nil)
			router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, chat.server.URL, api.server.URL, remote.server.URL), nil)
			if tc.limit > 0 {
				router.cfg.MaxRequestBytes = tc.limit
			}
			headers := map[string]string{accountIDHeader: "test-account"}
			if tc.auth {
				headers["Authorization"] = "Bearer test-token"
			}
			got := doRequest(router, tc.method, tc.path, []byte(tc.body), headers)
			if got.Code != tc.want {
				t.Fatalf("got %d: %s; want %d", got.Code, got.Body.String(), tc.want)
			}
			wantRequests := 0
			if tc.want == 200 {
				wantRequests = 1
			}
			if chat.count() != wantRequests || api.count() != 0 || remote.count() != 0 {
				t.Fatal("unexpected upstream request")
			}
		})
	}
}
