package live

import (
	"context"
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
	log "github.com/sirupsen/logrus"
)

// HandleTranslation reports that the Codex OAuth upstream has no translation session capability.
func (h *Handler) HandleTranslation(c *gin.Context) {
	writeCapabilityNotSupported(c, "Realtime translation sessions")
}

// HandleTranscriptionSession reports that the Codex OAuth upstream has no transcription-only capability.
func (h *Handler) HandleTranscriptionSession(c *gin.Context) {
	writeCapabilityNotSupported(c, "Realtime transcription-only sessions")
}

// HandleSIPControl reports that the Codex OAuth upstream has no SIP dialog capability.
func (h *Handler) HandleSIPControl(c *gin.Context) {
	action := "control"
	if c != nil && c.Request != nil && c.Request.URL != nil {
		parts := strings.Split(strings.Trim(c.Request.URL.Path, "/"), "/")
		if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) != "" {
			action = parts[len(parts)-1]
		}
	}
	writeCapabilityNotSupported(c, "Realtime SIP "+action)
}

// HandleHangup forwards hangup for a locally created WebRTC call using its pinned OAuth credential.
func (h *Handler) HandleHangup(c *gin.Context) {
	if h == nil || h.authManager == nil || h.sessions == nil {
		writeRealtimeError(
			c,
			http.StatusServiceUnavailable,
			"Codex live session service unavailable",
			"server_error",
			"realtime_session_unavailable",
		)
		return
	}
	if _, ephemeral := c.Get(ClientSecretPrincipalContextKey); ephemeral {
		writeRealtimeError(
			c,
			http.StatusForbidden,
			"Realtime client secrets cannot hang up calls",
			"invalid_request_error",
			"realtime_client_secret_scope_mismatch",
		)
		return
	}
	callID := strings.TrimSpace(c.Param("call_id"))
	if !callIDPattern.MatchString(callID) {
		writeRealtimeError(
			c,
			http.StatusBadRequest,
			"Invalid Realtime call ID",
			"invalid_request_error",
			"invalid_call_id",
		)
		return
	}
	session, ok := h.sessions.peek(callID)
	if !ok {
		writeRealtimeError(
			c,
			http.StatusNotFound,
			"Realtime call not found",
			"invalid_request_error",
			"realtime_call_not_found",
		)
		return
	}
	ownerPrincipal, ownerProvider := requestOwner(c)
	if session.ownerPrincipal != "" &&
		(ownerPrincipal != session.ownerPrincipal || ownerProvider != session.ownerProvider) {
		writeRealtimeError(
			c,
			http.StatusForbidden,
			"Realtime call belongs to another API principal",
			"invalid_request_error",
			"realtime_call_scope_mismatch",
		)
		return
	}

	ctx := util.WithGinContext(c.Request.Context(), c)
	selectionMetadata := liveSelectionMetadata(c, map[string]any{
		coreexecutor.ExecutionSessionMetadataKey: callID,
	})
	ctx = enrichLiveSessionContext(
		ctx,
		liveSelectionHeaders(c),
		nil,
		selectionMetadata,
	)
	if session.sessionID != "" {
		metadata := logging.GetClientRequestMetadata(ctx)
		metadata.SessionID = session.sessionID
		metadata.ParentSessionID = session.parentSessionID
		ctx = logging.WithClientRequestMetadata(ctx, metadata)
		ctx = util.WithSessionID(ctx, metadata.SessionID)
	}

	selection, selected, temporarySelection, errSelect := h.hangupSelection(ctx, c, session, callID)
	if errSelect != nil {
		writeSelectionError(c, errSelect)
		return
	}
	var releaseAttempt func()
	if selection != nil {
		attemptCtx, release, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			if temporarySelection {
				selection.End("attempt_bind_failed")
			}
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
		releaseAttempt = release
	}
	defer func() {
		if releaseAttempt != nil {
			releaseAttempt()
		}
		if temporarySelection && selection != nil {
			selection.End("request_closed")
		}
	}()
	if selected == nil {
		writeRealtimeError(
			c,
			http.StatusServiceUnavailable,
			"Codex auth unavailable",
			"server_error",
			"codex_auth_unavailable",
		)
		return
	}
	logging.SetGinCPATraceID(c, selected.EnsureIndex())

	body, errRead := readBody(c.Request.Body)
	if errRead != nil {
		writeRealtimeError(c, http.StatusBadRequest, errRead.Error(), "invalid_request_error", "invalid_request")
		return
	}
	upstreamURL := h.realtimeHTTPBaseURL() + "/realtime/calls/" + url.PathEscape(callID) + "/hangup"
	headers := protocolHeaders(c.Request.Header)
	if contentType := strings.TrimSpace(c.GetHeader("Content-Type")); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	setAccountHeader(headers, selected)
	request, errRequest := h.authManager.NewHttpRequest(
		ctx,
		selected,
		http.MethodPost,
		upstreamURL,
		body,
		headers,
	)
	if errRequest != nil {
		writeRealtimeError(
			c,
			clienterror.HTTPStatusFromErrorOr(errRequest, http.StatusBadGateway),
			errRequest.Error(),
			"api_error",
			"realtime_upstream_unavailable",
		)
		return
	}
	cfg := h.configSnapshot()
	runtimeexecutor.RecordAPIRequest(
		ctx,
		cfg,
		upstreamURL,
		http.MethodPost,
		headersForLogging(request.Header),
		body,
		"codex",
		selected,
	)
	response, errRequest := h.authManager.HttpRequest(ctx, selected, request)
	if errRequest != nil {
		runtimeexecutor.RecordAPIResponseError(ctx, cfg, errRequest)
		writeRealtimeError(
			c,
			clienterror.HTTPStatusFromErrorOr(errRequest, http.StatusBadGateway),
			errRequest.Error(),
			"api_error",
			"realtime_upstream_unavailable",
		)
		return
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Errorf("codex realtime hangup: close response body error: %v", errClose)
		}
	}()
	runtimeexecutor.RecordAPIResponseMetadata(ctx, cfg, response.StatusCode, callResponseHeaders(response.Header))
	responseBody, errResponse := readLimitedBody(response.Body)
	runtimeexecutor.AppendAPIResponseChunk(ctx, cfg, responseBody)
	if selection != nil && response.StatusCode == http.StatusUnauthorized {
		h.authManager.ReportHomeUnauthorized(ctx, selected, "codex", session.model, responseBody)
	}
	if errResponse != nil {
		runtimeexecutor.RecordAPIResponseError(ctx, cfg, errResponse)
		writeRealtimeError(
			c,
			http.StatusBadGateway,
			"Failed to read Realtime hangup response",
			"api_error",
			"realtime_upstream_unavailable",
		)
		return
	}
	if selection != nil && response.StatusCode == http.StatusUnauthorized {
		log.WithField("status", response.StatusCode).Warnf(
			"codex realtime hangup upstream request failed: %s",
			logging.SafeDiagnosticForLog(string(responseBody)),
		)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if releaseAttempt != nil {
			releaseAttempt()
			releaseAttempt = nil
		}
		h.sessions.complete(session, "client_hangup")
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		c.Header("Content-Type", contentType)
	}
	copyRealtimeHandshakeHeaders(c.Writer.Header(), response.Header)
	c.Status(response.StatusCode)
	if _, errWrite := c.Writer.Write(responseBody); errWrite != nil {
		log.WithError(errWrite).Warn("codex realtime hangup: write response body failed")
	}
}

func (h *Handler) hangupSelection(
	ctx context.Context,
	c *gin.Context,
	session liveSession,
	callID string,
) (*auth.HomeDispatchSelection, *auth.Auth, bool, error) {
	if session.homeSelection != nil && session.homeSelection.Active() {
		return session.homeSelection, session.homeSelection.CloneAuth(), false, nil
	}
	selectionOpts := coreexecutor.Options{
		Headers: liveSelectionHeaders(c),
		Metadata: liveSelectionMetadata(c, map[string]any{
			coreexecutor.PinnedAuthMetadataKey:       session.authID,
			coreexecutor.ExecutionSessionMetadataKey: callID,
		}),
	}
	selection, selected, errSelect := h.selectOAuth(ctx, session.model, selectionOpts)
	return selection, selected, selection != nil, errSelect
}

func (h *Handler) realtimeHTTPBaseURL() string {
	return strings.TrimRight(websocketHTTPURL(h.sidebandAPIBaseURL), "/")
}

func writeCapabilityNotSupported(c *gin.Context, capability string) {
	writeRealtimeError(
		c,
		http.StatusNotImplemented,
		"The ChatGPT/Codex OAuth upstream does not support "+capability,
		"not_supported_error",
		"realtime_capability_not_supported",
	)
}
