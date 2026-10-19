package pluginabi

import "encoding/json"

const (
	ABIVersion uint32 = 1
	// SchemaVersion is the newest plugin contract understood by the host.
	SchemaVersion uint32 = 6
	// SchemaVersionStreamChunkOmitRequestBody is the first schema version that omits
	// OriginalRequest and RequestBody from payload stream-chunk interceptor calls.
	SchemaVersionStreamChunkOmitRequestBody uint32 = 3
	// SchemaVersionWebSocketResponseObserver is the first schema version that supports
	// upstream WebSocket response event observation.
	SchemaVersionWebSocketResponseObserver uint32 = 4
	// SchemaVersionStreamChunkOmitHistory is the first schema version that omits
	// HistoryChunks from payload stream-chunk interceptor calls.
	SchemaVersionStreamChunkOmitHistory uint32 = 5
	// SchemaVersionRawManagementResponse guarantees raw JSON management response bytes.
	SchemaVersionRawManagementResponse uint32 = 6
)

// These method names are part of the public plugin ABI.
// noinspection GoUnusedConst
const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginQuiesce     = "plugin.quiesce"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodModelRegister = "model.register"
	MethodModelStatic   = "model.static"
	MethodModelForAuth  = "model.for_auth"

	MethodAuthIdentifier = "auth.identifier"
	MethodAuthParse      = "auth.parse"
	MethodAuthLoginStart = "auth.login.start"
	MethodAuthLoginPoll  = "auth.login.poll"
	MethodAuthRefresh    = "auth.refresh"

	MethodFrontendAuthIdentifier   = "frontend_auth.identifier"
	MethodFrontendAuthAuthenticate = "frontend_auth.authenticate"

	// MethodSchedulerPick asks a scheduler plugin to select an auth candidate.
	MethodSchedulerPick = "scheduler.pick"

	MethodExecutorIdentifier    = "executor.identifier"
	MethodExecutorExecute       = "executor.execute"
	MethodExecutorExecuteStream = "executor.execute_stream"
	MethodExecutorCountTokens   = "executor.count_tokens"
	MethodExecutorHTTPRequest   = "executor.http_request"

	MethodRequestTranslate       = "request.translate"
	MethodRequestNormalize       = "request.normalize"
	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"
	MethodRequestComplete        = "request.complete"

	MethodResponseTranslate            = "response.translate"
	MethodResponseNormalizeBefore      = "response.normalize_before"
	MethodResponseNormalizeAfter       = "response.normalize_after"
	MethodResponseInterceptAfter       = "response.intercept_after"
	MethodResponseInterceptStreamChunk = "response.intercept_stream_chunk"

	MethodWebSocketResponseEvent = "websocket.response_event"

	MethodThinkingIdentifier = "thinking.identifier"
	MethodThinkingApply      = "thinking.apply"

	MethodUsageHandle = "usage.handle"

	MethodCommandLineRegister = "command_line.register"
	MethodCommandLineExecute  = "command_line.execute"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodQuotaIdentifier = "quota.identifier"
	MethodQuotaDescribe   = "quota.describe"
	MethodQuotaFetch      = "quota.fetch"
	MethodQuotaReset      = "quota.reset"

	MethodHostAffinityLookup       = "host.affinity.lookup"
	MethodHostRoutingResetCooldown = "host.routing.reset_cooldown"
	MethodHostHTTPDo               = "host.http.do"
	MethodHostHTTPDoStream         = "host.http.do_stream"
	MethodHostHTTPOperationOpen    = "host.http.operation_open"
	MethodHostHTTPCancel           = "host.http.cancel"
	MethodHostHTTPStreamRead       = "host.http.stream_read"
	MethodHostHTTPStreamClose      = "host.http.stream_close"
	MethodHostModelExecute         = "host.model.execute"
	MethodHostModelExecuteStream   = "host.model.execute_stream"
	MethodHostModelStreamRead      = "host.model.stream_read"
	MethodHostModelStreamClose     = "host.model.stream_close"
	MethodHostStreamEmit           = "host.stream.emit"
	MethodHostStreamClose          = "host.stream.close"
	MethodHostLog                  = "host.log"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// NewErrorEnvelope encodes a failed plugin response with an optional HTTP status.
func NewErrorEnvelope(code, message string, httpStatus ...int) ([]byte, error) {
	response := &Error{Code: code, Message: message}
	if len(httpStatus) > 0 {
		response.HTTPStatus = httpStatus[0]
	}
	return json.Marshal(Envelope{OK: false, Error: response})
}
