package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func TestRequestHeaderSnapshotSurvivesDownstreamMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Request.Header["X-Audit"][0] = "downstream-value"
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
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
