// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

type handlerContextKey struct{}

// ReadRequestBody returns the request body after applying supported Content-Encoding decoders.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	return ReadRequestBodyWithLimit(c, maxDecodedRequestBodyBytes)
}

func ReadRequestBodyWithLimit(c *gin.Context, maxDecodedBytes int64) ([]byte, error) {
	if c == nil || c.Request == nil {
		return nil, nil
	}
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	decoded, errDecode := DecodeRequestBodyWithLimit(raw, c.GetHeader("Content-Encoding"), maxDecodedBytes)
	if errDecode != nil {
		return nil, errDecode
	}
	c.Request.Header.Del("Content-Encoding")
	c.Request.ContentLength = int64(len(decoded))
	return decoded, nil
}

func DecodeRequestBody(raw []byte, contentEncoding string) ([]byte, error) {
	return DecodeRequestBodyWithLimit(raw, contentEncoding, maxDecodedRequestBodyBytes)
}

func DecodeRequestBodyWithLimit(raw []byte, contentEncoding string, maxDecodedBytes int64) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	if encoding == "" || encoding == "identity" {
		if maxDecodedBytes > 0 && int64(len(raw)) > maxDecodedBytes {
			return nil, fmt.Errorf("decoded request body exceeds %d bytes", maxDecodedBytes)
		}
		return raw, nil
	}
	if strings.Contains(encoding, ",") {
		parts := strings.Split(encoding, ",")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		decoded := raw
		var err error
		for _, part := range parts {
			decoded, err = DecodeRequestBodyWithLimit(decoded, part, maxDecodedBytes)
			if err != nil {
				return nil, err
			}
		}
		return decoded, nil
	}

	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gz, errGzip := gzip.NewReader(bytes.NewReader(raw))
		if errGzip != nil {
			return nil, errGzip
		}
		reader = gz
	case "deflate":
		zr, errZlib := zlib.NewReader(bytes.NewReader(raw))
		if errZlib != nil {
			return nil, errZlib
		}
		reader = zr
	case "zstd":
		zr, errZstd := zstd.NewReader(bytes.NewReader(raw))
		if errZstd != nil {
			return nil, errZstd
		}
		reader = zr.IOReadCloser()
	default:
		return nil, fmt.Errorf("unsupported content-encoding: %s", contentEncoding)
	}
	defer func() { _ = reader.Close() }()
	limit := maxDecodedBytes
	if limit <= 0 {
		limit = maxDecodedRequestBodyBytes
	}
	decoded, errRead := io.ReadAll(io.LimitReader(reader, limit+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("decoded request body exceeds %d bytes", limit)
	}
	return decoded, nil
}

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds  = 0
	defaultStreamingBootstrapRetries  = 0
	maxDecodedRequestBodyBytes        = 50 << 20
	maxStreamInterceptorHistoryChunks = 64
	maxStreamInterceptorHistoryBytes  = 1 << 20
)

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}

// PluginInterceptorHost applies plugin interceptors around handler execution.
type PluginInterceptorHost interface {
	InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse
	InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse
}

type pluginInterceptorSkipHost interface {
	InterceptRequestBeforeAuthExcept(
		context.Context,
		pluginapi.RequestInterceptRequest,
		string,
	) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuthExcept(
		context.Context,
		pluginapi.RequestInterceptRequest,
		string,
	) pluginapi.RequestInterceptResponse
	InterceptResponseExcept(
		context.Context,
		pluginapi.ResponseInterceptRequest,
		string,
	) pluginapi.ResponseInterceptResponse
	InterceptStreamChunkExcept(
		context.Context,
		pluginapi.StreamChunkInterceptRequest,
		string,
	) pluginapi.StreamChunkInterceptResponse
}

type streamInterceptorDetector interface {
	HasStreamInterceptors() bool
}

type requestInterceptorDetector interface {
	HasRequestInterceptors() bool
}

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// WithDisallowFreeAuth returns a child context that requests skipping known free-tier credentials.
func WithDisallowFreeAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disallowFreeAuthContextKey{}, true)
}

func disallowFreeAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(disallowFreeAuthContextKey{}).(bool)
	return v
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(
		ErrorResponse{
			Error: ErrorDetail{
				Message: errText,
				Type:    errType,
				Code:    code,
			},
		},
	)
	if err != nil {
		return fmt.Appendf(
			nil,
			`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText,
		)
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
// noinspection GoUnusedExportedFunction
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
// noinspection GoUnusedExportedFunction
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestHeadersAndQuery(ctx context.Context) (http.Header, url.Values) {
	if ctx == nil {
		return nil, nil
	}
	ginCtx, ok := util.GinContextValue(ctx).(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return nil, nil
	}
	var headers http.Header
	if ginCtx.Request.Header != nil {
		headers = ginCtx.Request.Header.Clone()
	}
	var query url.Values
	if ginCtx.Request.URL != nil {
		query = ginCtx.Request.URL.Query()
	}
	return headers, query
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only forward it when the client explicitly provides it.
	key := ""
	requestPath := ""
	if ctx != nil {
		if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			requestPath = strings.TrimSpace(ginCtx.FullPath())
			if requestPath == "" && ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
		}
	}

	meta := make(map[string]any)
	if key != "" {
		meta[idempotencyKeyMetadataKey] = key
	}
	if requestPath != "" {
		meta[coreexecutor.RequestPathMetadataKey] = requestPath
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	return meta
}

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
}

func setServiceTierMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	serviceTier := coreusage.DefaultServiceTier
	if node := gjson.GetBytes(rawJSON, "service_tier"); node.Exists() {
		if value := strings.TrimSpace(node.String()); value != "" {
			serviceTier = value
		}
	}
	meta[coreexecutor.ServiceTierMetadataKey] = serviceTier
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig

	// PluginHost optionally applies plugin interceptors around upstream execution.
	PluginHost PluginInterceptorHost
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	return &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) { h.Cfg = cfg }

func (h *BaseAPIHandler) requestLoggingEnabled() bool {
	return h != nil && h.Cfg != nil && h.Cfg.RequestLog
}

// SetPluginHost configures the optional plugin interceptor host.
func (h *BaseAPIHandler) SetPluginHost(host PluginInterceptorHost) {
	if h == nil {
		return
	}
	if isNilPluginInterceptorHost(host) {
		h.PluginHost = nil
		return
	}
	h.PluginHost = host
}

func isNilPluginInterceptorHost(host PluginInterceptorHost) bool {
	if host == nil {
		return true
	}
	value := reflect.ValueOf(host)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (h *BaseAPIHandler) FilterModelsForCurrentAPIKey(c *gin.Context, models []map[string]any) []map[string]any {
	if h == nil || h.Cfg == nil || len(models) == 0 {
		return models
	}
	apiKey := apiKeyFromGinContext(c)
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		modelName := modelNameFromMap(model)
		if modelName == "" {
			filtered = append(filtered, model)
			continue
		}
		if h.modelCatalogModelDisabled(modelName) {
			continue
		}
		if apiKey != "" && h.apiKeyBlocksModel(apiKey, modelName) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

func (h *BaseAPIHandler) modelCatalogModelDisabled(model string) bool {
	if h == nil || h.Cfg == nil {
		return false
	}
	for _, candidate := range h.modelAccessCandidates(model) {
		if !h.Cfg.ModelCatalogModelEnabled(candidate) {
			return true
		}
	}
	return false
}

func (h *BaseAPIHandler) apiKeyBlocksModel(apiKey, model string) bool {
	if h == nil || h.Cfg == nil {
		return false
	}
	for _, candidate := range h.modelAccessCandidates(model) {
		if h.Cfg.APIKeyBlocksModel(apiKey, candidate) {
			return true
		}
	}
	return false
}

func (h *BaseAPIHandler) modelAccessCandidates(model string) []string {
	candidates := make([]string, 0, 6)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		candidates = append(candidates, value)
	}
	add(model)
	addInfo := func(value string) {
		value = strings.TrimSpace(strings.TrimPrefix(value, "models/"))
		if value == "" {
			return
		}
		if info := registry.LookupModelInfo(value); info != nil {
			add(info.ID)
			add(info.CatalogName)
			add(info.Name)
		}
	}
	addInfo(model)
	parsed := thinking.ParseSuffix(strings.TrimSpace(strings.TrimPrefix(model, "models/")))
	if parsed.ModelName != "" {
		add(parsed.ModelName)
		addInfo(parsed.ModelName)
	}
	return candidates
}

func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := util.GinContextValue(ctx).(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	return apiKeyFromGinContext(ginCtx)
}

func apiKeyFromGinContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if raw, ok := c.Get("apiKey"); ok {
		switch value := raw.(type) {
		case string:
			return strings.TrimSpace(value)
		case fmt.Stringer:
			return strings.TrimSpace(value.String())
		default:
			return strings.TrimSpace(fmt.Sprintf("%v", value))
		}
	}
	return ""
}

func modelNameFromMap(model map[string]any) string {
	if model == nil {
		return ""
	}
	for _, key := range []string{"id", "name", "slug"} {
		if value, ok := model[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimPrefix(strings.TrimSpace(value), "models/")
		}
	}
	return ""
}

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(
	handler interfaces.APIHandler,
	c *gin.Context,
	ctx context.Context,
) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID := logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)

	endpoint := ""
	if c != nil && c.Request != nil {
		path := strings.TrimSpace(c.FullPath())
		if path == "" && c.Request.URL != nil {
			path = strings.TrimSpace(c.Request.URL.Path)
		}
		if path != "" {
			method := strings.TrimSpace(c.Request.Method)
			if method != "" {
				endpoint = method + " " + path
			} else {
				endpoint = path
			}
		}
	}
	if endpoint != "" {
		newCtx = logging.WithEndpoint(newCtx, endpoint)
	}
	newCtx = logging.WithResponseStatusHolder(newCtx)
	newCtx = logging.WithResponseHeadersHolder(newCtx)
	newCtx = logging.WithResponseTimingHolder(newCtx)
	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-newCtx.Done():
			}
		}()
	}
	newCtx = util.WithGinContext(newCtx, c)
	newCtx = context.WithValue(newCtx, handlerContextKey{}, handler)
	return newCtx, func(params ...any) {
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if h.requestLoggingEnabled() && len(params) == 1 {
			if captured, exists := c.Get(logging.APIResponseCapturedContextKey); exists {
				if capturedBool, ok := captured.(bool); ok && capturedBool {
					cancel()
					return
				}
			}
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Go(
		func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stopChan:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = c.Writer.Write([]byte("\n"))
					flusher.Flush()
				}
			}
		},
	)

	return func() {
		stopOnce.Do(
			func() {
				close(stopChan)
			},
		)
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
		if timelineValue, ok := c.Get(logging.RequestTimelineGinKey); ok {
			if timeline, okTimeline := timelineValue.(*logging.RequestTimeline); okTimeline && timeline != nil {
				timeline.AddOnce("upstream.response.done", "")
			}
		}
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeNonStream(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, nil, nil, "", h.AuthManager.Execute,
	)
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeNonStream(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, nil, nil, "", h.AuthManager.ExecuteCount,
	)
}

func (h *BaseAPIHandler) executeNonStream(
	ctx context.Context,
	handlerType, responseType, modelName string,
	rawJSON []byte,
	alt string,
	requestHeaders http.Header,
	requestQuery url.Values,
	skipPluginID string,
	execFn func(context.Context, []string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error),
) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	if h.modelCatalogModelDisabled(normalizedModel) {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusForbidden,
			Error:      fmt.Errorf("model %s is disabled", normalizedModel),
		}
	}
	if h.apiKeyBlocksModel(apiKeyFromContext(ctx), normalizedModel) {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusForbidden,
			Error:      fmt.Errorf("model %s is blocked for this API key", normalizedModel),
		}
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	headers, query := requestHeadersAndQuery(ctx)
	if len(requestHeaders) > 0 {
		headers = cloneHeader(requestHeaders)
	}
	if len(requestQuery) > 0 {
		query = cloneURLValues(requestQuery)
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		Headers:                     headers,
		Query:                       query,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		ResponseFormat:              sdktranslator.FromString(responseType),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, skipPluginID),
	}
	opts.Metadata = reqMeta
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, modelName, req, opts, skipPluginID)
	resp, err := execFn(ctx, providers, req, opts)
	if err != nil {
		status := http.StatusInternalServerError
		if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, nil, &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(
		ctx, responseType, normalizedModel, modelName, executedOpts, rawResponseHeaders, responseHeaders,
		executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, skipPluginID,
	)
	return body, responseHeaders, nil
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, handlerType, modelName, rawJSON, alt, nil, nil, "")
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(
	ctx context.Context,
	handlerType, responseType, modelName string,
	rawJSON []byte,
	alt string,
	requestHeaders http.Header,
	requestQuery url.Values,
	skipPluginID string,
) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	if h.modelCatalogModelDisabled(normalizedModel) {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{
			StatusCode: http.StatusForbidden,
			Error:      fmt.Errorf("model %s is disabled", normalizedModel),
		}
		close(errChan)
		return nil, nil, errChan
	}
	if h.apiKeyBlocksModel(apiKeyFromContext(ctx), normalizedModel) {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{
			StatusCode: http.StatusForbidden,
			Error:      fmt.Errorf("model %s is blocked for this API key", normalizedModel),
		}
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	headers, query := requestHeadersAndQuery(ctx)
	if len(requestHeaders) > 0 {
		headers = cloneHeader(requestHeaders)
	}
	if len(requestQuery) > 0 {
		query = cloneURLValues(requestQuery)
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      true,
		Alt:                         alt,
		Headers:                     headers,
		Query:                       query,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		ResponseFormat:              sdktranslator.FromString(responseType),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, skipPluginID),
	}
	opts.Metadata = reqMeta
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, modelName, req, opts, skipPluginID)
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		status := http.StatusInternalServerError
		if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		errChan <- &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
		close(errChan)
		return nil, nil, errChan
	}
	executedRequest := func() (coreexecutor.Request, coreexecutor.Options) {
		return afterAuthCapture.apply(req, opts)
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	upstreamHeaders := downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	streamHeaderInitialized := false
	streamHeadersCommitted := false

	applyStreamHeaders := func(headers http.Header, clear []string) {
		rawStreamHeaders = mergeRequestInterceptorHeaders(rawStreamHeaders, headers, clear)
		if streamHeadersCommitted || upstreamHeaders == nil {
			return
		}
		replaceHeader(
			upstreamHeaders,
			downstreamHeadersAfterInterceptors(baseStreamHeaders, rawStreamHeaders, passthroughHeadersEnabled),
		)
	}

	applyStreamHeaderInit := func() {
		if !streamInterceptorsActive || streamHeaderInitialized {
			return
		}
		executedReq, executedOpts := executedRequest()
		intercepted := interceptStreamChunk(
			ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
				SourceFormat:    responseType,
				Model:           normalizedModel,
				RequestedModel:  modelName,
				RequestHeaders:  cloneHeader(executedOpts.Headers),
				ResponseHeaders: cloneHeader(rawStreamHeaders),
				OriginalRequest: cloneBytes(executedOpts.OriginalRequest),
				RequestBody:     cloneBytes(executedReq.Payload),
				ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
				Metadata:        executedOpts.Metadata,
			}, skipPluginID,
		)
		applyStreamHeaders(intercepted.Headers, intercepted.ClearHeaders)
		streamHeaderInitialized = true
	}

	pendingChunks := make([]coreexecutor.StreamChunk, 0, 1)
	streamClosedBeforeRead := false
	streamCanceledBeforeRead := false
	readInitialStreamChunks := func() {
		for {
			var chunk coreexecutor.StreamChunk
			var ok bool
			if ctx != nil {
				select {
				case <-ctx.Done():
					streamCanceledBeforeRead = true
					return
				case chunk, ok = <-chunks:
				}
			} else {
				chunk, ok = <-chunks
			}
			if !ok {
				streamClosedBeforeRead = true
				applyStreamHeaderInit()
				return
			}
			pendingChunks = append(pendingChunks, chunk)
			if chunk.Err != nil {
				return
			}
			if len(chunk.Payload) > 0 {
				applyStreamHeaderInit()
				return
			}
		}
	}
	readInitialStreamChunks()

	go func() {
		defer close(dataChan)
		defer close(errChan)
		if streamCanceledBeforeRead {
			return
		}
		sentPayload := false
		bootstrapRetries := 0
		chunkIndex := 0
		var historyChunks [][]byte
		maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		}

		bootstrapEligible := func(err error) bool {
			status := statusFromError(err)
			if status == 0 {
				return true
			}
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
				http.StatusRequestTimeout, http.StatusTooManyRequests:
				return true
			default:
				return status >= http.StatusInternalServerError
			}
		}

	outer:
		for {
			for {
				chunk, ok, canceled := nextStreamChunk(ctx, &pendingChunks, &streamClosedBeforeRead, chunks)
				if canceled {
					return
				}
				if !ok {
					applyStreamHeaderInit()
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
							if retryErr == nil {
								rawStreamHeaders = cloneHeader(retryResult.Headers)
								baseStreamHeaders = cloneHeader(retryResult.Headers)
								if upstreamHeaders != nil {
									replaceHeader(
										upstreamHeaders,
										downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled),
									)
								}
								streamHeaderInitialized = false
								streamHeadersCommitted = false
								pendingChunks = nil
								streamClosedBeforeRead = false
								chunks = retryResult.Chunks
								continue outer
							}
							streamErr = retryErr
						}
					}

					status := http.StatusInternalServerError
					if se, ok := streamErr.(interface{ StatusCode() int }); ok && se != nil {
						if code := se.StatusCode(); code > 0 {
							status = code
						}
					}
					var addon http.Header
					if he, ok := streamErr.(interface{ Headers() http.Header }); ok && he != nil {
						if hdr := he.Headers(); hdr != nil {
							addon = hdr.Clone()
						}
					}
					_ = sendErr(&interfaces.ErrorMessage{StatusCode: status, Error: streamErr, Addon: addon})
					return
				}
				if len(chunk.Payload) > 0 {
					applyStreamHeaderInit()
					payload := cloneBytes(chunk.Payload)
					if streamInterceptorsActive {
						executedReq, executedOpts := executedRequest()
						intercepted := interceptStreamChunk(
							ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
								SourceFormat:    responseType,
								Model:           normalizedModel,
								RequestedModel:  modelName,
								RequestHeaders:  cloneHeader(executedOpts.Headers),
								ResponseHeaders: cloneHeader(rawStreamHeaders),
								OriginalRequest: cloneBytes(executedOpts.OriginalRequest),
								RequestBody:     cloneBytes(executedReq.Payload),
								Body:            payload,
								HistoryChunks:   cloneByteSlices(historyChunks),
								ChunkIndex:      chunkIndex,
								Metadata:        executedOpts.Metadata,
							}, skipPluginID,
						)
						applyStreamHeaders(intercepted.Headers, intercepted.ClearHeaders)
						if len(intercepted.Body) > 0 {
							payload = cloneBytes(intercepted.Body)
						}
						chunkIndex++
						if intercepted.DropChunk {
							continue
						}
					} else {
						chunkIndex++
					}
					if responseType == "openai-response" {
						if errValidate := validateSSEDataJSON(payload); errValidate != nil {
							_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate})
							return
						}
					}
					sentPayload = true
					streamHeadersCommitted = true
					if okSendData := sendData(payload); !okSendData {
						return
					}
					if streamInterceptorsActive {
						historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
					}
				}
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

func cloneByteSlices(src [][]byte) [][]byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([][]byte, 0, len(src))
	for _, item := range src {
		dst = append(dst, cloneBytes(item))
	}
	return dst
}

func nextStreamChunk(
	ctx context.Context,
	pending *[]coreexecutor.StreamChunk,
	closed *bool,
	chunks <-chan coreexecutor.StreamChunk,
) (coreexecutor.StreamChunk, bool, bool) {
	if pending != nil && len(*pending) > 0 {
		chunk := (*pending)[0]
		(*pending)[0] = coreexecutor.StreamChunk{}
		*pending = (*pending)[1:]
		return chunk, true, false
	}
	if closed != nil && *closed {
		return coreexecutor.StreamChunk{}, false, false
	}
	var chunk coreexecutor.StreamChunk
	var ok bool
	if ctx != nil {
		select {
		case <-ctx.Done():
			return coreexecutor.StreamChunk{}, false, true
		case chunk, ok = <-chunks:
		}
	} else {
		chunk, ok = <-chunks
	}
	if !ok && closed != nil {
		*closed = true
	}
	return chunk, ok, false
}

func appendStreamInterceptorHistory(history [][]byte, chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return history
	}
	history = append(history, cloneBytes(chunk))
	for len(history) > maxStreamInterceptorHistoryChunks || byteSlicesSize(history) > maxStreamInterceptorHistoryBytes {
		history[0] = nil
		history = history[1:]
	}
	if len(history) == 0 {
		return nil
	}
	return history
}

func byteSlicesSize(items [][]byte) int {
	total := 0
	for _, item := range items {
		total += len(item)
	}
	return total
}

func downstreamHeadersFromExecutor(headers http.Header, passthrough bool) http.Header {
	if !passthrough {
		return nil
	}
	return FilterUpstreamHeaders(headers)
}

func downstreamHeadersAfterInterceptors(baseRaw, finalRaw http.Header, passthrough bool) http.Header {
	if passthrough {
		return FilterUpstreamHeaders(finalRaw)
	}
	return FilterUpstreamHeaders(diffHeaders(baseRaw, finalRaw))
}

func diffHeaders(base, next http.Header) http.Header {
	if len(next) == 0 {
		return nil
	}
	baseValues := make(map[string][]string, len(base))
	for key, values := range base {
		baseValues[http.CanonicalHeaderKey(key)] = values
	}
	out := make(http.Header)
	for key, values := range next {
		canonicalKey := http.CanonicalHeaderKey(key)
		if stringSlicesEqual(baseValues[canonicalKey], values) {
			continue
		}
		out[canonicalKey] = append([]string(nil), values...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (h *BaseAPIHandler) interceptorHost() PluginInterceptorHost {
	if h == nil {
		return nil
	}
	return h.PluginHost
}

func streamInterceptorsEnabled(host PluginInterceptorHost) bool {
	if host == nil {
		return false
	}
	if detector, ok := host.(streamInterceptorDetector); ok {
		return detector.HasStreamInterceptors()
	}
	return true
}

func requestInterceptorsEnabled(host PluginInterceptorHost) bool {
	if host == nil {
		return false
	}
	if detector, ok := host.(requestInterceptorDetector); ok {
		return detector.HasRequestInterceptors()
	}
	return true
}

type requestAfterAuthCapture struct {
	mu   sync.Mutex
	set  bool
	req  coreexecutor.Request
	opts coreexecutor.Options
}

func (c *requestAfterAuthCapture) record(req coreexecutor.Request, opts coreexecutor.Options) {
	if c == nil {
		return
	}
	req.Payload = cloneBytes(req.Payload)
	opts.Headers = cloneHeader(opts.Headers)
	opts.OriginalRequest = cloneBytes(opts.OriginalRequest)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = true
	c.req = req
	c.opts = opts
}

func (c *requestAfterAuthCapture) apply(req coreexecutor.Request, opts coreexecutor.Options) (
	coreexecutor.Request,
	coreexecutor.Options,
) {
	if c == nil {
		return req, opts
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set {
		return req, opts
	}
	return c.req, c.opts
}

func mergeRequestInterceptorHeaders(current, updates http.Header, clear []string) http.Header {
	if updates == nil && len(clear) == 0 {
		return cloneHeader(current)
	}
	out := cloneHeader(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(http.Header)
	}
	for _, key := range clear {
		out.Del(key)
	}
	for key, values := range updates {
		out.Del(key)
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func interceptRequestBeforeAuth(
	ctx context.Context,
	host PluginInterceptorHost,
	req pluginapi.RequestInterceptRequest,
	skipPluginID string,
) pluginapi.RequestInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptRequestBeforeAuthExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptRequestBeforeAuth(ctx, req)
}

func interceptRequestAfterAuth(
	ctx context.Context,
	host PluginInterceptorHost,
	req pluginapi.RequestInterceptRequest,
	skipPluginID string,
) pluginapi.RequestInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptRequestAfterAuthExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptRequestAfterAuth(ctx, req)
}

func interceptResponse(
	ctx context.Context,
	host PluginInterceptorHost,
	req pluginapi.ResponseInterceptRequest,
	skipPluginID string,
) pluginapi.ResponseInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptResponseExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptResponse(ctx, req)
}

func interceptStreamChunk(
	ctx context.Context,
	host PluginInterceptorHost,
	req pluginapi.StreamChunkInterceptRequest,
	skipPluginID string,
) pluginapi.StreamChunkInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptStreamChunkExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptStreamChunk(ctx, req)
}

func (h *BaseAPIHandler) applyRequestInterceptorsBeforeAuth(
	ctx context.Context,
	handlerType, requestedModel string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	skipPluginID string,
) (coreexecutor.Request, coreexecutor.Options) {
	host := h.interceptorHost()
	if !requestInterceptorsEnabled(host) {
		return req, opts
	}
	resp := interceptRequestBeforeAuth(
		ctx, host, pluginapi.RequestInterceptRequest{
			SourceFormat:   handlerType,
			Model:          req.Model,
			RequestedModel: requestedModel,
			Stream:         opts.Stream,
			Headers:        cloneHeader(opts.Headers),
			Body:           cloneBytes(req.Payload),
			Metadata:       opts.Metadata,
		}, skipPluginID,
	)
	opts.Headers = mergeRequestInterceptorHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = cloneBytes(resp.Body)
		opts.OriginalRequest = cloneBytes(resp.Body)
	}
	return req, opts
}

func (h *BaseAPIHandler) requestAfterAuthInterceptor(
	capture *requestAfterAuthCapture,
	skipPluginID string,
) coreexecutor.RequestAfterAuthInterceptor {
	if !requestInterceptorsEnabled(h.interceptorHost()) {
		return nil
	}
	return func(
		ctx context.Context,
		req coreexecutor.RequestAfterAuthInterceptRequest,
	) coreexecutor.RequestAfterAuthInterceptResponse {
		resp := h.applyRequestInterceptorsAfterAuth(ctx, req, skipPluginID)
		capturedReq := req.Request
		capturedOpts := req.Options
		if resp.Handled {
			capturedReq = resp.Request
			capturedOpts = resp.Options
		}
		if capture != nil {
			capture.record(capturedReq, capturedOpts)
		}
		return resp
	}
}

func (h *BaseAPIHandler) applyRequestInterceptorsAfterAuth(
	ctx context.Context,
	req coreexecutor.RequestAfterAuthInterceptRequest,
	skipPluginID string,
) coreexecutor.RequestAfterAuthInterceptResponse {
	host := h.interceptorHost()
	if !requestInterceptorsEnabled(host) {
		return coreexecutor.RequestAfterAuthInterceptResponse{}
	}
	resp := interceptRequestAfterAuth(
		ctx, host, pluginapi.RequestInterceptRequest{
			SourceFormat:   req.SourceFormat.String(),
			ToFormat:       req.ToFormat.String(),
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			Headers:        cloneHeader(req.Options.Headers),
			Body:           cloneBytes(req.Request.Payload),
			Metadata:       req.Options.Metadata,
		}, skipPluginID,
	)
	nextReq := req.Request
	nextOpts := req.Options
	nextOpts.Headers = mergeRequestInterceptorHeaders(nextOpts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		nextReq.Payload = cloneBytes(resp.Body)
		nextOpts.OriginalRequest = cloneBytes(resp.Body)
	}
	return coreexecutor.RequestAfterAuthInterceptResponse{Request: nextReq, Options: nextOpts, Handled: true}
}

func (h *BaseAPIHandler) applyResponseInterceptors(
	ctx context.Context,
	handlerType, normalizedModel, requestedModel string,
	opts coreexecutor.Options,
	rawResponseHeaders, responseHeaders http.Header,
	originalRequest, requestBody, body []byte,
	statusCode int,
	skipPluginID string,
) ([]byte, http.Header) {
	host := h.interceptorHost()
	if host == nil {
		return body, responseHeaders
	}
	resp := interceptResponse(
		ctx, host, pluginapi.ResponseInterceptRequest{
			SourceFormat:    handlerType,
			Model:           normalizedModel,
			RequestedModel:  requestedModel,
			Stream:          false,
			RequestHeaders:  cloneHeader(opts.Headers),
			ResponseHeaders: cloneHeader(rawResponseHeaders),
			OriginalRequest: cloneBytes(originalRequest),
			RequestBody:     cloneBytes(requestBody),
			Body:            cloneBytes(body),
			StatusCode:      statusCode,
			Metadata:        opts.Metadata,
		}, skipPluginID,
	)
	responseHeaders = downstreamHeadersAfterInterceptors(
		rawResponseHeaders,
		mergeRequestInterceptorHeaders(rawResponseHeaders, resp.Headers, resp.ClearHeaders),
		PassthroughHeadersEnabled(h.Cfg),
	)
	if len(resp.Body) > 0 {
		body = cloneBytes(resp.Body)
	}
	return body, responseHeaders
}

func validateSSEDataJSON(chunk []byte) error {
	for line := range bytes.SplitSeq(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		const previewLimit = 512
		preview := data
		if len(preview) > previewLimit {
			preview = preview[:previewLimit]
		}
		return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(data), preview)
	}
	return nil
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

func supportsContext1M(modelName string, providers []string) bool {
	baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	if baseModel == "" {
		return false
	}
	for _, provider := range providers {
		info := registry.LookupModelInfo(baseModel, strings.TrimSpace(provider))
		if info != nil {
			return info.ContextLength >= 1_000_000
		}
	}
	if info := registry.LookupStaticModelInfo(baseModel); info != nil {
		return info.ContextLength >= 1_000_000
	}
	return false
}

func requiresContext1MCapabilityValidation(providers []string) bool {
	return len(providers) > 0
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (
	providers []string,
	normalizedModel string,
	err *interfaces.ErrorMessage,
) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = util.ResolveAutoModel(modelName)
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)

	providers = util.GetProviderName(baseModel)
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}

	if len(providers) == 0 {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName),
		}
	}
	if thinking.HasContext1MTag(resolvedModelName) && requiresContext1MCapabilityValidation(providers) &&
		!supportsContext1M(resolvedModelName, providers) {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("model %s does not support [1m] context", modelName),
		}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.Cfg) {
		for key, values := range msg.Addon {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	body := BuildErrorResponseBody(status, errText)
	if h.requestLoggingEnabled() {
		captured, existsCaptured := c.Get(logging.APIResponseCapturedContextKey)
		if !existsCaptured || captured != true {
			var previous []byte
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
					previous = existingBytes
				}
			}
			appendAPIResponse(c, body)
			trimmedErrText := strings.TrimSpace(errText)
			trimmedBody := bytes.TrimSpace(body)
			if len(previous) > 0 {
				if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
					(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
					c.Set("API_RESPONSE", previous)
				}
			}
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.requestLoggingEnabled() {
		if ginContext, ok := util.GinContextValue(ctx).(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...any)
