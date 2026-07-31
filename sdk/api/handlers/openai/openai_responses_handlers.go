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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type ResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

func writeResponsesSSEChunk(w io.Writer, chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}
	_, _ = w.Write(chunk)
	if bytes.HasSuffix(chunk, []byte("\n\n")) || bytes.HasSuffix(chunk, []byte("\r\n\r\n")) {
		return
	}
	suffix := []byte("\n\n")
	if bytes.HasSuffix(chunk, []byte("\n")) || bytes.HasSuffix(chunk, []byte("\r\n")) {
		suffix = []byte("\n")
	}
	_, _ = w.Write(suffix)
}

func writeResponsesSSEFrames(w io.Writer, frames [][]byte) {
	for _, frame := range frames {
		writeResponsesSSEChunk(w, frame)
	}
}

type responsesStreamFrameWriter struct {
	writer        io.Writer
	deferred      [][]byte
	deferredBytes int
	holding       bool
	err           error
}

func (w *responsesStreamFrameWriter) writeFrames(frames [][]byte) {
	if w == nil || w.err != nil {
		return
	}
	for _, frame := range frames {
		if !w.holding && !isResponsesTerminalSSEFrame(frame) {
			writeResponsesSSEChunk(w.writer, frame)
			continue
		}
		w.holding = true
		if len(frame) > maxResponsesSSEFrameBytes-w.deferredBytes {
			w.deferred = nil
			w.deferredBytes = 0
			w.err = fmt.Errorf("Responses SSE terminal sequence exceeds maximum allowed size of %d bytes", maxResponsesSSEFrameBytes)
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
	writeResponsesSSEFrames(w.writer, w.deferred)
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

func writeResponsesStreamError(w io.Writer, errMsg *handlers.ErrorMessage) {
	if w == nil || errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && errMsg.Error.Error() != "" {
		errText = errMsg.Error.Error()
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
	_, _ = fmt.Fprintf(w, "\nevent: error\ndata: %s\n\n", string(chunk))
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
	c.JSON(
		http.StatusOK, gin.H{
			"object": "list",
			"data":   h.FilterModelsForCurrentAPIKey(c, h.Models()),
		},
	)
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

func receiveInitialResponsesStream(
	ctx context.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
) ([]byte, *handlers.ErrorMessage, error) {
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
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
	return nil, nil, fmt.Errorf("Responses stream ended before response.completed")
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
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(
		cliCtx, h.HandlerType(), modelName, rawJSON, "",
	)

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
	}

	acc := newResponsesSSEFrameAccumulator()
	chunk, errMsg, streamErr := receiveInitialResponsesStream(c.Request.Context(), dataChan, errChan)
	if streamErr != nil {
		if c.Request.Context().Err() != nil {
			cliCancel(streamErr)
			return
		}
		setSSEHeaders()
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		writeResponsesStreamError(c.Writer, &handlers.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      streamErr,
		})
		flusher.Flush()
		cliCancel(streamErr)
		return
	}
	if errMsg != nil {
		if isResponsesEmptyStreamError(errMsg.Error) {
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeResponsesStreamError(c.Writer, errMsg)
			flusher.Flush()
			cliCancel(errMsg.Error)
			return
		}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}

	setSSEHeaders()
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	h.forwardResponsesStream(
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
	if acc == nil {
		acc = newResponsesSSEFrameAccumulator()
	}
	frameWriter := &responsesStreamFrameWriter{writer: c.Writer}
	for _, chunk := range initialChunks {
		frameWriter.writeFrames(acc.AddChunk(chunk))
	}
	if len(initialChunks) > 0 {
		flusher.Flush()
	}

	doneError := func() error {
		if err := acc.Err(); err != nil {
			return err
		}
		return frameWriter.err
	}
	h.ForwardStream(
		c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
			WriteChunk: func(chunk []byte) {
				frameWriter.writeFrames(acc.AddChunk(chunk))
			},
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) {
				if errMsg == nil {
					return
				}
				frameWriter.writeFrames(acc.Flush())
				frameWriter.discardDeferred()
				writeResponsesStreamError(c.Writer, errMsg)
			},
			WriteDone: func() {
				frameWriter.writeFrames(acc.Finalize())
				if err := doneError(); err != nil {
					frameWriter.discardDeferred()
					writeResponsesStreamError(c.Writer, &handlers.ErrorMessage{
						StatusCode: http.StatusBadGateway,
						Error:      err,
					})
					return
				}
				frameWriter.writeDeferred()
				_, _ = c.Writer.Write([]byte("\n"))
			},
			DoneError: doneError,
		},
	)
}
