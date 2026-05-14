// Package gemini provides HTTP handlers for constant.Gemini CLI API functionality.
// This package implements handlers that process CLI-specific requests for constant.Gemini API operations,
// including content generation and streaming content generation endpoints.
// The handlers restrict access to localhost only and manage communication with the backend service.
package gemini

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// CLIAPIHandler contains the handlers for constant.Gemini CLI API endpoints.
// It holds a pool of clients to interact with the backend service.
type CLIAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewCLIAPIHandler creates a new constant.Gemini CLI API handlers instance.
// It takes an BaseAPIHandler instance as input and returns a CLIAPIHandler.
func NewCLIAPIHandler(apiHandlers *handlers.BaseAPIHandler) *CLIAPIHandler {
	return &CLIAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the type of this handler.
func (h *CLIAPIHandler) HandlerType() string {
	return constant.GeminiCLI
}

// Models returns a list of models supported by this handler.
func (h *CLIAPIHandler) Models() []map[string]any {
	return make([]map[string]any, 0)
}

// CLIHandler handles CLI-specific requests for constant.Gemini API operations.
// It restricts access to localhost only and routes requests to appropriate internal handlers.
func (h *CLIAPIHandler) CLIHandler(c *gin.Context) {
	remoteHost, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
	if remoteHost == "" {
		remoteHost = c.Request.RemoteAddr
	}
	if ip := net.ParseIP(remoteHost); ip == nil || !ip.IsLoopback() {
		c.JSON(
			http.StatusForbidden, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "CLI reply only allow local access",
					Type:    "forbidden",
				},
			},
		)
		return
	}

	rawJSON, _ := c.GetRawData()
	requestRawURI := c.Request.URL.Path

	if requestRawURI == "/v1internal:generateContent" {
		h.handleInternalGenerateContent(c, rawJSON)
	} else if requestRawURI == "/v1internal:streamGenerateContent" {
		h.handleInternalStreamGenerateContent(c, rawJSON)
	} else {
		reqBody := bytes.NewBuffer(rawJSON)
		req, err := http.NewRequest(
			"POST", fmt.Sprintf("https://cloudcode-pa.googleapis.com%s", c.Request.URL.RequestURI()), reqBody,
		)
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
		maps.Copy(req.Header, c.Request.Header)

		httpClient := util.SetProxy(h.Cfg, &http.Client{})

		resp, err := httpClient.Do(req)
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

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			defer func() {
				if err = resp.Body.Close(); err != nil {
					log.Printf("warn: failed to close response body: %v", err)
				}
			}()
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))

			c.JSON(
				http.StatusBadRequest, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: string(bodyBytes),
						Type:    "invalid_request_error",
					},
				},
			)
			return
		}

		defer func() {
			_ = resp.Body.Close()
		}()

		for key, value := range resp.Header {
			c.Header(key, value[0])
		}
		output, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		if err != nil {
			log.Errorf("Failed to read response body: %v", err)
			return
		}
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
		_, _ = c.Writer.Write(output)
		c.Set("API_RESPONSE", output)
	}
}

// handleInternalStreamGenerateContent handles streaming content generation requests.
// It sets up a server-sent event stream and forwards the request to the backend client.
// The function continuously proxies response chunks from the backend to the client.
func (h *CLIAPIHandler) handleInternalStreamGenerateContent(c *gin.Context, rawJSON []byte) {
	alt := h.GetAlt(c)

	if alt == "" {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
	}

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

	modelResult := gjson.GetBytes(rawJSON, "model")
	modelName := modelResult.String()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(
		cliCtx, h.HandlerType(), modelName, rawJSON, "",
	)
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	h.forwardCLIStream(c, flusher, "", func(err error) { cliCancel(err) }, dataChan, errChan)
}

// handleInternalGenerateContent handles non-streaming content generation requests.
// It sends a request to the backend client and proxies the entire response back to the client at once.
func (h *CLIAPIHandler) handleInternalGenerateContent(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")
	modelResult := gjson.GetBytes(rawJSON, "model")
	modelName := modelResult.String()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func (h *CLIAPIHandler) forwardCLIStream(
	c *gin.Context,
	flusher http.Flusher,
	alt string,
	cancel func(error),
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
) {
	forwardStream(
		h.BaseAPIHandler, c, flusher, alt, cancel, data, errs, func(chunk []byte) {
			if alt == "" {
				if bytes.Equal(chunk, []byte("data: [DONE]")) || bytes.Equal(chunk, []byte("[DONE]")) {
					return
				}

				if !bytes.HasPrefix(chunk, []byte("data:")) {
					_, _ = c.Writer.Write([]byte("data: "))
				}

				_, _ = c.Writer.Write(chunk)
				_, _ = c.Writer.Write([]byte("\n\n"))
			} else {
				_, _ = c.Writer.Write(chunk)
			}
		},
	)
}
