package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
)

// MetaAudioTranscribe preserves Meta's file transcription request and response formats.
func (h *APIHandler) MetaAudioTranscribe(c *gin.Context) {
	if h == nil || h.BaseAPIHandler == nil {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Transcription handler is unavailable", Type: "server_error",
				},
			},
		)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, runtimeexecutor.MetaAudioMaxRequestBytes)
	body, err := handlers.ReadRequestBody(c)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.WriteErrorResponse(c, &handlers.ErrorMessage{StatusCode: status, Error: err})
		return
	}
	model, err := runtimeexecutor.MetaAudioRequestModel(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.WriteErrorResponse(c, &handlers.ErrorMessage{StatusCode: http.StatusBadRequest, Error: err})
		return
	}
	accept := strings.TrimSpace(c.GetHeader("Accept"))
	switch accept {
	case "", "*/*", "application/json":
		accept = "application/json"
	case "text/plain", "text/event-stream":
	default:
		h.WriteErrorResponse(
			c, &handlers.ErrorMessage{
				StatusCode: http.StatusNotAcceptable,
				Error:      fmt.Errorf("request Accept must be application/json, text/plain, or text/event-stream"),
			},
		)
		return
	}
	ctx, cancel := h.GetContextWithCancel(h, c, context.Background())
	ctx, deferredUsage := withDeferredImageUsage(ctx)
	var finalErr error
	defer func() {
		deferredUsage.Complete(ctx, finalErr)
		cancel(finalErr)
	}()
	request := handlers.ProtocolExecutionRequest{
		EntryProtocol: "meta-audio", Model: model, Body: body, Headers: c.Request.Header.Clone(),
		Query: c.Request.URL.Query(), Path: "/v1/asr/transcribe", ForcedProvider: "meta",
		Stream: accept == "text/event-stream",
	}
	request.Headers.Set("Accept", accept)
	if !request.Stream {
		response, errMsg := h.ExecuteProtocolWithAuthManager(ctx, request)
		if errMsg != nil {
			finalErr = errMsg.Error
			h.WriteErrorResponse(c, sanitizeOpenAIUpstreamError(errMsg))
			return
		}
		handlers.WriteUpstreamHeaders(c.Writer.Header(), response.Headers)
		c.Header("Content-Type", accept)
		_, finalErr = c.Writer.Write(response.Body)
		return
	}
	response, errMsg := h.ExecuteProtocolStreamWithAuthManager(ctx, request)
	if errMsg != nil {
		finalErr = errMsg.Error
		h.WriteErrorResponse(c, sanitizeOpenAIUpstreamError(errMsg))
		return
	}
	handlers.SetSSEHeaders(c)
	handlers.WriteUpstreamHeaders(c.Writer.Header(), response.Headers)
	for chunk := range response.Chunks {
		if chunk.Err != nil {
			finalErr = chunk.Err
			_ = writeResponsesStreamError(c, &handlers.ErrorMessage{StatusCode: chunk.Err.StatusCode, Error: chunk.Err})
			return
		}
		if _, finalErr = c.Writer.Write(chunk.Payload); finalErr != nil {
			return
		}
		c.Writer.Flush()
	}
}
