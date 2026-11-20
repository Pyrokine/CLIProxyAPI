package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
)

func TestRequestLoggingMiddlewareEscapesInjectedMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name           string
		enabled        bool
		runtimeEnabled bool
		streaming      bool
		status         int
		wantLog        bool
		wantErrorFile  bool
	}{
		{name: "full logging", enabled: true, runtimeEnabled: true, status: http.StatusOK, wantLog: true},
		{
			name: "streaming logging", enabled: true, runtimeEnabled: true, streaming: true, status: http.StatusOK,
			wantLog: true,
		},
		{
			name: "request-log disabled error", runtimeEnabled: true, status: http.StatusBadGateway, wantLog: true,
			wantErrorFile: true,
		},
		{name: "request-log disabled success", runtimeEnabled: true, status: http.StatusOK},
		{name: "commercial runtime disabled", enabled: true, status: http.StatusBadGateway},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				logsDir := t.TempDir()
				logger := logging.NewFileRequestLogger(test.enabled, logsDir, "", 10)
				engine := gin.New()
				engine.Use(logging.GinLogrusLogger())
				engine.Use(
					RequestLoggingMiddleware(
						logger, RequestLoggingOptions{RuntimeEnabled: func() bool { return test.runtimeEnabled }},
					),
				)
				const body = `{"input":"hello","api_key":"body-secret-value"}`
				var handledBody string
				engine.NoRoute(
					func(c *gin.Context) {
						payload, err := io.ReadAll(c.Request.Body)
						if err != nil {
							t.Fatal(err)
						}
						handledBody = string(payload)
						c.Writer.Header()["X-Upstream"] = []string{"reply\r\n=== REQUEST INFO ===\n\x1b\"\\"}
						c.Header("Set-Cookie", "cookie-secret-value")
						if test.streaming {
							c.Header("Content-Type", "text/event-stream")
							c.Status(test.status)
							_, _ = c.Writer.Write([]byte("data: hello\n\n"))
							c.Writer.Flush()
							return
						}
						c.JSON(test.status, gin.H{"result": "hello"})
					},
				)
				path := "/v1/log-injection" + url.PathEscape("\r\n=== RESPONSE ===\nStatus: 200\x1b\"\\")
				request := httptest.NewRequest(
					http.MethodPost, path+"?api_key=query-secret-value&code=oauth-secret-value&model="+strings.Repeat(
						"model-", 70,
					)+"tail", strings.NewReader(body),
				)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer header-secret-value")
				request.Header.Set("X-Trace", "trace\r\n=== RESPONSE ===\nStatus: 200\x1b\"\\")
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, request)
				if recorder.Code != test.status || handledBody != body {
					t.Fatalf("handler status/body = %d/%q, want %d/%q", recorder.Code, handledBody, test.status, body)
				}
				entries, err := os.ReadDir(logsDir)
				if err != nil {
					t.Fatal(err)
				}
				if !test.wantLog {
					if len(entries) != 0 {
						t.Fatalf("disabled logging created %d artifacts", len(entries))
					}
					return
				}
				if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".log") {
					t.Fatalf("log artifacts = %v, want one completed log", entries)
				}
				if strings.HasPrefix(entries[0].Name(), "error-") != test.wantErrorFile {
					t.Fatalf("unexpected log filename %q", entries[0].Name())
				}
				data, err := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
				if err != nil {
					t.Fatal(err)
				}
				text := string(data)
				if strings.ContainsAny(text, "\r\x1b") || strings.Count(
					text, "=== REQUEST INFO ===\n",
				) != 1 || strings.Count(text, "=== RESPONSE ===\n") != 1 {
					t.Fatalf("metadata injected log structure: %q", text)
				}
				maskedURL := request.URL.Path + "?" + util.MaskSensitiveQuery(request.URL.RawQuery)
				quotedURL := strconv.Quote(maskedURL)
				//goland:noinspection SpellCheckingInspection
				for _, want := range []string{
					"URL: " + quotedURL[1:len(quotedURL)-1] + "\n",
					"Method: POST\n",
					"Authorization: Bearer head...alue\n",
					`X-Trace: trace\r\n=== RESPONSE ===\nStatus: 200\x1b\"\\`,
					`X-Upstream: reply\r\n=== REQUEST INFO ===\n\x1b\"\\`,
					"Set-Cookie: cook...alue\n",
					`"input": "hello"`,
				} {
					if !strings.Contains(text, want) {
						t.Errorf("log missing %q: %q", want, text)
					}
				}
				for _, secret := range []string{
					"query-secret-value", "oauth-secret-value", "header-secret-value", "body-secret-value",
					"cookie-secret-value",
				} {
					if strings.Contains(text, secret) {
						t.Errorf("log leaked %q", secret)
					}
				}
			},
		)
	}
}
