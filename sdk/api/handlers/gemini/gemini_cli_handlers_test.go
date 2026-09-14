package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
)

func TestGeminiCLIHandlerRequiresEndpointConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler := NewCLIAPIHandler(base)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost, "http://127.0.0.1:8080/v1internal:generateContent", strings.NewReader(`{}`),
	)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:8080"
	ctx.Request = req

	handler.CLIHandler(ctx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "Gemini CLI endpoint is disabled") {
		t.Fatalf("response body = %q, want disabled endpoint error", rec.Body.String())
	}
}

func TestGeminiCLIHandlerAllowsEndpointConfigBeforeLocalCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{EnableGeminiCLIEndpoint: true}, nil)
	handler := NewCLIAPIHandler(base)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost, "http://example.com/v1internal:generateContent", strings.NewReader(`{}`),
	)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "example.com"
	ctx.Request = req

	handler.CLIHandler(ctx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "CLI reply only allow local access") {
		t.Fatalf("response body = %q, want local access error", rec.Body.String())
	}
}

func TestGeminiCLILocalRequestRejectsForwardedClient(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1internal:generateContent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.41")
	client := access.ResolveClientAddress(req, []string{"127.0.0.1/32"})
	req = req.WithContext(access.WithClientAddress(req.Context(), client))

	if isGeminiCLILocalRequest(req) {
		t.Fatal("forwarded client was treated as a local Gemini CLI request")
	}
}

func TestGeminiCLILocalRequestRequiresLoopbackRemoteAndHost(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		host       string
		want       bool
	}{
		{name: "loopback remote and host", remoteAddr: "127.0.0.1:12345", host: "127.0.0.1:8080", want: true},
		{name: "loopback remote and localhost host", remoteAddr: "127.0.0.1:12345", host: "localhost:8080", want: true},
		{name: "loopback remote arbitrary host", remoteAddr: "127.0.0.1:12345", host: "example.com", want: false},
		{name: "non loopback remote", remoteAddr: "203.0.113.10:12345", host: "127.0.0.1:8080", want: false},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "http://"+tt.host+"/v1internal:generateContent", nil)
				req.RemoteAddr = tt.remoteAddr
				req.Host = tt.host

				if got := isGeminiCLILocalRequest(req); got != tt.want {
					t.Fatalf("isGeminiCLILocalRequest() = %v, want %v", got, tt.want)
				}
			},
		)
	}
}
