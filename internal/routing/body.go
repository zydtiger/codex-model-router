package routing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// requestError is a client-facing failure. The message is safe to return: it
// describes the request shape, never its content.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.message }

func clientError(status int, code, format string, args ...any) *requestError {
	return &requestError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// decodedBody keeps both representations of a request body. Native upstreams
// receive Raw so that an encoded body stays byte-for-byte intact; remote routes
// receive the decoded JSON because they are translated.
type decodedBody struct {
	Raw     []byte
	Decoded []byte
}

// readBody reads and decodes a request body within the configured bound.
//
// Codex sends JSON, sometimes compressed. The decoder set is deliberately
// small and an unsupported encoding is rejected instead of forwarded: routing
// decisions need the model ID, and an opaque body cannot be inspected.
func readBody(r *http.Request, limit int64) (*decodedBody, *requestError) {
	if r.Body == nil {
		return &decodedBody{}, nil
	}
	defer r.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return nil, clientError(statusClientClosed, "request_cancelled", "request cancelled: %v", ctxErr)
		}
		return nil, clientError(http.StatusBadRequest, "request_read_failed", "could not read the request body")
	}
	if int64(len(raw)) > limit {
		return nil, clientError(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds %d bytes", limit)
	}

	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return &decodedBody{Raw: raw, Decoded: raw}, nil
	}

	decoded, err := decodeBody(r.Context(), encoding, raw, limit)
	if err != nil {
		var tooLarge *bodyTooLargeError
		if errors.As(err, &tooLarge) {
			return nil, clientError(http.StatusRequestEntityTooLarge, "request_too_large", "decoded request body exceeds %d bytes", limit)
		}
		var unsupported *unsupportedEncodingError
		if errors.As(err, &unsupported) {
			return nil, clientError(http.StatusUnsupportedMediaType, "unsupported_content_encoding",
				"Content-Encoding %q is not supported; send identity, gzip, or zstd", encoding)
		}
		return nil, clientError(http.StatusBadRequest, "request_decode_failed", "could not decode the %s request body", encoding)
	}
	return &decodedBody{Raw: raw, Decoded: decoded}, nil
}

type unsupportedEncodingError struct{ encoding string }

func (e *unsupportedEncodingError) Error() string { return "unsupported encoding " + e.encoding }

type bodyTooLargeError struct{}

func (e *bodyTooLargeError) Error() string { return "decoded body too large" }

// maxConcurrentZstd bounds how many zstd bodies decode at once. Each decode
// preallocates a destination capped at the request limit, so the product of the
// two is the real memory ceiling for this path.
const maxConcurrentZstd = 4

var (
	zstdSlots = make(chan struct{}, maxConcurrentZstd)

	// zstdDecoder is shared and safe for concurrent DecodeAll calls. The
	// capacity limit turns a decompression bomb into an error instead of an
	// unbounded allocation.
	zstdDecoder = mustZstdDecoder()
)

func mustZstdDecoder() *zstd.Decoder {
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecodeAllCapLimit(true),
		zstd.WithDecoderMaxMemory(uint64(maxConcurrentZstd)*uint64(DefaultZstdMemoryBudget)),
	)
	if err != nil {
		panic("zstd decoder: " + err.Error())
	}
	return decoder
}

// DefaultZstdMemoryBudget is the per-decoder window budget used by the shared
// decoder. Request output size is bounded separately by the request limit.
const DefaultZstdMemoryBudget = 64 << 20

func decodeBody(ctx context.Context, encoding string, raw []byte, limit int64) ([]byte, error) {
	switch encoding {
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return readCapped(reader, limit)
	case "zstd":
		select {
		case zstdSlots <- struct{}{}:
			defer func() { <-zstdSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// The destination capacity is the decoder's output ceiling.
		decoded, err := zstdDecoder.DecodeAll(raw, make([]byte, 0, limit+1))
		if err != nil {
			if int64(len(decoded)) > limit {
				return nil, &bodyTooLargeError{}
			}
			return nil, err
		}
		if int64(len(decoded)) > limit {
			return nil, &bodyTooLargeError{}
		}
		return decoded, nil
	default:
		return nil, &unsupportedEncodingError{encoding: encoding}
	}
}

func readCapped(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		// A capped read can surface a decompression error at the cap; treat a
		// size overflow as the oversized case so the status code is honest.
		if int64(len(data)) > limit {
			return nil, &bodyTooLargeError{}
		}
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &bodyTooLargeError{}
	}
	return data, nil
}

// decodeJSON decodes a request body into its top-level fields.
func decodeJSON(body []byte) (map[string]json.RawMessage, *requestError) {
	if len(body) == 0 {
		return nil, clientError(http.StatusBadRequest, "empty_request_body", "a JSON request body is required")
	}
	if err := checkJSONShape(body); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, clientError(http.StatusBadRequest, "invalid_json", "the request body is not a JSON object")
	}
	return fields, nil
}

// checkJSONShape rejects bodies that are valid JSON but not an object. The
// decoder is configured to reject trailing content, which also catches a
// truncated or concatenated payload.
func checkJSONShape(body []byte) *requestError {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var first json.RawMessage
	if err := decoder.Decode(&first); err != nil {
		return clientError(http.StatusBadRequest, "invalid_json", "the request body is not valid JSON")
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return clientError(http.StatusBadRequest, "invalid_json", "the request body must contain exactly one JSON value")
	}
	if len(first) == 0 || first[0] != '{' {
		return clientError(http.StatusBadRequest, "invalid_json", "the request body must be a JSON object")
	}
	return nil
}

// encodeJSON marshals top-level fields back into a body.
func encodeJSON(fields map[string]json.RawMessage) ([]byte, *requestError) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, clientError(http.StatusInternalServerError, "encode_failed", "could not encode the routed request")
	}
	return encoded, nil
}
