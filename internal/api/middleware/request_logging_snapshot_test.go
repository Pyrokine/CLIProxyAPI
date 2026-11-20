package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/gin-gonic/gin"
)

func TestRequestHeaderSnapshotSurvivesDownstreamMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/responses", func(c *gin.Context) {
			c.Request.Header["X-Audit"][0] = "downstream-value"
			c.JSON(http.StatusOK, gin.H{"ok": true})
		},
	)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"synthetic"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Audit", "entry-value")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("response status = %d", recorder.Code)
	}
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("request logs = %d, want 1", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "entry-value") || strings.Contains(string(body), "downstream-value") {
		t.Fatalf("request log lost the entry header snapshot: %s", body)
	}
}

func TestCaptureRequestInfo_HeadersDeepCopy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Audit", "original-value")
	c.Request = req

	info, err := captureRequestInfo(c, false)
	if err != nil {
		t.Fatalf("captureRequestInfo failed: %v", err)
	}

	// Mutate the original request header slice in place
	c.Request.Header["X-Audit"][0] = "mutated-value"

	if got := info.Headers["X-Audit"][0]; got != "original-value" {
		t.Fatalf("header slice was aliased: got %q, want %q", got, "original-value")
	}
}

func TestRequestLoggingMiddleware_StreamingResponsesUpstreamSections(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	cfg := &config.Config{RequestLog: true}

	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST(
		"/v1/responses", func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			//lint:ignore SA1029 验证已发布的 gin 字符串键兼容路径
			executorCtx := context.WithValue(context.Background(), "gin", c)
			executor.RecordAPIRequest(
				executorCtx, cfg,
				"https://api.example.com/v1/responses", http.MethodPost,
				http.Header{"Content-Type": []string{"application/json"}},
				[]byte(`{"model":"gpt-5-codex","input":[]}`), "", nil,
			)
			executor.AppendAPIResponseChunk(
				executorCtx, cfg, []byte("data: {\"type\":\"response.output_item.added\"}\n\n"),
			)
			_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.output_item.added\"}\n\n"))
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		},
	)

	request := httptest.NewRequest(
		http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":[],"stream":true}`),
	)
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
	var logPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "v1-responses-") && strings.HasSuffix(entry.Name(), ".log") {
			logPath = logsDir + string(os.PathSeparator) + entry.Name()
			break
		}
	}
	if logPath == "" {
		t.Fatal("streaming request log was not created")
	}
	content, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read log file: %v", errReadLog)
	}
	if !bytes.Contains(content, []byte("=== API REQUEST 1 ===")) {
		t.Fatalf("streaming log missing API REQUEST: %s", string(content))
	}
	if !bytes.Contains(content, []byte("=== API RESPONSE 1 ===")) {
		t.Fatalf("streaming log missing API RESPONSE: %s", string(content))
	}
}
