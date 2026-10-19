// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
)

const (
	maxErrorOnlyCapturedRequestBodyBytes int64 = 1 << 20          // 1 MiB
	maxDeferredErrorRequestBodyBytes     int64 = (8 << 20) - 1024 // Reserve room for log status markers.
	maxCapturedRequestBodyBytes          int64 = 10 << 20         // 10 MiB
	maxDecodedRequestLogBodyBytes        int64 = 10 << 20         // 10 MiB
)

var errDecodedRequestLogBodyTooLarge = errors.New("decoded request log body exceeds 10 MiB")

type replayRequestBody struct {
	io.Reader
	io.Closer
}

type fileBodySourceFactory interface {
	NewFileBodySource(prefix string) (*logging.FileBodySource, error)
}

type deferredRequestBodyCapture struct {
	body          io.ReadCloser
	source        *logging.FileBodySource
	contentLength int64
	bytesRead     int64
	bytesCaptured int64
	captureErr    error
	finished      bool
	sawEOF        bool
	truncated     bool
}

func attachDeferredRequestBodyCapture(
	req *http.Request,
	logger logging.RequestLogger,
	requestInfo *requestInfo,
	loggerEnabled bool,
	bodyCaptured bool,
) *deferredRequestBodyCapture {
	if loggerEnabled || bodyCaptured || req == nil || req.Body == nil || req.Body == http.NoBody ||
		req.ContentLength == 0 || requestInfo == nil {
		return nil
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return nil
	}
	factory, ok := logger.(fileBodySourceFactory)
	if !ok || factory == nil {
		return nil
	}
	source, errSource := createRequestLogFileBodySource(factory, "request-body")
	if errSource != nil {
		log.WithError(errSource).Warn("failed to create deferred request body capture")
		return nil
	}
	capture := &deferredRequestBodyCapture{
		body:          req.Body,
		source:        source,
		contentLength: req.ContentLength,
	}
	req.Body = capture
	requestInfo.deferredBodyCapture = capture
	return capture
}

func (c *deferredRequestBodyCapture) Read(payload []byte) (int, error) {
	if c == nil || c.body == nil {
		return 0, io.EOF
	}
	n, errRead := c.body.Read(payload)
	if errRead == io.EOF {
		c.sawEOF = true
	}
	if n == 0 {
		return n, errRead
	}
	c.bytesRead += int64(n)
	if c.source == nil || c.captureErr != nil || c.finished {
		return n, errRead
	}

	remaining := maxDeferredErrorRequestBodyBytes - c.bytesCaptured
	if remaining <= 0 {
		c.truncated = true
		return n, errRead
	}
	writeLength := min(int64(n), remaining)
	if writeLength < int64(n) {
		c.truncated = true
	}
	if errAppend := c.source.AppendBytes(payload[:int(writeLength)]); errAppend != nil {
		c.captureErr = errAppend
		return n, errRead
	}
	c.bytesCaptured += writeLength
	return n, errRead
}

func (c *deferredRequestBodyCapture) Close() error {
	if c == nil {
		return nil
	}
	_ = c.finish()
	if c.body == nil {
		return nil
	}
	return c.body.Close()
}

func (c *deferredRequestBodyCapture) finish() error {
	if c == nil {
		return nil
	}
	if c.finished {
		return c.captureErr
	}
	c.finished = true
	return c.captureErr
}

func (c *deferredRequestBodyCapture) bytes() ([]byte, string, error) {
	if c == nil || c.source == nil {
		return nil, "", nil
	}
	if errFinish := c.finish(); errFinish != nil {
		return nil, "", errFinish
	}
	body, sourceTruncated, errBytes := c.source.ReadRawBytes(maxDeferredErrorRequestBodyBytes)
	if errBytes != nil {
		return nil, "", errBytes
	}
	c.truncated = c.truncated || sourceTruncated
	return body, c.statusMarker(), nil
}

func (c *deferredRequestBodyCapture) statusMarker() string {
	if c == nil {
		return ""
	}
	var markers []string
	if c.truncated {
		markers = append(
			markers,
			fmt.Sprintf("[REQUEST BODY TRUNCATED: captured first %d bytes]", c.bytesCaptured),
		)
	}
	complete := c.sawEOF || (c.contentLength >= 0 && c.bytesRead >= c.contentLength)
	if !complete {
		if c.contentLength >= 0 {
			markers = append(
				markers,
				fmt.Sprintf(
					"[REQUEST BODY CAPTURE INCOMPLETE: consumed %d of %d bytes]",
					c.bytesRead,
					c.contentLength,
				),
			)
		} else {
			markers = append(
				markers,
				fmt.Sprintf(
					"[REQUEST BODY CAPTURE INCOMPLETE: consumed %d bytes from an unknown-length body]",
					c.bytesRead,
				),
			)
		}
	}
	return strings.Join(markers, "\n")
}

func (c *deferredRequestBodyCapture) cleanup() error {
	if c == nil {
		return nil
	}
	source := c.source
	c.source = nil
	return errors.Join(c.finish(), cleanupFileBodySources(source))
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
// large and unknown-size bodies are spooled to disk and retained only for error logs.
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
		requestID := logging.GetGinRequestID(c)
		recordRequestLogArtifact(
			logger, logging.RequestLogArtifact{
				RequestID: requestID,
				State:     logging.RequestLogPending,
			},
		)
		if errCleanup := retryRequestLogCleanup(logger); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to retry request log cleanup")
		}
		if runtimeEnabled != nil && !runtimeEnabled() {
			recordRequestLogArtifact(
				logger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogDisabled,
					Reason:    "request logging is unavailable in the current runtime mode",
				},
			)
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			recordRequestLogArtifact(
				logger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    "request method is not captured",
				},
			)
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			recordRequestLogArtifact(
				logger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    "request path is not captured",
				},
			)
			c.Next()
			return
		}

		requestLogger, errSnapshot := snapshotRequestLogger(logger)
		if errSnapshot != nil {
			log.WithError(errSnapshot).Warn("failed to snapshot request logger")
			recordRequestLogArtifact(
				logger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    errSnapshot.Error(),
				},
			)
			c.Next()
			return
		}
		if requestLogger == nil {
			recordRequestLogArtifact(
				logger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    "request logger snapshot is unavailable",
				},
			)
			c.Next()
			return
		}
		loggerEnabled, errEnabled := requestLoggerEnabled(requestLogger)
		if errEnabled != nil {
			log.WithError(errEnabled).Warn("failed to read request logger enabled state")
			recordRequestLogArtifact(
				requestLogger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    errEnabled.Error(),
				},
			)
			c.Next()
			return
		}

		captureBody := shouldCaptureRequestBody(loggerEnabled, c.Request)

		// Capture request information
		requestInfo, err := captureRequestInfo(c, captureBody)
		if err != nil {
			recordRequestLogArtifact(
				requestLogger, logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogNotCaptured,
					Reason:    err.Error(),
				},
			)
			c.Next()
			return
		}

		initRequestTimeline(c, requestInfo.Timestamp)
		if errSources := attachRequestLogSources(c, requestLogger, loggerEnabled); errSources != nil {
			log.WithError(errSources).Warn("failed to attach request log sources")
		}
		attachDeferredRequestBodyCapture(c.Request, requestLogger, requestInfo, loggerEnabled, captureBody)

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
	if !isAPIWebsocketUpgrade(c.Request) {
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
	return !isAPIWebsocketUpgrade(req)
}

func isAPIWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil ||
		!strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
		return false
	}
	path := req.URL.Path
	if path == "/v1/responses" || path == "/responses" || path == "/backend-api/codex/responses" || path == "/v1/realtime" {
		return true
	}
	return hasSinglePathSegment(path, "/v1/live/") ||
		hasSinglePathSegment(path, "/v1/realtime/calls/")
}

func hasSinglePathSegment(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	segment := strings.TrimPrefix(path, prefix)
	return segment != "" && !strings.Contains(segment, "/")
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
	headers := c.Request.Header.Clone()

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

func decodeCapturedRequestBodyForLogWithLimit(raw []byte, encoding string, limit int64) []byte {
	if len(raw) == 0 || limit <= 0 {
		return raw
	}
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for _, part := range slices.Backward(parts) {
		enc := strings.ToLower(strings.TrimSpace(part))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, truncated, errDecode := decodeCapturedZstdRequestBodyWithLimit(body, limit)
			if errDecode != nil {
				return []byte(fmt.Sprintf("[request body omitted: %v]", errDecode))
			}
			body = decoded
			if truncated {
				if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
					body = append(body, '\n')
				}
				return append(body, "[DECOMPRESSED REQUEST BODY TRUNCATED]"...)
			}
		default:
			return []byte(fmt.Sprintf("[request body omitted: unsupported request content encoding: %s]", enc))
		}
	}
	return body
}

func decodeCapturedRequestBody(raw []byte, encoding string) ([]byte, error) {
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for _, part := range slices.Backward(parts) {
		enc := strings.ToLower(strings.TrimSpace(part))
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

func decodeCapturedZstdRequestBodyWithLimit(raw []byte, limit int64) ([]byte, bool, error) {
	decoder, errNewReader := zstd.NewReader(
		bytes.NewReader(raw),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxDecodedRequestLogBodyBytes)),
	)
	if errNewReader != nil {
		return nil, false, fmt.Errorf("failed to create zstd request decoder: %w", errNewReader)
	}
	defer decoder.Close()

	decoded, errRead := io.ReadAll(io.LimitReader(decoder, limit+1))
	if errRead != nil {
		return nil, false, fmt.Errorf("failed to decode zstd request body: %w", errRead)
	}
	if int64(len(decoded)) > limit {
		return decoded[:limit], true, nil
	}
	return decoded, false, nil
}

func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/v8/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}

	return true
}

func recordRequestLogArtifact(logger logging.RequestLogger, artifact logging.RequestLogArtifact) bool {
	if logger == nil || strings.TrimSpace(artifact.RequestID) == "" {
		return false
	}
	reporter, ok := logger.(logging.RequestLogArtifactReporter)
	return ok && reporter.RecordRequestLogArtifact(artifact)
}
