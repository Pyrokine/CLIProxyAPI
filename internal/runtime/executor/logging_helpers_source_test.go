package executor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
)

func TestRecordAPIRequestSkipsCommercialMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &config.Config{CommercialMode: true}
	cfg.RequestLog = true

	recordAPIRequest(ctx, cfg, upstreamRequestLog{URL: "https://upstream.example/v1", Method: http.MethodPost})

	if _, exists := ginCtx.Get(apiAttemptsKey); exists {
		t.Fatal("api attempts should not be recorded in commercial mode")
	}
	if _, exists := ginCtx.Get(apiRequestKey); exists {
		t.Fatal("api request should not be recorded in commercial mode")
	}
}

func TestRecordAPIRequestSourceFailureKeepsPreviousRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-request")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	ginCtx.Set(logging.APIRequestSourceContextKey, source)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &config.Config{}
	cfg.RequestLog = true

	recordAPIRequest(ctx, cfg, upstreamRequestLog{URL: "https://upstream.example/first", Method: http.MethodPost})
	paths := source.Paths()
	if len(paths) == 0 {
		t.Fatal("api request source did not create a part file")
	}
	if errChmod := os.Chmod(paths[0], 0o400); errChmod != nil {
		t.Fatalf("chmod source part: %v", errChmod)
	}

	recordAPIRequest(ctx, cfg, upstreamRequestLog{URL: "https://upstream.example/second", Method: http.MethodPost})

	if sourceAfter, ok := apiRequestSource(ginCtx); ok || sourceAfter != nil {
		t.Fatal("api request source should be disabled after write failure")
	}
	value, exists := ginCtx.Get(apiRequestKey)
	if !exists {
		t.Fatal("api request fallback was not aggregated")
	}
	got, _ := value.([]byte)
	gotText := string(got)
	firstIndex := strings.Index(gotText, "https://upstream.example/first")
	secondIndex := strings.Index(gotText, "https://upstream.example/second")
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("api request fallback = %q, want both requests", gotText)
	}
	if firstIndex > secondIndex {
		t.Fatalf("api request fallback order = %q, want first before second", gotText)
	}
}

func TestWriteAttemptResponseFallbackDisablesBrokenSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-response")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("=== API RESPONSE 1 ===\n")); errAppend != nil {
		t.Fatalf("append source prefix: %v", errAppend)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("cleanup source: %v", errCleanup)
	}
	ginCtx.Set(logging.APIResponseSourceContextKey, source)
	attempt := &upstreamAttempt{index: 1, responseSource: source, response: &strings.Builder{}}

	writeAttemptResponse(ginCtx, attempt, []byte("Body:\nhello"))
	updateAggregatedResponseIfMemoryBacked(ginCtx, []*upstreamAttempt{attempt})

	if attempt.responseSource != nil {
		t.Fatal("response source should be disabled after write failure")
	}
	if sourceAfter := apiResponseSourceOrNil(ginCtx); sourceAfter != nil {
		t.Fatal("gin context response source should be disabled after write failure")
	}
	value, exists := ginCtx.Get(apiResponseKey)
	if !exists {
		t.Fatal("api response fallback was not aggregated")
	}
	got, _ := value.([]byte)
	if !strings.Contains(string(got), "Body:\nhello") {
		t.Fatalf("api response fallback = %q, want body payload", string(got))
	}
}

func TestAppendAPIWebsocketTimelineSourceFailureKeepsOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-websocket-timeline")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	ginCtx.Set(logging.APIWebsocketTimelineSourceContextKey, source)

	appendAPIWebsocketTimeline(ginCtx, []byte("event-one"))
	paths := source.Paths()
	if len(paths) == 0 {
		t.Fatal("api websocket timeline source did not create a part file")
	}
	sourceDir := filepath.Dir(paths[0])
	if errChmod := os.Chmod(sourceDir, 0o500); errChmod != nil {
		t.Fatalf("chmod source dir: %v", errChmod)
	}
	defer func() {
		_ = os.Chmod(sourceDir, 0o700)
		_ = os.RemoveAll(sourceDir)
	}()

	appendAPIWebsocketTimeline(ginCtx, []byte("event-two"))

	if sourceAfter, ok := apiWebsocketTimelineSource(ginCtx); ok || sourceAfter != nil {
		t.Fatal("api websocket timeline source should be disabled after write failure")
	}
	value, exists := ginCtx.Get(apiWebsocketTimelineKey)
	if !exists {
		t.Fatal("api websocket timeline fallback was not aggregated")
	}
	got, _ := value.([]byte)
	gotText := string(got)
	firstIndex := strings.Index(gotText, "event-one")
	secondIndex := strings.Index(gotText, "event-two")
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("api websocket timeline fallback = %q, want both events", gotText)
	}
	if firstIndex > secondIndex {
		t.Fatalf("api websocket timeline fallback order = %q, want first before second", gotText)
	}
}
