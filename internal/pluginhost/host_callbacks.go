package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

type rpcHostHTTPRequest struct {
	HTTPClientID   string                     `json:"http_client_id,omitempty"`
	HostCallbackID string                     `json:"host_callback_id,omitempty"`
	OperationID    string                     `json:"operation_id,omitempty"`
	Method         string                     `json:"method,omitempty"`
	URL            string                     `json:"url,omitempty"`
	Headers        httpHeader                 `json:"headers,omitempty"`
	Body           []byte                     `json:"body,omitempty"`
	WireProfile    *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
	Request        *httpRequest               `json:"request,omitempty"`
}

type httpHeader map[string][]string

type httpRequest struct {
	Method      string                     `json:"method,omitempty"`
	URL         string                     `json:"url,omitempty"`
	Headers     httpHeader                 `json:"headers,omitempty"`
	Body        []byte                     `json:"body,omitempty"`
	WireProfile *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int                         `json:"status_code"`
	Headers    httpHeader                  `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPOperationOpenRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostHTTPOperationOpenResponse struct {
	OperationID string `json:"operation_id"`
}

type rpcHostHTTPCancelRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id"`
}

type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type dynamicHostCallbackEntry struct {
	host     *Host
	pluginID string
	instance *hostCallbackInstance
}

type hostCallbackInstance struct {
	id     uintptr
	closed atomic.Bool
}

type hostCallbackPluginIDKey struct{}
type hostCallbackInstanceIDKey struct{}
type hostCallbackInstanceKey struct{}

func withHostCallbackInstance(ctx context.Context, instance *hostCallbackInstance) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if instance == nil {
		return ctx
	}
	return context.WithValue(ctx, hostCallbackInstanceKey{}, instance)
}

func hostCallbackInstanceFromContext(ctx context.Context) *hostCallbackInstance {
	if ctx == nil {
		return nil
	}
	instance, _ := ctx.Value(hostCallbackInstanceKey{}).(*hostCallbackInstance)
	return instance
}

func withHostCallbackInstanceID(ctx context.Context, instanceID uintptr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if instanceID == 0 {
		return ctx
	}
	return context.WithValue(ctx, hostCallbackInstanceIDKey{}, instanceID)
}

func hostCallbackInstanceIDFromContext(ctx context.Context) uintptr {
	if ctx == nil {
		return 0
	}
	instanceID, _ := ctx.Value(hostCallbackInstanceIDKey{}).(uintptr)
	return instanceID
}

func withHostCallbackPluginID(ctx context.Context, pluginID string) context.Context {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		if ctx == nil {
			return context.Background()
		}
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, hostCallbackPluginIDKey{}, pluginID)
}

func hostCallbackPluginIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	pluginID, _ := ctx.Value(hostCallbackPluginIDKey{}).(string)
	return strings.TrimSpace(pluginID)
}

func (h *Host) callFromPlugin(ctx context.Context, method string, request []byte) ([]byte, error) {
	if instance := hostCallbackInstanceFromContext(ctx); instance != nil && instance.closed.Load() {
		return nil, fmt.Errorf("host plugin callback instance is closed")
	}
	switch method {
	case pluginabi.MethodHostAffinityLookup:
		return h.callHostAffinityLookup(ctx, request)
	case pluginabi.MethodHostRoutingResetCooldown:
		return h.callHostRoutingResetCooldown(ctx, request)
	case pluginabi.MethodHostModelExecute:
		return h.callHostModelExecute(ctx, request)
	case pluginabi.MethodHostModelExecuteStream:
		return h.callHostModelExecuteStream(ctx, request)
	case pluginabi.MethodHostModelStreamRead:
		return h.callHostModelStreamRead(ctx, request)
	case pluginabi.MethodHostModelStreamClose:
		return h.callHostModelStreamClose(ctx, request)
	case pluginabi.MethodHostHTTPDo:
		return h.callHostHTTPDo(ctx, request)
	case pluginabi.MethodHostHTTPDoStream:
		return h.callHostHTTPDoStream(ctx, request)
	case pluginabi.MethodHostHTTPOperationOpen:
		return h.callHostHTTPOperationOpen(ctx, request)
	case pluginabi.MethodHostHTTPCancel:
		return h.callHostHTTPCancel(ctx, request)
	case pluginabi.MethodHostHTTPStreamRead:
		return h.callHostHTTPStreamRead(ctx, request)
	case pluginabi.MethodHostHTTPStreamClose:
		return h.callHostHTTPStreamClose(ctx, request)
	case pluginabi.MethodHostStreamEmit:
		return h.callHostStreamEmit(ctx, request)
	case pluginabi.MethodHostStreamClose:
		return h.callHostStreamClose(request)
	case pluginabi.MethodHostLog:
		return h.callHostLog(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported host callback %s", method)
	}
}

func (h *Host) callbackCallerPluginID(ctx context.Context, callbackID string) string {
	if pluginID := hostCallbackPluginIDFromContext(ctx); pluginID != "" {
		return pluginID
	}
	return h.callbackContextPluginID(callbackID)
}

func (h *Host) callHostHTTPDo(ctx context.Context, request []byte) ([]byte, error) {
	httpReq, callbackID, operationID, errDecode := decodeHostHTTPRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	operation, errAcquire := h.acquireHostHTTPOperation(ctx, callbackID, operationID)
	if errAcquire != nil {
		return nil, errAcquire
	}
	defer operation.finish()
	resp, errDo := h.newHTTPClient(nil).Do(operation.ctx, httpReq)
	if errDo != nil {
		return nil, errDo
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostHTTPDoStream(ctx context.Context, request []byte) ([]byte, error) {
	httpReq, callbackID, operationID, errDecode := decodeHostHTTPRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	operation, errAcquire := h.acquireHostHTTPOperation(ctx, callbackID, operationID)
	if errAcquire != nil {
		return nil, errAcquire
	}
	var streamCtx context.Context
	var cancelStream context.CancelFunc
	if _, hasDeadline := operation.ctx.Deadline(); hasDeadline {
		streamCtx, cancelStream = context.WithCancel(operation.ctx)
	} else {
		streamCtx, cancelStream = context.WithTimeout(operation.ctx, pluginHTTPDefaultTimeout)
	}
	streamID := ""
	keepStream := false
	defer func() {
		if !keepStream {
			if streamID != "" && h.httpStreams != nil {
				h.httpStreams.close(operation.pluginID, hostCallbackInstanceIDFromContext(ctx), streamID)
			}
			cancelStream()
			operation.finish()
		}
	}()
	resp, errDo := h.newHTTPClient(nil).DoStream(streamCtx, httpReq)
	if errDo != nil {
		return nil, errDo
	}
	if h.httpStreams == nil {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	instanceID := hostCallbackInstanceIDFromContext(ctx)
	streamID = h.httpStreams.open(streamCtx, operation.pluginID, instanceID, resp.Chunks, cancelStream, operation.finish)
	if streamID == "" {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	rawResp, errMarshal := marshalRPCResult(rpcHostHTTPStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    httpHeader(resp.Headers),
		StreamID:   streamID,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	if !h.httpOperations.setStreamCleanup(operation.pluginID, operation.operationID, operation.entry, func() {
		h.httpStreams.close(operation.pluginID, instanceID, streamID)
	}) {
		return nil, context.Canceled
	}
	keepStream = true
	return rawResp, nil
}

func (h *Host) callHostHTTPOperationOpen(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostHTTPOperationOpenRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http operation open request: %w", errUnmarshal)
	}
	operationID, _, _, _, errOpen := h.createHostHTTPOperation(ctx, req.HostCallbackID, false)
	if errOpen != nil {
		return nil, errOpen
	}
	return marshalRPCResult(rpcHostHTTPOperationOpenResponse{OperationID: operationID})
}

func (h *Host) callHostHTTPCancel(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostHTTPCancelRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http cancel request: %w", errUnmarshal)
	}
	operationID := strings.TrimSpace(req.OperationID)
	if operationID == "" {
		return nil, fmt.Errorf("host http operation id is required")
	}
	if h != nil && h.httpOperations != nil {
		pluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
		instance := hostCallbackInstanceFromContext(ctx)
		if instance == nil {
			_, _, instance, _ = h.lookupCallbackContext(req.HostCallbackID)
		}
		h.httpOperations.cancel(pluginID, instance, operationID)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostHTTPStreamRead(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostHTTPStreamReadRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream read request: %w", errUnmarshal)
	}
	if h == nil || h.httpStreams == nil {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	chunk, done, errRead := h.httpStreams.read(
		ctx, hostCallbackPluginIDFromContext(ctx), hostCallbackInstanceIDFromContext(ctx), req.StreamID,
	)
	if errRead != nil {
		return nil, errRead
	}
	resp := rpcHostHTTPStreamReadResponse{
		Payload: append([]byte(nil), chunk.Payload...),
		Done:    done,
	}
	if chunk.Err != nil {
		resp.Error = chunk.Err.Error()
		resp.Done = true
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostHTTPStreamClose(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostHTTPStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream close request: %w", errUnmarshal)
	}
	if h != nil && h.httpStreams != nil {
		h.httpStreams.close(hostCallbackPluginIDFromContext(ctx), hostCallbackInstanceIDFromContext(ctx), req.StreamID)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func decodeHostHTTPRequest(raw []byte) (pluginapi.HTTPRequest, string, string, error) {
	var req rpcHostHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return pluginapi.HTTPRequest{}, "", "", fmt.Errorf("decode host http request: %w", errUnmarshal)
	}
	if req.Request != nil {
		wireProfile := req.Request.WireProfile
		if wireProfile == nil {
			wireProfile = req.WireProfile
		}
		return pluginapi.HTTPRequest{
			Method:      req.Request.Method,
			URL:         req.Request.URL,
			Headers:     cloneHeader(http.Header(req.Request.Headers)),
			Body:        append([]byte(nil), req.Request.Body...),
			WireProfile: cloneHTTPWireProfile(wireProfile),
		}, req.HostCallbackID, req.OperationID, nil
	}
	return pluginapi.HTTPRequest{
		Method:      req.Method,
		URL:         req.URL,
		Headers:     cloneHeader(http.Header(req.Headers)),
		Body:        append([]byte(nil), req.Body...),
		WireProfile: cloneHTTPWireProfile(req.WireProfile),
	}, req.HostCallbackID, req.OperationID, nil
}

func cloneHTTPWireProfile(src *pluginapi.HTTPWireProfile) *pluginapi.HTTPWireProfile {
	if src == nil {
		return nil
	}
	out := *src
	out.HeaderProfile = append([]string(nil), src.HeaderProfile...)
	return &out
}

func (h *Host) callHostStreamEmit(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcStreamEmitRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode stream emit request: %w", errUnmarshal)
	}
	chunk := pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), req.Payload...)}
	if req.Error != "" {
		chunk.Err = fmt.Errorf("%s", req.Error)
	}
	if errEmit := h.streams.emit(ctx, req.StreamID, chunk); errEmit != nil {
		return nil, errEmit
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostStreamClose(request []byte) ([]byte, error) {
	var req rpcStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode stream close request: %w", errUnmarshal)
	}
	h.streams.close(req.StreamID, req.Error)
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostModelExecute(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model execution request: %w", errUnmarshal)
	}
	if req.Stream {
		return nil, fmt.Errorf("host.model.execute requires stream=false")
	}
	if errProxy := validateHostModelProxy(req.ProxyURL); errProxy != nil {
		return nil, errProxy
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	ctx, errResolve := h.resolvePluginCallbackContext(ctx, req.HostCallbackID)
	if errResolve != nil {
		return nil, errResolve
	}
	ctx = coreusage.WithoutDeferredRecord(ctx)
	resp, errMsg := executor.ExecuteModel(
		ctx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID),
	)
	if errMsg != nil {
		return nil, modelExecutionError(errMsg)
	}
	return marshalRPCResult(
		pluginapi.HostModelExecutionResponse{
			StatusCode: resp.StatusCode,
			Headers:    cloneHeader(resp.Headers),
			Body:       append([]byte(nil), resp.Body...),
		},
	)
}

func (h *Host) callHostModelExecuteStream(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model execution stream request: %w", errUnmarshal)
	}
	if !req.Stream {
		return nil, fmt.Errorf("host.model.execute_stream requires stream=true")
	}
	if errProxy := validateHostModelProxy(req.ProxyURL); errProxy != nil {
		return nil, errProxy
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	ctx, errResolve := h.resolvePluginCallbackContext(ctx, req.HostCallbackID)
	if errResolve != nil {
		return nil, errResolve
	}
	ctx = coreusage.WithoutDeferredRecord(ctx)
	streamCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	stream, errMsg := executor.ExecuteModelStream(
		streamCtx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID),
	)
	if errMsg != nil {
		return nil, modelExecutionError(errMsg)
	}
	streamID := ""
	if h != nil && h.modelStreams != nil {
		streamID = h.modelStreams.open(streamCtx, stream.Chunks, cancel)
		cancel = nil
	}
	if streamID == "" {
		return nil, fmt.Errorf("host model stream bridge is unavailable")
	}
	if req.HostCallbackID != "" {
		removeCleanup, _ := h.addCallbackCleanupHandle(
			req.HostCallbackID, func() {
				_ = h.modelStreams.close(streamCtx, streamID)
			},
		)
		h.modelStreams.setCleanup(streamCtx, streamID, removeCleanup)
	}
	return marshalRPCResult(
		pluginapi.HostModelStreamResponse{
			StatusCode: stream.StatusCode,
			Headers:    cloneHeader(stream.Headers),
			StreamID:   streamID,
		},
	)
}

func (h *Host) callHostModelStreamRead(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamReadRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream read request: %w", errUnmarshal)
	}
	if h == nil || h.modelStreams == nil {
		return nil, fmt.Errorf("host model stream bridge is unavailable")
	}
	chunk, done, errRead := h.modelStreams.read(ctx, req.StreamID)
	if errRead != nil {
		return nil, errRead
	}
	resp := pluginapi.HostModelStreamReadResponse{
		Payload: append([]byte(nil), chunk.Payload...),
		Done:    done,
	}
	if chunk.Err != nil {
		resp.Error = chunk.Err.Error()
		resp.Done = true
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostModelStreamClose(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream close request: %w", errUnmarshal)
	}
	if h != nil && h.modelStreams != nil {
		if errClose := h.modelStreams.close(ctx, req.StreamID); errClose != nil {
			return nil, errClose
		}
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func modelExecutionRequestFromPlugin(
	req pluginapi.HostModelExecutionRequest,
	skipPluginID string,
) handlers.ModelExecutionRequest {
	return handlers.ModelExecutionRequest{
		EntryProtocol:           req.EntryProtocol,
		ExitProtocol:            req.ExitProtocol,
		Model:                   req.Model,
		Stream:                  req.Stream,
		Body:                    append([]byte(nil), req.Body...),
		Headers:                 cloneHeader(req.Headers),
		Query:                   cloneValues(req.Query),
		Alt:                     req.Alt,
		SkipInterceptorPluginID: skipPluginID,
		ForcedProvider:          req.ForcedProvider,
		AuthID:                  req.AuthID,
		ProxyURL:                strings.TrimSpace(req.ProxyURL),
		Path:                    strings.TrimSpace(req.Path),
	}
}

func validateHostModelProxy(raw string) error {
	if raw = strings.TrimSpace(raw); raw != "" && !proxyutil.ValidRequestProxy(raw) {
		return &modelExecutionStatusError{err: fmt.Errorf("invalid proxy_url"), statusCode: http.StatusBadRequest}
	}
	return nil
}

type modelExecutionStatusError struct {
	err        error
	statusCode int
}

func (e *modelExecutionStatusError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("model execution failed with status %d", e.statusCode)
}

func (e *modelExecutionStatusError) StatusCode() int {
	return e.statusCode
}

func (e *modelExecutionStatusError) Unwrap() error {
	return e.err
}

func modelExecutionError(errMsg *handlers.ErrorMessage) error {
	if errMsg == nil {
		return nil
	}
	if errMsg.StatusCode > 0 && clienterror.HTTPStatusFromError(errMsg.Error) != errMsg.StatusCode {
		return &modelExecutionStatusError{err: errMsg.Error, statusCode: errMsg.StatusCode}
	}
	if errMsg.Error != nil {
		return errMsg.Error
	}
	return fmt.Errorf("model execution failed")
}

func (h *Host) callHostLog(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostLogRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host log request: %w", errUnmarshal)
	}
	ctx, errResolve := h.resolvePluginCallbackContext(ctx, req.HostCallbackID)
	if errResolve != nil {
		return nil, errResolve
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		message = "plugin log"
	}
	fields := log.Fields{}
	for key, value := range req.Fields {
		key = strings.TrimSpace(key)
		if key != "" {
			fields[key] = value
		}
	}
	if requestID := logging.GetRequestID(ctx); requestID != "" {
		fields["request_id"] = requestID
	}
	entry := log.WithFields(fields)
	switch strings.ToLower(strings.TrimSpace(req.Level)) {
	case "trace":
		entry.Trace(message)
	case "info":
		entry.Info(message)
	case "warn", "warning":
		entry.Warn(message)
	case "error":
		entry.Error(message)
	default:
		entry.Debug(message)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}
