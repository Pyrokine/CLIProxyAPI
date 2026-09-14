package executor

import (
	"bytes"
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

func TestRecordAPIRequestClonesDeferredBodyWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	ctx := util.WithGinContext(context.Background(), ginCtx)
	body := []byte(`{"model":"original"}`)
	headers := http.Header{"Authorization": []string{"Bearer original-secret"}}

	recordAPIRequest(
		ctx, &config.Config{}, upstreamRequestLog{
			URL:     "https://api.example.com/v1/responses",
			Method:  http.MethodPost,
			Headers: headers,
			Body:    body,
		},
	)
	body[10] = 'X'
	headers.Set("Authorization", "Bearer mutated-secret")

	value, exists := ginCtx.Get(logging.DeferredAPIRequestContextKey)
	if !exists {
		t.Fatal("deferred API request was not captured")
	}
	requests, ok := value.([]logging.DeferredAPIRequest)
	if !ok || len(requests) != 1 {
		t.Fatalf("deferred API requests = %#v, want one request", value)
	}
	captured := string(requests[0]())
	if !strings.Contains(captured, `{"model":"original"}`) {
		t.Fatalf("captured API request = %q, want original body", captured)
	}
	if strings.Contains(captured, "original-secret") || strings.Contains(captured, "mutated-secret") {
		t.Fatalf("captured API request leaked Authorization: %q", captured)
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
	got, _ := value.(*logging.BoundedLogBuffer)
	gotText := got.String()
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
	attempt := &upstreamAttempt{index: 1, responseSource: source}

	writeAttemptResponse(ginCtx, attempt, []byte("Body:\nhello"))

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
	got, _ := value.(*logging.BoundedLogBuffer)
	if !strings.Contains(got.String(), "Body:\nhello") {
		t.Fatalf("api response fallback = %q, want body payload", got.String())
	}
}

func TestMoveFileBodySourceToFallbackRetainsUnreadablePrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "unreadable-prefix")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("confirmed-prefix")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	partPath := paths[0]
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
		t.Fatalf("create source part directory: %v", errMkdir)
	}
	nestedPath := filepath.Join(partPath, "nested")
	if errWrite := os.WriteFile(nestedPath, []byte("force read failure"), 0o600); errWrite != nil {
		t.Fatalf("write nested file: %v", errWrite)
	}
	ginCtx.Set(logging.APIResponseSourceContextKey, source)
	fallback := logging.NewBoundedLogBuffer()

	moved := moveFileBodySourceToFallback(
		ginCtx,
		logging.APIResponseSourceContextKey,
		apiResponseSourceFailedKey,
		source,
		fallback,
	)
	if moved {
		t.Fatal("unreadable source was detached")
	}
	if !fileBodySourceFailed(ginCtx, apiResponseSourceFailedKey) {
		t.Fatal("source failure state was not retained")
	}
	if sourceAfter := apiResponseSourceOrNil(ginCtx); sourceAfter != source {
		t.Fatal("source ownership was not retained")
	}
	if fallback.Len() != 0 {
		t.Fatalf("fallback contains unconfirmed prefix: %q", fallback.String())
	}

	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove nested file: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part directory: %v", errRemove)
	}
	if errWrite := os.WriteFile(partPath, []byte("confirmed-prefix"), 0o600); errWrite != nil {
		t.Fatalf("restore source part: %v", errWrite)
	}
	got, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes after restore: %v", errBytes)
	}
	if string(got) != "confirmed-prefix" {
		t.Fatalf("restored source = %q, want confirmed prefix", got)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
}

func TestWriteAttemptResponseUsesBoundedFallbackBuffer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	attempt := &upstreamAttempt{index: 1}
	writeAttemptResponse(ginCtx, attempt, []byte("first"))
	value, exists := ginCtx.Get(apiResponseKey)
	if !exists {
		t.Fatal("missing response fallback buffer")
	}
	buffer, ok := value.(*logging.BoundedLogBuffer)
	if !ok || buffer == nil {
		t.Fatalf("response fallback type = %T", value)
	}
	writeAttemptResponse(ginCtx, attempt, bytes.Repeat([]byte("x"), 9<<20))
	valueAfter, _ := ginCtx.Get(apiResponseKey)
	if valueAfter != buffer {
		t.Fatal("response fallback buffer was rebuilt")
	}
	if !buffer.Truncated() {
		t.Fatal("oversized response fallback was not truncated")
	}
}

func TestAppendAPIWebsocketTimelineUsesBoundedFallbackBuffer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	appendAPIWebsocketTimeline(ginCtx, bytes.Repeat([]byte("x"), 9<<20))

	value, exists := ginCtx.Get(apiWebsocketTimelineKey)
	if !exists {
		t.Fatal("missing API websocket fallback buffer")
	}
	buffer, ok := value.(*logging.BoundedLogBuffer)
	if !ok || buffer == nil || !buffer.Truncated() {
		t.Fatalf("API websocket fallback = %T, truncated = %t", value, ok && buffer.Truncated())
	}
}

func TestDisableFileBodySourceRetainsFailedCleanupOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "cleanup-owner")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("source payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	partPath := paths[0]
	sourceDir := filepath.Dir(partPath)
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
		t.Fatalf("create non-empty part directory: %v", errMkdir)
	}
	nestedPath := filepath.Join(partPath, "nested")
	if errWrite := os.WriteFile(nestedPath, []byte("keep directory non-empty"), 0o600); errWrite != nil {
		t.Fatalf("write nested file: %v", errWrite)
	}
	ginCtx.Set(logging.APIRequestSourceContextKey, source)
	queue := &logging.FileBodySourceCleanupQueue{}
	ginCtx.Set(logging.FileBodySourceCleanupQueueContextKey, queue)

	disableFileBodySource(ginCtx, logging.APIRequestSourceContextKey, source)
	if sourceAfter, ok := apiRequestSource(ginCtx); ok || sourceAfter != nil {
		t.Fatal("source should be disabled after cleanup failure")
	}
	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove nested file: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove part directory: %v", errRemove)
	}
	if errCleanup := queue.Cleanup(); errCleanup != nil {
		t.Fatalf("retry cleanup queue: %v", errCleanup)
	}
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("source directory %q still exists, stat error = %v", sourceDir, errStat)
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
	got, _ := value.(*logging.BoundedLogBuffer)
	gotText := got.String()
	firstIndex := strings.Index(gotText, "event-one")
	secondIndex := strings.Index(gotText, "event-two")
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("api websocket timeline fallback = %q, want both events", gotText)
	}
	if firstIndex > secondIndex {
		t.Fatalf("api websocket timeline fallback order = %q, want first before second", gotText)
	}
}
