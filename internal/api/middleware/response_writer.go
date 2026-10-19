// Package middleware provides Gin HTTP middleware for the CLI Proxy API server.
// It includes a sophisticated response writer wrapper designed to capture and log request and response data,
// including support for streaming responses, without impacting latency.
package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/httpwire"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const requestBodyOverrideContextKey = "REQUEST_BODY_OVERRIDE"
const responseBodyOverrideContextKey = "RESPONSE_BODY_OVERRIDE"
const websocketTimelineOverrideContextKey = "WEBSOCKET_TIMELINE_OVERRIDE"
const maxBufferedResponseBodyBytes = 8 << 20

var truncatedResponseBody = []byte("[response body omitted: exceeds capture limit]")

// requestInfo holds essential details of an incoming HTTP request for logging purposes.
type requestInfo struct {
	URL                 string                      // URL is the request URL.
	Method              string                      // Method is the HTTP method (e.g., GET, POST).
	Headers             map[string][]string         // Headers contains the request headers.
	Body                []byte                      // Body is the raw request body.
	RequestID           string                      // RequestID is the unique identifier for the request.
	Timestamp           time.Time                   // Timestamp is when the request was received.
	deferredBodyCapture *deferredRequestBodyCapture // deferredBodyCapture spools large error-only request bodies.
}

// responseWriterWrapper wraps the standard gin.ResponseWriter to intercept and log response data.
// It is designed to handle both standard and streaming responses, ensuring that logging operations do not block the client response.
type responseWriterWrapper struct {
	gin.ResponseWriter
	body                *bytes.Buffer // body is a buffer to store the response body for non-streaming responses.
	bodyTruncated       bool
	isStreaming         bool                       // isStreaming indicates whether the response is a streaming type (e.g., text/event-stream).
	streamWriter        logging.StreamingLogWriter // streamWriter is a writer for handling streaming log entries.
	chunkChannel        chan []byte                // chunkChannel is a channel for asynchronously passing response chunks to the logger.
	streamDone          chan struct{}              // streamDone signals when the streaming goroutine completes.
	streamErrMu         sync.Mutex
	streamErr           error
	logger              logging.RequestLogger // logger is the instance of the request logger service.
	loggerEnabled       bool                  // loggerEnabled freezes the request-entry enabled state.
	requestInfo         *requestInfo          // requestInfo holds the details of the original request.
	statusCode          int                   // statusCode stores the HTTP status code of the response.
	headers             map[string][]string   // headers stores the response headers.
	headerInitialized   bool
	handlerPanicked     bool
	logOnErrorOnly      bool      // logOnErrorOnly enables logging only when an error response is detected.
	firstChunkTimestamp time.Time // firstChunkTimestamp captures TTFB for streaming responses.
	firstWriteTimestamp time.Time
	timeline            *logging.RequestTimeline
	finalizeOnce        sync.Once
	finalized           atomic.Bool
	finalizeErr         error
}

// Unwrap returns the original response writer.
func (w *responseWriterWrapper) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func newResponseWriterWrapper(
	w gin.ResponseWriter,
	logger logging.RequestLogger,
	requestInfo *requestInfo,
	timeline *logging.RequestTimeline,
	enabledSnapshot ...bool,
) *responseWriterWrapper {
	loggerEnabled := false
	var enabledErr error
	if len(enabledSnapshot) > 0 {
		loggerEnabled = enabledSnapshot[0]
	} else {
		loggerEnabled, enabledErr = requestLoggerEnabled(logger)
	}
	wrapper := &responseWriterWrapper{
		ResponseWriter: w,
		body:           &bytes.Buffer{},
		logger:         logger,
		loggerEnabled:  loggerEnabled,
		requestInfo:    requestInfo,
		headers:        make(map[string][]string),
		timeline:       timeline,
	}
	wrapper.addStreamError(enabledErr)
	return wrapper
}

// Write wraps the underlying ResponseWriter's Write method to capture response data.
// For non-streaming responses, it writes to an internal buffer. For streaming responses,
// it sends data chunks to a non-blocking channel for asynchronous logging.
func (w *responseWriterWrapper) Write(data []byte) (int, error) {
	w.commitHeaders()

	n, err := w.ResponseWriter.Write(data)
	written := data[:min(max(n, 0), len(data))]
	if len(written) > 0 {
		w.markFirstWrite()
	}

	if w.isStreaming && w.chunkChannel != nil {
		w.captureFirstChunkTimestamp(written)
		if len(written) > 0 {
			select {
			case w.chunkChannel <- bytes.Clone(written):
			default:
				log.Warn("request log streaming buffer full, dropped response chunk")
			}
		}
		return n, err
	}

	if len(written) > 0 && w.shouldBufferResponseBody() {
		w.bufferResponseBody(written)
	}

	return n, err
}

func (w *responseWriterWrapper) bufferResponseBody(data []byte) {
	if w == nil || w.body == nil || len(data) == 0 || w.bodyTruncated {
		return
	}
	if w.body.Len()+len(data) > maxBufferedResponseBodyBytes {
		w.body.Reset()
		_, _ = w.body.Write(truncatedResponseBody)
		w.bodyTruncated = true
		return
	}
	_, _ = w.body.Write(data)
}

func (w *responseWriterWrapper) shouldBufferResponseBody() bool {
	if w.loggerEnabled {
		return true
	}
	if !w.logOnErrorOnly {
		return false
	}
	status := w.ResponseWriter.Status()
	if status == 0 {
		status = http.StatusOK
	}
	return status >= http.StatusBadRequest
}

func (w *responseWriterWrapper) addTimelineEvent(name string, detail string) {
	if w.timeline == nil {
		return
	}
	w.timeline.Add(name, detail)
}

func (w *responseWriterWrapper) addTimelineEventOnce(name string, detail string) {
	if w.timeline == nil {
		return
	}
	w.timeline.AddOnce(name, detail)
}

func (w *responseWriterWrapper) markFirstWrite() {
	if !w.firstWriteTimestamp.IsZero() {
		return
	}
	w.firstWriteTimestamp = time.Now()
	w.addTimelineEventOnce("client.response.first_write", "")
}

// WriteString wraps the underlying ResponseWriter's WriteString method to capture response data.
// Some handlers (and fmt/io helpers) write via io.StringWriter; without this override, those writes
// bypass Write() and would be missing from request logs.
func (w *responseWriterWrapper) WriteString(data string) (int, error) {
	w.commitHeaders()

	n, err := w.ResponseWriter.WriteString(data)
	written := data[:min(max(n, 0), len(data))]
	if len(written) > 0 {
		w.markFirstWrite()
	}

	if w.isStreaming && w.chunkChannel != nil {
		w.captureFirstChunkTimestamp([]byte(written))
		if len(written) > 0 {
			select {
			case w.chunkChannel <- []byte(written):
			default:
				log.Warn("request log streaming buffer full, dropped response chunk")
			}
		}
		return n, err
	}

	if len(written) > 0 && w.shouldBufferResponseBody() {
		w.bufferResponseBody([]byte(written))
	}
	return n, err
}

// WriteHeader updates Gin's pending response status. Streaming logging is initialized
// only when the response headers are actually committed.
func (w *responseWriterWrapper) WriteHeader(statusCode int) {
	if w == nil {
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
	if !w.headerInitialized {
		w.statusCode = w.ResponseWriter.Status()
	}
}

func (w *responseWriterWrapper) WriteHeaderNow() {
	w.commitHeaders()
}

func (w *responseWriterWrapper) Flush() {
	_ = w.FlushError()
}

func (w *responseWriterWrapper) FlushError() error {
	w.commitHeaders()
	return httpwire.FlushResponse(w.ResponseWriter)
}

func (w *responseWriterWrapper) commitHeaders() {
	if w == nil || w.headerInitialized {
		return
	}
	w.headerInitialized = true
	w.statusCode = w.ResponseWriter.Status()
	w.captureCurrentHeaders()
	w.isStreaming = w.detectStreaming(w.ResponseWriter.Header().Get("Content-Type"))

	if !w.finalized.Load() && w.isStreaming && w.loggerEnabled && w.requestInfo != nil {
		streamWriter, errStream := invokeStreamingLogWriter(
			"initialize streaming request log",
			func() (logging.StreamingLogWriter, error) {
				return w.logger.LogStreamingRequest(
					w.requestInfo.URL,
					w.requestInfo.Method,
					w.requestInfo.Headers,
					w.requestInfo.Body,
					w.requestInfo.RequestID,
				)
			},
		)
		w.addStreamError(errStream)
		if errStream == nil {
			w.streamWriter = streamWriter
			w.chunkChannel = make(chan []byte, 100)
			w.streamDone = make(chan struct{})
			go w.processStreamingChunks(w.streamDone, streamWriter, w.chunkChannel)
			w.addStreamError(
				invokeRequestLogOperation(
					"write streaming request log status",
					func() error { return streamWriter.WriteStatus(w.statusCode, w.headers) },
				),
			)
		}
	}

	w.ResponseWriter.WriteHeaderNow()
	w.statusCode = w.ResponseWriter.Status()
	w.addTimelineEventOnce("client.response.headers", fmt.Sprintf("status=%d", w.statusCode))
}

func (w *responseWriterWrapper) captureFirstChunkTimestamp(data []byte) {
	if len(data) == 0 || !w.firstChunkTimestamp.IsZero() {
		return
	}
	w.firstChunkTimestamp = w.firstWriteTimestamp
	if w.firstChunkTimestamp.IsZero() {
		w.firstChunkTimestamp = time.Now()
	}
}

// ensureHeadersCaptured refreshes pending headers and preserves the committed snapshot.
func (w *responseWriterWrapper) ensureHeadersCaptured() {
	if !w.headerInitialized {
		w.captureCurrentHeaders()
	}
}

// captureCurrentHeaders reads all headers from the underlying ResponseWriter and stores copies.
func (w *responseWriterWrapper) captureCurrentHeaders() {
	headers := make(map[string][]string, len(w.ResponseWriter.Header()))
	for key, values := range w.ResponseWriter.Header() {
		headers[key] = append([]string(nil), values...)
	}
	w.headers = headers
}

// detectStreaming determines if a response should be treated as a streaming response.
// It checks for a "text/event-stream" Content-Type or a '"stream": true'
// field in the original request body.
func (w *responseWriterWrapper) detectStreaming(contentType string) bool {
	// Check Content-Type for Server-Sent Events
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// If a concrete Content-Type is already set (e.g., application/json for error responses),
	// treat it as non-streaming instead of inferring from the request payload.
	if strings.TrimSpace(contentType) != "" {
		return false
	}

	// Only fall back to request payload hints when Content-Type is not set yet.
	if w.requestInfo != nil && len(w.requestInfo.Body) > 0 {
		return bytes.Contains(w.requestInfo.Body, []byte(`"stream": true`)) ||
			bytes.Contains(w.requestInfo.Body, []byte(`"stream":true`))
	}

	return false
}

// processStreamingChunks runs in a separate goroutine to process response chunks from the chunkChannel.
// It asynchronously writes each chunk to the streaming log writer.
func (w *responseWriterWrapper) processStreamingChunks(
	done chan struct{},
	streamWriter logging.StreamingLogWriter,
	chunkChannel <-chan []byte,
) {
	if done == nil {
		return
	}

	defer func() {
		if panicValue := recover(); panicValue != nil {
			w.addStreamError(
				requestLogPanicError(
					"write streaming request log response chunk",
					panicValue,
				),
			)
		}
		close(done)
	}()

	if isNilStreamingLogWriter(streamWriter) || chunkChannel == nil {
		return
	}

	for chunk := range chunkChannel {
		streamWriter.WriteChunkAsync(chunk)
	}
}

// finalize completes the logging process for the request and response.
// For streaming responses, it closes the chunk channel and the stream writer.
// For non-streaming responses, it logs the complete request and response details,
// including any API-specific request/response data stored in the Gin context.
func (w *responseWriterWrapper) finalize(c *gin.Context) error {
	if w == nil {
		return nil
	}
	w.finalizeOnce.Do(
		func() {
			w.finalized.Store(true)
			w.finalizeErr = invokeRequestLogOperation(
				"finalize request log",
				func() error { return w.finalizeRequest(c) },
			)
		},
	)
	return w.finalizeErr
}

func (w *responseWriterWrapper) finalizeRequest(c *gin.Context) (err error) {
	websocketTimelineSource := w.extractWebsocketTimelineSource(c)
	apiRequestSource := w.extractAPIRequestSource(c)
	apiResponseSource := w.extractAPIResponseSource(c)
	apiWebsocketTimelineSource := w.extractAPIWebsocketTimelineSource(c)
	cleanupQueue := extractFileBodySourceCleanupQueue(c)
	var deferredBodyCapture *deferredRequestBodyCapture
	if w.requestInfo != nil {
		deferredBodyCapture = w.requestInfo.deferredBodyCapture
	}
	defer func() {
		err = errors.Join(
			err,
			deferredBodyCapture.cleanup(),
			cleanupFileBodySources(
				websocketTimelineSource,
				apiRequestSource,
				apiResponseSource,
				apiWebsocketTimelineSource,
			),
			cleanupQueue.CleanupAndRetainFailures(),
		)
	}()

	if w.logger == nil {
		return nil
	}

	finalStatusCode := w.ResponseWriter.Status()
	if finalStatusCode == 0 {
		finalStatusCode = http.StatusOK
	}

	var requestOutcome logging.RequestOutcome
	if c != nil && c.Request != nil {
		requestContext := c.Request.Context()
		switch {
		case w.handlerPanicked:
			logging.MarkRequestOutcomeFailure(
				requestContext,
				logging.RequestOutcomePanic,
				"handler",
				"handler panicked",
			)
		case errors.Is(requestContext.Err(), context.DeadlineExceeded):
			logging.MarkRequestOutcomeFailure(
				requestContext,
				logging.RequestOutcomeDeadline,
				"request",
				context.DeadlineExceeded.Error(),
			)
		case errors.Is(requestContext.Err(), context.Canceled):
			logging.MarkRequestOutcomeFailure(
				requestContext,
				logging.RequestOutcomeCanceled,
				"request",
				context.Canceled.Error(),
			)
		case finalStatusCode >= http.StatusInternalServerError:
			logging.MarkRequestOutcomeFailure(
				requestContext,
				logging.RequestOutcomeServerError,
				"http",
				fmt.Sprintf("status %d", finalStatusCode),
			)
		case finalStatusCode >= http.StatusBadRequest:
			logging.MarkRequestOutcomeFailure(
				requestContext,
				logging.RequestOutcomeClientError,
				"http",
				fmt.Sprintf("status %d", finalStatusCode),
			)
		}
		if state := logging.RequestOutcomeStateFromContext(requestContext); state != nil {
			requestOutcome = state.Snapshot()
		}
	}

	var slicesAPIResponseError []*interfaces.ErrorMessage
	if c != nil {
		apiResponseError, isExist := c.Get("API_RESPONSE_ERROR")
		if isExist {
			if apiErrors, ok := apiResponseError.([]*interfaces.ErrorMessage); ok {
				slicesAPIResponseError = apiErrors
			}
		}
	}

	hasAPIError := requestOutcome.Failed || w.handlerPanicked ||
		len(slicesAPIResponseError) > 0 || finalStatusCode >= http.StatusBadRequest
	forceLog := w.logOnErrorOnly && hasAPIError && !w.loggerEnabled
	if !w.loggerEnabled && !forceLog {
		if w.requestInfo != nil {
			recordRequestLogArtifact(
				w.logger, logging.RequestLogArtifact{
					RequestID: w.requestInfo.RequestID,
					State:     logging.RequestLogDisabled,
					Reason:    "request logging is disabled",
				},
			)
		}
		return nil
	}

	handlerDetail := fmt.Sprintf("status=%d", finalStatusCode)
	if w.handlerPanicked {
		handlerDetail += " panic=true"
	}
	w.addTimelineEvent("request.handler.done", handlerDetail)
	w.addTimelineEvent("request.log.start", "")

	if w.isStreaming && !isNilStreamingLogWriter(w.streamWriter) {
		return w.finalizeStreamingRequest(
			c,
			websocketTimelineSource,
			apiRequestSource,
			apiResponseSource,
			apiWebsocketTimelineSource,
		)
	}

	apiRequest := w.extractAPIRequest(c)
	if forceLog && len(apiRequest) == 0 {
		apiRequest = w.extractDeferredAPIRequest(c)
	}
	return errors.Join(
		w.streamError(),
		w.logRequest(
			w.extractRequestBody(c), finalStatusCode, w.cloneHeaders(), w.extractResponseBody(c),
			w.extractWebsocketTimeline(c), websocketTimelineSource, apiRequest, apiRequestSource,
			w.extractAPIResponse(c), apiResponseSource, w.extractAPIWebsocketTimeline(c), apiWebsocketTimelineSource,
			requestTimelineBytes(c), w.extractAPIResponseTimestamp(c), slicesAPIResponseError, forceLog,
		),
	)
}

func (w *responseWriterWrapper) finalizeStreamingRequest(
	c *gin.Context,
	websocketTimelineSource *logging.FileBodySource,
	apiRequestSource *logging.FileBodySource,
	apiResponseSource *logging.FileBodySource,
	apiWebsocketTimelineSource *logging.FileBodySource,
) (streamErr error) {
	streamWriter := w.streamWriter
	w.streamWriter = nil
	defer func() {
		streamErr = errors.Join(
			streamErr,
			cleanupFileBodySources(
				websocketTimelineSource,
				apiRequestSource,
				apiResponseSource,
				apiWebsocketTimelineSource,
			),
		)
	}()
	defer func() {
		streamErr = errors.Join(
			streamErr,
			invokeRequestLogOperation(
				"close streaming request log",
				streamWriter.Close,
			),
		)
	}()

	if w.chunkChannel != nil {
		close(w.chunkChannel)
		w.chunkChannel = nil
	}
	if w.streamDone != nil {
		<-w.streamDone
		w.streamDone = nil
	}
	streamErr = errors.Join(streamErr, w.streamError())
	streamErr = errors.Join(
		streamErr,
		invokeRequestLogAction(
			"set streaming request log first chunk timestamp",
			func() { streamWriter.SetFirstChunkTimestamp(w.firstChunkTimestamp) },
		),
	)

	apiRequest := w.extractAPIRequest(c)
	apiResponse := w.extractAPIResponse(c)
	apiWebsocketTimeline := w.extractAPIWebsocketTimeline(c)
	if sourceWriter, ok := streamWriter.(interface {
		WriteAPIRequestSource(*logging.FileBodySource) error
		WriteAPIResponseSource(*logging.FileBodySource) error
		WriteAPIWebsocketTimelineSource(*logging.FileBodySource) error
	}); ok {
		if apiRequestSource != nil && apiRequestSource.HasPayload() {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API request source",
					func() error { return sourceWriter.WriteAPIRequestSource(apiRequestSource) },
				),
			)
		}
		if len(apiRequest) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API request",
					func() error { return streamWriter.WriteAPIRequest(apiRequest) },
				),
			)
		}
		if apiResponseSource != nil && apiResponseSource.HasPayload() {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API response source",
					func() error { return sourceWriter.WriteAPIResponseSource(apiResponseSource) },
				),
			)
		}
		if len(apiResponse) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API response",
					func() error { return streamWriter.WriteAPIResponse(apiResponse) },
				),
			)
		}
		if apiWebsocketTimelineSource != nil && apiWebsocketTimelineSource.HasPayload() {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API websocket timeline source",
					func() error { return sourceWriter.WriteAPIWebsocketTimelineSource(apiWebsocketTimelineSource) },
				),
			)
		}
		if len(apiWebsocketTimeline) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API websocket timeline",
					func() error { return streamWriter.WriteAPIWebsocketTimeline(apiWebsocketTimeline) },
				),
			)
		}
	} else {
		mergedAPIRequest, errMerge := mergeFileBodySource(apiRequest, apiRequestSource)
		streamErr = joinStreamingLogError(streamErr, "merge streaming request log API request source", errMerge)
		if len(mergedAPIRequest) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API request",
					func() error { return streamWriter.WriteAPIRequest(mergedAPIRequest) },
				),
			)
		}
		mergedAPIResponse, errMerge := mergeFileBodySource(apiResponse, apiResponseSource)
		streamErr = joinStreamingLogError(streamErr, "merge streaming request log API response source", errMerge)
		if len(mergedAPIResponse) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API response",
					func() error { return streamWriter.WriteAPIResponse(mergedAPIResponse) },
				),
			)
		}
		mergedAPIWebsocketTimeline, errMerge := mergeFileBodySource(
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
		)
		streamErr = joinStreamingLogError(
			streamErr,
			"merge streaming request log API websocket timeline source",
			errMerge,
		)
		if len(mergedAPIWebsocketTimeline) > 0 {
			streamErr = errors.Join(
				streamErr, invokeRequestLogOperation(
					"write streaming request log API websocket timeline",
					func() error { return streamWriter.WriteAPIWebsocketTimeline(mergedAPIWebsocketTimeline) },
				),
			)
		}
	}
	requestTimeline := requestTimelineBytes(c)
	if len(requestTimeline) > 0 {
		streamErr = errors.Join(
			streamErr, invokeRequestLogOperation(
				"write streaming request log request timeline",
				func() error { return streamWriter.WriteRequestTimeline(requestTimeline) },
			),
		)
	}
	return streamErr
}

func (w *responseWriterWrapper) cloneHeaders() map[string][]string {
	w.ensureHeadersCaptured()

	finalHeaders := make(map[string][]string, len(w.headers))
	for key, values := range w.headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		finalHeaders[key] = headerValues
	}

	return finalHeaders
}

func (w *responseWriterWrapper) extractAPIRequest(c *gin.Context) []byte {
	return extractRequestLogPayload(c, "API_REQUEST")
}

func (w *responseWriterWrapper) extractDeferredAPIRequest(c *gin.Context) []byte {
	if c == nil {
		return nil
	}
	value, exists := c.Get(logging.DeferredAPIRequestContextKey)
	if !exists {
		return nil
	}
	requests, ok := value.([]logging.DeferredAPIRequest)
	if !ok || len(requests) == 0 {
		return nil
	}
	var body bytes.Buffer
	for _, buildRequest := range requests {
		if buildRequest == nil {
			continue
		}
		payload, errBuild := invokeDeferredAPIRequest(buildRequest)
		if errBuild != nil {
			log.WithError(errBuild).Warn("failed to build deferred API request log")
			continue
		}
		body.Write(payload)
	}
	return body.Bytes()
}

func invokeDeferredAPIRequest(buildRequest logging.DeferredAPIRequest) (payload []byte, err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			payload = nil
			err = requestLogPanicError("build deferred API request log", panicValue)
		}
	}()
	return buildRequest(), nil
}

func (w *responseWriterWrapper) extractAPIResponse(c *gin.Context) []byte {
	return extractRequestLogPayload(c, "API_RESPONSE")
}

func (w *responseWriterWrapper) extractAPIRequestSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIRequestSourceContextKey)
}

func (w *responseWriterWrapper) extractAPIResponseSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIResponseSourceContextKey)
}

func (w *responseWriterWrapper) extractAPIWebsocketTimeline(c *gin.Context) []byte {
	return extractRequestLogPayload(c, "API_WEBSOCKET_TIMELINE")
}

func extractRequestLogPayload(c *gin.Context, key string) []byte {
	if c == nil {
		return nil
	}
	value, exists := c.Get(key)
	if !exists {
		return nil
	}
	switch payload := value.(type) {
	case []byte:
		return bytes.Clone(payload)
	case string:
		return []byte(payload)
	case *logging.BoundedLogBuffer:
		return payload.Bytes()
	default:
		return nil
	}
}

func (w *responseWriterWrapper) extractAPIResponseTimestamp(c *gin.Context) time.Time {
	if c == nil {
		return time.Time{}
	}
	ts, isExist := c.Get("API_RESPONSE_TIMESTAMP")
	if !isExist {
		return time.Time{}
	}
	if t, ok := ts.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func (w *responseWriterWrapper) extractRequestBody(c *gin.Context) []byte {
	if body := extractBodyOverride(c, requestBodyOverrideContextKey); len(body) > 0 {
		return body
	}
	if w.requestInfo == nil {
		return nil
	}
	if len(w.requestInfo.Body) > 0 {
		return w.requestInfo.Body
	}
	if w.requestInfo.deferredBodyCapture == nil {
		return nil
	}
	body, statusMarker, errRead := w.requestInfo.deferredBodyCapture.bytes()
	if errRead != nil {
		log.WithError(errRead).Warn("failed to read deferred request body capture")
		return []byte("[request body unavailable: deferred capture failed]")
	}
	encoding := ""
	for key, values := range w.requestInfo.Headers {
		if strings.EqualFold(key, "Content-Encoding") && len(values) > 0 {
			encoding = values[0]
			break
		}
	}
	body = decodeCapturedRequestBodyForLogWithLimit(body, encoding, maxDeferredErrorRequestBodyBytes)
	if statusMarker == "" {
		return body
	}
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		body = append(body, '\n')
	}
	return append(body, statusMarker...)
}

func (w *responseWriterWrapper) extractResponseBody(c *gin.Context) []byte {
	if body := extractBodyOverride(c, responseBodyOverrideContextKey); len(body) > 0 {
		return body
	}
	if w.body == nil || w.body.Len() == 0 {
		return nil
	}
	return bytes.Clone(w.body.Bytes())
}

func (w *responseWriterWrapper) extractWebsocketTimeline(c *gin.Context) []byte {
	return extractBodyOverride(c, websocketTimelineOverrideContextKey)
}

func (w *responseWriterWrapper) extractWebsocketTimelineSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.WebsocketTimelineSourceContextKey)
}

func (w *responseWriterWrapper) extractAPIWebsocketTimelineSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIWebsocketTimelineSourceContextKey)
}

func extractFileBodySourceCleanupQueue(c *gin.Context) *logging.FileBodySourceCleanupQueue {
	if c == nil {
		return nil
	}
	value, exists := c.Get(logging.FileBodySourceCleanupQueueContextKey)
	if !exists {
		return nil
	}
	queue, _ := value.(*logging.FileBodySourceCleanupQueue)
	return queue
}

func extractFileBodySource(c *gin.Context, key string) *logging.FileBodySource {
	if c == nil {
		return nil
	}
	value, exists := c.Get(key)
	if !exists {
		return nil
	}
	source, ok := value.(*logging.FileBodySource)
	if !ok || source == nil {
		return nil
	}
	return source
}

func extractBodyOverride(c *gin.Context, key string) []byte {
	if c == nil {
		return nil
	}
	bodyOverride, isExist := c.Get(key)
	if !isExist {
		return nil
	}
	switch value := bodyOverride.(type) {
	case []byte:
		if len(value) > 0 {
			return bytes.Clone(value)
		}
	case string:
		if strings.TrimSpace(value) != "" {
			return []byte(value)
		}
	}
	return nil
}

func (w *responseWriterWrapper) logRequest(
	requestBody []byte,
	statusCode int,
	headers map[string][]string,
	body []byte,
	websocketTimeline []byte,
	websocketTimelineSource *logging.FileBodySource,
	apiRequestBody []byte,
	apiRequestSource *logging.FileBodySource,
	apiResponseBody []byte,
	apiResponseSource *logging.FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *logging.FileBodySource,
	requestTimeline []byte,
	apiResponseTimestamp time.Time,
	apiResponseErrors []*interfaces.ErrorMessage,
	forceLog bool,
) (err error) {
	defer func() {
		err = errors.Join(
			err,
			cleanupFileBodySources(
				websocketTimelineSource,
				apiRequestSource,
				apiResponseSource,
				apiWebsocketTimelineSource,
			),
		)
	}()
	if w.requestInfo == nil {
		return nil
	}

	if loggerWithAllSources, ok := w.logger.(interface {
		LogRequestWithOptionsAndAllSources(
			string,
			string,
			map[string][]string,
			[]byte,
			int,
			map[string][]string,
			[]byte,
			[]byte,
			*logging.FileBodySource,
			[]byte,
			*logging.FileBodySource,
			[]byte,
			*logging.FileBodySource,
			[]byte,
			*logging.FileBodySource,
			[]byte,
			[]*interfaces.ErrorMessage,
			bool,
			string,
			time.Time,
			time.Time,
		) error
	}); ok {
		return invokeRequestLogOperation(
			"write request log with file body sources",
			func() error {
				return loggerWithAllSources.LogRequestWithOptionsAndAllSources(
					w.requestInfo.URL,
					w.requestInfo.Method,
					w.requestInfo.Headers,
					requestBody,
					statusCode,
					headers,
					body,
					websocketTimeline,
					websocketTimelineSource,
					apiRequestBody,
					apiRequestSource,
					apiResponseBody,
					apiResponseSource,
					apiWebsocketTimeline,
					apiWebsocketTimelineSource,
					requestTimeline,
					apiResponseErrors,
					forceLog,
					w.requestInfo.RequestID,
					w.requestInfo.Timestamp,
					apiResponseTimestamp,
				)
			},
		)
	}

	var mergeErr error
	var errMerge error
	websocketTimeline, errMerge = mergeFileBodySource(websocketTimeline, websocketTimelineSource)
	mergeErr = joinStreamingLogError(mergeErr, "merge request log websocket timeline source", errMerge)
	apiRequestBody, errMerge = mergeFileBodySource(apiRequestBody, apiRequestSource)
	mergeErr = joinStreamingLogError(mergeErr, "merge request log API request source", errMerge)
	apiResponseBody, errMerge = mergeFileBodySource(apiResponseBody, apiResponseSource)
	mergeErr = joinStreamingLogError(mergeErr, "merge request log API response source", errMerge)
	apiWebsocketTimeline, errMerge = mergeFileBodySource(apiWebsocketTimeline, apiWebsocketTimelineSource)
	mergeErr = joinStreamingLogError(
		mergeErr,
		"merge request log API websocket timeline source",
		errMerge,
	)

	if loggerWithTimeline, ok := w.logger.(interface {
		LogRequestWithOptionsAndTimeline(
			string,
			string,
			map[string][]string,
			[]byte,
			int,
			map[string][]string,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]*interfaces.ErrorMessage,
			bool,
			string,
			time.Time,
			time.Time,
		) error
	}); ok {
		logErr := invokeRequestLogOperation(
			"write request log with timeline",
			func() error {
				return loggerWithTimeline.LogRequestWithOptionsAndTimeline(
					w.requestInfo.URL,
					w.requestInfo.Method,
					w.requestInfo.Headers,
					requestBody,
					statusCode,
					headers,
					body,
					websocketTimeline,
					apiRequestBody,
					apiResponseBody,
					apiWebsocketTimeline,
					requestTimeline,
					apiResponseErrors,
					forceLog,
					w.requestInfo.RequestID,
					w.requestInfo.Timestamp,
					apiResponseTimestamp,
				)
			},
		)
		return errors.Join(mergeErr, logErr)
	}

	logErr := invokeRequestLogOperation(
		"write request log",
		func() error {
			return w.logger.LogRequest(
				w.requestInfo.URL,
				w.requestInfo.Method,
				w.requestInfo.Headers,
				requestBody,
				statusCode,
				headers,
				body,
				websocketTimeline,
				apiRequestBody,
				apiResponseBody,
				apiWebsocketTimeline,
				apiResponseErrors,
				w.requestInfo.RequestID,
				w.requestInfo.Timestamp,
				apiResponseTimestamp,
			)
		},
	)
	return errors.Join(mergeErr, logErr)
}

func (w *responseWriterWrapper) addStreamError(err error) {
	if w == nil || err == nil {
		return
	}
	w.streamErrMu.Lock()
	w.streamErr = errors.Join(w.streamErr, err)
	w.streamErrMu.Unlock()
}

func (w *responseWriterWrapper) streamError() error {
	if w == nil {
		return nil
	}
	w.streamErrMu.Lock()
	defer w.streamErrMu.Unlock()
	return w.streamErr
}

func requestLoggerEnabled(logger logging.RequestLogger) (enabled bool, err error) {
	if logger == nil {
		return false, nil
	}
	defer func() {
		if panicValue := recover(); panicValue != nil {
			enabled = false
			err = requestLogPanicError("read request logger enabled state", panicValue)
		}
	}()
	return logger.IsEnabled(), nil
}

func invokeStreamingLogWriter(
	operation string,
	callback func() (logging.StreamingLogWriter, error),
) (writer logging.StreamingLogWriter, err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			writer = nil
			err = requestLogPanicError(operation, panicValue)
		}
	}()
	writer, err = callback()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	if isNilStreamingLogWriter(writer) {
		return nil, fmt.Errorf("%s: logger returned nil writer", operation)
	}
	return writer, nil
}

func invokeRequestLogOperation(operation string, callback func() error) (err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = requestLogPanicError(operation, panicValue)
		}
	}()
	if errCallback := callback(); errCallback != nil {
		return fmt.Errorf("%s: %w", operation, errCallback)
	}
	return nil
}

func invokeRequestLogAction(operation string, callback func()) (err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = requestLogPanicError(operation, panicValue)
		}
	}()
	callback()
	return nil
}

func requestLogPanicError(operation string, panicValue any) error {
	if panicErr, ok := panicValue.(error); ok {
		return fmt.Errorf("%s panic: %w", operation, panicErr)
	}
	return fmt.Errorf("%s panic: %v", operation, panicValue)
}

func isNilStreamingLogWriter(writer logging.StreamingLogWriter) bool {
	if writer == nil {
		return true
	}
	value := reflect.ValueOf(writer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func joinStreamingLogError(current error, operation string, err error) error {
	if err == nil {
		return current
	}
	return errors.Join(current, fmt.Errorf("%s: %w", operation, err))
}

func mergeFileBodySource(
	payload []byte,
	source *logging.FileBodySource,
) (merged []byte, err error) {
	return logging.MergeFileBodySource(payload, source)
}

func cleanupFileBodySources(sources ...*logging.FileBodySource) error {
	return logging.CleanupFileBodySources(sources...)
}
