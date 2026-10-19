package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
)

// ModelExecutionRequest describes an internal model execution request.
type ModelExecutionRequest struct {
	EntryProtocol           string
	ExitProtocol            string
	Model                   string
	Stream                  bool
	Body                    []byte
	Headers                 http.Header
	Query                   url.Values
	Alt                     string
	SkipInterceptorPluginID string
	ForcedProvider          string
	AuthID                  string
	ProxyURL                string
	Path                    string
}

// ModelExecutionResponse describes a non-streaming internal model execution response.
type ModelExecutionResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ModelExecutionStream describes a streaming internal model execution response.
type ModelExecutionStream struct {
	StatusCode int
	Headers    http.Header
	Chunks     <-chan ModelExecutionChunk
}

// ModelExecutionChunk carries either a streaming payload or a terminal stream error.
type ModelExecutionChunk struct {
	Payload []byte
	Err     *ModelExecutionStreamError
}

// ModelExecutionStreamError carries a JSON-friendly terminal stream error.
type ModelExecutionStreamError struct {
	StatusCode int         `json:"status_code"`
	Message    string      `json:"message"`
	Headers    http.Header `json:"headers"`
}

// Error returns the stream error message or the HTTP status text.
func (e *ModelExecutionStreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.StatusCode)
}

type forcedModelProviderContextKey struct{}
type modelExecutionPathContextKey struct{}

func withModelExecutionPath(ctx context.Context, path string) context.Context {
	if path == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, modelExecutionPathContextKey{}, path)
}

func validModelExecutionPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	return err == nil && parsed.Host == "" && parsed.Path != ""
}

func withForcedModelProvider(ctx context.Context, provider string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, forcedModelProviderContextKey{}, strings.ToLower(strings.TrimSpace(provider)))
}

// ExecuteModel executes an internal non-streaming model request.
// Host model callbacks are non-recursive for their caller: when
// SkipInterceptorPluginID is set, that plugin's interceptors are skipped for the
// nested model execution while other plugins may still run.
func (h *BaseAPIHandler) ExecuteModel(ctx context.Context, req ModelExecutionRequest) (
	ModelExecutionResponse,
	*ErrorMessage,
) {
	if req.Stream {
		return ModelExecutionResponse{}, modelExecutionModeError("ExecuteModel requires Stream=false")
	}
	if errProxy := validateModelExecutionProxy(req.ProxyURL); errProxy != nil {
		return ModelExecutionResponse{}, errProxy
	}
	path := strings.TrimSpace(req.Path)
	if path != "" && !validModelExecutionPath(path) {
		return ModelExecutionResponse{}, modelExecutionModeError("invalid request path")
	}
	ctx = coreexecutor.WithoutStreamDeliveryAcknowledgement(ctx)
	ctx = withModelExecutionPath(ctx, path)
	ctx = withForcedModelProvider(ctx, req.ForcedProvider)
	ctx = WithPinnedAuthID(ctx, req.AuthID)
	ctx = coreexecutor.WithRequestProxyURL(ctx, req.ProxyURL)
	responseProtocol := modelExecutionResponseProtocol(req.EntryProtocol, req.ExitProtocol)
	body, headers, errMsg := h.executeNonStream(
		ctx, req.EntryProtocol, responseProtocol, req.Model, cloneBytes(req.Body), req.Alt,
		isModelExecutionImageProtocol(req.EntryProtocol) || isModelExecutionImageProtocol(req.ExitProtocol), true,
		modelExecutionHeaders(ctx, req.Headers), cloneURLValues(req.Query), req.SkipInterceptorPluginID,
		h.AuthManager.Execute,
	)
	if errMsg != nil {
		return ModelExecutionResponse{}, errMsg
	}
	return ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Body:       cloneBytes(body),
	}, nil
}

// ExecuteModelStream executes an internal streaming model request.
// Host model callbacks are non-recursive for their caller: when
// SkipInterceptorPluginID is set, that plugin's interceptors are skipped for the
// nested model execution while other plugins may still run.
func (h *BaseAPIHandler) ExecuteModelStream(ctx context.Context, req ModelExecutionRequest) (
	ModelExecutionStream,
	*ErrorMessage,
) {
	if !req.Stream {
		return ModelExecutionStream{}, modelExecutionModeError("ExecuteModelStream requires Stream=true")
	}
	if errProxy := validateModelExecutionProxy(req.ProxyURL); errProxy != nil {
		return ModelExecutionStream{}, errProxy
	}
	path := strings.TrimSpace(req.Path)
	if path != "" && !validModelExecutionPath(path) {
		return ModelExecutionStream{}, modelExecutionModeError("invalid request path")
	}
	ctx = coreexecutor.WithoutStreamDeliveryAcknowledgement(ctx)
	ctx = withModelExecutionPath(ctx, path)
	ctx = withForcedModelProvider(ctx, req.ForcedProvider)
	ctx = WithPinnedAuthID(ctx, req.AuthID)
	ctx = coreexecutor.WithRequestProxyURL(ctx, req.ProxyURL)
	responseProtocol := modelExecutionResponseProtocol(req.EntryProtocol, req.ExitProtocol)
	dataChan, headers, errChan := h.executeStreamWithAuthManager(
		ctx, req.EntryProtocol, responseProtocol, req.Model, cloneBytes(req.Body), req.Alt,
		isModelExecutionImageProtocol(req.EntryProtocol) || isModelExecutionImageProtocol(req.ExitProtocol), true,
		modelExecutionHeaders(ctx, req.Headers), cloneURLValues(req.Query), req.SkipInterceptorPluginID,
	)
	chunks, errMsg := prepareModelExecutionStream(ctx, dataChan, errChan)
	if errMsg != nil {
		return ModelExecutionStream{}, errMsg
	}
	return ModelExecutionStream{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Chunks:     chunks,
	}, nil
}

func modelExecutionModeError(message string) *ErrorMessage {
	return &ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(message)}
}

func isModelExecutionImageProtocol(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), "openai-image")
}

func validateModelExecutionProxy(raw string) *ErrorMessage {
	if strings.TrimSpace(raw) != "" && !proxyutil.ValidRequestProxy(raw) {
		return &ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New("invalid proxy_url")}
	}
	return nil
}

func modelExecutionResponseProtocol(entryProtocol, exitProtocol string) string {
	if exitProtocol == "" {
		return entryProtocol
	}
	return exitProtocol
}

func modelExecutionHeaders(ctx context.Context, headers http.Header) http.Header {
	if len(headers) > 0 {
		return cloneHeader(headers)
	}
	requestHeaders, _ := requestHeadersAndQuery(ctx)
	return requestHeaders
}

func cloneURLValues(src url.Values) url.Values {
	if src == nil {
		return nil
	}
	dst := make(url.Values, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func prepareModelExecutionStream(
	ctx context.Context,
	dataChan <-chan []byte,
	errChan <-chan *ErrorMessage,
) (<-chan ModelExecutionChunk, *ErrorMessage) {
	pending, nextDataChan, nextErrChan, errMsg := receiveInitialModelExecutionChunk(ctx, dataChan, errChan)
	if errMsg != nil {
		return nil, errMsg
	}
	return wrapModelExecutionChunks(ctx, nextDataChan, nextErrChan, pending), nil
}

func receiveInitialModelExecutionChunk(
	ctx context.Context,
	dataChan <-chan []byte,
	errChan <-chan *ErrorMessage,
) ([]ModelExecutionChunk, <-chan []byte, <-chan *ErrorMessage, *ErrorMessage) {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	for dataChan != nil || errChan != nil {
		select {
		case payload, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			return []ModelExecutionChunk{{Payload: cloneBytes(payload)}}, dataChan, errChan, nil
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errMsg != nil {
				return nil, dataChan, errChan, errMsg
			}
		case <-done:
			return nil, dataChan, errChan, nil
		}
	}
	return nil, dataChan, errChan, nil
}

func wrapModelExecutionChunks(
	ctx context.Context,
	dataChan <-chan []byte,
	errChan <-chan *ErrorMessage,
	pending []ModelExecutionChunk,
) <-chan ModelExecutionChunk {
	chunks := make(chan ModelExecutionChunk)
	go func() {
		defer close(chunks)
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		for _, chunk := range pending {
			if !sendModelExecutionChunk(ctx, chunks, chunk) {
				return
			}
		}
		for dataChan != nil || errChan != nil {
			select {
			case <-done:
				return
			case payload, ok := <-dataChan:
				if !ok {
					dataChan = nil
					continue
				}
				if !sendModelExecutionChunk(ctx, chunks, ModelExecutionChunk{Payload: cloneBytes(payload)}) {
					return
				}
			case errMsg, ok := <-errChan:
				if !ok {
					errChan = nil
					continue
				}
				if errMsg != nil {
					_ = sendModelExecutionChunk(
						ctx, chunks, ModelExecutionChunk{Err: modelExecutionStreamErrorFromMessage(errMsg)},
					)
					return
				}
			}
		}
	}()
	return chunks
}

func modelExecutionStreamErrorFromMessage(errMsg *ErrorMessage) *ModelExecutionStreamError {
	if errMsg == nil {
		return nil
	}
	message := ""
	if errMsg.Error != nil {
		message = errMsg.Error.Error()
	}
	return &ModelExecutionStreamError{
		StatusCode: errMsg.StatusCode,
		Message:    message,
		Headers:    cloneHeader(errMsg.Addon),
	}
}

func sendModelExecutionChunk(ctx context.Context, chunks chan<- ModelExecutionChunk, chunk ModelExecutionChunk) bool {
	if ctx == nil {
		chunks <- chunk
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case chunks <- chunk:
		return true
	}
}
