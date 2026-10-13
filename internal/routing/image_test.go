package routing

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestImageGuards(t *testing.T) {
	for _, endpoint := range []string{"/images/generations", "/images/edits"} {
		for _, tc := range []struct {
			name, method, suffix, body string
			auth                       bool
			limit                      int64
			want                       int
		}{
			{"no model", "POST", "", `{}`, true, 0, 200},
			{"missing auth", "POST", "", `{}`, false, 0, 401},
			{"wrong method", "GET", "", `{}`, true, 0, 405},
			{"subpath", "POST", "/extra", `{}`, true, 0, 404},
			{"too large", "POST", "", strings.Repeat("x", 17), true, 16, 413},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				native := newUpstream(t, nil)
				remote := newUpstream(t, nil)
				router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, native.server.URL, blockedUpstream, remote.server.URL), nil)
				if tc.limit > 0 {
					router.cfg.MaxRequestBytes = tc.limit
				}
				headers := map[string]string{accountIDHeader: "test-account"}
				if tc.auth {
					headers["Authorization"] = "Bearer test-token"
				}
				got := doRequest(router, tc.method, "/v1"+endpoint+tc.suffix, []byte(tc.body), headers)
				if got.Code != tc.want {
					t.Fatalf("got %d: %s", got.Code, got.Body.String())
				}
				want := 0
				if tc.want == 200 {
					want = 1
				}
				if native.count() != want || remote.count() != 0 {
					t.Fatal("unexpected destination")
				}
			})
		}
	}
}

func TestImageMultipartAndUpstreamError(t *testing.T) {
	body := []byte("--image-boundary\r\nContent-Disposition: form-data; name=\"image\"; filename=\"test.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG\x00\xff\r\n--image-boundary--\r\n")
	contentType := "multipart/form-data; boundary=image-boundary"
	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"image limit"}`)
	})
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, upstream.server.URL, blockedUpstream, blockedUpstream), nil)
	got := doRequest(router, "POST", "/v1/images/edits", body, map[string]string{"Authorization": "Bearer test", accountIDHeader: "account", "Content-Type": contentType})
	if got.Code != 429 || got.Header().Get("Retry-After") != "7" || got.Body.String() != `{"error":"image limit"}` {
		t.Fatal("upstream error changed")
	}
	req := upstream.requests()[0]
	if !bytes.Equal(req.Body, body) || req.Header.Get("Content-Type") != contentType {
		t.Fatal("multipart changed")
	}
}

func TestRemoteChatThenNativeImage(t *testing.T) {
	native := newUpstream(t, nil)
	remote := newUpstream(t, nil)
	router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, native.server.URL, blockedUpstream, remote.server.URL), nil)
	headers := map[string]string{"Authorization": "Bearer native-secret", accountIDHeader: "account"}
	chat := doRequest(router, "POST", "/v1/responses", []byte(`{"model":"qwen3-32b","input":"draw a circle"}`), headers)
	image := doRequest(router, "POST", "/v1/images/generations", []byte(`{"prompt":"a circle"}`), headers)
	if chat.Code != 200 || image.Code != 200 || remote.count() != 1 || native.count() != 1 {
		t.Fatal("mixed route failed")
	}
	if remote.requests()[0].Header.Get("Authorization") == "Bearer native-secret" || remote.requests()[0].Header.Get(accountIDHeader) != "" {
		t.Fatal("native credentials leaked")
	}
	if native.requests()[0].Header.Get("Authorization") != "Bearer native-secret" {
		t.Fatal("native credentials lost")
	}
}

func TestImageStreamingAndCancellation(t *testing.T) {
	for _, endpoint := range []string{"/images/generations", "/images/edits"} {
		t.Run(endpoint, func(t *testing.T) {
			cancelled := make(chan struct{})
			// Larger than the configured request limit: image output is not buffered or
			// subjected to the Responses adapter's size bound.
			chunk := strings.Repeat("x", 4096)
			native := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: "+chunk+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					close(cancelled)
				case <-time.After(5 * time.Second):
				}
			})
			router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, native.server.URL, blockedUpstream, blockedUpstream), nil)
			router.cfg.MaxRequestBytes = 16
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1"+endpoint, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer test")
			req.Header.Set(accountIDHeader, "account")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data := make([]byte, len(chunk)+8)
			if _, err := io.ReadFull(resp.Body, data); err != nil {
				t.Fatal(err)
			}
			if string(data) != "data: "+chunk+"\n\n" {
				t.Fatal("stream changed")
			}
			cancel()
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not cancelled")
			}
		})
	}
}

func TestRejectedEndpointLogging(t *testing.T) {
	router, logs := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, blockedUpstream, blockedUpstream, blockedUpstream), nil)
	router.cfg.Log.Requests = false
	for _, path := range []string{"/v1/unknown", "/outside"} {
		got := doRequest(router, "POST", path+"?secret=hidden", []byte("private-body"), map[string]string{"Authorization": "Bearer secret-token"})
		if got.Code != 404 {
			t.Fatal(got.Code)
		}
		if !strings.Contains(logs.String(), "method=POST path="+path) {
			t.Fatal("missing rejected endpoint metadata")
		}
	}
	for _, secret := range []string{"hidden", "private-body", "secret-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("sensitive log data")
		}
	}
}

func TestImageDecodedUploadLimit(t *testing.T) {
	for _, endpoint := range []string{"/images/generations", "/images/edits"} {
		t.Run(endpoint, func(t *testing.T) {
			var body bytes.Buffer
			zw := gzip.NewWriter(&body)
			_, _ = zw.Write(bytes.Repeat([]byte("x"), 4096))
			_ = zw.Close()
			router, _ := newRouter(t, fmt.Sprintf(nativeChatGPTConfigTemplate, blockedUpstream, blockedUpstream, blockedUpstream), nil)
			router.cfg.MaxRequestBytes = 128
			if body.Len() >= 128 {
				t.Fatal("fixture must fit encoded limit")
			}
			got := doRequest(router, "POST", "/v1"+endpoint, body.Bytes(), map[string]string{"Authorization": "Bearer test", "Content-Encoding": "gzip"})
			if got.Code != 413 {
				t.Fatalf("got %d: %s", got.Code, got.Body.String())
			}
		})
	}
}
