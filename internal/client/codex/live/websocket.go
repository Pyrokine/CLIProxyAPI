package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const defaultStandardRealtimeModel = "gpt-realtime"

// HandleRealtimeWebsocket dispatches a standard Realtime WebSocket or an existing call sideband.
func (h *Handler) HandleRealtimeWebsocket(c *gin.Context) {
	if strings.TrimSpace(c.Query("call_id")) != "" {
		h.HandleSideband(c)
		return
	}
	h.HandleDirectWebsocket(c)
}

// HandleDirectWebsocket relays a standard Realtime WebSocket through Codex OAuth.
func (h *Handler) HandleDirectWebsocket(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeRealtimeError(
			c,
			http.StatusServiceUnavailable,
			"Codex auth manager unavailable",
			"server_error",
			"codex_auth_unavailable",
		)
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.Header("Upgrade", "websocket")
		writeRealtimeError(
			c,
			http.StatusUpgradeRequired,
			"WebSocket upgrade required",
			"invalid_request_error",
			"websocket_upgrade_required",
		)
		return
	}

	requestedModel := strings.TrimSpace(c.Query("model"))
	if requestedModel == "" {
		requestedModel = defaultStandardRealtimeModel
	}
	selectionModel := codexRealtimeModel(requestedModel)
	tokenSession := clientSecretSession(c)
	if len(tokenSession) > 0 {
		tokenModel := codexRealtimeModel(modelFromJSON(tokenSession))
		if selectionModel != tokenModel {
			writeRealtimeError(
				c,
				http.StatusForbidden,
				"Realtime client secret is not valid for the requested model",
				"invalid_request_error",
				"realtime_client_secret_scope_mismatch",
			)
			return
		}
	}

	ctx := util.WithGinContext(c.Request.Context(), c)
	ctx = coreexecutor.WithDownstreamWebsocket(ctx)
	selectionHeaders := liveSelectionHeaders(c)
	selectionMetadata := liveSelectionMetadata(c, nil)
	ctx = enrichLiveSessionContext(ctx, selectionHeaders, nil, selectionMetadata)
	selectionOpts := coreexecutor.Options{
		Headers:  selectionHeaders,
		Metadata: selectionMetadata,
	}
	selection, selected, errSelect := h.selectOAuth(ctx, selectionModel, selectionOpts)
	if errSelect != nil {
		writeSelectionError(c, errSelect)
		return
	}
	if selected == nil {
		if selection != nil {
			selection.End("missing_auth")
		}
		writeRealtimeError(
			c,
			http.StatusServiceUnavailable,
			"Codex auth unavailable",
			"server_error",
			"codex_auth_unavailable",
		)
		return
	}
	if selection != nil {
		attemptCtx, releaseAttempt, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			selection.End("attempt_bind_failed")
			writeRealtimeError(
				c,
				http.StatusServiceUnavailable,
				errAttempt.Error(),
				"server_error",
				"realtime_upstream_unavailable",
			)
			return
		}
		ctx = attemptCtx
		defer releaseAttempt()
		selection.Retain()
		defer selection.End("session_closed")
	}
	logging.SetGinCPATraceID(c, selected.EnsureIndex())

	upstreamURL := h.directRealtimeURL(requestedModel)
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, websocketHTTPURL(upstreamURL), nil)
	if errRequest != nil {
		writeRealtimeError(
			c,
			http.StatusBadGateway,
			"Codex Realtime WebSocket upstream unavailable",
			"api_error",
			"realtime_websocket_upstream_unavailable",
		)
		return
	}
	request.Header = directRealtimeHeaders(c.Request.Header)
	setAccountHeader(request.Header, selected)
	if errPrepare := h.authManager.PrepareHttpRequest(ctx, selected, request); errPrepare != nil {
		writeRealtimeError(
			c,
			http.StatusBadGateway,
			"Codex Realtime WebSocket upstream unavailable",
			"api_error",
			"realtime_websocket_upstream_unavailable",
		)
		return
	}
	cfg := h.configSnapshot()
	runtimeexecutor.RecordAPIWebsocketRequest(
		ctx,
		cfg,
		upstreamURL,
		"WEBSOCKET",
		headersForLogging(request.Header),
		nil,
		"codex",
		selected,
	)
	dialer := newProxyAwareSidebandDialer(cfg, selected)
	dialer.Subprotocols = websocket.Subprotocols(c.Request)
	upstream, handshakeResponse, errDial := dialer.DialContext(ctx, upstreamURL, request.Header)
	if errDial != nil {
		h.handleDirectWebsocketDialError(c, ctx, selection, selected, selectionModel, handshakeResponse, errDial)
		return
	}
	closeHandshakeBody(handshakeResponse, "direct websocket handshake")
	closeUpstream := websocketCloseFunc("upstream", upstream)
	defer func() { _ = closeUpstream() }()

	if len(tokenSession) > 0 {
		updateSession, errSession := realtimeSessionUpdate(tokenSession)
		if errSession != nil {
			writeRealtimeError(
				c,
				http.StatusInternalServerError,
				"Failed to apply Realtime client secret session",
				"server_error",
				"realtime_session_failed",
			)
			return
		}
		update, errMarshal := json.Marshal(struct {
			Type    string          `json:"type"`
			Session json.RawMessage `json:"session"`
		}{Type: "session.update", Session: updateSession})
		if errMarshal != nil || upstream.WriteMessage(websocket.TextMessage, update) != nil {
			writeRealtimeError(
				c,
				http.StatusBadGateway,
				"Failed to apply Realtime client secret session",
				"api_error",
				"realtime_upstream_unavailable",
			)
			return
		}
	}
	if selection != nil {
		if errBind := selection.Bind(closeUpstream); errBind != nil {
			writeRealtimeError(
				c,
				http.StatusServiceUnavailable,
				errBind.Error(),
				"server_error",
				"realtime_upstream_unavailable",
			)
			return
		}
	}

	upgradeHeaders := make(http.Header)
	if subprotocol := upstream.Subprotocol(); subprotocol != "" {
		upgradeHeaders.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	downstream, errUpgrade := sidebandUpgrader.Upgrade(c.Writer, c.Request, upgradeHeaders)
	if errUpgrade != nil {
		return
	}
	closeDownstream := websocketCloseFunc("downstream", downstream)
	defer func() { _ = closeDownstream() }()
	if selection != nil {
		if errBind := selection.Bind(closeDownstream); errBind != nil {
			return
		}
	}
	if errRelay := relayWebsockets(downstream, upstream); errRelay != nil && !isNormalWebsocketClose(errRelay) {
		runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "relay", errRelay)
		log.WithError(errRelay).Debug("codex realtime direct websocket relay closed")
	}
}

func (h *Handler) handleDirectWebsocketDialError(
	c *gin.Context,
	ctx context.Context,
	selection *auth.HomeDispatchSelection,
	selected *auth.Auth,
	selectionModel string,
	response *http.Response,
	errDial error,
) {
	status := clienterror.HTTPStatusFromErrorOr(errDial, http.StatusBadGateway)
	cfg := h.configSnapshot()
	var responseBody []byte
	if response != nil {
		if response.StatusCode > 0 {
			status = response.StatusCode
		}
		copyRealtimeHandshakeHeaders(c.Writer.Header(), response.Header)
		runtimeexecutor.RecordAPIWebsocketHandshake(ctx, cfg, response.StatusCode, callResponseHeaders(response.Header))
		if response.Body != nil {
			var errRead error
			responseBody, errRead = readLimitedBody(response.Body)
			runtimeexecutor.AppendAPIWebsocketResponse(ctx, cfg, responseBody)
			if errRead != nil {
				runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "read", errRead)
				log.WithError(errRead).Warn("codex realtime: read rejected handshake body")
			}
		}
	}
	closeHandshakeBody(response, "direct websocket rejected")
	if selection != nil && status == http.StatusUnauthorized {
		diagnosticBody := responseBody
		if len(diagnosticBody) == 0 {
			diagnosticBody = []byte(errDial.Error())
		}
		h.authManager.ReportHomeUnauthorized(ctx, selected, "codex", selectionModel, diagnosticBody)
		log.WithField("status", status).Warnf(
			"codex realtime websocket upstream handshake failed: %s",
			logging.SafeDiagnosticForLog(string(diagnosticBody)),
		)
	}
	runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "dial", errDial)
	if response != nil && response.StatusCode == http.StatusUnauthorized {
		if contentType := response.Header.Get("Content-Type"); contentType != "" {
			c.Header("Content-Type", contentType)
		}
		c.Status(response.StatusCode)
		if len(responseBody) > 0 {
			if _, errWrite := c.Writer.Write(responseBody); errWrite != nil {
				log.WithError(errWrite).Warn("codex realtime: write rejected handshake body failed")
			}
		}
		return
	}
	message := "Codex Realtime WebSocket upstream unavailable"
	errorType := "api_error"
	code := "realtime_websocket_upstream_unavailable"
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		status = http.StatusNotImplemented
		message = "Direct Realtime WebSocket is not supported by the Codex OAuth upstream"
		errorType = "not_supported_error"
		code = "realtime_capability_not_supported"
	} else if status == http.StatusUnauthorized {
		errorType = "authentication_error"
		code = "realtime_upstream_unauthorized"
	}
	writeRealtimeError(c, status, message, errorType, code)
}

func realtimeSessionUpdate(session json.RawMessage) (json.RawMessage, error) {
	var update map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(session, &update); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	for _, field := range []string{"model", "id", "object", "expires_at", "client_secret"} {
		delete(update, field)
	}
	return json.Marshal(update)
}

func (h *Handler) directRealtimeURL(model string) string {
	values := make(url.Values)
	values.Set("model", strings.TrimSpace(model))
	return strings.TrimRight(h.sidebandAPIBaseURL, "/") + "/realtime?" + values.Encode()
}

func directRealtimeHeaders(source http.Header) http.Header {
	headers := protocolHeaders(source)
	headers.Del("OpenAI-Alpha")
	if headers.Get("Originator") == "" {
		headers.Set("Originator", "Codex Desktop")
	}
	return headers
}

func copyRealtimeHandshakeHeaders(destination, source http.Header) {
	for _, name := range []string{"Retry-After", "X-Request-Id", "OpenAI-Request-Id"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func closeHandshakeBody(response *http.Response, label string) {
	if response == nil || response.Body == nil {
		return
	}
	if errClose := response.Body.Close(); errClose != nil {
		log.Errorf("codex realtime: close %s response body error: %v", label, errClose)
	}
}
