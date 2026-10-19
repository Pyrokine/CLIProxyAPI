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
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/httpwire"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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
	if maxDecodedBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDecodedBytes)
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

	limit := maxDecodedBytes
	if limit <= 0 {
		limit = maxDecodedRequestBodyBytes
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
		zr, errZstd := zstd.NewReader(
			bytes.NewReader(raw),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(uint64(limit)),
			zstd.WithDecoderMaxMemory(uint64(limit)),
		)
		if errZstd != nil {
			return nil, errZstd
		}
		reader = zr.IOReadCloser()
	default:
		return nil, fmt.Errorf("unsupported content-encoding: %s", contentEncoding)
	}
	defer func() { _ = reader.Close() }()
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
	defaultStreamDeliveryTimeout      = 30 * time.Second
	maxDecodedRequestBodyBytes        = 50 << 20
	maxStreamInterceptorHistoryChunks = 64
	maxStreamInterceptorHistoryBytes  = 1 << 20
)

type pinnedAuthContextKey struct{}
type pinSelectedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type selectedAuthInfoCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}
type requireResponsesContinuationContextKey struct{}

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

type ImageArtifactRecord struct {
	ID       string
	SHA256   string
	MIMEType string
	Size     int64
}

type ImageArtifactStore interface {
	PutBytes(context.Context, []byte, string) (ImageArtifactRecord, error)
	PutDataURL(context.Context, string) (ImageArtifactRecord, error)
	PutRemoteImage(context.Context, string) (ImageArtifactRecord, []byte, error)
}

type ImageArtifactURLBuilder interface {
	ImageArtifactContentURL(*gin.Context, ImageArtifactRecord) (string, error)
}

type streamInterceptorDetector interface {
	HasStreamInterceptors() bool
}

type requestInterceptorDetector interface {
	HasRequestInterceptors() bool
}

type webSocketResponseObserverDetector interface {
	HasWebSocketResponseObservers() bool
}

type webSocketResponseObserverHost interface {
	ObserveWebSocketResponseEvent(context.Context, pluginapi.WebSocketResponseEvent)
}

type webSocketResponseObserverSkipHost interface {
	ObserveWebSocketResponseEventExcept(context.Context, pluginapi.WebSocketResponseEvent, string)
}

type webSocketResponseCallbackHost interface {
	OpenWebSocketResponseCallback(context.Context, string) (pluginapi.WebSocketResponseCallback, func())
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

// WithSelectedAuthInfoCallback returns a child context that receives non-secret selected auth details.
func WithSelectedAuthInfoCallback(ctx context.Context, callback func(coreexecutor.SelectedAuthInfo)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthInfoCallbackContextKey{}, callback)
}

// WithPinSelectedAuth locks retries to the first auth selected for this request.
func WithPinSelectedAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinSelectedAuthContextKey{}, true)
}

// WithRequireResponsesContinuation restricts auth selection to native continuation owners.
func WithRequireResponsesContinuation(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requireResponsesContinuationContextKey{}, true)
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
// Valid JSON error text retains its payload and is compacted for single-line SSE data.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(trimmed)); err == nil {
			return compact.Bytes()
		}
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
	case http.StatusRequestTimeout:
		errType = "server_error"
		code = "request_timeout"
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

func extractSessionIDsFromRequest(request *http.Request) (string, string) {
	if request == nil {
		return "", ""
	}
	if info, ok := coresession.ExtractSessionInfo(request.Header, nil, nil); ok {
		return info.SessionID, info.ParentSessionID
	}
	return "", ""
}

// EnrichContextWithSessionHierarchy records the session hierarchy extracted from the current request state.
func EnrichContextWithSessionHierarchy(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	metadata map[string]any,
) context.Context {
	ctx = enrichContextWithSessionHierarchy(ctx, headers, payload, metadata)
	if util.HasExplicitSessionID(ctx) {
		return util.WithSessionID(ctx, logging.GetClientRequestMetadata(ctx).SessionID)
	}
	return ctx
}

func enrichContextWithSessionHierarchy(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	metadata map[string]any,
) context.Context {
	clientMetadata := logging.GetClientRequestMetadata(ctx)
	if info, ok := coresession.ExtractSessionInfo(headers, payload, metadata); ok {
		clientMetadata.SessionID = info.SessionID
		clientMetadata.ParentSessionID = info.ParentSessionID
		clientMetadata.NodeKind = info.NodeKind
		clientMetadata.IsFork = info.IsFork
		clientMetadata.IsCompaction = info.IsCompaction
		if clientMetadata.SessionID == clientMetadata.ParentSessionID {
			clientMetadata.ParentSessionID = ""
		}
		ctx = logging.WithClientRequestMetadata(ctx, clientMetadata)
		return util.WithInferredSessionID(ctx, clientMetadata.SessionID)
	}
	if clientMetadata.SessionID != "" || clientMetadata.ParentSessionID != "" || clientMetadata.NodeKind != "" ||
		clientMetadata.IsFork || clientMetadata.IsCompaction || util.SessionIDFromContext(ctx) != "" {
		clientMetadata.SessionID = ""
		clientMetadata.ParentSessionID = ""
		clientMetadata.NodeKind = ""
		clientMetadata.IsFork = false
		clientMetadata.IsCompaction = false
		ctx = logging.WithClientRequestMetadata(ctx, clientMetadata)
		return util.WithInferredSessionID(ctx, "")
	}
	return ctx
}

func pluginExecutionMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		switch key {
		case coreexecutor.SelectedAuthCallbackMetadataKey,
			coreexecutor.SelectedAuthInfoCallbackMetadataKey,
			coreexecutor.SelectedAuthIndexCallbackMetadataKey,
			coreexecutor.RouteTraceCallbackMetadataKey,
			coreexecutor.RouteTraceTruncatedCallbackMetadataKey:
			continue
		}
		if strings.HasPrefix(key, "__route_trace_") {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only forward it when the client explicitly provides it.
	key := ""
	requestPath := ""
	var ginCtx *gin.Context
	if ctx != nil {
		if currentGinCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && currentGinCtx != nil && currentGinCtx.Request != nil {
			ginCtx = currentGinCtx
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			requestPath = strings.TrimSpace(ginCtx.FullPath())
			if requestPath == "" && ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
		}
		if path, _ := ctx.Value(modelExecutionPathContextKey{}).(string); path != "" {
			requestPath = path
		}
	}

	meta := make(map[string]any)
	meta[coreexecutor.ServiceTierMetadataKey] = coreusage.ServiceTierFromContext(ctx)
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
	if selectedInfoCallback := selectedAuthInfoCallbackFromContext(ctx); selectedInfoCallback != nil {
		meta[coreexecutor.SelectedAuthInfoCallbackMetadataKey] = selectedInfoCallback
	}
	if ginCtx != nil {
		if routeTraceCallback := logging.GinRouteTraceCallback(ginCtx); routeTraceCallback != nil {
			meta[coreexecutor.RouteTraceCallbackMetadataKey] = routeTraceCallback
		}
		if truncatedCallback := logging.GinRouteTraceTruncatedCallback(ginCtx); truncatedCallback != nil {
			meta[coreexecutor.RouteTraceTruncatedCallbackMetadataKey] = truncatedCallback
		}
		if !websocket.IsWebSocketUpgrade(ginCtx.Request) {
			if traceCallback := logging.GinCPATraceIDCallback(ginCtx); traceCallback != nil {
				meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey] = traceCallback
			}
		}
	}
	if pinSelectedAuthFromContext(ctx) {
		meta[coreexecutor.PinSelectedAuthMetadataKey] = true
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if callerScope := requestCallerScope(ginCtx); callerScope != "" {
		meta[coreexecutor.CallerScopeMetadataKey] = callerScope
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	if requireResponsesContinuationFromContext(ctx) {
		meta[coreexecutor.RequireResponsesContinuationMetadataKey] = true
	}
	return meta
}

func requestCallerScope(ginCtx *gin.Context) string {
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get("apiKey")
	if !exists || value == nil {
		return ""
	}
	return coresession.CallerScope(fmt.Sprint(value))
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
	switch value := meta[coreexecutor.ServiceTierMetadataKey].(type) {
	case string:
		if value = strings.TrimSpace(value); value != "" {
			serviceTier = value
		}
	case []byte:
		if value := strings.TrimSpace(string(value)); value != "" {
			serviceTier = value
		}
	}
	if node := gjson.GetBytes(rawJSON, "service_tier"); node.Type == gjson.String {
		if value := strings.TrimSpace(node.String()); value != "" {
			serviceTier = value
		}
	}
	meta[coreexecutor.ServiceTierMetadataKey] = serviceTier
}

func setGenerateMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	generate := true
	if node := gjson.GetBytes(rawJSON, "generate"); node.Exists() && node.IsBool() && !node.Bool() {
		generate = false
	}
	meta[coreexecutor.GenerateMetadataKey] = generate
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

func selectedAuthInfoCallbackFromContext(ctx context.Context) func(coreexecutor.SelectedAuthInfo) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthInfoCallbackContextKey{})
	if callback, ok := raw.(func(coreexecutor.SelectedAuthInfo)); ok && callback != nil {
		return callback
	}
	return nil
}

func requireResponsesContinuationFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(requireResponsesContinuationContextKey{}).(bool)
	return value
}

func pinSelectedAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(pinSelectedAuthContextKey{}).(bool)
	return value
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
	cfgMu sync.RWMutex
	Cfg   *config.SDKConfig

	// PluginHost optionally applies plugin interceptors around upstream execution.
	PluginHost PluginInterceptorHost

	ImageArtifactStore      ImageArtifactStore
	ImageArtifactURLBuilder ImageArtifactURLBuilder
	TranslatorPluginHooks   sdktranslator.PluginHooks
	UsageManager            *coreusage.Manager

	apiKeyPolicyMu    sync.RWMutex
	apiKeyPolicyStore coreusage.APIKeyReservationStore
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
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.cfgMu.Lock()
	h.Cfg = cfg
	h.cfgMu.Unlock()
}

func (h *BaseAPIHandler) ConfigSnapshot() *config.SDKConfig {
	if h == nil {
		return nil
	}
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.Cfg
}

func (h *BaseAPIHandler) requestLoggingEnabled() bool {
	cfg := h.ConfigSnapshot()
	return cfg != nil && cfg.RequestLog
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

func (h *BaseAPIHandler) SetImageArtifactStore(store ImageArtifactStore) {
	if h == nil {
		return
	}
	h.ImageArtifactStore = store
}

func (h *BaseAPIHandler) SetImageArtifactURLBuilder(builder ImageArtifactURLBuilder) {
	if h == nil {
		return
	}
	h.ImageArtifactURLBuilder = builder
}

// SetUsageManager assigns the service-owned usage dispatcher.
func (h *BaseAPIHandler) SetUsageManager(manager *coreusage.Manager) {
	if h == nil {
		return
	}
	h.UsageManager = manager
}

func (h *BaseAPIHandler) SetAPIKeyPolicyStore(store coreusage.APIKeyReservationStore) {
	if h == nil {
		return
	}
	h.apiKeyPolicyMu.Lock()
	h.apiKeyPolicyStore = store
	h.apiKeyPolicyMu.Unlock()
}

func (h *BaseAPIHandler) apiKeyPolicyStoreSnapshot() coreusage.APIKeyReservationStore {
	if h == nil {
		return nil
	}
	h.apiKeyPolicyMu.RLock()
	defer h.apiKeyPolicyMu.RUnlock()
	return h.apiKeyPolicyStore
}

// SetTranslatorPluginHooks assigns translator hooks owned by this server.
func (h *BaseAPIHandler) SetTranslatorPluginHooks(hooks sdktranslator.PluginHooks) {
	if h == nil {
		return
	}
	h.TranslatorPluginHooks = hooks
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
	if h == nil || h.ConfigSnapshot() == nil || len(models) == 0 {
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

// ModelDetailIDContextKey selects a model from the final, plugin-filtered catalog.
const ModelDetailIDContextKey = "cliproxy.model_detail_id"

// WriteModelListResponse applies response interceptors to a successful model catalog response.
func (h *BaseAPIHandler) WriteModelListResponse(c *gin.Context, sourceFormat string, payload any) {
	if c == nil {
		return
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: ErrorDetail{
			Message: "Failed to encode model catalog",
			Type:    "server_error",
		}})
		return
	}
	h.WriteModelListResponseBody(c, sourceFormat, body)
}

// WriteModelListResponseBody applies response interceptors to a pre-encoded JSON model catalog.
func (h *BaseAPIHandler) WriteModelListResponseBody(c *gin.Context, sourceFormat string, body []byte) {
	if c == nil {
		return
	}
	ctx := context.Background()
	var requestHeaders http.Header
	if c.Request != nil {
		ctx = c.Request.Context()
		requestHeaders = c.Request.Header
	}
	requestID := logging.GetRequestID(ctx)
	var lifecycle *requestLifecycleTracker
	if _, detail := c.Get(ModelDetailIDContextKey); detail {
		lifecycle = h.newRequestLifecycleTracker(ctx, requestID, sourceFormat, "", "", false, nil, "")
		requestID = lifecycle.requestID()
	}
	body, responseHeaders := h.applyResponseInterceptors(
		ctx,
		sourceFormat,
		"",
		"",
		coreexecutor.Options{Headers: cloneHeader(requestHeaders)},
		nil,
		nil,
		nil,
		nil,
		body,
		http.StatusOK,
		requestID,
		"",
		false,
	)
	WriteUpstreamHeaders(c.Writer.Header(), responseHeaders)
	if modelID, detail := c.Get(ModelDetailIDContextKey); detail {
		c.Writer.Header().Del("Content-Length")
		var catalog struct {
			Data   []json.RawMessage `json:"data"`
			Models []json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(body, &catalog); err != nil {
			lifecycle.complete(pluginapi.RequestCompletionFailed, http.StatusBadGateway, err)
			c.JSON(http.StatusBadGateway, ErrorResponse{Error: ErrorDetail{Message: "Invalid model catalog", Type: "api_error"}})
			return
		}
		var selected json.RawMessage
		for _, model := range append(catalog.Data, catalog.Models...) {
			var item struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			}
			if err := json.Unmarshal(model, &item); err != nil {
				continue
			}
			if item.ID == "" {
				item.ID = item.Slug
			}
			if item.ID != "" && item.ID == modelID {
				selected = model
				break
			}
		}
		if selected == nil {
			lifecycle.complete(pluginapi.RequestCompletionFailed, http.StatusNotFound, nil)
			c.JSON(http.StatusNotFound, ErrorResponse{Error: ErrorDetail{
				Message: "Model not found", Type: "invalid_request_error", Code: "model_not_found",
			}})
			return
		}
		body = selected
	}
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (h *BaseAPIHandler) modelCatalogModelDisabled(model string) bool {
	cfg := h.ConfigSnapshot()
	if cfg == nil {
		return false
	}
	for _, candidate := range h.modelAccessCandidates(model) {
		if !cfg.ModelCatalogModelEnabled(candidate) {
			return true
		}
	}
	return false
}

func (h *BaseAPIHandler) apiKeyBlocksModel(apiKey, model string) bool {
	cfg := h.ConfigSnapshot()
	if cfg == nil {
		return false
	}
	for _, candidate := range h.modelAccessCandidates(model) {
		if cfg.APIKeyBlocksModel(apiKey, candidate) {
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
	handler APIHandler,
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
	if requestCtx != nil {
		if outcomeState := logging.RequestOutcomeStateFromContext(requestCtx); outcomeState != nil {
			parentCtx = logging.WithRequestOutcomeState(parentCtx, outcomeState)
		}
	}
	newCtx, cancel := context.WithCancelCause(parentCtx)

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
	if c != nil && c.Request != nil {
		clientMetadata := logging.ExtractClientRequestMetadata(c.Request)
		clientMetadata.SessionID, clientMetadata.ParentSessionID = extractSessionIDsFromRequest(c.Request)
		newCtx = logging.WithClientRequestMetadata(newCtx, clientMetadata)
		newCtx = util.WithInferredSessionID(newCtx, clientMetadata.SessionID)
	}
	newCtx = logging.WithResponseStatusHolder(newCtx)
	newCtx = logging.WithResponseHeadersHolder(newCtx)
	newCtx = logging.WithResponseTimingHolder(newCtx)
	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		watchCtx := newCtx
		go func() {
			select {
			case <-requestCtx.Done():
				cancel(context.Cause(requestCtx))
			case <-watchCtx.Done():
			}
		}()
	}
	newCtx = util.WithGinContext(newCtx, c)
	newCtx = coreusage.WithManager(newCtx, h.UsageManager)
	newCtx = sdktranslator.WithPluginHooks(newCtx, h.TranslatorPluginHooks)
	newCtx = context.WithValue(newCtx, handlerContextKey{}, handler)
	return newCtx, func(params ...any) {
		var cause error
		if len(params) == 1 {
			cause, _ = params[0].(error)
		}
		defer cancel(cause)
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if h.requestLoggingEnabled() && len(params) == 1 {
			if captured, exists := c.Get(logging.APIResponseCapturedContextKey); exists {
				if capturedBool, ok := captured.(bool); ok && capturedBool {
					return
				}
			}
			if existingBytes := requestLogContextBytes(c, "API_RESPONSE"); len(bytes.TrimSpace(existingBytes)) > 0 {
				switch params[0].(type) {
				case error, string:
					return
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
				existingBytes := requestLogContextBytes(c, "API_RESPONSE")
				trimmedPayload := bytes.TrimSpace(payload)
				if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
					return
				}
				appendAPIResponse(c, payload)
			}
		}

	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.ConfigSnapshot())
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

	buffer := requestLogContextBuffer(c, "API_RESPONSE")
	if buffer.Len() > 0 && !buffer.HasSuffix([]byte("\n")) {
		buffer.Append([]byte("\n"))
	}
	buffer.Append(data)
}

func requestLogContextBuffer(c *gin.Context, key string) *logging.BoundedLogBuffer {
	if value, exists := c.Get(key); exists {
		if buffer, ok := value.(*logging.BoundedLogBuffer); ok && buffer != nil {
			return buffer
		}
		if data, ok := value.([]byte); ok {
			buffer := logging.NewBoundedLogBuffer()
			buffer.Append(data)
			c.Set(key, buffer)
			return buffer
		}
	}
	buffer := logging.NewBoundedLogBuffer()
	c.Set(key, buffer)
	return buffer
}

func requestLogContextBytes(c *gin.Context, key string) []byte {
	if c == nil {
		return nil
	}
	value, exists := c.Get(key)
	if !exists {
		return nil
	}
	switch data := value.(type) {
	case []byte:
		return bytes.Clone(data)
	case *logging.BoundedLogBuffer:
		return data.Bytes()
	default:
		return nil
	}
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) ([]byte, http.Header, *ErrorMessage) {
	return h.executeNonStream(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, false, false, nil, nil, "", h.AuthManager.Execute,
	)
}

// ExecuteImageWithAuthManager executes a non-streaming image request via the core auth manager.
func (h *BaseAPIHandler) ExecuteImageWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) ([]byte, http.Header, *ErrorMessage) {
	return h.executeNonStream(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, true, false, nil, nil, "", h.AuthManager.Execute,
	)
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) ([]byte, http.Header, *ErrorMessage) {
	return h.executeNonStream(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, false, false, nil, nil, "", h.AuthManager.ExecuteCount,
	)
}

func (h *BaseAPIHandler) executeNonStream(
	ctx context.Context,
	handlerType, responseType, modelName string,
	rawJSON []byte,
	alt string,
	allowImageModel, internalExecution bool,
	requestHeaders http.Header,
	requestQuery url.Values,
	skipPluginID string,
	execFn func(context.Context, []string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error),
) ([]byte, http.Header, *ErrorMessage) {
	executionID := newExecutionRequestID()
	ctx, providers, normalizedModel, reservation, errMsg := h.prepareAPIKeyExecution(
		ctx, handlerType, modelName, allowImageModel, executionID,
	)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
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
	lifecycle := h.newRequestLifecycleTracker(
		ctx, executionID, handlerType, normalizedModel, modelName, false, reqMeta, skipPluginID,
	)
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		Headers:                     headers,
		Query:                       query,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		ResponseFormat:              sdktranslator.FromString(responseType),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, lifecycle, skipPluginID),
		WebSocketResponseObserver:   h.webSocketResponseObserver(lifecycle, skipPluginID),
		ProxyURL:                    coreexecutor.RequestProxyURL(ctx),

		Metadata: reqMeta}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	var termination *ErrorMessage
	req, opts, termination = h.applyRequestInterceptorsBeforeAuth(
		ctx, handlerType, modelName, req, opts, lifecycle, skipPluginID,
	)
	if termination != nil {
		reservation.release(ctx, "request terminated before dispatch")
		lifecycle.completeError(ctx, termination)
		return nil, nil, termination
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	resp, err := execFn(ctx, providers, req, opts)
	if err != nil {
		reservation.settleError(ctx, err)
		errMsg = executionErrorMessage(err)
		lifecycle.completeError(ctx, errMsg)
		return nil, nil, errMsg
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	ctx = enrichContextWithSessionHierarchy(ctx, executedOpts.Headers, executedReq.Payload, executedOpts.Metadata)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, internalExecution || PassthroughHeadersEnabled(h.ConfigSnapshot()))
	body, responseHeaders := h.applyResponseInterceptors(
		ctx, responseType, normalizedModel, modelName, executedOpts, rawResponseHeaders, responseHeaders,
		executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, lifecycle.requestID(),
		skipPluginID, internalExecution,
	)
	lifecycle.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)
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
) (<-chan []byte, http.Header, <-chan *ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, handlerType, modelName, rawJSON, alt, false, false, nil, nil, "")
}

// ExecuteStreamChunksWithAuthManager preserves per-chunk delivery acknowledgements.
func (h *BaseAPIHandler) ExecuteStreamChunksWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) (<-chan coreexecutor.StreamChunk, http.Header, <-chan *ErrorMessage) {
	ctx = coreexecutor.WithStreamDeliveryAcknowledgement(ctx)
	return h.executeStreamChunksWithAuthManager(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, false, false, nil, nil, "",
	)
}

// ExecuteImageStreamWithAuthManager executes a streaming image request via the core auth manager.
func (h *BaseAPIHandler) ExecuteImageStreamWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) (<-chan []byte, http.Header, <-chan *ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, handlerType, modelName, rawJSON, alt, true, false, nil, nil, "")
}

// ExecuteImageStreamChunksWithAuthManager preserves image-stream delivery acknowledgements.
func (h *BaseAPIHandler) ExecuteImageStreamChunksWithAuthManager(
	ctx context.Context,
	handlerType, modelName string,
	rawJSON []byte,
	alt string,
) (<-chan coreexecutor.StreamChunk, http.Header, <-chan *ErrorMessage) {
	ctx = coreexecutor.WithStreamDeliveryAcknowledgement(ctx)
	return h.executeStreamChunksWithAuthManager(
		ctx, handlerType, handlerType, modelName, rawJSON, alt, true, false, nil, nil, "",
	)
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(
	ctx context.Context,
	handlerType, responseType, modelName string,
	rawJSON []byte,
	alt string,
	allowImageModel, internalExecution bool,
	requestHeaders http.Header,
	requestQuery url.Values,
	skipPluginID string,
) (<-chan []byte, http.Header, <-chan *ErrorMessage) {
	chunks, headers, errs := h.executeStreamChunksWithAuthManager(
		ctx, handlerType, responseType, modelName, rawJSON, alt, allowImageModel, internalExecution,
		requestHeaders, requestQuery, skipPluginID,
	)
	return flattenStreamChunks(ctx, chunks), headers, errs
}

func flattenStreamChunks(ctx context.Context, chunks <-chan coreexecutor.StreamChunk) <-chan []byte {
	if chunks == nil {
		return nil
	}
	data := make(chan []byte)
	go func() {
		defer close(data)
		for chunk := range chunks {
			if ctx == nil {
				data <- chunk.Payload
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
				continue
			}
			select {
			case data <- chunk.Payload:
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			case <-ctx.Done():
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(ctx.Err())
				}
				return
			}
		}
	}()
	return data
}

func (h *BaseAPIHandler) executeStreamChunksWithAuthManager(
	ctx context.Context,
	handlerType, responseType, modelName string,
	rawJSON []byte,
	alt string,
	allowImageModel, internalExecution bool,
	requestHeaders http.Header,
	requestQuery url.Values,
	skipPluginID string,
) (<-chan coreexecutor.StreamChunk, http.Header, <-chan *ErrorMessage) {
	requireTransportTerminal := coreexecutor.StreamDeliveryAcknowledgement(ctx)
	executionID := newExecutionRequestID()
	ctx, providers, normalizedModel, reservation, errMsg := h.prepareAPIKeyExecution(
		ctx, handlerType, modelName, allowImageModel, executionID,
	)
	if errMsg != nil {
		errChan := make(chan *ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
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
	lifecycle := h.newRequestLifecycleTracker(
		ctx, executionID, handlerType, normalizedModel, modelName, true, reqMeta, skipPluginID,
	)
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      true,
		Alt:                         alt,
		Headers:                     headers,
		Query:                       query,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		ResponseFormat:              sdktranslator.FromString(responseType),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, lifecycle, skipPluginID),
		WebSocketResponseObserver:   h.webSocketResponseObserver(lifecycle, skipPluginID),
		ProxyURL:                    coreexecutor.RequestProxyURL(ctx),

		Metadata: reqMeta}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	var termination *ErrorMessage
	req, opts, termination = h.applyRequestInterceptorsBeforeAuth(
		ctx, handlerType, modelName, req, opts, lifecycle, skipPluginID,
	)
	if termination != nil {
		errChan := make(chan *ErrorMessage, 1)
		reservation.release(ctx, "request terminated before dispatch")
		lifecycle.completeError(ctx, termination)
		errChan <- termination
		close(errChan)
		return nil, nil, errChan
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		errChan := make(chan *ErrorMessage, 1)
		reservation.settleError(ctx, err)
		errMsg = executionErrorMessage(err)
		lifecycle.completeError(ctx, errMsg)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	executedRequest := func() (coreexecutor.Request, coreexecutor.Options) {
		return afterAuthCapture.apply(req, opts)
	}
	executedReq, executedOpts := executedRequest()
	ctx = enrichContextWithSessionHierarchy(ctx, executedOpts.Headers, executedReq.Payload, executedOpts.Metadata)
	passthroughHeadersEnabled := internalExecution || PassthroughHeadersEnabled(h.ConfigSnapshot())
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	upstreamHeaders := downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}
	chunks := streamResult.Chunks
	dataChan := make(chan coreexecutor.StreamChunk)
	errChan := make(chan *ErrorMessage, 1)
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
				RequestID:       lifecycle.requestID(),
				SourceFormat:    responseType,
				Model:           normalizedModel,
				RequestedModel:  modelName,
				RequestHeaders:  cloneHeader(executedOpts.Headers),
				ResponseHeaders: cloneHeader(rawStreamHeaders),
				OriginalRequest: cloneBytes(executedOpts.OriginalRequest),
				RequestBody:     cloneBytes(executedReq.Payload),
				ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
				Metadata:        pluginExecutionMetadata(executedOpts.Metadata),
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
			if chunk.Err != nil || chunk.Terminal {
				applyStreamHeaderInit()
				return
			}
			if len(chunk.Payload) > 0 {
				applyStreamHeaderInit()
				return
			}
		}
	}
	readInitialStreamChunks()
	responseHeaders := make(chan http.Header, 1)
	headersPublished := false
	publishHeaders := func() {
		if headersPublished {
			return
		}
		headersPublished = true
		responseHeaders <- cloneHeader(upstreamHeaders)
	}

	go func() {
		completionOutcome := pluginapi.RequestCompletionSucceeded
		completionStatus := http.StatusOK
		var completionErr error
		defer func() {
			if completionOutcome == pluginapi.RequestCompletionFailed && ctx != nil && ctx.Err() != nil && errors.Is(completionErr, ctx.Err()) {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
			}
			lifecycle.complete(completionOutcome, completionStatus, completionErr)
		}()
		defer close(dataChan)
		defer close(errChan)
		defer publishHeaders()
		if streamCanceledBeforeRead {
			reservation.markUnknown(ctx)
			completionOutcome = pluginapi.RequestCompletionCanceled
			completionStatus = 0
			if ctx != nil {
				completionErr = ctx.Err()
			}
			return
		}
		sentPayload := false
		terminalObserved := false
		bootstrapRetries := 0
		chunkIndex := 0
		var historyChunks [][]byte
		maxBootstrapRetries := StreamingBootstrapRetries(h.ConfigSnapshot())
		var deliveryFailureMu sync.Mutex
		var deliveryFailure error
		recordDeliveryFailure := func(err error) {
			if err == nil {
				return
			}
			deliveryFailureMu.Lock()
			if deliveryFailure == nil {
				deliveryFailure = err
			}
			deliveryFailureMu.Unlock()
		}
		currentDeliveryFailure := func() error {
			deliveryFailureMu.Lock()
			defer deliveryFailureMu.Unlock()
			return deliveryFailure
		}
		trackAcknowledgement := func(acknowledge func(error)) func(error) {
			return func(err error) {
				recordDeliveryFailure(err)
				if acknowledge != nil {
					acknowledge(err)
				}
			}
		}
		completeCancellation := func() {
			if errDelivery := currentDeliveryFailure(); errDelivery != nil {
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = http.StatusInternalServerError
				completionErr = errDelivery
				return
			}
			if completionErr != nil {
				return
			}
			completionOutcome = pluginapi.RequestCompletionCanceled
			completionStatus = 0
			if ctx != nil {
				completionErr = ctx.Err()
			}
		}

		sendErr := func(msg *ErrorMessage) bool {
			publishHeaders()
			var sourceAcknowledge func(error)
			if msg != nil {
				sourceAcknowledge = msg.Acknowledge
				msg.Acknowledge = trackAcknowledgement(sourceAcknowledge)
			}
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				if sourceAcknowledge != nil {
					sourceAcknowledge(ctx.Err())
				}
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk coreexecutor.StreamChunk) bool {
			publishHeaders()
			sourceAcknowledge := chunk.Acknowledge
			chunk.Acknowledge = trackAcknowledgement(sourceAcknowledge)
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				if sourceAcknowledge != nil {
					sourceAcknowledge(ctx.Err())
				}
				return false
			case dataChan <- chunk:
				return true
			}
		}

		bootstrapEligible := func(err error) bool {
			if isRequestTerminatedExecutionError(err) {
				return false
			}
			var dispositionErr coreexecutor.ExecutionDispositionError
			if !errors.As(err, &dispositionErr) || dispositionErr == nil {
				return false
			}
			disposition := dispositionErr.ExecutionDisposition()
			if disposition.Outcome != coreexecutor.ExecutionOutcomeRejectedBeforeProcessing {
				return false
			}
			switch disposition.RetryDirective {
			case coreexecutor.RetryDirectiveNextAuth,
				coreexecutor.RetryDirectiveNextProvider,
				coreexecutor.RetryDirectiveRetryAfter:
				return true
			default:
				return false
			}
		}

	outer:
		for {
			for {
				chunk, ok, canceled := nextStreamChunk(ctx, &pendingChunks, &streamClosedBeforeRead, chunks)
				if canceled {
					reservation.markUnknown(ctx)
					completeCancellation()
					return
				}
				if !ok {
					applyStreamHeaderInit()
					if errDelivery := currentDeliveryFailure(); errDelivery != nil {
						completionOutcome = pluginapi.RequestCompletionFailed
						completionStatus = http.StatusInternalServerError
						completionErr = errDelivery
						return
					}
					if !terminalObserved {
						reservation.markUnknown(ctx)
						if requireTransportTerminal {
							completionOutcome = pluginapi.RequestCompletionFailed
							completionStatus = http.StatusBadGateway
							completionErr = errStreamClosedBeforeTransportTerminal
						}
					}
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							if errDrain := acknowledgeAndDrainBootstrapAttempt(ctx, chunk, chunks); errDrain != nil {
								reservation.markUnknown(ctx)
								completionOutcome = pluginapi.RequestCompletionCanceled
								completionStatus = 0
								completionErr = errDrain
								return
							}
							chunk.Acknowledge = nil
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

					reservation.settleError(ctx, streamErr)
					errMsg := executionErrorMessage(streamErr)
					errMsg.Acknowledge = reservation.wrapAcknowledgement(ctx, chunk.Acknowledge)
					completionOutcome = pluginapi.RequestCompletionFailed
					if isRequestTerminatedExecutionError(streamErr) {
						completionOutcome = pluginapi.RequestCompletionRejected
					}
					completionStatus = errMsg.StatusCode
					completionErr = streamErr
					if !sendErr(errMsg) && ctx != nil && ctx.Err() != nil {
						completeCancellation()
						return
					}
					for {
						_, ok, canceled := nextStreamChunk(ctx, nil, nil, chunks)
						if canceled {
							completeCancellation()
							return
						}
						if !ok {
							if errDelivery := currentDeliveryFailure(); errDelivery != nil {
								completionOutcome = pluginapi.RequestCompletionFailed
								completionStatus = http.StatusInternalServerError
								completionErr = errDelivery
							}
							return
						}
					}
				}
				if chunk.Terminal {
					terminalObserved = true
					chunk.Acknowledge = reservation.wrapAcknowledgement(ctx, chunk.Acknowledge)
					if okSendData := sendData(chunk); !okSendData {
						completeCancellation()
						return
					}
					continue
				}
				if len(chunk.Payload) > 0 {
					applyStreamHeaderInit()
					payload := cloneBytes(chunk.Payload)
					if streamInterceptorsActive {
						executedReq, executedOpts := executedRequest()
						intercepted := interceptStreamChunk(
							ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
								RequestID:       lifecycle.requestID(),
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
								Metadata:        pluginExecutionMetadata(executedOpts.Metadata),
							}, skipPluginID,
						)
						applyStreamHeaders(intercepted.Headers, intercepted.ClearHeaders)
						if len(intercepted.Body) > 0 {
							payload = cloneBytes(intercepted.Body)
						}
						chunkIndex++
						if intercepted.DropChunk {
							streamErr := fmt.Errorf("stream chunk dropped by interceptor")
							errMsg := executionErrorMessage(streamErr)
							if chunk.Acknowledge != nil {
								errMsg.Acknowledge = func(errDelivery error) {
									if errDelivery != nil {
										chunk.Acknowledge(errDelivery)
										return
									}
									chunk.Acknowledge(streamErr)
								}
							}
							reservation.markUnknown(ctx)
							errMsg.Acknowledge = reservation.wrapAcknowledgement(ctx, errMsg.Acknowledge)
							completionOutcome = pluginapi.RequestCompletionFailed
							completionStatus = errMsg.StatusCode
							completionErr = streamErr
							if !sendErr(errMsg) && ctx != nil && ctx.Err() != nil {
								completeCancellation()
								return
							}
							for {
								_, ok, canceled := nextStreamChunk(ctx, nil, nil, chunks)
								if canceled {
									completeCancellation()
									return
								}
								if !ok {
									if errDelivery := currentDeliveryFailure(); errDelivery != nil {
										completionOutcome = pluginapi.RequestCompletionFailed
										completionStatus = http.StatusInternalServerError
										completionErr = errDelivery
									}
									return
								}
							}
						}
					} else {
						chunkIndex++
					}
					sentPayload = true
					streamHeadersCommitted = true
					if okSendData := sendData(
						coreexecutor.StreamChunk{
							Payload:     payload,
							Acknowledge: reservation.wrapAcknowledgement(ctx, chunk.Acknowledge),
						},
					); !okSendData {
						completeCancellation()
						return
					}
					if streamInterceptorsActive {
						historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
					}
				} else if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
		}
	}()
	if upstreamHeaders == nil {
		return dataChan, nil, errChan
	}
	if ctx == nil {
		return dataChan, <-responseHeaders, errChan
	}
	select {
	case snapshot := <-responseHeaders:
		return dataChan, snapshot, errChan
	case <-ctx.Done():
		return dataChan, nil, errChan
	}
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

func acknowledgeAndDrainBootstrapAttempt(
	ctx context.Context,
	failure coreexecutor.StreamChunk,
	chunks <-chan coreexecutor.StreamChunk,
) error {
	if failure.Acknowledge != nil {
		failure.Acknowledge(nil)
	}
	for {
		chunk, ok, canceled := nextStreamChunk(ctx, nil, nil, chunks)
		if canceled {
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return context.Canceled
		}
		if !ok {
			return nil
		}
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
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

func webSocketResponseObserversEnabled(host PluginInterceptorHost) bool {
	if host == nil {
		return false
	}
	if detector, ok := host.(webSocketResponseObserverDetector); ok {
		return detector.HasWebSocketResponseObservers()
	}
	_, callbackHost := host.(webSocketResponseCallbackHost)
	_, observerHost := host.(webSocketResponseObserverHost)
	return callbackHost || observerHost
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
		for name := range out {
			if strings.EqualFold(name, key) {
				delete(out, name)
			}
		}
	}
	for key, values := range updates {
		for name := range out {
			if strings.EqualFold(name, key) {
				delete(out, name)
			}
		}
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

func overrideRequestPathMetadata(opts *coreexecutor.Options, path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	if !validModelExecutionPath(path) {
		return false
	}
	metadata := maps.Clone(opts.Metadata)
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata[coreexecutor.RequestPathMetadataKey] = path
	opts.Metadata = metadata
	return true
}

func (h *BaseAPIHandler) applyRequestInterceptorsBeforeAuth(
	ctx context.Context,
	handlerType, requestedModel string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	lifecycle *requestLifecycleTracker,
	skipPluginID string,
) (coreexecutor.Request, coreexecutor.Options, *ErrorMessage) {
	host := h.interceptorHost()
	if !requestInterceptorsEnabled(host) {
		return req, opts, nil
	}
	resp := interceptRequestBeforeAuth(
		ctx, host, pluginapi.RequestInterceptRequest{
			RequestID:      lifecycle.requestID(),
			TraceID:        lifecycle.traceID(),
			SourceFormat:   handlerType,
			Model:          req.Model,
			RequestedModel: requestedModel,
			Stream:         opts.Stream,
			Headers:        cloneHeader(opts.Headers),
			Body:           cloneBytes(req.Payload),
			Metadata:       pluginExecutionMetadata(opts.Metadata),
		}, skipPluginID,
	)
	opts.Headers = mergeRequestInterceptorHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = cloneBytes(resp.Body)
		opts.OriginalRequest = cloneBytes(resp.Body)
	}
	if !overrideRequestPathMetadata(&opts, resp.Path) {
		return req, opts, modelExecutionModeError("invalid request path")
	}
	if resp.Terminate {
		return req, opts, requestTerminationError(resp)
	}
	return req, opts, nil
}

func (h *BaseAPIHandler) requestAfterAuthInterceptor(
	capture *requestAfterAuthCapture,
	lifecycle *requestLifecycleTracker,
	skipPluginID string,
) coreexecutor.RequestAfterAuthInterceptor {
	if !requestInterceptorsEnabled(h.interceptorHost()) {
		return nil
	}
	return func(
		ctx context.Context,
		req coreexecutor.RequestAfterAuthInterceptRequest,
	) coreexecutor.RequestAfterAuthInterceptResponse {
		resp := h.applyRequestInterceptorsAfterAuth(ctx, req, lifecycle, skipPluginID)
		capturedReq := req.Request
		capturedOpts := req.Options
		if resp.Handled && !resp.Terminate {
			capturedReq = resp.Request
			capturedOpts = resp.Options
		}
		if capture != nil {
			capture.record(capturedReq, capturedOpts)
		}
		return resp
	}
}

func (h *BaseAPIHandler) webSocketResponseObserver(
	lifecycle *requestLifecycleTracker,
	skipPluginID string,
) coreexecutor.WebSocketResponseObserver {
	host := h.interceptorHost()
	if !webSocketResponseObserversEnabled(host) {
		return nil
	}
	callbackHost, hasCallbackHost := host.(webSocketResponseCallbackHost)
	observerHost, hasObserverHost := host.(webSocketResponseObserverHost)
	skipHost, _ := host.(webSocketResponseObserverSkipHost)
	if !hasCallbackHost && !hasObserverHost {
		return nil
	}

	var openCallback sync.Once
	var callback pluginapi.WebSocketResponseCallback
	return func(ctx context.Context, event coreexecutor.WebSocketResponseEvent) {
		if lifecycle != nil {
			if event.RequestID == "" {
				event.RequestID = lifecycle.requestID()
			}
			if event.TraceID == "" {
				event.TraceID = lifecycle.traceID()
			}
		}
		pluginEvent := pluginapi.WebSocketResponseEvent{
			RequestID:      event.RequestID,
			TraceID:        event.TraceID,
			SourceFormat:   event.SourceFormat,
			Model:          event.Model,
			RequestedModel: event.RequestedModel,
			Provider:       event.Provider,
			AuthID:         event.AuthID,
			AuthLabel:      event.AuthLabel,
			AuthType:       event.AuthType,
			EventType:      event.EventType,
			Payload:        cloneBytes(event.Payload),
			Metadata:       pluginExecutionMetadata(event.Metadata),
		}
		if hasCallbackHost {
			openCallback.Do(func() {
				callback, _ = callbackHost.OpenWebSocketResponseCallback(ctx, skipPluginID)
			})
			if callback != nil {
				callback(pluginEvent)
			}
			return
		}
		if skipPluginID != "" && skipHost != nil {
			skipHost.ObserveWebSocketResponseEventExcept(ctx, pluginEvent, skipPluginID)
			return
		}
		observerHost.ObserveWebSocketResponseEvent(ctx, pluginEvent)
	}
}

func (h *BaseAPIHandler) applyRequestInterceptorsAfterAuth(
	ctx context.Context,
	req coreexecutor.RequestAfterAuthInterceptRequest,
	lifecycle *requestLifecycleTracker,
	skipPluginID string,
) coreexecutor.RequestAfterAuthInterceptResponse {
	host := h.interceptorHost()
	if !requestInterceptorsEnabled(host) {
		return coreexecutor.RequestAfterAuthInterceptResponse{}
	}
	resp := interceptRequestAfterAuth(
		ctx, host, pluginapi.RequestInterceptRequest{
			RequestID:      lifecycle.requestID(),
			TraceID:        lifecycle.traceID(),
			SourceFormat:   req.SourceFormat.String(),
			ToFormat:       req.ToFormat.String(),
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			Headers:        cloneHeader(req.Options.Headers),
			Body:           cloneBytes(req.Request.Payload),
			Metadata:       pluginExecutionMetadata(req.Options.Metadata),
		}, skipPluginID,
	)
	nextReq := req.Request
	nextOpts := req.Options
	nextOpts.Headers = mergeRequestInterceptorHeaders(nextOpts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		nextReq.Payload = cloneBytes(resp.Body)
		nextOpts.OriginalRequest = cloneBytes(resp.Body)
	}
	previousPayload := req.Options.OriginalRequest
	if len(previousPayload) == 0 {
		previousPayload = req.Request.Payload
	}
	currentPayload := nextOpts.OriginalRequest
	if len(currentPayload) == 0 {
		currentPayload = nextReq.Payload
	}
	previousSession, _ := coresession.ExtractSessionInfo(req.Options.Headers, previousPayload, nil)
	currentSession, _ := coresession.ExtractSessionInfo(nextOpts.Headers, currentPayload, nil)
	if previousSession.SessionID != currentSession.SessionID ||
		previousSession.ParentSessionID != currentSession.ParentSessionID ||
		previousSession.NodeKind != currentSession.NodeKind ||
		previousSession.IsFork != currentSession.IsFork ||
		previousSession.IsCompaction != currentSession.IsCompaction ||
		len(resp.Body) > 0 && !bytes.Equal(resp.Body, req.Request.Payload) {
		nextReq.Metadata = maps.Clone(nextReq.Metadata)
		nextOpts.Metadata = maps.Clone(nextOpts.Metadata)
		for _, key := range []string{
			coreexecutor.CanonicalSessionIDMetadataKey, coreexecutor.ParentSessionIDMetadataKey,
			coreexecutor.DerivedSessionIDMetadataKey, coreexecutor.LCPAffinitySessionIDMetadataKey,
			coreexecutor.LCPFingerprintMetadataKey, coreexecutor.LCPMinPrefixLengthMetadataKey,
			coreexecutor.LCPTailFingerprintsMetadataKey, coreexecutor.LCPEnvironmentDigestMetadataKey,
			coreexecutor.LCPAccessGenerationMetadataKey, coreexecutor.NodeKindMetadataKey,
			coreexecutor.IsForkMetadataKey, coreexecutor.IsCompactionMetadataKey,
			coreexecutor.SessionAffinityProviderMetadataKey, coreexecutor.SessionAffinityModelMetadataKey,
		} {
			delete(nextReq.Metadata, key)
			delete(nextOpts.Metadata, key)
		}
		nextReq, nextOpts = coresession.Enrich(nextReq, nextOpts)
	}
	if !overrideRequestPathMetadata(&nextOpts, resp.Path) {
		return coreexecutor.RequestAfterAuthInterceptResponse{
			Terminate:    true,
			StatusCode:   http.StatusBadRequest,
			ResponseBody: BuildErrorResponseBody(http.StatusBadRequest, "invalid request path"),
		}
	}
	return coreexecutor.RequestAfterAuthInterceptResponse{
		Request:         nextReq,
		Options:         nextOpts,
		Handled:         true,
		Terminate:       resp.Terminate,
		StatusCode:      normalizedTerminationStatus(resp.StatusCode),
		ResponseHeaders: cloneHeader(resp.ResponseHeaders),
		ResponseBody:    cloneBytes(resp.ResponseBody),
	}
}

func (h *BaseAPIHandler) applyResponseInterceptors(
	ctx context.Context,
	handlerType, normalizedModel, requestedModel string,
	opts coreexecutor.Options,
	rawResponseHeaders, responseHeaders http.Header,
	originalRequest, requestBody, body []byte,
	statusCode int,
	requestID,
	skipPluginID string,
	internalExecution bool,
) ([]byte, http.Header) {
	host := h.interceptorHost()
	if host == nil {
		return body, responseHeaders
	}
	resp := interceptResponse(
		ctx, host, pluginapi.ResponseInterceptRequest{
			RequestID:       requestID,
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
			Metadata:        pluginExecutionMetadata(opts.Metadata),
		}, skipPluginID,
	)
	responseHeaders = downstreamHeadersAfterInterceptors(
		rawResponseHeaders,
		mergeRequestInterceptorHeaders(rawResponseHeaders, resp.Headers, resp.ClearHeaders),
		internalExecution || PassthroughHeadersEnabled(h.ConfigSnapshot()),
	)
	if len(resp.Body) > 0 {
		body = cloneBytes(resp.Body)
	}
	return body, responseHeaders
}

func statusFromError(err error) int {
	return clienterror.HTTPStatusFromError(err)
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

func (h *BaseAPIHandler) SupportsApplyPatchModel(model string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}
	providers, _, err := h.getRequestDetails(model)
	return err == nil && h.AuthManager.SupportsApplyPatchForProviders(providers)
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (
	providers []string,
	normalizedModel string,
	err *ErrorMessage,
) {
	return h.getRequestDetailsWithOptions(modelName, false)
}

func (h *BaseAPIHandler) getRequestDetailsWithOptions(modelName string, allowImageModel bool) (
	providers []string,
	normalizedModel string,
	err *ErrorMessage,
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

	if errMsg := h.validateImageOnlyModel(baseModel, allowImageModel); errMsg != nil {
		return nil, "", errMsg
	}

	providers = util.GetProviderName(baseModel)
	if len(providers) == 0 && allowImageModel {
		if registeredModel := registry.GetGlobalRegistry().ResolveModelIDCaseInsensitive(baseModel); registeredModel != "" {
			providers = util.GetProviderName(registeredModel)
			if len(providers) > 0 {
				resolvedModelName = registeredModel
			}
		}
	}
	if len(providers) == 0 && allowImageModel && allowsBuiltinImageModelBareFallback(baseModel) {
		if canonicalModel := canonicalImageOnlyModel(baseModel); canonicalModel != "" {
			registeredModel := registry.GetGlobalRegistry().ResolveModelIDCaseInsensitive(canonicalModel)
			if registeredModel == "" {
				registeredModel = canonicalModel
			}
			providers = util.GetProviderName(registeredModel)
			if len(providers) > 0 {
				resolvedModelName = registeredModel
			}
		}
	}
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}

	if len(providers) == 0 {
		body, errMarshal := json.Marshal(
			ErrorResponse{
				Error: ErrorDetail{
					Message: "unknown provider for model " + modelName,
					Type:    "invalid_request_error",
					Code:    "model_not_found",
				},
			},
		)
		if errMarshal != nil {
			body = []byte(`{"error":{"message":"unknown provider for model","type":"invalid_request_error","code":"model_not_found"}}`)
		}
		return nil, "", &ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errors.New(string(body)),
		}
	}
	if thinking.HasContext1MTag(resolvedModelName) && requiresContext1MCapabilityValidation(providers) &&
		!supportsContext1M(resolvedModelName, providers) {
		return nil, "", &ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("model %s does not support [1m] context", modelName),
		}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func (h *BaseAPIHandler) validateImageOnlyModel(modelName string, allowImageModel bool) *ErrorMessage {
	baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	if baseModel == "" {
		baseModel = strings.TrimSpace(modelName)
	}
	if isOpenAIImageOnlyModel(baseModel) && !allowImageModel {
		return &ErrorMessage{
			StatusCode: http.StatusServiceUnavailable,
			Error: fmt.Errorf(
				"model %s is only supported on /v1/images/generations and /v1/images/edits",
				routeModelBaseName(baseModel),
			),
		}
	}
	return nil
}

func isOpenAIImageOnlyModel(model string) bool {
	return canonicalImageOnlyModel(model) != ""
}

func allowsBuiltinImageModelBareFallback(model string) bool {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	idx := strings.LastIndex(model, "/")
	if idx < 0 {
		return true
	}
	if idx >= len(model)-1 {
		return false
	}

	prefix := strings.ToLower(strings.TrimSpace(model[:idx]))
	canonical := canonicalImageOnlyModel(model)
	if registry.IsCodexImageModel(canonical) {
		return prefix == "codex" || prefix == "openai"
	}
	switch canonical {
	case "grok-imagine-image", "grok-imagine-image-2.0", "grok-imagine-image-quality":
		return prefix == "xai" || prefix == "x-ai" || prefix == "grok"
	default:
		return false
	}
}

func canonicalImageOnlyModel(model string) string {
	baseModel := strings.ToLower(strings.TrimSpace(routeModelBaseName(model)))
	if canonical := registry.CanonicalCodexImageModel(baseModel); canonical != "" {
		return canonical
	}
	switch baseModel {
	case "grok-imagine-image":
		return "grok-imagine-image"
	case "grok-imagine-image-2.0":
		return "grok-imagine-image-2.0"
	case "grok-imagine-image-quality":
		return "grok-imagine-image-quality"
	default:
		return ""
	}
}

func routeModelBaseName(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[idx+1:])
	}
	return model
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

// WriteStreamBytes writes every byte or returns the first write failure.
func WriteStreamBytes(writer io.Writer, parts ...[]byte) error {
	if writer == nil {
		return fmt.Errorf("stream writer is nil")
	}
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		n, err := writer.Write(part)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
	}
	return nil
}

// FlushStreamResponse flushes buffered response bytes and returns observable flush failures.
func FlushStreamResponse(writer http.ResponseWriter, flusher http.Flusher) error {
	if writer != nil {
		err := httpwire.FlushResponse(writer)
		if err == nil || !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	if flusher == nil {
		return http.ErrNotSupported
	}
	flusher.Flush()
	return nil
}

// DeliverStreamResponse writes and flushes one downstream stream unit under a bounded write deadline.
func DeliverStreamResponse(
	writer http.ResponseWriter,
	flusher http.Flusher,
	write func() error,
) error {
	return deliverStreamResponseWithTimeout(writer, flusher, defaultStreamDeliveryTimeout, write)
}

func deliverStreamResponseWithTimeout(
	writer http.ResponseWriter,
	flusher http.Flusher,
	timeout time.Duration,
	write func() error,
) error {
	return withStreamWriteDeadline(
		writer, timeout, func() error {
			if write != nil {
				if errWrite := write(); errWrite != nil {
					return errWrite
				}
			}
			return FlushStreamResponse(writer, flusher)
		},
	)
}

func withStreamWriteDeadline(
	writer http.ResponseWriter,
	timeout time.Duration,
	write func() error,
) (err error) {
	if write == nil {
		return nil
	}
	controller := http.NewResponseController(writer)
	deadlineSet := false
	if writer != nil && timeout > 0 {
		errDeadline := controller.SetWriteDeadline(time.Now().Add(timeout))
		if errDeadline == nil {
			deadlineSet = true
		} else if !errors.Is(errDeadline, http.ErrNotSupported) {
			return errDeadline
		}
	}
	if deadlineSet {
		defer func() {
			errClear := controller.SetWriteDeadline(time.Time{})
			if errClear != nil && !errors.Is(errClear, http.ErrNotSupported) {
				err = errors.Join(err, errClear)
			}
		}()
	}
	return write()
}

// AcknowledgeStreamError reports terminal error response delivery at most once upstream.
func AcknowledgeStreamError(msg *ErrorMessage, err error) {
	if msg != nil && msg.Acknowledge != nil {
		msg.Acknowledge(err)
	}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *ErrorMessage) {
	_ = h.WriteErrorResponseWithError(c, msg)
}

// WriteAcknowledgedStreamError writes, flushes, and acknowledges an initial stream error response.
func (h *BaseAPIHandler) WriteAcknowledgedStreamError(
	c *gin.Context,
	flusher http.Flusher,
	msg *ErrorMessage,
) error {
	err := DeliverStreamResponse(
		c.Writer, flusher, func() error {
			return h.WriteErrorResponseWithError(c, msg)
		},
	)
	AcknowledgeStreamError(msg, err)
	return err
}

// WriteErrorResponseWithError writes an error response and reports downstream write failures.
func (h *BaseAPIHandler) WriteErrorResponseWithError(c *gin.Context, msg *ErrorMessage) error {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.DirectResponse {
		return h.writeDirectErrorResponse(c, status, msg)
	}
	if msg != nil && msg.Error != nil {
		for _, value := range coreauth.SafeResponseHeaders(msg.Error).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.ConfigSnapshot()) {
		for key, values := range msg.Addon {
			if len(values) == 0 || IsCPAReservedResponseHeader(key) {
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
			previous := requestLogContextBytes(c, "API_RESPONSE")
			trimmedErrText := strings.TrimSpace(errText)
			trimmedBody := bytes.TrimSpace(body)
			alreadyCaptured := len(previous) > 0 &&
				((trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
					(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)))
			if !alreadyCaptured {
				appendAPIResponse(c, body)
			}
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	return WriteStreamBytes(c.Writer, body)
}

func (h *BaseAPIHandler) writeDirectErrorResponse(c *gin.Context, status int, msg *ErrorMessage) error {
	for key, values := range FilterUpstreamHeaders(msg.Headers) {
		if len(values) == 0 || IsCPAReservedResponseHeader(key) {
			continue
		}
		c.Writer.Header().Del(key)
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	body := cloneBytes(msg.Body)
	if h.requestLoggingEnabled() {
		captured, existsCaptured := c.Get(logging.APIResponseCapturedContextKey)
		if !existsCaptured || captured != true {
			appendAPIResponse(c, body)
		}
	}
	if !c.Writer.Written() && c.Writer.Header().Get("Content-Type") == "" {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	return WriteStreamBytes(c.Writer, body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *ErrorMessage) {
	ginContext, ok := util.GinContextValue(ctx).(*gin.Context)
	if !ok || ginContext == nil {
		return
	}
	if apiResponseErrors, exists := ginContext.Get("API_RESPONSE_ERROR"); exists {
		if errorsList, valid := apiResponseErrors.([]*ErrorMessage); valid {
			ginContext.Set("API_RESPONSE_ERROR", append(errorsList, err))
		}
	} else {
		ginContext.Set("API_RESPONSE_ERROR", []*ErrorMessage{err})
	}

	requestContext := ctx
	if ginContext.Request != nil {
		requestContext = ginContext.Request.Context()
	}
	kind := logging.RequestOutcomeUpstreamError
	stage := "upstream_response"
	reason := ""
	if err != nil {
		if err.StatusCode >= http.StatusBadRequest && err.StatusCode < http.StatusInternalServerError {
			kind = logging.RequestOutcomeClientError
		}
		if err.Error != nil {
			reason = strings.TrimSpace(err.Error.Error())
		}
	}
	logging.MarkRequestOutcomeFailure(requestContext, kind, stage, reason)
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...any)
