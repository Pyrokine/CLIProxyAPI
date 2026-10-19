// Package claude provides HTTP handlers for constant.Claude API code-related functionality.
// This package implements constant.Claude-compatible streaming chat completions with sophisticated
// client rotation and quota management systems to ensure high availability and optimal
// resource utilization across multiple backend clients. It handles request translation
// between constant.Claude API format and the underlying constant.Gemini backend, providing seamless
// API compatibility while maintaining robust error handling and connection management.
package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodeAPIHandler contains the handlers for constant.Claude API endpoints.
// It holds a pool of clients to interact with the backend service.
type CodeAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewCodeAPIHandler creates a new constant.Claude API handlers instance.
// It takes an BaseAPIHandler instance as input and returns a CodeAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handler instance.
//
// Returns:
//   - *CodeAPIHandler: A new constant.Claude code API handler instance.
func NewCodeAPIHandler(apiHandlers *handlers.BaseAPIHandler) *CodeAPIHandler {
	return &CodeAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

const (
	claudeHeartbeatDetailLimit       = 8
	claudeRequestBodyReadIdleTimeout = 100 * time.Second
	claudeStreamKeepAliveInterval    = 15 * time.Second
)

type claudeStreamStart struct {
	data    <-chan coreexecutor.StreamChunk
	headers http.Header
	errs    <-chan *handlers.ErrorMessage
}

type claudeHeartbeatTelemetry struct {
	attempts       int
	written        int
	failures       int
	suppressed     int
	flushes        int
	detailEvents   int
	summaryWritten bool
	lastAt         time.Time
}

func requestTimeline(c *gin.Context) *internallogging.RequestTimeline {
	if c == nil {
		return nil
	}
	value, exists := c.Get(internallogging.RequestTimelineGinKey)
	if !exists {
		return nil
	}
	timeline, _ := value.(*internallogging.RequestTimeline)
	return timeline
}

type claudeRequestBodyIdleDeadline struct {
	io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
}

func (b *claudeRequestBodyIdleDeadline) Read(p []byte) (int, error) {
	if err := b.controller.SetReadDeadline(time.Now().Add(b.timeout)); err != nil {
		log.WithError(err).Warn("failed to refresh Claude request body read deadline")
	}
	return b.ReadCloser.Read(p)
}

func readClaudeRequestBody(c *gin.Context, idleTimeout time.Duration) ([]byte, int, error) {
	deadlineSet := false
	controller := http.NewResponseController(c.Writer)
	if idleTimeout > 0 {
		if errDeadline := controller.SetReadDeadline(time.Now().Add(idleTimeout)); errDeadline == nil {
			deadlineSet = true
			c.Request.Body = &claudeRequestBodyIdleDeadline{
				ReadCloser: c.Request.Body,
				controller: controller,
				timeout:    idleTimeout,
			}
		} else if !errors.Is(errDeadline, http.ErrNotSupported) {
			log.WithError(errDeadline).Warn("failed to set Claude request body read deadline")
		}
	}
	if timeline := requestTimeline(c); timeline != nil {
		timeline.Add(
			"request.body.read.start",
			fmt.Sprintf(
				"content_length=%d idle_timeout_ms=%d deadline_set=%t",
				c.Request.ContentLength,
				idleTimeout.Milliseconds(),
				deadlineSet,
			),
		)
	}

	rawJSON, errRead := handlers.ReadRequestBody(c)
	if deadlineSet {
		if errClear := controller.SetReadDeadline(time.Time{}); errClear != nil {
			log.WithError(errClear).Warn("failed to clear Claude request body deadline")
		}
	}
	status := http.StatusBadRequest
	result := "complete"
	if errRead != nil {
		result = "error"
		var timeoutError net.Error
		if errors.As(errRead, &timeoutError) && timeoutError.Timeout() {
			status = http.StatusRequestTimeout
			result = "timeout"
			c.Request.Close = true
			c.Header("Connection", "close")
		}
	}
	if timeline := requestTimeline(c); timeline != nil {
		timeline.Add(
			"request.body.read.done",
			fmt.Sprintf("bytes=%d result=%s", len(rawJSON), result),
		)
	}
	if errRead != nil {
		return nil, status, errRead
	}
	return rawJSON, http.StatusOK, nil
}

func (t *claudeHeartbeatTelemetry) recordWrite(c *gin.Context, result handlers.KeepAliveWriteResult) {
	if t == nil {
		return
	}
	t.attempts++
	t.lastAt = time.Now()
	status := "written"
	switch {
	case result.Err != nil:
		t.failures++
		status = "error"
	case result.Suppressed:
		t.suppressed++
		status = "suppressed"
	case result.Written:
		t.written++
	default:
		status = "empty"
	}
	if t.detailEvents >= claudeHeartbeatDetailLimit {
		return
	}
	t.detailEvents++
	requestTimeline(c).Add(
		"client.stream.heartbeat.write",
		fmt.Sprintf("sequence=%d event=anthropic_ping bytes=%d status=%s", t.attempts, result.Bytes, status),
	)
}

func (t *claudeHeartbeatTelemetry) recordFlush(c *gin.Context) {
	if t == nil {
		return
	}
	t.flushes++
	if t.detailEvents >= claudeHeartbeatDetailLimit {
		return
	}
	t.detailEvents++
	requestTimeline(c).Add(
		"client.stream.heartbeat.flush",
		fmt.Sprintf("sequence=%d event=anthropic_ping returned=true", t.flushes),
	)
}

func (t *claudeHeartbeatTelemetry) summary(c *gin.Context) {
	if t == nil || t.summaryWritten {
		return
	}
	t.summaryWritten = true
	lastAt := ""
	if !t.lastAt.IsZero() {
		lastAt = t.lastAt.Format(time.RFC3339Nano)
	}
	requestTimeline(c).Add(
		"client.stream.heartbeat.summary",
		fmt.Sprintf(
			"attempts=%d written=%d failures=%d suppressed=%d flushes=%d last_at=%s",
			t.attempts, t.written, t.failures, t.suppressed, t.flushes, lastAt,
		),
	)
}

// HandlerType returns the identifier for this handler implementation.
func (h *CodeAPIHandler) HandlerType() string {
	return constant.Claude
}

// Models returns a list of models supported by this handler.
func (h *CodeAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("claude")
}

// ClaudeMessages handles constant.Claude-compatible streaming chat completions.
// This function implements a sophisticated client rotation and quota management system
// to ensure high availability and optimal resource utilization across multiple backend clients.
//
// Parameters:
//   - c: The Gin context for the request.
func (h *CodeAPIHandler) ClaudeMessages(c *gin.Context) {
	// Extract raw JSON data from the incoming request
	rawJSON, errorStatus, err := readClaudeRequestBody(c, claudeRequestBodyReadIdleTimeout)
	if err != nil {
		message := fmt.Sprintf("Invalid request: %v", err)
		if errorStatus == http.StatusRequestTimeout {
			message = "Request body timed out before it was fully received"
		}
		h.WriteErrorResponse(c, &handlers.ErrorMessage{StatusCode: errorStatus, Error: errors.New(message)})
		return
	}

	rawJSON = resolveClaudeClientModelInBody(
		rawJSON,
		h.Models(),
		claudeModelListCloakingDisabled(h.ConfigSnapshot()),
	)

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if !streamResult.Exists() || streamResult.Type == gjson.False {
		h.handleNonStreamingResponse(c, rawJSON)
	} else {
		h.handleStreamingResponse(c, rawJSON)
	}
}

// ClaudeCountTokens handles constant.Claude-compatible streaming chat completions.
// This function implements a sophisticated client rotation and quota management system
// to ensure high availability and optimal resource utilization across multiple backend clients.
//
// Parameters:
//   - c: The Gin context for the request.
func (h *CodeAPIHandler) ClaudeCountTokens(c *gin.Context) {
	// Extract raw JSON data from the incoming request
	rawJSON, errorStatus, err := readClaudeRequestBody(c, claudeRequestBodyReadIdleTimeout)
	if err != nil {
		message := fmt.Sprintf("Invalid request: %v", err)
		if errorStatus == http.StatusRequestTimeout {
			message = "Request body timed out before it was fully received"
		}
		h.WriteErrorResponse(c, &handlers.ErrorMessage{StatusCode: errorStatus, Error: errors.New(message)})
		return
	}

	rawJSON = resolveClaudeClientModelInBody(
		rawJSON,
		h.Models(),
		claudeModelListCloakingDisabled(h.ConfigSnapshot()),
	)

	c.Header("Content-Type", "application/json")

	alt := h.GetAlt(c)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())

	modelName := gjson.GetBytes(rawJSON, "model").String()

	resp, upstreamHeaders, errMsg := h.ExecuteCountWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, alt)
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// ClaudeModels handles the constant.Claude models listing endpoint.
// It returns a JSON response containing available constant.Claude models and their specifications.
//
// Parameters:
//   - c: The Gin context for the request.
func (h *CodeAPIHandler) ClaudeModels(c *gin.Context) {
	models := h.FilterModelsForCurrentAPIKey(c, h.Models())
	disableCloaking := claudeModelListCloakingDisabled(h.ConfigSnapshot())
	h.WriteModelListResponse(c, h.HandlerType(), claudemodels.NewCatalog(models, disableCloaking).Response())
}

func claudeModelListCloakingDisabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.ClaudeCode.DisableCloakingModelList
}

func resolveClaudeClientModelInBody(
	rawJSON []byte,
	availableModels []map[string]any,
	disableCloaking bool,
) []byte {
	modelName := gjson.GetBytes(rawJSON, "model").String()
	resolved := claudemodels.NewCatalog(availableModels, disableCloaking).ResolveModelID(modelName)
	if resolved == modelName {
		return rawJSON
	}
	updated, err := sjson.SetBytes(rawJSON, "model", resolved)
	if err != nil {
		return rawJSON
	}
	return updated
}

// handleNonStreamingResponse handles non-streaming content generation requests for constant.Claude models.
// This function processes the request synchronously and returns the complete generated
// response in a single API call. It supports various generation parameters and
// response formats.
//
// Parameters:
//   - c: The Gin context for the request
//   - modelName: The name of the constant.Gemini model to use for content generation
//   - rawJSON: The raw JSON request body containing generation parameters and content
func (h *CodeAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")
	alt := h.GetAlt(c)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	modelName := gjson.GetBytes(rawJSON, "model").String()

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, alt)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}

	// Decompress gzipped responses - constant.Claude API sometimes returns gzip without Content-Encoding header
	// This fixes title generation and other non-streaming responses that arrive compressed
	if len(resp) >= 2 && resp[0] == 0x1f && resp[1] == 0x8b {
		gzReader, errGzip := gzip.NewReader(bytes.NewReader(resp))
		if errGzip != nil {
			log.Warnf("failed to decompress gzipped constant.Claude response: %v", errGzip)
		} else {
			defer func() {
				if errClose := gzReader.Close(); errClose != nil {
					log.Warnf("failed to close constant.Claude gzip reader: %v", errClose)
				}
			}()
			decompressed, errRead := io.ReadAll(io.LimitReader(gzReader, 50<<20))
			if errRead != nil {
				log.Warnf("failed to read decompressed constant.Claude response: %v", errRead)
			} else {
				resp = decompressed
			}
		}
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse streams constant.Claude-compatible responses backed by constant.Gemini.
// It sets up SSE, selects a backend client with rotation/quota logic,
// forwards chunks, and translates them to constant.Claude CLI format.
//
// Parameters:
//   - c: The Gin context for the request.
//   - rawJSON: The raw JSON request body.
func (h *CodeAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Streaming not supported",
					Type:    "server_error",
				},
			},
		)
		return
	}

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	startChan := make(chan claudeStreamStart, 1)
	go func() {
		dataChan, upstreamHeaders, errChan := h.ExecuteStreamChunksWithAuthManager(
			cliCtx, h.HandlerType(), modelName, rawJSON, "",
		)
		select {
		case <-cliCtx.Done():
		case startChan <- claudeStreamStart{data: dataChan, headers: upstreamHeaders, errs: errChan}:
		}
	}()

	h.forwardClaudeStreamingResponse(c, flusher, cliCancel, startChan, claudeStreamKeepAliveInterval)
}

func (h *CodeAPIHandler) forwardClaudeStreamingResponse(
	c *gin.Context,
	flusher http.Flusher,
	cancel handlers.APIHandlerCancelFunc,
	startChan <-chan claudeStreamStart,
	keepAliveInterval time.Duration,
) {
	headersSent := false
	streamLifecycle := newClaudeStreamLifecycle()
	heartbeatTelemetry := &claudeHeartbeatTelemetry{}
	defer heartbeatTelemetry.summary(c)
	sendSSEHeaders := func(upstreamHeaders http.Header) {
		if headersSent {
			return
		}
		handlers.SetSSEHeaders(c)
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		headersSent = true
	}

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case <-keepAlive.C:
			sendSSEHeaders(nil)
			if errDelivery := deliverClaudeStreamKeepAlive(c, flusher, heartbeatTelemetry); errDelivery != nil {
				handlers.MarkStreamRequestFailure(c, errDelivery)
				cancel(errDelivery)
				return
			}
		case started := <-startChan:
			dataChan := started.data
			errChan := started.errs
			for {
				select {
				case <-c.Request.Context().Done():
					cancel(c.Request.Context().Err())
					return
				case <-keepAlive.C:
					sendSSEHeaders(started.headers)
					if errDelivery := deliverClaudeStreamKeepAlive(c, flusher, heartbeatTelemetry); errDelivery != nil {
						handlers.MarkStreamRequestFailure(c, errDelivery)
						cancel(errDelivery)
						return
					}
				case errMsg, ok := <-errChan:
					if !ok {
						errChan = nil
						continue
					}
					if errMsg != nil {
						var errWrite error
						if headersSent {
							errWrite = h.writeClaudeStreamError(c, flusher, streamLifecycle, errMsg)
							handlers.AcknowledgeStreamError(errMsg, errWrite)
						} else {
							errWrite = h.WriteAcknowledgedStreamError(c, flusher, errMsg)
						}
						if errWrite != nil {
							cancel(errWrite)
						} else {
							cancel(errMsg.Error)
						}
					} else {
						cancel(nil)
					}
					return
				case chunk, ok := <-dataChan:
					if !ok {
						sendSSEHeaders(started.headers)
						h.forwardClaudeStreamChunksWithLifecycle(
							c,
							flusher,
							func(err error) { cancel(err) },
							dataChan,
							errChan,
							streamLifecycle,
							heartbeatTelemetry,
							keepAliveInterval,
						)
						return
					}
					sendSSEHeaders(started.headers)
					h.forwardClaudeStreamChunksWithLifecycle(
						c, flusher, func(err error) { cancel(err) }, dataChan, errChan, streamLifecycle,
						heartbeatTelemetry,
						keepAliveInterval, chunk,
					)
					return
				}
			}
		}
	}
}

func writeClaudeStreamKeepAlive(c *gin.Context, telemetry *claudeHeartbeatTelemetry) handlers.KeepAliveWriteResult {
	payload := []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	n, err := c.Writer.Write(payload)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}
	result := handlers.KeepAliveWriteResult{Written: n > 0, Bytes: n, Err: err}
	telemetry.recordWrite(c, result)
	return result
}

func flushClaudeStreamKeepAlive(
	flusher http.Flusher,
	c *gin.Context,
	telemetry *claudeHeartbeatTelemetry,
	result handlers.KeepAliveWriteResult,
) error {
	if result.Err != nil || !result.Written {
		return result.Err
	}
	if err := handlers.FlushStreamResponse(c.Writer, flusher); err != nil {
		return err
	}
	telemetry.recordFlush(c)
	return nil
}

func deliverClaudeStreamKeepAlive(
	c *gin.Context,
	flusher http.Flusher,
	telemetry *claudeHeartbeatTelemetry,
) error {
	var result handlers.KeepAliveWriteResult
	errDelivery := handlers.DeliverStreamResponse(
		c.Writer, flusher, func() error {
			result = writeClaudeStreamKeepAlive(c, telemetry)
			return result.Err
		},
	)
	if errDelivery == nil && result.Written {
		telemetry.recordFlush(c)
	}
	return errDelivery
}

func (h *CodeAPIHandler) writeClaudeStreamError(
	c *gin.Context,
	flusher http.Flusher,
	lifecycle *claudeStreamLifecycle,
	errMsg *handlers.ErrorMessage,
) error {
	terminal := h.claudeStreamErrorEvents(lifecycle, errMsg)
	if len(terminal) == 0 {
		return nil
	}
	return handlers.DeliverStreamResponse(
		c.Writer, flusher, func() error {
			return handlers.WriteStreamBytes(c.Writer, terminal)
		},
	)
}

func (h *CodeAPIHandler) forwardClaudeStream(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
) {
	telemetry := &claudeHeartbeatTelemetry{}
	defer telemetry.summary(c)
	h.forwardClaudeStreamWithLifecycle(
		c, flusher, cancel, data, errs, newClaudeStreamLifecycle(), telemetry, claudeStreamKeepAliveInterval,
	)
}

func (h *CodeAPIHandler) forwardClaudeStreamWithLifecycle(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	lifecycle *claudeStreamLifecycle,
	telemetry *claudeHeartbeatTelemetry,
	keepAliveInterval time.Duration,
) {
	if telemetry == nil {
		telemetry = &claudeHeartbeatTelemetry{}
		defer telemetry.summary(c)
	}
	h.ForwardStream(
		c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
			KeepAliveInterval: &keepAliveInterval,
			WriteChunkError: func(chunk []byte) error {
				if len(chunk) == 0 || lifecycle.terminalSeen {
					return nil
				}
				lifecycle.observe(chunk)
				return handlers.WriteStreamBytes(c.Writer, chunk)
			},
			WriteKeepAlive: func() handlers.KeepAliveWriteResult {
				if lifecycle.terminalSeen {
					result := handlers.KeepAliveWriteResult{Suppressed: true}
					telemetry.recordWrite(c, result)
					return result
				}
				return writeClaudeStreamKeepAlive(c, telemetry)
			},
			OnKeepAliveFlush: func() {
				telemetry.recordFlush(c)
			},
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) {
				terminal := h.claudeStreamErrorEvents(lifecycle, errMsg)
				if len(terminal) == 0 {
					return
				}
				if bytes.Contains(terminal, []byte("event: error\n")) {
					status := http.StatusInternalServerError
					if errMsg.StatusCode > 0 {
						status = errMsg.StatusCode
					}
					c.Status(status)
				}
				_, _ = c.Writer.Write(terminal)
			},
		},
	)
}

func (h *CodeAPIHandler) forwardClaudeStreamChunksWithLifecycle(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	lifecycle *claudeStreamLifecycle,
	telemetry *claudeHeartbeatTelemetry,
	keepAliveInterval time.Duration,
	initial ...coreexecutor.StreamChunk,
) {
	if telemetry == nil {
		telemetry = &claudeHeartbeatTelemetry{}
		defer telemetry.summary(c)
	}
	var terminalProtocolErr error
	h.ForwardStreamChunks(
		c, flusher, cancel, data, errs, handlers.StreamChunkForwardOptions{
			KeepAliveInterval: &keepAliveInterval,
			RequireTerminal:   true,
			ProtocolTerminal: func() bool {
				return lifecycle.terminalSeen
			},
			WriteChunk: func(chunk []byte) error {
				if len(chunk) == 0 || lifecycle.terminalSeen {
					return nil
				}
				lifecycle.observe(chunk)
				return handlers.WriteStreamBytes(c.Writer, chunk)
			},
			WriteKeepAlive: func() handlers.KeepAliveWriteResult {
				if lifecycle.terminalSeen {
					result := handlers.KeepAliveWriteResult{Suppressed: true}
					telemetry.recordWrite(c, result)
					return result
				}
				return writeClaudeStreamKeepAlive(c, telemetry)
			},
			OnKeepAliveFlush: func() {
				telemetry.recordFlush(c)
			},
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) error {
				terminal := h.claudeStreamErrorEvents(lifecycle, errMsg)
				if len(terminal) == 0 {
					return nil
				}
				if bytes.Contains(terminal, []byte("event: error\n")) {
					status := http.StatusInternalServerError
					if errMsg.StatusCode > 0 {
						status = errMsg.StatusCode
					}
					c.Status(status)
				}
				return handlers.WriteStreamBytes(c.Writer, terminal)
			},
			WriteDone: func() error {
				var terminal []byte
				terminal, terminalProtocolErr = h.claudeStreamTerminalEvents(lifecycle)
				if len(terminal) == 0 {
					return nil
				}
				return handlers.WriteStreamBytes(c.Writer, terminal)
			},
			DoneError: func() error {
				return terminalProtocolErr
			},
		},
		initial...,
	)
}

func (h *CodeAPIHandler) claudeStreamErrorEvents(
	lifecycle *claudeStreamLifecycle,
	errMsg *handlers.ErrorMessage,
) []byte {
	if errMsg == nil {
		return nil
	}
	errorBytes, _ := json.Marshal(h.toClaudeError(errMsg))
	return lifecycle.finalizeError(errorBytes)
}

func (h *CodeAPIHandler) claudeStreamTerminalEvents(
	lifecycle *claudeStreamLifecycle,
) ([]byte, error) {
	if lifecycle == nil {
		return nil, errors.New("claude stream lifecycle is unavailable")
	}
	if lifecycle.terminalSeen {
		return nil, nil
	}
	if lifecycle.messageDeltaSeen {
		return lifecycle.finalizeError(nil), nil
	}
	protocolErr := errors.New("claude stream ended before message_stop")
	errorBytes, _ := json.Marshal(
		h.toClaudeError(
			&handlers.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      protocolErr,
			},
		),
	)
	return lifecycle.finalizeError(errorBytes), protocolErr
}

type claudeStreamLifecycle struct {
	pending          []byte
	openBlocks       map[int]bool
	blockOrder       []int
	messageDeltaSeen bool
	terminalSeen     bool
}

func newClaudeStreamLifecycle() *claudeStreamLifecycle {
	return &claudeStreamLifecycle{openBlocks: make(map[int]bool)}
}

func (lifecycle *claudeStreamLifecycle) observe(chunk []byte) {
	if lifecycle == nil || len(chunk) == 0 {
		return
	}
	lifecycle.pending = append(lifecycle.pending, bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))...)
	for {
		separator := bytes.Index(lifecycle.pending, []byte("\n\n"))
		if separator < 0 {
			return
		}
		block := lifecycle.pending[:separator]
		lifecycle.pending = lifecycle.pending[separator+2:]
		lifecycle.observeBlock(block)
	}
}

func (lifecycle *claudeStreamLifecycle) observeBlock(block []byte) {
	var data []byte
	for line := range bytes.SplitSeq(block, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data: ")) {
			data = bytes.TrimSpace(line[len("data: "):])
			break
		}
	}
	if !gjson.ValidBytes(data) {
		return
	}
	payload := gjson.ParseBytes(data)
	switch payload.Get("type").String() {
	case "content_block_start":
		index := int(payload.Get("index").Int())
		if !lifecycle.openBlocks[index] {
			lifecycle.openBlocks[index] = true
			lifecycle.blockOrder = append(lifecycle.blockOrder, index)
		}
	case "content_block_stop":
		delete(lifecycle.openBlocks, int(payload.Get("index").Int()))
	case "message_delta":
		lifecycle.messageDeltaSeen = true
	case "message_stop", "error":
		lifecycle.terminalSeen = true
	}
}

func (lifecycle *claudeStreamLifecycle) finalizeError(errorBytes []byte) []byte {
	if lifecycle == nil || lifecycle.terminalSeen {
		return nil
	}
	output := make([]byte, 0, len(errorBytes)+128)
	for _, index := range lifecycle.blockOrder {
		if !lifecycle.openBlocks[index] {
			continue
		}
		output = fmt.Appendf(
			output,
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n",
			index,
		)
		delete(lifecycle.openBlocks, index)
	}
	if lifecycle.messageDeltaSeen {
		output = append(output, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"...)
	} else {
		output = fmt.Appendf(output, "event: error\ndata: %s\n\n", errorBytes)
	}
	lifecycle.terminalSeen = true
	return output
}

type claudeErrorDetail struct {
	Type    string              `json:"type"`
	Message string              `json:"message"`
	Details *claudeErrorDetails `json:"details,omitempty"`
}

type claudeErrorDetails struct {
	ErrorCode string `json:"error_code"`
}

type claudeErrorResponse struct {
	Type  string            `json:"type"`
	Error claudeErrorDetail `json:"error"`
}

func (h *CodeAPIHandler) toClaudeError(msg *handlers.ErrorMessage) claudeErrorResponse {
	status := http.StatusInternalServerError
	message := http.StatusText(status)
	var originalError error
	if msg != nil {
		originalError = msg.Error
		if msg.StatusCode > 0 {
			status = msg.StatusCode
			message = http.StatusText(status)
		}
		if msg.Error != nil {
			if text := strings.TrimSpace(claudeErrorText(msg.Error)); text != "" {
				message = text
			}
		}
	}

	errType := claudeErrorTypeFromStatus(status)
	if json.Valid([]byte(message)) {
		payload := gjson.Parse(message)
		if errPayload := payload.Get("error"); errPayload.IsObject() {
			if value := strings.TrimSpace(errPayload.Get("type").String()); value != "" {
				errType = value
			}
			if value := strings.TrimSpace(errPayload.Get("message").String()); value != "" {
				message = value
			} else if value := strings.TrimSpace(errPayload.Get("code").String()); value != "" {
				message = value
			}
		} else {
			if value := strings.TrimSpace(payload.Get("type").String()); value != "" && value != "error" {
				errType = value
			}
			if value := strings.TrimSpace(payload.Get("message").String()); value != "" {
				message = value
			}
		}
	}

	response := claudeErrorResponse{Type: "error", Error: claudeErrorDetail{Type: errType, Message: message}}
	if clienterror.IsClaudeThreadNotFound(status, originalError) {
		response.Error.Details = &claudeErrorDetails{ErrorCode: "thread_not_found"}
	}
	return response
}

func claudeErrorText(err error) string {
	var body interface{ ResponseBody() []byte }
	if errors.As(err, &body) && body != nil && len(body.ResponseBody()) > 0 {
		return string(body.ResponseBody())
	}
	return err.Error()
}

// WriteErrorResponse writes a Claude-compatible error response.
func (h *CodeAPIHandler) WriteErrorResponse(c *gin.Context, msg *handlers.ErrorMessage) {
	_ = h.WriteErrorResponseWithError(c, msg)
}

func (h *CodeAPIHandler) WriteErrorResponseWithError(c *gin.Context, msg *handlers.ErrorMessage) error {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.DirectResponse {
		if !clienterror.IsClaudeThreadNotFound(status, errors.New(string(msg.Body))) {
			return h.BaseAPIHandler.WriteErrorResponseWithError(c, msg)
		}
		body, err := json.Marshal(h.toClaudeError(&handlers.ErrorMessage{
			StatusCode: status,
			Error:      errors.New(string(msg.Body)),
		}))
		if err != nil {
			return err
		}
		marked := *msg
		marked.Body = body
		return h.BaseAPIHandler.WriteErrorResponseWithError(c, &marked)
	}

	if msg != nil && msg.Error != nil {
		for _, value := range coreauth.SafeResponseHeaders(msg.Error).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
	}
	body, err := json.Marshal(h.toClaudeError(msg))
	if err != nil {
		return err
	}
	encoded := handlers.ErrorMessage{StatusCode: status, Error: errors.New(string(body))}
	if msg != nil {
		encoded.Addon = msg.Addon
	}
	return h.BaseAPIHandler.WriteErrorResponseWithError(c, &encoded)
}

func (h *CodeAPIHandler) WriteAcknowledgedStreamError(
	c *gin.Context,
	flusher http.Flusher,
	msg *handlers.ErrorMessage,
) error {
	err := handlers.DeliverStreamResponse(c.Writer, flusher, func() error {
		return h.WriteErrorResponseWithError(c, msg)
	})
	handlers.AcknowledgeStreamError(msg, err)
	return err
}

func claudeErrorTypeFromStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= http.StatusInternalServerError {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
