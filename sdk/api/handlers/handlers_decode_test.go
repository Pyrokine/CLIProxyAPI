package handlers

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestDecodeRequestBody(t *testing.T) {
	input := []byte("request payload")
	gzipBody := gzipBytes(t, input)
	deflateBody := zlibBytes(t, input)
	zstdBody := zstdBytes(t, input)
	chainedBody := zstdBytes(t, gzipBody)

	tests := []struct {
		name     string
		body     []byte
		encoding string
	}{
		{name: "identity", body: input, encoding: ""},
		{name: "gzip", body: gzipBody, encoding: "gzip"},
		{name: "deflate", body: deflateBody, encoding: "deflate"},
		{name: "zstd", body: zstdBody, encoding: "zstd"},
		{name: "chained", body: chainedBody, encoding: "gzip, zstd"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				decoded, errDecode := DecodeRequestBody(tt.body, tt.encoding)
				if errDecode != nil {
					t.Fatalf("DecodeRequestBody returned error: %v", errDecode)
				}
				if !bytes.Equal(decoded, input) {
					t.Fatalf("decoded = %q, want %q", decoded, input)
				}
			},
		)
	}
}

func TestReadRequestBodyWithLimitRejectsOversizedRawBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader("12345"),
	)

	_, err := ReadRequestBodyWithLimit(ginContext, 4)
	if err == nil || !strings.Contains(err.Error(), "request body too large") {
		t.Fatalf("ReadRequestBodyWithLimit error = %v, want request body too large", err)
	}
}

func TestDecodeRequestBodyRejectsUnsupportedEncoding(t *testing.T) {
	_, errDecode := DecodeRequestBody([]byte("payload"), "br")
	if errDecode == nil {
		t.Fatal("expected unsupported encoding error")
	}
}

func TestDecodeRequestBodyRejectsOversizedDecodedBody(t *testing.T) {
	oversized := strings.Repeat("x", maxDecodedRequestBodyBytes+1)
	_, errDecode := DecodeRequestBody(gzipBytes(t, []byte(oversized)), "gzip")
	if errDecode == nil {
		t.Fatal("expected oversized decoded body error")
	}
}

func gzipBytes(t *testing.T, input []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, errWrite := writer.Write(input); errWrite != nil {
		t.Fatalf("gzip write failed: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("gzip close failed: %v", errClose)
	}
	return out.Bytes()
}

func zlibBytes(t *testing.T, input []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zlib.NewWriter(&out)
	if _, errWrite := writer.Write(input); errWrite != nil {
		t.Fatalf("zlib write failed: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("zlib close failed: %v", errClose)
	}
	return out.Bytes()
}

func zstdBytes(t *testing.T, input []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer, errWriter := zstd.NewWriter(&out)
	if errWriter != nil {
		t.Fatalf("zstd writer failed: %v", errWriter)
	}
	if _, errWrite := writer.Write(input); errWrite != nil {
		t.Fatalf("zstd write failed: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("zstd close failed: %v", errClose)
	}
	return out.Bytes()
}
