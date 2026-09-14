package executor

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

type claudeDecodeTrackingBody struct {
	io.Reader
	closeCount int
	closeErr   error
}

func (b *claudeDecodeTrackingBody) Close() error {
	b.closeCount++
	return b.closeErr
}

func TestDecodeResponseBodyStackedRepeatedHeaders(t *testing.T) {
	payload := []byte("stacked Claude response")
	compressed := encodeClaudeTestBrotli(t, encodeClaudeTestGzip(t, payload))
	header := make(http.Header)
	header.Add("Content-Encoding", "gzip")
	header.Add("Content-Encoding", "br")

	decoded, errDecode := decodeResponseBody(
		io.NopCloser(bytes.NewReader(compressed)),
		claudeResponseContentEncoding(header),
	)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	t.Cleanup(func() {
		if errClose := decoded.Close(); errClose != nil {
			t.Errorf("close decoded response: %v", errClose)
		}
	})

	got, errRead := io.ReadAll(decoded)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded body = %q, want %q", got, payload)
	}
}

func TestDecodeResponseBodyDeflateVariants(t *testing.T) {
	payload := []byte("Claude deflate response")
	tests := []struct {
		name       string
		compressed []byte
	}{
		{name: "zlib", compressed: encodeClaudeTestZlib(t, payload)},
		{name: "raw", compressed: encodeClaudeTestRawDeflate(t, payload)},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				decoded, errDecode := decodeResponseBody(
					io.NopCloser(bytes.NewReader(test.compressed)),
					"deflate",
				)
				if errDecode != nil {
					t.Fatal(errDecode)
				}
					t.Cleanup(func() {
						if errClose := decoded.Close(); errClose != nil {
							t.Errorf("close decoded response: %v", errClose)
						}
					})

				got, errRead := io.ReadAll(decoded)
				if errRead != nil {
					t.Fatal(errRead)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("decoded body = %q, want %q", got, payload)
				}
			},
		)
	}
}

func TestDecodeResponseBodyRejectsUnknownEncodingAndClosesBody(t *testing.T) {
	compressed := encodeClaudeTestGzip(t, []byte("encoded payload"))
	body := &claudeDecodeTrackingBody{Reader: bytes.NewReader(compressed)}

	decoded, errDecode := decodeResponseBody(body, "unknown, gzip")
	if decoded != nil {
		t.Fatal("decoded body should be nil")
	}
	if errDecode == nil || !strings.Contains(errDecode.Error(), `unsupported content encoding "unknown"`) {
		t.Fatalf("decode error = %v", errDecode)
	}
	if body.closeCount != 1 {
		t.Fatalf("body close count = %d, want 1", body.closeCount)
	}
}

func TestDecodeResponseBodyClosesBodyWhenInnerDecoderFails(t *testing.T) {
	compressed := encodeClaudeTestBrotli(t, []byte("not a gzip stream"))
	body := &claudeDecodeTrackingBody{Reader: bytes.NewReader(compressed)}

	decoded, errDecode := decodeResponseBody(body, "gzip, br")
	if decoded != nil {
		t.Fatal("decoded body should be nil")
	}
	if errDecode == nil || !strings.Contains(errDecode.Error(), "failed to create gzip reader") {
		t.Fatalf("decode error = %v", errDecode)
	}
	if body.closeCount != 1 {
		t.Fatalf("body close count = %d, want 1", body.closeCount)
	}
}

func TestDecodeResponseBodyClosePropagatesBodyError(t *testing.T) {
	closeErr := errors.New("close body")
	body := &claudeDecodeTrackingBody{
		Reader:   bytes.NewReader(encodeClaudeTestGzip(t, []byte("payload"))),
		closeErr: closeErr,
	}
	decoded, errDecode := decodeResponseBody(body, "gzip")
	if errDecode != nil {
		t.Fatal(errDecode)
	}

	if errClose := decoded.Close(); !errors.Is(errClose, closeErr) {
		t.Fatalf("close error = %v, want %v", errClose, closeErr)
	}
	if body.closeCount != 1 {
		t.Fatalf("body close count = %d, want 1", body.closeCount)
	}
}

func encodeClaudeTestGzip(t *testing.T, payload []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return output.Bytes()
}

func encodeClaudeTestBrotli(t *testing.T, payload []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := brotli.NewWriter(&output)
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return output.Bytes()
}

func encodeClaudeTestZlib(t *testing.T, payload []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zlib.NewWriter(&output)
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return output.Bytes()
}

func encodeClaudeTestRawDeflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, errWriter := flate.NewWriter(&output, flate.DefaultCompression)
	if errWriter != nil {
		t.Fatal(errWriter)
	}
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return output.Bytes()
}
