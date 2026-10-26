package routing

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestLargeZstdRequestReachesNativeUpstream(t *testing.T) {
	upstream := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, upstream.server.URL, blockedUpstream, blockedUpstream), nil)
	// Reproduce an image-heavy request above the former 128 MiB local setting.
	payload := []byte(`{"model":"gpt-5","input":"` + strings.Repeat("x", 131<<20) + `"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(payload, nil)
	encoder.Close()
	payload = nil
	got := doRequest(router, "POST", "/v1/responses", compressed, map[string]string{
		"Authorization": "Bearer test", accountIDHeader: "test-account", "Content-Encoding": "zstd",
	})
	if got.Code != 200 || upstream.count() != 1 {
		t.Fatalf("status=%d upstream calls=%d", got.Code, upstream.count())
	}
	forwarded := upstream.requests()[0]
	if !bytes.Equal(forwarded.Body, compressed) || forwarded.Header.Get("Content-Encoding") != "zstd" {
		t.Fatal("native compressed body changed")
	}
}

func TestUpstreamOwnsRequestLimits(t *testing.T) {
	for _, model := range []string{"gpt-5", "qwen3-32b"} {
		for _, encoding := range []string{"identity", "gzip", "zstd"} {
			for _, status := range []int{200, 413} {
				t.Run(fmt.Sprintf("%s/%s/%d", model, encoding, status), func(t *testing.T) {
					reply := `{"error":{"code":"upstream_size_policy","message":"provider decides"}}`
					upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-Request-ID", "upstream-id")
						w.WriteHeader(status)
						fmt.Fprint(w, reply)
					})
					router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, upstream.server.URL, blockedUpstream, upstream.server.URL), nil)
					payload := jsonBody(t, map[string]any{"model": model, "input": strings.Repeat("x", 9000)})
					body := payload
					switch encoding {
					case "gzip":
						var b bytes.Buffer
						w := gzip.NewWriter(&b)
						if _, err := w.Write(payload); err != nil {
							t.Fatal(err)
						}
						if err := w.Close(); err != nil {
							t.Fatal(err)
						}
						body = b.Bytes()
					case "zstd":
						w, err := zstd.NewWriter(nil)
						if err != nil {
							t.Fatal(err)
						}
						body = w.EncodeAll(payload, nil)
						w.Close()
					}
					got := doRequest(router, "POST", "/v1/responses", body, map[string]string{"Authorization": "Bearer test", accountIDHeader: "test-account", "Content-Encoding": encoding})
					if upstream.count() != 1 || got.Code != status || got.Body.String() != reply || got.Header().Get("X-Request-ID") != "upstream-id" {
						t.Fatalf("status=%d calls=%d body=%s", got.Code, upstream.count(), got.Body.String())
					}
					forwarded := upstream.requests()[0]
					if model == "gpt-5" {
						if !bytes.Equal(forwarded.Body, body) {
							t.Fatal("native payload changed")
						}
					} else if !bytes.Contains(forwarded.Body, []byte(strings.Repeat("x", 9000))) {
						t.Fatal("remote input truncated")
					}
				})
			}
		}
	}
}
