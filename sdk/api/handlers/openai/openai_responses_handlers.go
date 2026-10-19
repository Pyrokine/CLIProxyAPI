// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	multiagentv2 "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/optimize-multi-agent-v2"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type ResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

var responsesStreamKeepAliveInterval = 15 * time.Second

func writeResponsesSSEChunk(w io.Writer, chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	if err := handlers.WriteStreamBytes(w, chunk); err != nil {
		return err
	}
	if bytes.HasSuffix(chunk, []byte("\n\n")) || bytes.HasSuffix(chunk, []byte("\r\n\r\n")) {
		return nil
	}
	suffix := []byte("\n\n")
	if bytes.HasSuffix(chunk, []byte("\n")) || bytes.HasSuffix(chunk, []byte("\r\n")) {
		suffix = []byte("\n")
	}
	return handlers.WriteStreamBytes(w, suffix)
}

func writeResponsesSSEFrames(w io.Writer, frames [][]byte) error {
	for _, frame := range frames {
		if err := writeResponsesSSEChunk(w, frame); err != nil {
			return err
		}
	}
	return nil
}

type responsesStreamFrameWriter struct {
	writer        io.Writer
	deferred      [][]byte
	deferredBytes int
	holding       bool
	deferTerminal bool
	failedWritten bool
	err           error
}

func (w *responsesStreamFrameWriter) writeFrames(frames [][]byte) {
	if w == nil || w.err != nil {
		return
	}
	for _, frame := range frames {
		if !w.deferTerminal || (!w.holding && !isResponsesTerminalSSEFrame(frame)) {
			if err := writeResponsesSSEChunk(w.writer, frame); err != nil {
				w.err = err
				return
			}
			if payload, hasData, valid := responsesSSEDataPayload(frame); hasData && valid && gjson.GetBytes(payload, "type").String() == "response.failed" {
				w.failedWritten = true
			}
			continue
		}
		w.holding = true
		if len(frame) > maxResponsesSSEFrameBytes-w.deferredBytes {
			w.deferred = nil
			w.deferredBytes = 0
			w.err = fmt.Errorf(
				"responses SSE terminal sequence exceeds maximum allowed size of %d bytes",
				maxResponsesSSEFrameBytes,
			)
			return
		}
		w.deferred = append(w.deferred, frame)
		w.deferredBytes += len(frame)
	}
}

func (w *responsesStreamFrameWriter) writeDeferred() {
	if w == nil || w.err != nil {
		return
	}
	if err := writeResponsesSSEFrames(w.writer, w.deferred); err != nil {
		w.err = err
		return
	}
	w.discardDeferred()
}

func (w *responsesStreamFrameWriter) discardDeferred() {
	if w == nil {
		return
	}
	w.deferred = nil
	w.deferredBytes = 0
	w.holding = false
}

func isResponsesTerminalSSEFrame(frame []byte) bool {
	payload, hasData, valid := responsesSSEDataPayload(frame)
	if !hasData || !valid || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "response.completed", "response.done":
		return true
	default:
		return false
	}
}

func writeResponsesStreamError(c *gin.Context, errMsg *handlers.ErrorMessage) error {
	if c == nil || c.Writer == nil || errMsg == nil {
		return nil
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && errMsg.Error.Error() != "" {
		errText = errMsg.Error.Error()
	}
	if isCodexResponsesClientRequest(c) {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, 0)
		return handlers.WriteStreamBytes(
			c.Writer, []byte("\nevent: response.failed\ndata: "), chunk, []byte("\n\n"),
		)
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
	return handlers.WriteStreamBytes(c.Writer, []byte("\nevent: error\ndata: "), chunk, []byte("\n\n"))
}

func writeResponsesStreamTerminalError(c *gin.Context, errMsg *handlers.ErrorMessage) error {
	if c == nil || errMsg == nil {
		return nil
	}
	if shouldExposeResponsesUpstreamError(errMsg) {
		return writeResponsesStreamError(c, errMsg)
	}
	if isCodexResponsesClientRequest(c) {
		return writeResponsesStreamError(
			c, &handlers.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      errors.New(http.StatusText(http.StatusBadGateway)),
			},
		)
	}
	return nil
}

func (h *ResponsesAPIHandler) writeInitialResponsesStreamError(
	c *gin.Context,
	flusher http.Flusher,
	setSSEHeaders func(),
	errMsg *handlers.ErrorMessage,
) error {
	errWrite := handlers.DeliverStreamResponse(
		c.Writer, flusher, func() error {
			switch {
			case c.Writer.Written():
				return writeResponsesStreamTerminalError(c, errMsg)
			case isResponsesEmptyStreamError(errMsg.Error) && !isCodexResponsesClientRequest(c):
				setSSEHeaders()
				return handlers.WriteStreamBytes(c.Writer, []byte("\n"))
			default:
				return h.WriteErrorResponseWithError(c, errMsg)
			}
		},
	)
	handlers.AcknowledgeStreamError(errMsg, errWrite)
	return errWrite
}

func isCodexResponsesClientRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if multiagentv2.IsCodexClientUserAgent(c.GetHeader("User-Agent")) {
		return true
	}

	switch originator := strings.ToLower(strings.TrimSpace(c.GetHeader("Originator"))); originator {
	case "codex desktop", "codex-tui", "codex_cli_rs":
		return true
	default:
		return strings.HasPrefix(originator, "codex desktop/") ||
			strings.HasPrefix(originator, "codex-tui/") ||
			strings.HasPrefix(originator, "codex_cli_rs/")
	}
}

func isResponsesEmptyStreamError(err error) bool {
	var authErr *coreauth.Error
	return errors.As(err, &authErr) && authErr.Code == "empty_stream"
}

// NewResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an ResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *ResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *ResponsesAPIHandler {
	return &ResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

func withResponsesContinuationAuthPin(ctx context.Context, rawJSON []byte) context.Context {
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) == "" {
		return ctx
	}
	return handlers.WithPinSelectedAuth(ctx)
}

// HandlerType returns the identifier for this handler implementation.
func (h *ResponsesAPIHandler) HandlerType() string {
	return constant.OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *ResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *ResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	h.WriteModelListResponse(c, h.HandlerType(), gin.H{
		"object": "list",
		"data":   h.FilterModelsForCurrentAPIKey(c, h.Models()),
	})
}

func (h *ResponsesAPIHandler) prepareCodexMultiAgentV2Tools(c *gin.Context, payload []byte) []byte {
	cfg := h.ConfigSnapshot()
	if cfg == nil {
		return payload
	}

	requestCtx := context.Background()
	var requestHeaders http.Header
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
		requestHeaders = c.Request.Header
	}
	requestCtx = util.WithGinContext(requestCtx, c)

	homeEnabled := h.AuthManager != nil && h.AuthManager.HomeEnabled()
	updated, prepared := multiagentv2.PrepareCodexMultiAgentV2Tools(
		requestCtx,
		requestHeaders,
		payload,
		cfg.CodexOptimizeMultiAgentV2,
		homeEnabled,
	)
	if prepared && c != nil {
		c.Set(multiagentv2.CodexMultiAgentV2ToolsPreparedContextKey, true)
	}
	return updated
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *ResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	rawJSON = h.prepareCodexMultiAgentV2Tools(c, rawJSON)

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

func (h *ResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Streaming not supported for compact responses",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withResponsesContinuationAuthPin(cliCtx, rawJSON)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(
		cliCtx, h.HandlerType(), modelName, rawJSON, "responses/compact",
	)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for constant.Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *ResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withResponsesContinuationAuthPin(cliCtx, rawJSON)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func receiveInitialResponsesStreamChunk(
	ctx context.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	keepAlive <-chan time.Time,
	writeKeepAlive func() error,
) (coreexecutor.StreamChunk, *handlers.ErrorMessage, error) {
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return coreexecutor.StreamChunk{}, nil, ctx.Err()
		case <-keepAlive:
			if writeKeepAlive == nil {
				continue
			}
			if err := writeKeepAlive(); err != nil {
				return coreexecutor.StreamChunk{}, nil, err
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return coreexecutor.StreamChunk{}, errMsg, nil
			}
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			return chunk, nil, nil
		}
	}
	return coreexecutor.StreamChunk{}, nil, fmt.Errorf("responses stream ended before response.completed")
}

func receiveInitialResponsesStream(
	ctx context.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	keepAlive <-chan time.Time,
	writeKeepAlive func() error,
) ([]byte, *handlers.ErrorMessage, error) {
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-keepAlive:
			if writeKeepAlive == nil {
				continue
			}
			if err := writeKeepAlive(); err != nil {
				return nil, nil, err
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return nil, errMsg, nil
			}
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			return chunk, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("responses stream ended before response.completed")
}

// handleStreamingResponse handles streaming responses for constant.Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *ResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
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

	// New core execution path
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withResponsesContinuationAuthPin(cliCtx, rawJSON)

	var upstreamHeaders http.Header
	headersSet := false
	setSSEHeaders := func() {
		if headersSet {
			return
		}
		handlers.SetSSEHeaders(c)
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		headersSet = true
	}
	writeKeepAlive := func() error {
		setSSEHeaders()
		return handlers.DeliverStreamResponse(
			c.Writer, flusher, func() error {
				return handlers.WriteStreamBytes(c.Writer, []byte(": keep-alive\n\n"))
			},
		)
	}
	keepAlive := time.NewTicker(responsesStreamKeepAliveInterval)
	defer keepAlive.Stop()

	type streamSetup struct {
		data    <-chan coreexecutor.StreamChunk
		headers http.Header
		errs    <-chan *handlers.ErrorMessage
	}
	setupChan := make(chan streamSetup, 1)
	go func() {
		data, headers, errs := h.ExecuteStreamChunksWithAuthManager(
			cliCtx, h.HandlerType(), modelName, rawJSON, "",
		)
		setupChan <- streamSetup{data: data, headers: headers, errs: errs}
	}()

	var dataChan <-chan coreexecutor.StreamChunk
	var errChan <-chan *handlers.ErrorMessage
	setupReady := false
	for !setupReady {
		select {
		case setup := <-setupChan:
			dataChan = setup.data
			upstreamHeaders = setup.headers
			errChan = setup.errs
			setupReady = true
		case <-c.Request.Context().Done():
			if !c.Writer.Written() {
				c.Status(clienterror.StatusClientClosedRequest)
				c.Writer.WriteHeaderNow()
			}
			cliCancel(c.Request.Context().Err())
			return
		case <-keepAlive.C:
			if err := writeKeepAlive(); err != nil {
				internallogging.MarkRequestOutcomeFailure(
					c.Request.Context(),
					internallogging.RequestOutcomeStreamError,
					"stream_initial",
					err.Error(),
				)
				cliCancel(err)
				return
			}
		}
	}

	acc := newResponsesSSEFrameAccumulator()
	chunk, errMsg, streamErr := receiveInitialResponsesStreamChunk(
		c.Request.Context(), dataChan, errChan, keepAlive.C, writeKeepAlive,
	)
	if streamErr != nil {
		internallogging.MarkRequestOutcomeFailure(
			c.Request.Context(),
			internallogging.RequestOutcomeStreamError,
			"stream_initial",
			streamErr.Error(),
		)
		if c.Request.Context().Err() != nil {
			if !c.Writer.Written() {
				c.Status(clienterror.StatusClientClosedRequest)
				c.Writer.WriteHeaderNow()
			}
			cliCancel(streamErr)
			return
		}
		clientErr := &handlers.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      errors.New(http.StatusText(http.StatusBadGateway)),
		}
		if errWrite := h.writeInitialResponsesStreamError(c, flusher, setSSEHeaders, clientErr); errWrite != nil {
			cliCancel(errWrite)
		} else {
			cliCancel(streamErr)
		}
		return
	}
	if errMsg != nil {
		if isResponsesEmptyStreamError(errMsg.Error) {
			internallogging.MarkRequestOutcomeFailure(
				c.Request.Context(),
				internallogging.RequestOutcomeStreamError,
				"stream_initial",
				errMsg.Error.Error(),
			)
		}
		if errWrite := h.writeInitialResponsesStreamError(c, flusher, setSSEHeaders, errMsg); errWrite != nil {
			cliCancel(errWrite)
		} else {
			cliCancel(errMsg.Error)
		}
		return
	}

	setSSEHeaders()
	h.forwardResponsesStreamChunks(
		c, flusher, func(err error) { cliCancel(err) }, acc, dataChan, errChan, chunk,
	)
}

func (h *ResponsesAPIHandler) forwardResponsesStream(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	acc *sseFrameAccumulator,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	initialChunks ...[]byte,
) {
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		for chunk := range data {
			select {
			case <-c.Request.Context().Done():
				return
			case chunks <- coreexecutor.StreamChunk{Payload: chunk}:
			}
		}
	}()
	initial := make([]coreexecutor.StreamChunk, 0, len(initialChunks))
	for _, chunk := range initialChunks {
		initial = append(initial, coreexecutor.StreamChunk{Payload: chunk})
	}
	h.forwardResponsesStreamChunksWithMode(c, flusher, cancel, acc, chunks, errs, true, initial...)
}

func (h *ResponsesAPIHandler) forwardResponsesStreamChunks(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	acc *sseFrameAccumulator,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	initialChunks ...coreexecutor.StreamChunk,
) {
	h.forwardResponsesStreamChunksWithMode(c, flusher, cancel, acc, data, errs, false, initialChunks...)
}

func (h *ResponsesAPIHandler) forwardResponsesStreamChunksWithMode(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	acc *sseFrameAccumulator,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	deferTerminal bool,
	initialChunks ...coreexecutor.StreamChunk,
) {
	if acc == nil {
		acc = newResponsesSSEFrameAccumulator()
	}
	acc.isCodexClient = isCodexResponsesClientRequest(c)
	frameWriter := &responsesStreamFrameWriter{writer: c.Writer, deferTerminal: deferTerminal}
	doneError := func() error {
		if err := acc.Err(); err != nil {
			return err
		}
		return frameWriter.err
	}
	keepAliveInterval := responsesStreamKeepAliveInterval
	h.ForwardStreamChunks(
		c, flusher, cancel, data, errs, handlers.StreamChunkForwardOptions{
			KeepAliveInterval: &keepAliveInterval,
			RequireTerminal:   !deferTerminal,
			WriteChunk: func(chunk []byte) error {
				frameWriter.writeFrames(acc.AddChunk(chunk))
				return doneError()
			},
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) error {
				if errMsg == nil {
					return nil
				}
				frameWriter.writeFrames(acc.Flush())
				frameWriter.discardDeferred()
				if err := doneError(); err != nil {
					return err
				}
				if frameWriter.failedWritten {
					return nil
				}
				return writeResponsesStreamTerminalError(c, errMsg)
			},
			WriteDone: func() error {
				frameWriter.writeFrames(acc.Finalize())
				if err := doneError(); err != nil {
					frameWriter.discardDeferred()
					return err
				}
				frameWriter.writeDeferred()
				if err := doneError(); err != nil {
					return err
				}
				return handlers.WriteStreamBytes(c.Writer, []byte("\n"))
			},
			DoneError: doneError,
		},
		initialChunks...,
	)
}
