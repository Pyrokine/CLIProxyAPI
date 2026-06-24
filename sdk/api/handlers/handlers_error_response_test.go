package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestWriteErrorResponse_AddonHeadersDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(
		c, &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New("rate limit"),
			Addon: http.Header{
				"Retry-After":  {"30"},
				"X-Request-Id": {"req-1"},
			},
		},
	)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After should be empty when passthrough is disabled, got %q", got)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id should be empty when passthrough is disabled, got %q", got)
	}
}

func TestWriteErrorResponse_DoesNotAppendAPIResponseWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(
		c, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      errors.New("upstream failure"),
		},
	)

	if _, exists := c.Get("API_RESPONSE"); exists {
		t.Fatal("API_RESPONSE should not be captured when request logging is disabled")
	}
}

func TestWriteErrorResponse_DoesNotAppendAPIResponseWhenCaptured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set(logging.APIResponseCapturedContextKey, true)
	c.Set("API_RESPONSE", []byte("upstream response"))

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(
		c, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      errors.New("upstream failure"),
		},
	)

	if got, _ := c.Get("API_RESPONSE"); string(got.([]byte)) != "upstream response" {
		t.Fatalf("API_RESPONSE = %q, want upstream response", got)
	}
}

func TestWriteErrorResponse_AddonHeadersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Writer.Header().Set("X-Request-Id", "old-value")

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(
		c, &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New("rate limit"),
			Addon: http.Header{
				"Retry-After":  {"30"},
				"X-Request-Id": {"new-1", "new-2"},
			},
		},
	)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := recorder.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"new-1", "new-2"}) {
		t.Fatalf("X-Request-Id = %#v, want %#v", got, []string{"new-1", "new-2"})
	}
}
