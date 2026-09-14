package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestRequestExecutionMetadataAddsHTTPTraceCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request = request.WithContext(logging.WithRequestID(request.Context(), "1234abcd"))
	ctx := util.WithGinContext(context.Background(), ginCtx)

	meta := requestExecutionMetadata(ctx)
	callback, ok := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey].(func(string))
	if !ok || callback == nil {
		t.Fatal("missing selected auth index callback")
	}
	callback("auth-index")
	if got := logging.GetGinCPATraceID(ginCtx); !strings.HasSuffix(got, "-auth-index-1234abcd") {
		t.Fatalf("trace ID = %q", got)
	}
}

func TestRequestExecutionMetadataSkipsWebsocketTraceCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	ginCtx.Request = request.WithContext(logging.WithRequestID(request.Context(), "1234abcd"))
	ctx := util.WithGinContext(context.Background(), ginCtx)

	meta := requestExecutionMetadata(ctx)
	if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; exists {
		t.Fatal("unexpected selected auth index callback for websocket upgrade")
	}
}

func TestWriteErrorResponsePreservesCPAReservedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ginCtx.Writer.Header().Set(logging.CPATraceIDHeader, "local-trace")
	ginCtx.Writer.Header().Set("Access-Control-Expose-Headers", logging.CPATraceIDHeader)

	handler := NewBaseAPIHandlers(&config.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(
		ginCtx, &ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New("rate limit"),
			Addon: http.Header{
				"Retry-After":                 {"30"},
				logging.CPATraceIDHeader:      {"upstream-trace"},
				"Access-Control-Allow-Origin": {"https://upstream.example.com"},
				"Access-Control-Expose-Headers": {
					"upstream-header",
				},
			},
		},
	)

	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := recorder.Header().Get(logging.CPATraceIDHeader); got != "local-trace" {
		t.Fatalf("trace ID = %q, want local trace", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
	if got := recorder.Header().Get("Access-Control-Expose-Headers"); got != logging.CPATraceIDHeader {
		t.Fatalf("Access-Control-Expose-Headers = %q, want %q", got, logging.CPATraceIDHeader)
	}
}
