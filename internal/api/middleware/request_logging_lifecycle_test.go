package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

type requestLogCapture struct {
	mu                   sync.Mutex
	enabled              bool
	calls                int
	streamCalls          int
	statuses             []int
	response             []byte
	responseHeaders      map[string][]string
	apiRequest           []byte
	apiWebsocketTimeline []byte
	streamWriter         logging.StreamingLogWriter
	streamErr            error
	streamPanic          any
}

func (l *requestLogCapture) LogRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, _ []byte,
	apiRequest []byte,
	_ []byte,
	apiWebsocketTimeline []byte,
	_ []*interfaces.ErrorMessage,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.statuses = append(l.statuses, statusCode)
	l.response = bytes.Clone(response)
	l.responseHeaders = cloneTestHeaders(responseHeaders)
	l.apiRequest = bytes.Clone(apiRequest)
	l.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	return nil
}

func (l *requestLogCapture) LogStreamingRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ string,
) (logging.StreamingLogWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streamCalls++
	if l.streamPanic != nil {
		panic(l.streamPanic)
	}
	return l.streamWriter, l.streamErr
}

func (l *requestLogCapture) IsEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

func (l *requestLogCapture) snapshot() (int, []int, []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, append([]int(nil), l.statuses...), bytes.Clone(l.apiWebsocketTimeline)
}

func (l *requestLogCapture) streamCallCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streamCalls
}

func (l *requestLogCapture) responseSnapshot() ([]byte, map[string][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.response), cloneTestHeaders(l.responseHeaders)
}

func cloneTestHeaders(headers map[string][]string) map[string][]string {
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

type cleanupRetryRequestLogCapture struct {
	*requestLogCapture
	mu           sync.Mutex
	cleanupCalls int
	cleanupErr   error
	cleanupPanic any
}

func (l *cleanupRetryRequestLogCapture) RetryPendingCleanup() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupCalls++
	if l.cleanupPanic != nil {
		panic(l.cleanupPanic)
	}
	return l.cleanupErr
}

func (l *cleanupRetryRequestLogCapture) cleanupCallCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleanupCalls
}

type sourceRequestLogCapture struct {
	*requestLogCapture
	dir      string
	sourceMu sync.Mutex
	sources  []*logging.FileBodySource
}

func (l *sourceRequestLogCapture) NewFileBodySource(prefix string) (*logging.FileBodySource, error) {
	source, errSource := logging.NewFileBodySourceInDir(l.dir, prefix)
	if errSource != nil {
		return nil, errSource
	}
	l.sourceMu.Lock()
	l.sources = append(l.sources, source)
	l.sourceMu.Unlock()
	return source, nil
}

type sourceFactoryFuncLogger struct {
	*requestLogCapture
	factory func(string) (*logging.FileBodySource, error)
}

func (l *sourceFactoryFuncLogger) NewFileBodySource(prefix string) (*logging.FileBodySource, error) {
	return l.factory(prefix)
}

type panicSourceFactoryLogger struct {
	*requestLogCapture
	dir     string
	mu      sync.Mutex
	calls   int
	sources []*logging.FileBodySource
}

func (l *panicSourceFactoryLogger) NewFileBodySource(prefix string) (*logging.FileBodySource, error) {
	l.mu.Lock()
	l.calls++
	call := l.calls
	l.mu.Unlock()
	if call == 2 {
		panic("source factory panic")
	}
	source, errSource := logging.NewFileBodySourceInDir(l.dir, prefix)
	if errSource != nil {
		return nil, errSource
	}
	l.mu.Lock()
	l.sources = append(l.sources, source)
	l.mu.Unlock()
	return source, nil
}

type requestLoggerSnapshotHolder struct {
	*requestLogCapture
	mu       sync.Mutex
	snapshot logging.RequestLogger
}

func (l *requestLoggerSnapshotHolder) Snapshot() logging.RequestLogger {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshot
}

func (l *requestLoggerSnapshotHolder) setSnapshot(snapshot logging.RequestLogger) {
	l.mu.Lock()
	l.snapshot = snapshot
	l.mu.Unlock()
}

type panicEnabledRequestLogger struct {
	*sourceRequestLogCapture
	mu         sync.Mutex
	enabledOps int
	panicAfter int
}

func (l *panicEnabledRequestLogger) IsEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabledOps++
	if l.enabledOps >= l.panicAfter {
		panic("enabled state panic")
	}
	return l.requestLogCapture.IsEnabled()
}

type partialResponseWriter struct {
	gin.ResponseWriter
	limit int
	err   error
}

func (w *partialResponseWriter) Write(data []byte) (int, error) {
	limit := min(w.limit, len(data))
	if limit > 0 {
		_, _ = w.ResponseWriter.Write(data[:limit])
	}
	return limit, w.err
}

func (w *partialResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

type streamingLogCapture struct {
	mu                   sync.Mutex
	chunks               []byte
	status               int
	headers              map[string][]string
	apiRequest           []byte
	apiResponse          []byte
	apiWebsocketTimeline []byte
	closeCalls           int
}

func (w *streamingLogCapture) WriteChunkAsync(chunk []byte) {
	w.mu.Lock()
	w.chunks = append(w.chunks, chunk...)
	w.mu.Unlock()
}

func (w *streamingLogCapture) WriteStatus(status int, headers map[string][]string) error {
	w.mu.Lock()
	w.status = status
	w.headers = cloneTestHeaders(headers)
	w.mu.Unlock()
	return nil
}

func (w *streamingLogCapture) WriteAPIRequest(apiRequest []byte) error {
	w.mu.Lock()
	w.apiRequest = bytes.Clone(apiRequest)
	w.mu.Unlock()
	return nil
}

func (w *streamingLogCapture) WriteAPIResponse(apiResponse []byte) error {
	w.mu.Lock()
	w.apiResponse = bytes.Clone(apiResponse)
	w.mu.Unlock()
	return nil
}

func (w *streamingLogCapture) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	w.mu.Lock()
	w.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	w.mu.Unlock()
	return nil
}

func (w *streamingLogCapture) SetFirstChunkTimestamp(_ time.Time) {}

func (w *streamingLogCapture) WriteRequestTimeline(_ []byte) error {
	return nil
}

func (w *streamingLogCapture) Close() error {
	w.mu.Lock()
	w.closeCalls++
	w.mu.Unlock()
	return nil
}

func (w *streamingLogCapture) snapshot() ([]byte, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.apiWebsocketTimeline), w.closeCalls
}

type sourceStreamingLogCapture struct {
	*streamingLogCapture
	mu                         sync.Mutex
	apiRequestSource           *logging.FileBodySource
	apiResponseSource          *logging.FileBodySource
	apiWebsocketTimelineSource *logging.FileBodySource
}

func (w *sourceStreamingLogCapture) WriteAPIRequestSource(source *logging.FileBodySource) error {
	w.mu.Lock()
	w.apiRequestSource = source
	w.mu.Unlock()
	return nil
}

func (w *sourceStreamingLogCapture) WriteAPIResponseSource(source *logging.FileBodySource) error {
	w.mu.Lock()
	w.apiResponseSource = source
	w.mu.Unlock()
	return nil
}

func (w *sourceStreamingLogCapture) WriteAPIWebsocketTimelineSource(source *logging.FileBodySource) error {
	w.mu.Lock()
	w.apiWebsocketTimelineSource = source
	w.mu.Unlock()
	return nil
}

func (w *sourceStreamingLogCapture) Close() error {
	w.mu.Lock()
	sources := []*logging.FileBodySource{
		w.apiRequestSource,
		w.apiResponseSource,
		w.apiWebsocketTimelineSource,
	}
	w.mu.Unlock()
	cleanupFileBodySources(sources...)
	return w.streamingLogCapture.Close()
}

type errorStreamingLogCapture struct {
	*streamingLogCapture
	statusErr           error
	apiRequestSourceErr error
	closeErr            error
}

func (w *errorStreamingLogCapture) WriteStatus(_ int, _ map[string][]string) error {
	return w.statusErr
}

func (w *errorStreamingLogCapture) WriteAPIRequestSource(*logging.FileBodySource) error {
	return w.apiRequestSourceErr
}

func (*errorStreamingLogCapture) WriteAPIResponseSource(*logging.FileBodySource) error {
	return nil
}

func (*errorStreamingLogCapture) WriteAPIWebsocketTimelineSource(*logging.FileBodySource) error {
	return nil
}

func (w *errorStreamingLogCapture) Close() error {
	_ = w.streamingLogCapture.Close()
	return w.closeErr
}

type panicStreamingLogCapture struct {
	*sourceStreamingLogCapture
	chunkPanic      any
	apiRequestPanic any
	closePanic      any
}

func (w *panicStreamingLogCapture) WriteChunkAsync(chunk []byte) {
	if w.chunkPanic != nil {
		panic(w.chunkPanic)
	}
	w.streamingLogCapture.WriteChunkAsync(chunk)
}

func (w *panicStreamingLogCapture) WriteAPIRequest(apiRequest []byte) error {
	if w.apiRequestPanic != nil {
		panic(w.apiRequestPanic)
	}
	return w.streamingLogCapture.WriteAPIRequest(apiRequest)
}

func (w *panicStreamingLogCapture) Close() error {
	errClose := w.sourceStreamingLogCapture.Close()
	if w.closePanic != nil {
		panic(w.closePanic)
	}
	return errClose
}

func TestRequestLoggingMiddlewareRetriesCleanupWhenLoggerDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &cleanupRetryRequestLogCapture{
		requestLogCapture: &requestLogCapture{enabled: false},
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		bytes.NewBufferString(`{"model":"claude"}`),
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if calls := logger.cleanupCallCount(); calls != 1 {
		t.Fatalf("cleanup retry calls = %d, want 1", calls)
	}
	calls, _, _ := logger.requestLogCapture.snapshot()
	if calls != 0 {
		t.Fatalf("request log calls = %d, want 0", calls)
	}
}

func TestRequestLoggingMiddlewareRetriesCleanupWhenRuntimeDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &cleanupRetryRequestLogCapture{
		requestLogCapture: &requestLogCapture{enabled: false},
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(
		logger,
		RequestLoggingOptions{RuntimeEnabled: func() bool { return false }},
	))
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		bytes.NewBufferString(`{"model":"claude"}`),
	)
	engine.ServeHTTP(recorder, request)
	if calls := logger.cleanupCallCount(); calls != 1 {
		t.Fatalf("cleanup retry calls = %d, want 1", calls)
	}
	calls, _, _ := logger.requestLogCapture.snapshot()
	if calls != 0 {
		t.Fatalf("request log calls = %d, want 0", calls)
	}
}

func TestRequestLoggingMiddlewarePanicFinalizesAndRethrows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &sourceRequestLogCapture{
		requestLogCapture: &requestLogCapture{enabled: true},
		dir:               t.TempDir(),
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))

	var partPaths []string
	engine.GET("/v1/responses", func(c *gin.Context) {
		for _, key := range []string{
			logging.APIRequestSourceContextKey,
			logging.APIResponseSourceContextKey,
			logging.WebsocketTimelineSourceContextKey,
			logging.APIWebsocketTimelineSourceContextKey,
		} {
			value, exists := c.Get(key)
			if !exists {
				t.Fatalf("source %q missing", key)
			}
			source, ok := value.(*logging.FileBodySource)
			if !ok || source == nil {
				t.Fatalf("source %q has unexpected type", key)
			}
			if errAppend := source.AppendBytes([]byte(`{"visible":"value"}`)); errAppend != nil {
				t.Fatalf("AppendBytes(%s): %v", key, errAppend)
			}
			partPaths = append(partPaths, source.Paths()...)
		}
		panic("request panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		engine.ServeHTTP(recorder, request)
	}()

	if panicValue != "request panic" {
		t.Fatalf("panic = %v, want request panic", panicValue)
	}
	calls, statuses, _ := logger.requestLogCapture.snapshot()
	if calls != 1 || len(statuses) != 1 || statuses[0] != http.StatusInternalServerError {
		t.Fatalf("log calls = %d, statuses = %v", calls, statuses)
	}
	if len(partPaths) != 4 {
		t.Fatalf("part path count = %d, want 4", len(partPaths))
	}
	assertFilesRemoved(t, partPaths)
}

func TestRequestLoggingMiddlewareRepanicReachesGinRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &requestLogCapture{enabled: true}
	engine := gin.New()
	engine.Use(gin.RecoveryWithWriter(io.Discard), RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(*gin.Context) {
		panic("request panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	calls, statuses, _ := logger.snapshot()
	if calls != 1 || len(statuses) != 1 || statuses[0] != http.StatusInternalServerError {
		t.Fatalf("log calls = %d, statuses = %v", calls, statuses)
	}
}

func TestRequestLoggingMiddlewareClosesStreamingLoggerBeforeOuterRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &streamingLogCapture{}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	engine := gin.New()
	engine.Use(gin.RecoveryWithWriter(io.Discard), RequestLoggingMiddleware(logger))
	engine.POST("/v1/responses", func(*gin.Context) {
		panic("request panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		bytes.NewBufferString(`{"stream":true}`),
	)
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if calls := logger.streamCallCount(); calls != 1 {
		t.Fatalf("stream initialization calls = %d, want 1", calls)
	}
	streamWriter.mu.Lock()
	status := streamWriter.status
	closeCalls := streamWriter.closeCalls
	streamWriter.mu.Unlock()
	if status != http.StatusInternalServerError {
		t.Fatalf("stream status = %d, want %d", status, http.StatusInternalServerError)
	}
	if closeCalls != 1 {
		t.Fatalf("stream close calls = %d, want 1", closeCalls)
	}
}

func TestRequestLoggingMiddlewarePreservesCommittedStatusAfterPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &requestLogCapture{enabled: false}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.String(http.StatusAccepted, "accepted")
		panic("request panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		engine.ServeHTTP(recorder, request)
	}()

	if panicValue != "request panic" {
		t.Fatalf("panic = %v, want request panic", panicValue)
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	calls, statuses, _ := logger.snapshot()
	if calls != 1 || len(statuses) != 1 || statuses[0] != http.StatusAccepted {
		t.Fatalf("forced log calls = %d, statuses = %v", calls, statuses)
	}
}

func TestRequestLoggingMiddlewareLoggerPanicDoesNotReplaceHandlerPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &panicStreamingLogCapture{
		sourceStreamingLogCapture: &sourceStreamingLogCapture{
			streamingLogCapture: &streamingLogCapture{},
		},
		apiRequestPanic: "logger panic",
	}
	logger := &sourceRequestLogCapture{
		requestLogCapture: &requestLogCapture{
			enabled:      true,
			streamWriter: streamWriter,
		},
		dir: t.TempDir(),
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))

	var partPath string
	engine.POST("/v1/responses", func(c *gin.Context) {
		value, exists := c.Get(logging.APIRequestSourceContextKey)
		if !exists {
			t.Fatal("API request source missing")
		}
		source, ok := value.(*logging.FileBodySource)
		if !ok || source == nil {
			t.Fatalf("API request source has unexpected type %T", value)
		}
		if errAppend := source.AppendBytes([]byte(`{"visible":"value"}`)); errAppend != nil {
			t.Fatalf("AppendBytes: %v", errAppend)
		}
		paths := source.Paths()
		if len(paths) != 1 {
			t.Fatalf("source paths = %v, want one path", paths)
		}
		partPath = paths[0]
		c.Set("API_REQUEST", []byte("memory request"))
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		if _, errWrite := c.Writer.Write([]byte("data: ok\n\n")); errWrite != nil {
			t.Fatalf("write stream: %v", errWrite)
		}
		panic("handler panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"stream":true}`))
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		engine.ServeHTTP(recorder, request)
	}()

	if panicValue != "handler panic" {
		t.Fatalf("panic = %v, want handler panic", panicValue)
	}
	_, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestRequestLoggingMiddlewareCleansSourcesWhenFactoryPanics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &panicSourceFactoryLogger{
		requestLogCapture: &requestLogCapture{enabled: true},
		dir:               t.TempDir(),
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	handlerCalled := false
	engine.POST("/v1/messages", func(c *gin.Context) {
		handlerCalled = true
		if _, exists := c.Get(logging.APIRequestSourceContextKey); exists {
			t.Fatal("partially created source was attached")
		}
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		bytes.NewBufferString(`{"model":"claude"}`),
	)
	engine.ServeHTTP(recorder, request)

	if !handlerCalled {
		t.Fatal("handler was not called after source factory panic")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	logger.mu.Lock()
	sources := append([]*logging.FileBodySource(nil), logger.sources...)
	logger.mu.Unlock()
	if len(sources) != 1 {
		t.Fatalf("created sources = %d, want 1", len(sources))
	}
	if sources[0].HasPayload() {
		t.Fatal("source retained payload after factory panic cleanup")
	}
	paths := sources[0].Paths()
	if len(paths) != 0 {
		t.Fatalf("source paths = %v, want none", paths)
	}
	entries, errRead := os.ReadDir(logger.dir)
	if errRead != nil {
		t.Fatalf("read source base directory: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("source directories remain after cleanup: %v", entries)
	}
}

func TestRequestLoggingMiddlewareRetainsFactoryRollbackCleanupFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initialPending := logging.PendingFileBodySourceCleanupCount()
	baseDir := t.TempDir()
	var source *logging.FileBodySource
	var partPath string
	var nestedPath string
	factoryCalls := 0
	logger := &sourceFactoryFuncLogger{
		requestLogCapture: &requestLogCapture{enabled: true},
		factory: func(prefix string) (*logging.FileBodySource, error) {
			factoryCalls++
			if factoryCalls > 1 {
				return nil, errors.New("source factory failure")
			}
			var errSource error
			source, errSource = logging.NewFileBodySourceInDir(baseDir, prefix)
			if errSource != nil {
				return nil, errSource
			}
			if errAppend := source.AppendBytes([]byte("source payload")); errAppend != nil {
				return nil, errAppend
			}
			paths := source.Paths()
			if len(paths) != 1 {
				return nil, errors.New("source has unexpected part count")
			}
			partPath = paths[0]
			if errRemove := os.Remove(partPath); errRemove != nil {
				return nil, errRemove
			}
			if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
				return nil, errMkdir
			}
			nestedPath = partPath + "/nested"
			if errWrite := os.WriteFile(nestedPath, []byte("keep directory non-empty"), 0o600); errWrite != nil {
				return nil, errWrite
			}
			return source, nil
		},
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		bytes.NewBufferString(`{"model":"claude"}`),
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending+1 {
		t.Fatalf("pending cleanup count = %d, want %d", pending, initialPending+1)
	}
	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove nested file: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove part directory: %v", errRemove)
	}
	if errCleanup := logging.RetryPendingFileBodySourceCleanup(); errCleanup != nil {
		t.Fatalf("RetryPendingFileBodySourceCleanup: %v", errCleanup)
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending {
		t.Fatalf("pending cleanup count after retry = %d, want %d", pending, initialPending)
	}
}

func TestRequestLoggingMiddlewareUsesEntrySnapshot(t *testing.T) {
	tests := []struct {
		name           string
		entryEnabled   bool
		replacement    bool
		wantEntryCalls int
	}{
		{name: "enabled remains enabled for request", entryEnabled: true, replacement: false, wantEntryCalls: 1},
		{name: "disabled remains disabled for request", entryEnabled: false, replacement: true, wantEntryCalls: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			entry := &requestLogCapture{enabled: test.entryEnabled}
			replacement := &requestLogCapture{enabled: test.replacement}
			holder := &requestLoggerSnapshotHolder{
				requestLogCapture: &requestLogCapture{enabled: true},
				snapshot:          entry,
			}
			engine := gin.New()
			engine.Use(RequestLoggingMiddleware(holder))
			engine.POST("/v1/messages", func(c *gin.Context) {
				holder.setSnapshot(replacement)
				c.String(http.StatusOK, "ok")
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
			engine.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			entryCalls, _, _ := entry.snapshot()
			replacementCalls, _, _ := replacement.snapshot()
			if entryCalls != test.wantEntryCalls || replacementCalls != 0 {
				t.Fatalf("entry calls = %d, replacement calls = %d", entryCalls, replacementCalls)
			}
		})
	}
}

func TestResponseWriterImplicitCommitInitializesStreaming(t *testing.T) {
	tests := []struct {
		name   string
		commit func(*responseWriterWrapper) error
	}{
		{
			name: "Write",
			commit: func(wrapper *responseWriterWrapper) error {
				_, errWrite := wrapper.Write([]byte("data: ok\n\n"))
				return errWrite
			},
		},
		{
			name: "WriteString",
			commit: func(wrapper *responseWriterWrapper) error {
				_, errWrite := wrapper.WriteString("data: ok\n\n")
				return errWrite
			},
		},
		{
			name: "WriteHeaderNow",
			commit: func(wrapper *responseWriterWrapper) error {
				wrapper.WriteHeaderNow()
				return nil
			},
		},
		{
			name: "Flush",
			commit: func(wrapper *responseWriterWrapper) error {
				wrapper.Flush()
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			streamWriter := &streamingLogCapture{}
			logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
				URL:       "/v1/responses",
				Method:    http.MethodPost,
				Body:      []byte(`{"stream":true}`),
				Timestamp: time.Now(),
			}, nil)
			c.Writer = wrapper
			c.Header("Content-Type", "text/event-stream")

			if errCommit := test.commit(wrapper); errCommit != nil {
				t.Fatalf("commit response: %v", errCommit)
			}
			if calls := logger.streamCallCount(); calls != 1 {
				t.Fatalf("stream initialization calls = %d, want 1", calls)
			}
			if !wrapper.headerInitialized {
				t.Fatal("response headers were not committed")
			}
			if errFinalize := wrapper.finalize(c); errFinalize != nil {
				t.Fatalf("finalize: %v", errFinalize)
			}
		})
	}
}

func TestResponseWriterDoesNotInitializeStreamingAfterFinalize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &requestLogCapture{enabled: true, streamWriter: &streamingLogCapture{}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/responses",
		Method:    http.MethodPost,
		Body:      []byte(`{"stream":true}`),
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.Header("Content-Type", "text/event-stream")

	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	wrapper.WriteHeaderNow()
	if calls := logger.streamCallCount(); calls != 0 {
		t.Fatalf("stream initialization calls = %d, want 0", calls)
	}
}

func TestResponseWriterUsesLastPendingStatusAtCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &streamingLogCapture{}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/responses",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusAccepted)
	c.Status(http.StatusCreated)
	if _, errWrite := c.Writer.Write([]byte("data: ok\n\n")); errWrite != nil {
		t.Fatalf("write response: %v", errWrite)
	}
	if recorder.Code != http.StatusCreated {
		t.Fatalf("client status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	streamWriter.mu.Lock()
	streamStatus := streamWriter.status
	streamWriter.mu.Unlock()
	if streamStatus != http.StatusCreated {
		t.Fatalf("stream log status = %d, want %d", streamStatus, http.StatusCreated)
	}
	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
}

func TestRequestLoggingMiddlewareDoesNotRecheckEnabledStateDuringFinalize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &panicEnabledRequestLogger{
		sourceRequestLogCapture: &sourceRequestLogCapture{
			requestLogCapture: &requestLogCapture{enabled: true},
			dir:               t.TempDir(),
		},
		panicAfter: 2,
	}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))

	var partPath string
	engine.POST("/v1/messages", func(c *gin.Context) {
		value, exists := c.Get(logging.APIRequestSourceContextKey)
		if !exists {
			t.Fatal("API request source missing")
		}
		source, ok := value.(*logging.FileBodySource)
		if !ok || source == nil {
			t.Fatalf("API request source has unexpected type %T", value)
		}
		if errAppend := source.AppendBytes([]byte("source request")); errAppend != nil {
			t.Fatalf("AppendBytes: %v", errAppend)
		}
		partPath = source.Paths()[0]
		c.Status(http.StatusAccepted)
		panic("handler panic")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		engine.ServeHTTP(recorder, request)
	}()

	if panicValue != "handler panic" {
		t.Fatalf("panic = %v, want handler panic", panicValue)
	}
	logger.mu.Lock()
	enabledOps := logger.enabledOps
	logger.mu.Unlock()
	if enabledOps != 1 {
		t.Fatalf("IsEnabled calls = %d, want 1", enabledOps)
	}
	calls, statuses, _ := logger.requestLogCapture.snapshot()
	if calls != 1 || len(statuses) != 1 || statuses[0] != http.StatusInternalServerError {
		t.Fatalf("log calls = %d, statuses = %v", calls, statuses)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestResponseWriterFinalizeRetainsPersistentlyFailingSourceCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initialPending := logging.PendingFileBodySourceCleanupCount()
	baseDir := t.TempDir()
	source, errSource := logging.NewFileBodySourceInDir(baseDir, "queued-cleanup")
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
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
		t.Fatalf("create non-empty part directory: %v", errMkdir)
	}
	nestedPath := partPath + "/nested"
	if errWrite := os.WriteFile(nestedPath, []byte("keep directory non-empty"), 0o600); errWrite != nil {
		t.Fatalf("write nested file: %v", errWrite)
	}
	if errCleanup := source.Cleanup(); errCleanup == nil {
		t.Fatal("Cleanup returned nil for non-empty part directory")
	}
	queue := &logging.FileBodySourceCleanupQueue{}
	queue.Add(source)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(logging.FileBodySourceCleanupQueueContextKey, queue)
	wrapper := newResponseWriterWrapper(c.Writer, nil, &requestInfo{
		URL:       "/v1/messages",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	if errFinalize := wrapper.finalize(c); errFinalize == nil {
		t.Fatal("finalize returned nil for persistent cleanup failure")
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending+1 {
		t.Fatalf("pending cleanup count = %d, want %d", pending, initialPending+1)
	}
	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove nested file: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove part directory: %v", errRemove)
	}
	if errCleanup := logging.RetryPendingFileBodySourceCleanup(); errCleanup != nil {
		t.Fatalf("retry pending cleanup: %v", errCleanup)
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending {
		t.Fatalf("pending cleanup count = %d, want %d", pending, initialPending)
	}
	entries, errRead := os.ReadDir(baseDir)
	if errRead != nil {
		t.Fatalf("read source base directory: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("source directories remain after finalize: %v", entries)
	}
}

func TestRetryRequestLogCleanupContinuesGlobalCleanupAfterLoggerPanic(t *testing.T) {
	initialPending := logging.PendingFileBodySourceCleanupCount()
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), "panic-cleanup")
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
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
		t.Fatalf("create source part directory: %v", errMkdir)
	}
	nestedPath := filepath.Join(partPath, "nested")
	if errWrite := os.WriteFile(nestedPath, []byte("block cleanup"), 0o600); errWrite != nil {
		t.Fatalf("write cleanup blocker: %v", errWrite)
	}
	if errCleanup := logging.CleanupFileBodySources(source); errCleanup == nil {
		t.Fatal("CleanupFileBodySources returned nil")
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending+1 {
		t.Fatalf("pending cleanup count = %d, want %d", pending, initialPending+1)
	}
	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove cleanup blocker: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part directory: %v", errRemove)
	}
	logger := &cleanupRetryRequestLogCapture{
		requestLogCapture: &requestLogCapture{},
		cleanupPanic:      "logger cleanup panic",
	}
	errCleanup := retryRequestLogCleanup(logger)
	if errCleanup == nil || !strings.Contains(errCleanup.Error(), "logger cleanup panic") {
		t.Fatalf("retryRequestLogCleanup error = %v", errCleanup)
	}
	if pending := logging.PendingFileBodySourceCleanupCount(); pending != initialPending {
		t.Fatalf("pending cleanup count = %d, want %d", pending, initialPending)
	}
}

func TestResponseWriterHeaderSnapshotDropsRemovedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	logger := &requestLogCapture{enabled: true}
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/messages",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.Header("X-Transient", "old")
	wrapper.ensureHeadersCaptured()
	c.Header("X-Transient", "")
	c.Writer.Header().Del("X-Transient")
	if _, errWrite := c.Writer.Write([]byte("ok")); errWrite != nil {
		t.Fatalf("write response: %v", errWrite)
	}
	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	_, headers := logger.responseSnapshot()
	if _, exists := headers["X-Transient"]; exists {
		t.Fatalf("removed header remained in snapshot: %v", headers)
	}
}

func TestResponseWriterPartialWriteCapturesOnlyWrittenBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writeErr := errors.New("partial write")
	partialWriter := &partialResponseWriter{
		ResponseWriter: c.Writer,
		limit:          2,
		err:            writeErr,
	}
	logger := &requestLogCapture{enabled: true}
	wrapper := newResponseWriterWrapper(partialWriter, logger, &requestInfo{
		URL:       "/v1/messages",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper

	n, errWrite := c.Writer.Write([]byte("abcdef"))
	if n != 2 || !errors.Is(errWrite, writeErr) {
		t.Fatalf("Write = (%d, %v), want (2, partial write)", n, errWrite)
	}
	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	response, _ := logger.responseSnapshot()
	if string(response) != "ab" {
		t.Fatalf("logged response = %q, want %q", string(response), "ab")
	}
	if recorder.Body.String() != "ab" {
		t.Fatalf("client response = %q, want %q", recorder.Body.String(), "ab")
	}
}

func TestResponseWriterFinalizeIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	logger := &requestLogCapture{enabled: true}
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/messages",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.String(http.StatusOK, "ok")

	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("first finalize: %v", errFinalize)
	}
	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("second finalize: %v", errFinalize)
	}
	calls, _, _ := logger.snapshot()
	if calls != 1 {
		t.Fatalf("log calls = %d, want 1", calls)
	}
}

func TestResponseWriterFinalizeStreamingConvertsAsyncChunkPanicToError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &panicStreamingLogCapture{
		sourceStreamingLogCapture: &sourceStreamingLogCapture{
			streamingLogCapture: &streamingLogCapture{},
		},
		chunkPanic: "chunk panic",
	}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)

	errFinalize := wrapper.finalize(c)
	if errFinalize == nil || !strings.Contains(errFinalize.Error(), "write streaming request log response chunk panic") {
		t.Fatalf("finalize error = %v, want async chunk panic", errFinalize)
	}
	_, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestResponseWriterFinalizeStreamingConvertsClosePanicToError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &panicStreamingLogCapture{
		sourceStreamingLogCapture: &sourceStreamingLogCapture{
			streamingLogCapture: &streamingLogCapture{},
		},
		closePanic: "close panic",
	}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)

	errFinalize := wrapper.finalize(c)
	if errFinalize == nil || !strings.Contains(errFinalize.Error(), "close streaming request log panic") {
		t.Fatalf("finalize error = %v, want close panic", errFinalize)
	}
	_, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestResponseWriterRepeatedWriteHeaderInitializesStreamingOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &streamingLogCapture{}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"stream":true}`))
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/responses",
		Method:    http.MethodPost,
		Body:      []byte(`{"stream":true}`),
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusOK)
	c.Status(http.StatusCreated)
	if _, errWrite := c.Writer.Write([]byte("data: ok\n\n")); errWrite != nil {
		t.Fatalf("write stream: %v", errWrite)
	}

	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	if calls := logger.streamCallCount(); calls != 1 {
		t.Fatalf("stream initialization calls = %d, want 1", calls)
	}
	_, closeCalls := streamWriter.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestResponseWriterFinalizeStreamingPassesAPIWebsocketTimelineSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &sourceStreamingLogCapture{streamingLogCapture: &streamingLogCapture{}}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)
	source, partPath := newAPIWebsocketTimelineSource(t)
	c.Set("API_WEBSOCKET_TIMELINE", []byte("memory-event"))
	c.Set(logging.APIWebsocketTimelineSourceContextKey, source)

	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	streamWriter.mu.Lock()
	gotSource := streamWriter.apiWebsocketTimelineSource
	streamWriter.mu.Unlock()
	if gotSource != source {
		t.Fatal("API websocket timeline source was not passed directly")
	}
	apiTimeline, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if string(apiTimeline) != "memory-event" {
		t.Fatalf("API websocket timeline = %q, want memory-only payload", string(apiTimeline))
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestResponseWriterFinalizeStreamingMergesAPIWebsocketTimelineSourceFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &streamingLogCapture{}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)
	source, partPath := newAPIWebsocketTimelineSource(t)
	c.Set("API_WEBSOCKET_TIMELINE", []byte("memory-event"))
	c.Set(logging.APIWebsocketTimelineSourceContextKey, source)

	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	apiTimeline, closeCalls := streamWriter.snapshot()
	memoryIndex := bytes.Index(apiTimeline, []byte("memory-event"))
	sourceIndex := bytes.Index(apiTimeline, []byte("source-event"))
	if sourceIndex < 0 || memoryIndex <= sourceIndex {
		t.Fatalf("merged API websocket timeline has wrong order: %q", string(apiTimeline))
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestResponseWriterNonStreamingPreservesMemoryWhenSourceReadFails(t *testing.T) {
	logger := &requestLogCapture{enabled: true}
	wrapper := &responseWriterWrapper{
		logger: logger,
		requestInfo: &requestInfo{
			URL:       "/v1/messages",
			Method:    http.MethodPost,
			Timestamp: time.Now(),
		},
	}
	source, partPath := newFileBodySource(t, "api-request", []byte("source-request"))
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	errLog := wrapper.logRequest(
		nil,
		http.StatusOK,
		nil,
		nil,
		nil,
		nil,
		[]byte("memory-request"),
		source,
		nil,
		nil,
		nil,
		nil,
		nil,
		time.Time{},
		nil,
		false,
	)
	if errLog == nil || !strings.Contains(errLog.Error(), "merge request log API request source") {
		t.Fatalf("logRequest error = %v", errLog)
	}
	logger.mu.Lock()
	calls := logger.calls
	captured := bytes.Clone(logger.apiRequest)
	logger.mu.Unlock()
	if calls != 1 {
		t.Fatalf("log calls = %d, want 1", calls)
	}
	if !bytes.Contains(captured, []byte("[source prefix unavailable: read failed]")) ||
		!bytes.Contains(captured, []byte("memory-request")) {
		t.Fatalf("captured API request = %q", captured)
	}
}

func TestResponseWriterFinalizeStreamingPreservesMemoryWhenSourceReadFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &streamingLogCapture{}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)
	source, partPath := newAPIWebsocketTimelineSource(t)
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	c.Set("API_WEBSOCKET_TIMELINE", []byte("memory-event"))
	c.Set(logging.APIWebsocketTimelineSourceContextKey, source)

	errFinalize := wrapper.finalize(c)
	if errFinalize == nil || !strings.Contains(
		errFinalize.Error(),
		"merge streaming request log API websocket timeline source",
	) {
		t.Fatalf("finalize error = %v", errFinalize)
	}
	apiTimeline, closeCalls := streamWriter.snapshot()
	if !bytes.Contains(apiTimeline, []byte("[source prefix unavailable: read failed]")) ||
		!bytes.Contains(apiTimeline, []byte("memory-event")) {
		t.Fatalf("API websocket timeline = %q", apiTimeline)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestResponseWriterFinalizeStreamingReturnsStatusAndCloseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	statusErr := errors.New("status write failed")
	closeErr := errors.New("close failed")
	streamWriter := &errorStreamingLogCapture{
		streamingLogCapture: &streamingLogCapture{},
		statusErr:           statusErr,
		closeErr:            closeErr,
	}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)

	errFinalize := wrapper.finalize(c)
	if !errors.Is(errFinalize, statusErr) || !errors.Is(errFinalize, closeErr) {
		t.Fatalf("finalize error = %v, want status and close errors", errFinalize)
	}
	if !strings.Contains(errFinalize.Error(), "write streaming request log status") ||
		!strings.Contains(errFinalize.Error(), "close streaming request log") {
		t.Fatalf("finalize error lacks operation context: %v", errFinalize)
	}
	_, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestResponseWriterFinalizeStreamingCleansRejectedSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sourceErr := errors.New("source write failed")
	streamWriter := &errorStreamingLogCapture{
		streamingLogCapture: &streamingLogCapture{},
		apiRequestSourceErr: sourceErr,
	}
	logger := &requestLogCapture{enabled: true, streamWriter: streamWriter}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)
	source, partPath := newFileBodySource(t, "api-request", []byte("source-request"))
	c.Set(logging.APIRequestSourceContextKey, source)

	errFinalize := wrapper.finalize(c)
	if !errors.Is(errFinalize, sourceErr) {
		t.Fatalf("finalize error = %v, want source error", errFinalize)
	}
	if !strings.Contains(errFinalize.Error(), "write streaming request log API request source") {
		t.Fatalf("finalize error lacks source operation context: %v", errFinalize)
	}
	_, closeCalls := streamWriter.streamingLogCapture.snapshot()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	assertFilesRemoved(t, []string{partPath})
}

func TestResponseWriterStreamingInitializationFailureFallsBackToCompleteLog(t *testing.T) {
	var typedNilWriter *streamingLogCapture
	tests := []struct {
		name         string
		streamWriter logging.StreamingLogWriter
		streamErr    error
		streamPanic  any
		wantMessage  string
	}{
		{
			name:      "initialization error",
			streamErr: errors.New("stream initialization failed"),
		},
		{
			name:        "initialization panic",
			streamPanic: "stream initialization panic",
			wantMessage: "initialize streaming request log panic",
		},
		{
			name:        "nil writer",
			wantMessage: "logger returned nil writer",
		},
		{
			name:         "typed nil writer",
			streamWriter: typedNilWriter,
			wantMessage:  "logger returned nil writer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			logger := &requestLogCapture{
				enabled:      true,
				streamWriter: test.streamWriter,
				streamErr:    test.streamErr,
				streamPanic:  test.streamPanic,
			}
			c, wrapper := newStreamingResponseWriterTestContext(t, logger)

			errFinalize := wrapper.finalize(c)
			if errFinalize == nil {
				t.Fatal("finalize returned nil error")
			}
			if test.streamErr != nil && !errors.Is(errFinalize, test.streamErr) {
				t.Fatalf("finalize error = %v, want initialization error", errFinalize)
			}
			if test.wantMessage != "" && !strings.Contains(errFinalize.Error(), test.wantMessage) {
				t.Fatalf("finalize error = %v, want %q", errFinalize, test.wantMessage)
			}
			calls, statuses, _ := logger.snapshot()
			if calls != 1 || len(statuses) != 1 || statuses[0] != http.StatusOK {
				t.Fatalf("fallback log calls = %d, statuses = %v", calls, statuses)
			}
		})
	}
}

func TestResponseWriterLiteralTruncationMarkerDoesNotStopCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &requestLogCapture{enabled: true}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/messages",
		Method:    http.MethodPost,
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	if _, errWrite := c.Writer.Write(truncatedResponseBody); errWrite != nil {
		t.Fatalf("write literal marker: %v", errWrite)
	}
	if _, errWrite := c.Writer.Write([]byte("-tail")); errWrite != nil {
		t.Fatalf("write tail: %v", errWrite)
	}
	if errFinalize := wrapper.finalize(c); errFinalize != nil {
		t.Fatalf("finalize: %v", errFinalize)
	}
	response, _ := logger.responseSnapshot()
	want := append(bytes.Clone(truncatedResponseBody), []byte("-tail")...)
	if !bytes.Equal(response, want) {
		t.Fatalf("captured response = %q, want %q", response, want)
	}
}

func TestResponseWriterStreamingInitializationFailureBoundsFallbackBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamErr := errors.New("stream initialization failed")
	logger := &requestLogCapture{enabled: true, streamErr: streamErr}
	c, wrapper := newStreamingResponseWriterTestContext(t, logger)
	if _, errWrite := c.Writer.Write(bytes.Repeat([]byte("x"), maxBufferedResponseBodyBytes)); errWrite != nil {
		t.Fatalf("write oversized stream: %v", errWrite)
	}

	errFinalize := wrapper.finalize(c)
	if !errors.Is(errFinalize, streamErr) {
		t.Fatalf("finalize error = %v, want stream initialization error", errFinalize)
	}
	response, _ := logger.responseSnapshot()
	if !bytes.Equal(response, truncatedResponseBody) {
		t.Fatalf("fallback response = %q, want truncation marker", response)
	}
}

func newStreamingResponseWriterTestContext(
	t *testing.T,
	logger logging.RequestLogger,
) (*gin.Context, *responseWriterWrapper) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"stream":true}`))
	wrapper := newResponseWriterWrapper(c.Writer, logger, &requestInfo{
		URL:       "/v1/responses",
		Method:    http.MethodPost,
		Body:      []byte(`{"stream":true}`),
		Timestamp: time.Now(),
	}, nil)
	c.Writer = wrapper
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusOK)
	if _, errWrite := c.Writer.Write([]byte("data: ok\n\n")); errWrite != nil {
		t.Fatalf("write stream: %v", errWrite)
	}
	return c, wrapper
}

func newAPIWebsocketTimelineSource(t *testing.T) (*logging.FileBodySource, string) {
	t.Helper()
	return newFileBodySource(t, "api-websocket-timeline", []byte("source-event"))
}

func newFileBodySource(t *testing.T, prefix string, payload []byte) (*logging.FileBodySource, string) {
	t.Helper()
	source, errSource := logging.NewFileBodySourceInDir(t.TempDir(), prefix)
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes(payload); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	return source, paths[0]
}

func assertFilesRemoved(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("path %q still exists, stat error = %v", path, errStat)
		}
	}
}
