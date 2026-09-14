package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkhandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestShouldSkipMethodForRequestLogging(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		skip bool
	}{
		{
			name: "nil request",
			req:  nil,
			skip: true,
		},
		{
			name: "post request should not skip",
			req: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Path: "/v1/responses"},
			},
			skip: false,
		},
		{
			name: "plain get should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/models"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "responses get without upgrade should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "live websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/live/call-1"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "realtime calls websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/realtime/calls/call-1"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "realtime query websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/realtime", RawQuery: "call_id=call-1"},
				Header: http.Header{"Upgrade": []string{"WebSocket"}},
			},
			skip: false,
		},
		{
			name: "live route without call id should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/live"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: true,
		},
		{
			name: "live route with extra segment should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/live/call-1/extra"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: true,
		},
		{
			name: "similar live prefix should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/lively/call-1"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: true,
		},
	}

	for i := range tests {
		got := shouldSkipMethodForRequestLogging(tests[i].req)
		if got != tests[i].skip {
			t.Fatalf("%s: got skip=%t, want %t", tests[i].name, got, tests[i].skip)
		}
	}
}

func TestShouldCaptureRequestBody(t *testing.T) {
	tests := []struct {
		name          string
		loggerEnabled bool
		req           *http.Request
		want          bool
	}{
		{
			name:          "logger enabled always captures",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "nil request",
			loggerEnabled: false,
			req:           nil,
			want:          false,
		},
		{
			name:          "small known size json in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: 2,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "large known size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: maxErrorOnlyCapturedRequestBodyBytes + 1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "unknown size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "multipart skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: 1,
				Header:        http.Header{"Content-Type": []string{"multipart/form-data; boundary=abc"}},
			},
			want: false,
		},
	}

	for i := range tests {
		got := shouldCaptureRequestBody(tests[i].loggerEnabled, tests[i].req)
		if got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestDeferredRequestBodyCaptureFinishNilReceiver(t *testing.T) {
	var capture *deferredRequestBodyCapture
	if errFinish := capture.finish(); errFinish != nil {
		t.Fatalf("nil capture Finish() error = %v, want nil", errFinish)
	}
}

func TestDeferredRequestBodyCaptureDoesNotDrainUnreadBody(t *testing.T) {
	logger := logging.NewFileRequestLogger(false, t.TempDir(), "", 10)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("remaining-body"))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	requestInfo := &requestInfo{Headers: map[string][]string{"Content-Type": {"application/json"}}}
	capture := attachDeferredRequestBodyCapture(request, logger, requestInfo, false, false)
	if capture == nil {
		t.Fatal("deferred request body capture was not attached")
	}
	defer func() {
		if errCleanup := capture.cleanup(); errCleanup != nil {
			t.Fatalf("cleanup deferred request body capture: %v", errCleanup)
		}
	}()

	firstByte := make([]byte, 1)
	if _, errRead := request.Body.Read(firstByte); errRead != nil {
		t.Fatalf("read first request byte: %v", errRead)
	}
	captured, marker, errCaptured := capture.bytes()
	if errCaptured != nil {
		t.Fatalf("read captured body: %v", errCaptured)
	}
	if string(captured) != "r" {
		t.Fatalf("captured body = %q, want %q", string(captured), "r")
	}
	if !strings.Contains(marker, "REQUEST BODY CAPTURE INCOMPLETE") {
		t.Fatalf("capture marker = %q, want incomplete marker", marker)
	}
	remaining, errRemaining := io.ReadAll(capture.body)
	if errRemaining != nil {
		t.Fatalf("read remaining body: %v", errRemaining)
	}
	if string(remaining) != "emaining-body" {
		t.Fatalf("remaining body = %q, want %q", string(remaining), "emaining-body")
	}
}

func TestRequestLoggingMiddlewareCapturesLargeErrorRequestAndDeferredAPIRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := append(
		[]byte(`{"marker":"large-error-body","api_key":"sk-deferred-secret","padding":"`),
		bytes.Repeat([]byte("x"), int(maxErrorOnlyCapturedRequestBodyBytes))...,
	)
	payload = append(payload, []byte(`"}`)...)
	upstreamBody := []byte(`{"model":"upstream-model","input":"translated"}`)

	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST(
		"/v1/responses", func(c *gin.Context) {
			body, errRead := io.ReadAll(c.Request.Body)
			if errRead != nil || !bytes.Equal(body, payload) {
				c.Status(http.StatusInternalServerError)
				return
			}
			executor.RecordAPIRequest(
				util.WithGinContext(context.Background(), c),
				&config.Config{},
				"https://api.example.com/v1/responses",
				http.MethodPost,
				http.Header{"Content-Type": []string{"application/json"}},
				upstreamBody,
				"openai",
				nil,
			)
			c.JSON(http.StatusBadRequest, gin.H{"error": "upstream rejected request"})
		},
	)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	var logPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			logPath = filepath.Join(logsDir, entry.Name())
			break
		}
	}
	if logPath == "" {
		t.Fatal("forced error log was not created")
	}
	content, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read error log: %v", errReadLog)
	}
	for _, expected := range [][]byte{
		[]byte("large-error-body"),
		[]byte("=== API REQUEST 1 ==="),
		[]byte("upstream-model"),
		[]byte("translated"),
	} {
		if !bytes.Contains(content, expected) {
			t.Fatalf("error log does not contain %q", expected)
		}
	}
	if bytes.Contains(content, []byte("sk-deferred-secret")) {
		t.Fatal("error log leaked deferred request secret")
	}
}

func TestRequestLoggingMiddlewareCleansDeferredCaptureAfterSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := bytes.Repeat([]byte("x"), int(maxErrorOnlyCapturedRequestBodyBytes)+1)
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST(
		"/v1/responses", func(c *gin.Context) {
			if _, errRead := io.Copy(io.Discard, c.Request.Body); errRead != nil {
				c.Status(http.StatusInternalServerError)
				return
			}
			c.Status(http.StatusOK)
		},
	)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusOK)
	}
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("successful request left log artifacts: %v", entries)
	}
}

func TestDeferredRequestBodyCaptureDecodesZstdAfterSpooling(t *testing.T) {
	payload := []byte(`{"model":"test-model","stream":true}`)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}

	logger := logging.NewFileRequestLogger(false, t.TempDir(), "", 10)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed.Bytes()))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	info := &requestInfo{Headers: map[string][]string{"Content-Encoding": {"zstd"}}}
	capture := attachDeferredRequestBodyCapture(request, logger, info, false, false)
	if capture == nil {
		t.Fatal("deferred request body capture was not attached")
	}
	defer func() {
		if errCleanup := capture.cleanup(); errCleanup != nil {
			t.Fatalf("cleanup deferred request body capture: %v", errCleanup)
		}
	}()
	if _, errRead := io.Copy(io.Discard, request.Body); errRead != nil {
		t.Fatalf("consume compressed request: %v", errRead)
	}

	wrapper := &responseWriterWrapper{requestInfo: info}
	if got := wrapper.extractRequestBody(nil); !bytes.Equal(got, payload) {
		t.Fatalf("decoded deferred body = %q, want %q", got, payload)
	}
}

func TestDecodeCapturedRequestBodyForLogWithLimitTruncatesZstdExpansion(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}

	decoded := decodeCapturedRequestBodyForLogWithLimit(compressed.Bytes(), "zstd", 64)
	if len(decoded) > 128 {
		t.Fatalf("limited decoded body length = %d, want bounded output", len(decoded))
	}
	if !bytes.Contains(decoded, []byte("DECOMPRESSED REQUEST BODY TRUNCATED")) {
		t.Fatalf("decoded body = %q, want truncation marker", string(decoded))
	}
}

func TestDecodeCapturedRequestBodyForLogWithLimitReportsDecodeFailure(t *testing.T) {
	decoded := decodeCapturedRequestBodyForLogWithLimit([]byte("not-zstd"), "zstd", 64)
	if !bytes.Contains(decoded, []byte("request body omitted")) {
		t.Fatalf("decode failure output = %q, want omission marker", decoded)
	}
}

func TestCaptureRequestInfoDecodesZstdRequestBodyForLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model","stream":true}`)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter error = %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write error = %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close error = %v", errClose)
	}
	compressedBytes := compressed.Bytes()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBytes))
	req.Header.Set("Content-Encoding", "zstd")
	c.Request = req

	info, errCapture := captureRequestInfo(c, true)
	if errCapture != nil {
		t.Fatalf("captureRequestInfo error = %v", errCapture)
	}
	if !bytes.Equal(info.Body, payload) {
		t.Fatalf("logged request body = %q, want %q", string(info.Body), string(payload))
	}

	restoredBody, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatalf("read restored request body error = %v", errRead)
	}
	if !bytes.Equal(restoredBody, compressedBytes) {
		t.Fatal("request body was not restored with original compressed bytes")
	}
}

func TestCaptureRequestInfo_BodyLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var capturedBodyLen int
	var restoredBodyLen int
	engine.POST(
		"/test", func(c *gin.Context) {
			info, err := captureRequestInfo(c, true)
			if err != nil {
				t.Errorf("captureRequestInfo error: %v", err)
				return
			}
			capturedBodyLen = len(info.Body)
			restored, errRead := io.ReadAll(c.Request.Body)
			if errRead != nil {
				t.Errorf("read restored request body: %v", errRead)
				return
			}
			restoredBodyLen = len(restored)
			c.Status(http.StatusOK)
		},
	)

	bigBody := bytes.Repeat([]byte{'A'}, 11<<20)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if capturedBodyLen != int(maxCapturedRequestBodyBytes) {
		t.Fatalf("captured body length = %d, want %d", capturedBodyLen, maxCapturedRequestBodyBytes)
	}
	if restoredBodyLen != len(bigBody) {
		t.Fatalf("restored body length = %d, want %d", restoredBodyLen, len(bigBody))
	}
}

func TestCaptureRequestInfoOmitsOversizedDecodedZstdBody(t *testing.T) {
	payload := bytes.Repeat([]byte{'A'}, int(maxDecodedRequestLogBodyBytes)+1)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter error = %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write error = %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close error = %v", errClose)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	compressedBytes := compressed.Bytes()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBytes))
	req.Header.Set("Content-Encoding", "zstd")
	c.Request = req

	info, errCapture := captureRequestInfo(c, true)
	if errCapture != nil {
		t.Fatalf("captureRequestInfo error = %v", errCapture)
	}
	if !strings.Contains(string(info.Body), errDecodedRequestLogBodyTooLarge.Error()) {
		t.Fatalf("logged request body = %q, want omission marker", info.Body)
	}
	restored, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatalf("read restored request body: %v", errRead)
	}
	if !bytes.Equal(restored, compressedBytes) {
		t.Fatal("compressed request body was not restored")
	}
}

func TestRequestLoggingMiddlewareLogsHTTP200FailedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	baseHandler := sdkhandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	router := gin.New()
	router.Use(logging.GinLogrusLogger())
	router.Use(RequestLoggingMiddleware(logger))
	router.POST(
		"/v1/responses", func(c *gin.Context) {
			executionContext, cancel := baseHandler.GetContextWithCancel(nil, c, context.Background())
			defer cancel()
			if errPublish := coreusage.PublishRecord(
				executionContext, coreusage.Record{
					Model:  "test-model",
					Failed: true,
					Fail: coreusage.Failure{
						StatusCode: http.StatusBadGateway,
						Body:       "upstream failed after response start",
					},
				},
			); errPublish != nil {
				c.Status(http.StatusInternalServerError)
				return
			}
			c.JSON(http.StatusOK, gin.H{"partial": true})
		},
	)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusOK)
	}

	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	var matched string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			matched = entry.Name()
			break
		}
	}
	if matched == "" {
		t.Fatal("HTTP 200 semantic failure did not create an error request log")
	}
}

func TestRequestLoggingMiddlewareLogsCanceledAndDeadlineRequests(t *testing.T) {
	tests := []struct {
		name        string
		requestCtx  func() (context.Context, context.CancelFunc)
		wantOutcome logging.RequestOutcomeKind
	}{
		{
			name: "canceled",
			requestCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantOutcome: logging.RequestOutcomeCanceled,
		},
		{
			name: "deadline",
			requestCtx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantOutcome: logging.RequestOutcomeDeadline,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				logsDir := t.TempDir()
				logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
				var outcomeState *logging.RequestOutcomeState
				router := gin.New()
				router.Use(logging.GinLogrusLogger())
				router.Use(RequestLoggingMiddleware(logger))
				router.POST(
					"/v1/responses", func(c *gin.Context) {
						outcomeState = logging.RequestOutcomeStateFromContext(c.Request.Context())
						c.JSON(http.StatusOK, gin.H{"partial": true})
					},
				)

				requestContext, cancel := test.requestCtx()
				defer cancel()
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test"}`))
				request = request.WithContext(requestContext)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				if outcomeState == nil {
					t.Fatal("request outcome state was not attached")
				}
				outcome := outcomeState.Snapshot()
				if !outcome.Failed || outcome.Kind != test.wantOutcome {
					t.Fatalf("request outcome = %+v, want %q", outcome, test.wantOutcome)
				}
				assertErrorRequestLogExists(t, logsDir)
			},
		)
	}
}

func TestRequestLoggingMiddlewareLogsPanics(t *testing.T) {
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	var outcomeState *logging.RequestOutcomeState
	router := gin.New()
	router.Use(logging.GinLogrusLogger())
	router.Use(logging.GinLogrusRecovery())
	router.Use(RequestLoggingMiddleware(logger))
	router.POST(
		"/v1/responses", func(c *gin.Context) {
			outcomeState = logging.RequestOutcomeStateFromContext(c.Request.Context())
			panic("request panic")
		},
	)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if outcomeState == nil {
		t.Fatal("request outcome state was not attached")
	}
	outcome := outcomeState.Snapshot()
	if !outcome.Failed || outcome.Kind != logging.RequestOutcomePanic {
		t.Fatalf("request outcome = %+v, want panic", outcome)
	}
	assertErrorRequestLogExists(t, logsDir)
}

func assertErrorRequestLogExists(t *testing.T, logsDir string) {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatalf("read logs dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			return
		}
	}
	t.Fatal("error request log was not created")
}
