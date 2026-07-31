// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
)

const (
	maxErrorOnlyCapturedRequestBodyBytes int64 = 1 << 20  // 1 MiB
	maxCapturedRequestBodyBytes          int64 = 10 << 20 // 10 MiB
	maxDecodedRequestLogBodyBytes        int64 = 10 << 20 // 10 MiB
)

var errDecodedRequestLogBodyTooLarge = errors.New("decoded request log body exceeds 10 MiB")

type replayRequestBody struct {
	io.Reader
	io.Closer
}

type fileBodySourceFactory interface {
	NewFileBodySource(prefix string) (*logging.FileBodySource, error)
}

type requestLoggerSnapshotter interface {
	Snapshot() logging.RequestLogger
}

type requestLogCleanupRetrier interface {
	RetryPendingCleanup() error
}

type RequestLoggingOptions struct {
	RuntimeEnabled func() bool
}

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When full request logging is disabled,
// body capture is limited to small known-size payloads to avoid large per-request memory spikes.
func RequestLoggingMiddleware(logger logging.RequestLogger, opts ...RequestLoggingOptions) gin.HandlerFunc {
	var runtimeEnabled func() bool
	if len(opts) > 0 {
		runtimeEnabled = opts[0].RuntimeEnabled
	}
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}
		if errCleanup := retryRequestLogCleanup(logger); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to retry request log cleanup")
		}
		if runtimeEnabled != nil && !runtimeEnabled() {
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		requestLogger, errSnapshot := snapshotRequestLogger(logger)
		if errSnapshot != nil {
			log.WithError(errSnapshot).Warn("failed to snapshot request logger")
			c.Next()
			return
		}
		if requestLogger == nil {
			c.Next()
			return
		}
		loggerEnabled, errEnabled := requestLoggerEnabled(requestLogger)
		if errEnabled != nil {
			log.WithError(errEnabled).Warn("failed to read request logger enabled state")
			c.Next()
			return
		}

		// Capture request information
		requestInfo, err := captureRequestInfo(c, shouldCaptureRequestBody(loggerEnabled, c.Request))
		if err != nil {
			c.Next()
			return
		}

		initRequestTimeline(c, requestInfo.Timestamp)
		if errSources := attachRequestLogSources(c, requestLogger, loggerEnabled); errSources != nil {
			log.WithError(errSources).Warn("failed to attach request log sources")
		}

		// Create response writer wrapper
		wrapper := newResponseWriterWrapper(c.Writer, requestLogger, requestInfo, requestTimeline(c), loggerEnabled)
		if !loggerEnabled {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper

		defer func() {
			panicValue := recover()
			if panicValue != nil {
				wrapper.handlerPanicked = true
				if !c.Writer.Written() {
					c.Writer.WriteHeader(http.StatusInternalServerError)
					c.Writer.WriteHeaderNow()
				}
			}
			if errFinalize := wrapper.finalize(c); errFinalize != nil {
				log.WithError(errFinalize).Warn("failed to write request log")
			}
			if panicValue != nil {
				panic(panicValue)
			}
		}()

		c.Next()
	}
}

func retryRequestLogCleanup(logger logging.RequestLogger) error {
	if logger == nil {
		return nil
	}
	var loggerCleanupErr error
	if retrier, ok := logger.(requestLogCleanupRetrier); ok && retrier != nil {
		loggerCleanupErr = invokeRequestLogOperation(
			"retry request logger cleanup",
			retrier.RetryPendingCleanup,
		)
	}
	fileSourceCleanupErr := invokeRequestLogOperation(
		"retry request log file source cleanup",
		logging.RetryPendingFileBodySourceCleanup,
	)
	return errors.Join(loggerCleanupErr, fileSourceCleanupErr)
}

func snapshotRequestLogger(logger logging.RequestLogger) (snapshot logging.RequestLogger, err error) {
	if logger == nil {
		return nil, nil
	}
	defer func() {
		if panicValue := recover(); panicValue != nil {
			snapshot = nil
			err = requestLogPanicError("snapshot request logger", panicValue)
		}
	}()
	if snapshotter, ok := logger.(requestLoggerSnapshotter); ok {
		return snapshotter.Snapshot(), nil
	}
	return logger, nil
}

func attachRequestLogSources(c *gin.Context, logger logging.RequestLogger, loggerEnabled bool) error {
	if c == nil || !loggerEnabled {
		return nil
	}
	factory, ok := logger.(fileBodySourceFactory)
	if !ok || factory == nil {
		return nil
	}

	sourceSpecs := []struct {
		prefix string
		key    string
	}{
		{prefix: "api-request", key: logging.APIRequestSourceContextKey},
		{prefix: "api-response", key: logging.APIResponseSourceContextKey},
		{prefix: "websocket-timeline", key: logging.WebsocketTimelineSourceContextKey},
		{prefix: "api-websocket-timeline", key: logging.APIWebsocketTimelineSourceContextKey},
	}
	if !isResponsesWebsocketUpgrade(c.Request) {
		sourceSpecs = sourceSpecs[:2]
	}

	sources := make([]*logging.FileBodySource, 0, len(sourceSpecs))
	for _, spec := range sourceSpecs {
		source, errSource := createRequestLogFileBodySource(factory, spec.prefix)
		if errSource != nil {
			return errors.Join(
				fmt.Errorf("create %s request log source: %w", spec.prefix, errSource),
				cleanupFileBodySources(sources...),
			)
		}
		sources = append(sources, source)
	}
	for i, spec := range sourceSpecs {
		c.Set(spec.key, sources[i])
	}
	c.Set(logging.FileBodySourceCleanupQueueContextKey, &logging.FileBodySourceCleanupQueue{})
	return nil
}

func createRequestLogFileBodySource(
	factory fileBodySourceFactory,
	prefix string,
) (source *logging.FileBodySource, err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			source = nil
			err = requestLogPanicError("create request log file body source", panicValue)
		}
	}()
	source, err = factory.NewFileBodySource(prefix)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("request logger returned nil file body source")
	}
	return source, nil
}

func shouldSkipMethodForRequestLogging(req *http.Request) bool {
	if req == nil {
		return true
	}
	if req.Method != http.MethodGet {
		return false
	}
	return !isResponsesWebsocketUpgrade(req)
}

func isResponsesWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.URL.Path != "/v1/responses" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func shouldCaptureRequestBody(loggerEnabled bool, req *http.Request) bool {
	if loggerEnabled {
		return true
	}
	if req == nil || req.Body == nil {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return false
	}
	if req.ContentLength <= 0 {
		return false
	}
	return req.ContentLength <= maxErrorOnlyCapturedRequestBodyBytes
}

// captureRequestInfo extracts relevant information from the incoming HTTP request.
// It captures the URL, method, headers, and body. The request body is read and then
// restored so that it can be processed by subsequent handlers.
func captureRequestInfo(c *gin.Context, captureBody bool) (*requestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string)
	maps.Copy(headers, c.Request.Header)

	// Capture request body
	var body []byte
	if captureBody && c.Request.Body != nil {
		originalBody := c.Request.Body
		bodyBytes, errRead := io.ReadAll(io.LimitReader(originalBody, maxCapturedRequestBodyBytes))
		c.Request.Body = &replayRequestBody{
			Reader: io.MultiReader(bytes.NewReader(bodyBytes), originalBody),
			Closer: originalBody,
		}
		if errRead != nil {
			return nil, errRead
		}
		body = decodeCapturedRequestBodyForLog(bodyBytes, c.Request.Header.Get("Content-Encoding"))
	}

	return &requestInfo{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		RequestID: logging.GetGinRequestID(c),
		Timestamp: time.Now(),
	}, nil
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func decodeCapturedRequestBodyForLog(raw []byte, encoding string) []byte {
	if len(raw) == 0 {
		return raw
	}
	decoded, errDecode := decodeCapturedRequestBody(raw, encoding)
	if errDecode != nil {
		return []byte(fmt.Sprintf("[request body omitted: %v]", errDecode))
	}
	return decoded
}

func decodeCapturedRequestBody(raw []byte, encoding string) ([]byte, error) {
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, errDecode := decodeCapturedZstdRequestBody(body)
			if errDecode != nil {
				return nil, errDecode
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeCapturedZstdRequestBody(raw []byte) ([]byte, error) {
	decoder, errNewReader := zstd.NewReader(
		bytes.NewReader(raw),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxDecodedRequestLogBodyBytes)),
	)
	if errNewReader != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", errNewReader)
	}
	defer decoder.Close()

	decoded, errRead := io.ReadAll(io.LimitReader(decoder, maxDecodedRequestLogBodyBytes+1))
	if errRead != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", errRead)
	}
	if int64(len(decoded)) > maxDecodedRequestLogBodyBytes {
		return nil, errDecodedRequestLogBodyTooLarge
	}
	return decoded, nil
}

func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}

	return true
}
