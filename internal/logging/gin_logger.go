// Package logging provides Gin middleware for HTTP request logging and panic recovery.
// It integrates Gin web framework with logrus for structured logging of HTTP requests,
// responses, and error handling with panic recovery capabilities.
package logging

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// aiAPIPrefixes defines path prefixes for AI API requests that should have request ID tracking.
var aiAPIPrefixes = []string{
	"/v1",
	"/v1beta",
	"/openai/v1",
	"/backend-api/codex",
	"/api/provider",
}

// GinLogrusLogger returns a Gin middleware handler that logs HTTP requests and responses
// using logrus. It captures request details including method, path, status code, latency,
// client IP, and any error messages. Request IDs are added for AI and management API requests.
//
// Output format (tracked request): [2025-12-23 20:14:10] [info ] | a1b2c3d4 | 200 |       23.559s | ...
// Output format (others): [2025-12-23 20:14:10] [info ] | -------- | 200 |       23.559s | ...
//
// Returns:
//   - gin.HandlerFunc: A middleware handler for request logging
func GinLogrusLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := util.MaskSensitiveQuery(c.Request.URL.RawQuery)

		var requestID string
		if requiresRequestID(path) {
			requestID = EnsureGinRequestID(c)
			ctx := WithRequestOutcomeState(c.Request.Context(), NewRequestOutcomeState())
			c.Request = c.Request.WithContext(ctx)
		}

		c.Next()

		if raw != "" {
			path = path + "?" + raw
		}

		latency := time.Since(start)
		if latency > time.Minute {
			latency = latency.Truncate(time.Second)
		} else {
			latency = latency.Truncate(time.Millisecond)
		}

		statusCode := c.Writer.Status()
		clientIP := ExtractClientRequestMetadata(c.Request).ClientIP
		method := c.Request.Method
		errorMessage := c.Errors.ByType(gin.ErrorTypePrivate).String()
		if shouldSuppressAccessLog(method, c.Request.URL.Path, statusCode) {
			return
		}

		if requestID == "" {
			requestID = "--------"
		}
		logLine := fmt.Sprintf("%3d | %13v | %15s | %-7s \"%s\"", statusCode, latency, clientIP, method, path)
		if errorMessage != "" {
			logLine = logLine + " | " + errorMessage
		}

		entry := log.WithField("request_id", requestID)

		switch {
		case statusCode >= http.StatusInternalServerError:
			entry.Error(logLine)
		case statusCode >= http.StatusBadRequest:
			entry.Warn(logLine)
		default:
			entry.Info(logLine)
		}
	}
}

func shouldSuppressAccessLog(method, path string, statusCode int) bool {
	return path == "/healthz" &&
		(method == http.MethodGet || method == http.MethodHead) &&
		statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices
}

func requiresRequestID(path string) bool {
	return isAIAPIPath(path) || path == "/v0/management" || strings.HasPrefix(path, "/v0/management/")
}

// isAIAPIPath checks if the given path is an AI API endpoint that should have request ID tracking.
func isAIAPIPath(path string) bool {
	if path == "/responses" || strings.HasPrefix(path, "/responses/") {
		return true
	}
	for _, prefix := range aiAPIPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// GinLogrusRecovery returns a Gin middleware handler that recovers from panics and logs
// them using logrus. When a panic occurs, it captures the panic value, stack trace,
// and request path, then returns a 500 Internal Server Error response to the client.
//
// Returns:
//   - gin.HandlerFunc: A middleware handler for panic recovery
func GinLogrusRecovery() gin.HandlerFunc {
	return gin.CustomRecovery(
		func(c *gin.Context, recovered any) {
			// gin v1.12.0 handles http.ErrAbortHandler internally as broken pipe,
			// so only non-ErrAbortHandler panics reach this callback.

			log.WithFields(
				log.Fields{
					"panic": recovered,
					"stack": string(debug.Stack()),
					"path":  c.Request.URL.Path,
				},
			).Error("recovered from panic")

			if c.Request != nil {
				MarkRequestOutcomeFailure(
					c.Request.Context(),
					RequestOutcomePanic,
					"handler",
					fmt.Sprint(recovered),
				)
			}
			c.AbortWithStatus(http.StatusInternalServerError)
		},
	)
}
