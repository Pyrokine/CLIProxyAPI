package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetSSEHeadersUsesUnbufferedContractForHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		protoMajor     int
		wantConnection string
	}{
		{name: "HTTP/1.x", protoMajor: 1, wantConnection: "keep-alive"},
		{name: "HTTP/2", protoMajor: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/stream", nil)
			context.Request.ProtoMajor = test.protoMajor

			SetSSEHeaders(context)
			WriteUpstreamHeaders(
				context.Writer.Header(),
				http.Header{
					"Content-Type":      []string{"application/json"},
					"Cache-Control":     []string{"public, max-age=60"},
					"X-Accel-Buffering": []string{"yes"},
				},
			)

			header := context.Writer.Header()
			if got := header.Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", got)
			}
			if got := header.Get("Cache-Control"); got != "no-cache, no-transform" {
				t.Fatalf("Cache-Control = %q, want no-cache, no-transform", got)
			}
			if got := header.Get("X-Accel-Buffering"); got != "no" {
				t.Fatalf("X-Accel-Buffering = %q, want no", got)
			}
			if got := header.Get("Connection"); got != test.wantConnection {
				t.Fatalf("Connection = %q, want %q", got, test.wantConnection)
			}
		})
	}
}

func TestFilterUpstreamHeaders_RemovesConnectionScopedHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "keep-alive, x-hop-a, x-hop-b")
	src.Add("Connection", "x-hop-c")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("X-Hop-C", "c")
	src.Set("X-Request-Id", "req-1")
	src.Set("Set-Cookie", "session=secret")
	src.Set("x-cpa-trace-id", "upstream-trace")
	src.Set("Access-Control-Allow-Origin", "https://upstream.example.com")
	src.Set("Access-Control-Expose-Headers", "upstream-header")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected filtered headers, got nil")
	}

	requestID := filtered.Get("X-Request-Id")
	if requestID != "req-1" {
		t.Fatalf("expected X-Request-Id to be preserved, got %q", requestID)
	}

	blockedHeaderKeys := []string{
		"Connection",
		"Keep-Alive",
		"X-Hop-A",
		"X-Hop-B",
		"X-Hop-C",
		"Set-Cookie",
		"x-cpa-trace-id",
		"Access-Control-Allow-Origin",
		"Access-Control-Expose-Headers",
	}
	for _, key := range blockedHeaderKeys {
		value := filtered.Get(key)
		if value != "" {
			t.Fatalf("expected %s to be removed, got %q", key, value)
		}
	}
}

func TestFilterUpstreamHeaders_ReturnsNilWhenAllHeadersBlocked(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "x-hop-a")
	src.Set("X-Hop-A", "a")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered != nil {
		t.Fatalf("expected nil when all headers are filtered, got %#v", filtered)
	}
}
