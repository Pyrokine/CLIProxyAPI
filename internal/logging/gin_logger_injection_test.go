package logging

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestGinLogrusLoggerEscapesUntrustedFields(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput, previousFormatter, previousLevel := logger.Out, logger.Formatter, logger.GetLevel()
	t.Cleanup(
		func() {
			logger.SetOutput(previousOutput)
			logger.SetFormatter(previousFormatter)
			logger.SetLevel(previousLevel)
		},
	)
	logger.SetFormatter(&LogFormatter{})
	logger.SetLevel(log.InfoLevel)
	gin.SetMode(gin.TestMode)

	//goland:noinspection SpellCheckingInspection
	for _, test := range []struct {
		name   string
		target string
		method string
		err    string
	}{
		{name: "encoded LF", target: "/v1/missing%0Aforged-access-line"},
		{name: "encoded CR", target: "/v1/missing%0Dforged-access-line"},
		{name: "encoded ESC", target: "/v1/missing%1B[2J"},
		{name: "quotes and backslashes", target: "/v1/missing%22%5C"},
		{name: "long URL", target: "/v1/" + strings.Repeat("long-path-", 50) + "tail"},
		{name: "normal URL", target: "/v1/models?model=demo&limit=10"},
		{name: "sensitive query", target: "/v1/models?api_key=query-secret-value&code=oauth-secret-value&model=demo"},
		{name: "private error", target: "/v1/models", err: "upstream\r\n=== RESPONSE ===\nStatus: 200\x1b[2J\"\\"},
		{name: "method", target: "/v1/models", method: "GET\r\nforged\x1b\"\\"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				var output bytes.Buffer
				logger.SetOutput(&output)
				engine := gin.New()
				engine.Use(GinLogrusLogger())
				var privateErrors string
				engine.NoRoute(
					func(c *gin.Context) {
						if test.err != "" {
							_ = c.Error(errors.New(test.err))
							privateErrors = c.Errors.ByType(gin.ErrorTypePrivate).String()
						}
						c.Status(http.StatusUnauthorized)
					},
				)
				request := httptest.NewRequest(http.MethodGet, test.target, nil)
				if test.method != "" {
					request.Method = test.method
				}
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", recorder.Code)
				}
				line := output.String()
				if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") {
					t.Fatalf("access log contains raw controls: %q", line)
				}
				path := request.URL.Path
				if query := util.MaskSensitiveQuery(request.URL.RawQuery); query != "" {
					path += "?" + query
				}
				if !strings.Contains(line, strconv.Quote(path)) {
					t.Fatalf("access log lost or failed to escape path %q: %q", path, line)
				}
				if privateErrors != "" {
					quoted := strconv.Quote(privateErrors)
					if !strings.Contains(line, " | "+quoted[1:len(quoted)-1]) {
						t.Fatalf("access log lost or failed to escape private error: %q", line)
					}
				}
				for _, secret := range []string{"query-secret-value", "oauth-secret-value"} {
					if strings.Contains(line, secret) {
						t.Fatalf("access log leaked %q: %q", secret, line)
					}
				}
			},
		)
	}
}
