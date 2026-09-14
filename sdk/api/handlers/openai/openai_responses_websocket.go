package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate                   = "response.create"
	wsRequestTypeAppend                   = "response.append"
	wsEventTypeError                      = "error"
	wsEventTypeCompleted                  = "response.completed"
	wsEventTypeDone                       = "response.done"
	wsDoneMarker                          = "[DONE]"
	wsTurnStateHeader                     = "x-codex-turn-state"
	wsTimelineBodyKey                     = "WEBSOCKET_TIMELINE_OVERRIDE"
	wsPayloadLogMaxSize                   = 2048
	wsCloseReasonMaxBytes                 = 123
	wsHTTPReplayRequiredCloseReason       = "upstream requires HTTP replay"
	responsesWebsocketWriteTimeout        = 30 * time.Second
	responsesWebsocketUpstreamModeUnknown = ""
	responsesWebsocketUpstreamModeWS      = "websocket"
	responsesWebsocketUpstreamModeHTTP    = "http"

	codexLocalCompactionSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"
)

type responsesWebsocketLineage int

type responsesWebsocketResponseOwner struct {
	authID               string
	supportsContinuation bool
}

func rememberResponsesWebsocketResponseOwner(
	owners map[string]responsesWebsocketResponseOwner,
	responseID string,
	authID string,
	supportsContinuation bool,
) {
	responseID = strings.TrimSpace(responseID)
	authID = strings.TrimSpace(authID)
	if responseID == "" || authID == "" {
		return
	}
	if _, exists := owners[responseID]; exists {
		return
	}
	owners[responseID] = responsesWebsocketResponseOwner{
		authID:               authID,
		supportsContinuation: supportsContinuation,
	}
}

const (
	responsesWebsocketLineageLocal responsesWebsocketLineage = iota
	responsesWebsocketLineageFullTranscript
	responsesWebsocketLineageReplacement
	responsesWebsocketLineageExternal
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients don't send Origin
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == r.Host || host+":"+u.Port() == r.Host
	},
}

func websocketClosePayloadForUpstreamError(err error) (bool, []byte) {
	if err == nil {
		return false, nil
	}

	if cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		return true, websocket.FormatCloseMessage(
			websocket.CloseServiceRestart,
			truncateWebsocketCloseReason(wsHTTPReplayRequiredCloseReason, wsCloseReasonMaxBytes),
		)
	}

	code := 0
	reason := ""
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		code = closeErr.Code
		reason = closeErr.Text
	} else {
		var statusErr interface{ StatusCode() int }
		errText := err.Error()
		if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusRequestEntityTooLarge ||
			gjson.Get(errText, "error.code").String() != "message_too_big" {
			return false, nil
		}
		code = websocket.CloseMessageTooBig
		reason = strings.TrimSpace(gjson.Get(errText, "error.message").String())
	}
	if reason == "" {
		reason = "message too big"
	}
	return true, websocket.FormatCloseMessage(code, truncateWebsocketCloseReason(reason, wsCloseReasonMaxBytes))
}

type responsesWebsocketWriter struct {
	conn         *websocket.Conn
	writeTimeout time.Duration
	writeMu      sync.Mutex
	closing      atomic.Bool
}

func newResponsesWebsocketWriter(conn *websocket.Conn) *responsesWebsocketWriter {
	return &responsesWebsocketWriter{conn: conn, writeTimeout: responsesWebsocketWriteTimeout}
}

func (w *responsesWebsocketWriter) writeText(payload []byte) error {
	if w == nil || w.conn == nil {
		return fmt.Errorf("responses websocket: writer is nil")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.closing.Load() {
		return websocket.ErrCloseSent
	}
	return w.writeMessageLocked(websocket.TextMessage, payload)
}

func (w *responsesWebsocketWriter) writeMessageLocked(messageType int, payload []byte) (err error) {
	if w == nil || w.conn == nil {
		return fmt.Errorf("responses websocket: writer is nil")
	}
	if w.writeTimeout > 0 {
		if errDeadline := w.conn.SetWriteDeadline(time.Now().Add(w.writeTimeout)); errDeadline != nil {
			return errDeadline
		}
		defer func() {
			err = errors.Join(err, w.conn.SetWriteDeadline(time.Time{}))
		}()
	}
	return w.conn.WriteMessage(messageType, payload)
}

// closeForUpstreamError never waits behind a blocked downstream data writer.
func (w *responsesWebsocketWriter) closeForUpstreamError(err error) (bool, bool, error) {
	if w == nil || w.conn == nil {
		return false, false, nil
	}
	matched, payload := websocketClosePayloadForUpstreamError(err)
	if !matched {
		return false, false, nil
	}
	if !w.closing.CompareAndSwap(false, true) {
		return true, false, nil
	}
	if !w.writeMu.TryLock() {
		return true, false, w.conn.Close()
	}
	defer w.writeMu.Unlock()

	deadline := time.Time{}
	if w.writeTimeout > 0 {
		deadline = time.Now().Add(w.writeTimeout)
	}
	errWrite := w.conn.WriteControl(websocket.CloseMessage, payload, deadline)
	errClose := w.conn.Close()
	if errWrite != nil {
		return true, false, errWrite
	}
	return true, true, errClose
}

func (w *responsesWebsocketWriter) closeWithoutError() (bool, error) {
	if w == nil || w.conn == nil {
		return false, nil
	}
	if !w.closing.CompareAndSwap(false, true) {
		return false, nil
	}
	return true, w.conn.Close()
}

func (w *responsesWebsocketWriter) closeWithPayload(payload []byte) (bool, error) {
	if w == nil || w.conn == nil {
		return false, nil
	}
	if !w.closing.CompareAndSwap(false, true) {
		return false, nil
	}
	if !w.writeMu.TryLock() {
		return false, w.conn.Close()
	}
	defer w.writeMu.Unlock()

	errWrite := w.writeMessageLocked(websocket.TextMessage, payload)
	errClose := w.conn.Close()
	if errWrite != nil {
		return false, errWrite
	}
	return true, errClose
}

func isWebsocketConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, websocket.ErrCloseSent) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

func markResponsesWebsocketTransportFailure(c *gin.Context, err error) {
	if c == nil || c.Request == nil || err == nil || isWebsocketConnectionClosedError(err) ||
		websocket.IsCloseError(
			err,
			websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
		) {
		return
	}
	requestContext := c.Request.Context()
	if state := internallogging.RequestOutcomeStateFromContext(requestContext); state != nil && state.Snapshot().Failed {
		return
	}
	internallogging.MarkRequestOutcomeFailure(
		requestContext,
		internallogging.RequestOutcomeWebsocketError,
		"websocket",
		err.Error(),
	)
}

func truncateWebsocketCloseReason(reason string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	reason = strings.TrimSpace(reason)
	if len(reason) <= maxBytes {
		return reason
	}
	truncated := reason[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *ResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	conn.SetReadLimit(10 << 20) // 10MB per message
	writer := newResponsesWebsocketWriter(conn)
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	retainResponsesWebsocketToolCaches(downstreamSessionKey)
	passthroughSessionID := uuid.NewString()
	clientRemoteAddr := responsesWebsocketClientIP(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientRemoteAddr)
	var wsTerminateErr error
	wsTimelineLog := newWebsocketTimelineRecorder(c)
	defer func() {
		releaseResponsesWebsocketToolCaches(downstreamSessionKey)
		markResponsesWebsocketTransportFailure(c, wsTerminateErr)
		if wsTerminateErr != nil {
			appendWebsocketEvent(wsTimelineLog, "disconnect", []byte(wsTerminateErr.Error()))
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		setWebsocketTimelineBody(c, wsTimelineLog.String())
		if errClose := conn.Close(); errClose != nil && !isWebsocketConnectionClosedError(errClose) {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	lastResponseID := ""
	lastResponseOwnerAuthID := ""
	lastResponseOwnerSupportsContinuation := false
	responseOwners := make(map[string]responsesWebsocketResponseOwner)
	var lastResponsePendingToolCallIDs []string
	var replayRequest []byte
	pinnedAuthID := ""
	pinnedAuthSupportsContinuation := false
	pinnedResponseID := ""
	forceTranscriptReplayNextRequest := false
	var replacementRetryRequest []byte
	replacementRetryOwnerID := ""
	replacementRetryOwnerSupportsContinuation := false
	upstreamMode := responsesWebsocketUpstreamModeUnknown
	upstreamWebsocketAuthID := ""

	for {
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			if websocket.IsCloseError(
				errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived,
			) {
				log.Infof(
					"responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage,
				)
			} else {
				// log.Warnf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		appendWebsocketEvent(wsTimelineLog, "request", payload)

		framePinnedAuthID := pinnedAuthID
		framePinnedAuthSupportsContinuation := pinnedAuthSupportsContinuation
		framePinnedResponseID := pinnedResponseID
		frameForceTranscriptReplay := forceTranscriptReplayNextRequest
		frameReplayRequest := bytes.Clone(replayRequest)
		frameReplacementRetryRequest := bytes.Clone(replacementRetryRequest)
		frameReplacementRetryOwnerID := replacementRetryOwnerID
		frameReplacementRetryOwnerSupportsContinuation := replacementRetryOwnerSupportsContinuation

		rawPreviousResponseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
		if upstreamMode == responsesWebsocketUpstreamModeUnknown && rawPreviousResponseID != "" {
			errMsg := responsesWebsocketPreviousResponseNotFoundError()
			_, errWrite := h.sendWebsocketError(c, writer, errMsg, wsTimelineLog, passthroughSessionID)
			if errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			continue
		}
		unknownPreviousResponseOwner := false
		if rawPreviousResponseID != "" {
			knownOwner, hasKnownOwner := responseOwners[rawPreviousResponseID]
			unknownPreviousResponseOwner = !hasKnownOwner
			forceReplayCurrentResponse := forceTranscriptReplayNextRequest &&
				rawPreviousResponseID == strings.TrimSpace(lastResponseID)
			switch {
			case !forceReplayCurrentResponse && hasKnownOwner && knownOwner.authID != "":
				pinnedAuthID = knownOwner.authID
				pinnedAuthSupportsContinuation = knownOwner.supportsContinuation
				pinnedResponseID = rawPreviousResponseID
			case pinnedAuthID != "" && pinnedAuthID == lastResponseOwnerAuthID:
			case rawPreviousResponseID != strings.TrimSpace(pinnedResponseID):
				pinnedAuthID = ""
				pinnedAuthSupportsContinuation = false
				pinnedResponseID = ""
			}
		} else if pinnedResponseID != "" && pinnedResponseID != strings.TrimSpace(lastResponseID) &&
			lastResponseOwnerAuthID != "" {
			pinnedAuthID = lastResponseOwnerAuthID
			pinnedAuthSupportsContinuation = lastResponseOwnerSupportsContinuation
			pinnedResponseID = lastResponseID
		}
		if forceTranscriptReplayNextRequest && rawPreviousResponseID != "" &&
			rawPreviousResponseID != strings.TrimSpace(lastResponseID) {
			forceTranscriptReplayNextRequest = false
			replayRequest = nil
		}

		replacementRetryBaseline := bytes.Clone(replacementRetryRequest)
		retryingTranscriptReplacement := len(replacementRetryBaseline) > 0
		input := gjson.GetBytes(payload, "input")
		resumeCommittedContinuation := retryingTranscriptReplacement &&
			((rawPreviousResponseID != "" && rawPreviousResponseID == strings.TrimSpace(lastResponseID)) ||
				(rawPreviousResponseID == "" && input.Exists() && input.IsArray() &&
					inputSatisfiesPendingToolCalls(input, lastResponsePendingToolCallIDs)))
		if resumeCommittedContinuation {
			if replacementRetryOwnerID != "" {
				pinnedAuthID = replacementRetryOwnerID
				pinnedAuthSupportsContinuation = replacementRetryOwnerSupportsContinuation
				pinnedResponseID = lastResponseID
			}
			replacementRetryRequest = nil
			replacementRetryOwnerID = ""
			replacementRetryOwnerSupportsContinuation = false
			retryingTranscriptReplacement = false
		} else if retryingTranscriptReplacement && rawPreviousResponseID != "" {
			replacementRetryRequest = nil
			replacementRetryOwnerID = ""
			replacementRetryOwnerSupportsContinuation = false
			retryingTranscriptReplacement = false
		}

		toolCacheTurn := newResponsesWebsocketToolCacheTurn(downstreamSessionKey)
		previousLastRequest := bytes.Clone(lastRequest)
		previousLastResponseOutput := bytes.Clone(lastResponseOutput)
		previousLastResponseID := lastResponseID
		previousLastResponsePendingToolCallIDs := append([]string(nil), lastResponsePendingToolCallIDs...)
		forceTranscriptReplacement := !retryingTranscriptReplacement && forceTranscriptReplayNextRequest &&
			rawPreviousResponseID == "" && len(previousLastRequest) > 0 &&
			!inputSatisfiesPendingToolCalls(gjson.GetBytes(payload, "input"), previousLastResponsePendingToolCallIDs)
		forcedTranscriptReplay := forceTranscriptReplayNextRequest && !forceTranscriptReplacement
		normalizationPreviousRequest := previousLastRequest
		if forcedTranscriptReplay && len(replayRequest) > 0 {
			normalizationPreviousRequest = bytes.Clone(replayRequest)
		}

		requestRequiresCurrentUpstreamWebsocket := responsesWebsocketRequestRequiresCurrentUpstream(payload)
		nativeWebsocketContinuation := false
		if upstreamMode == responsesWebsocketUpstreamModeWS && pinnedAuthID != "" &&
			pinnedAuthID == upstreamWebsocketAuthID && h != nil && h.AuthManager != nil {
			if pinnedAuth, ok := h.AuthManager.GetByID(pinnedAuthID); ok {
				nativeWebsocketContinuation = responsesWebsocketAuthUsesUpstreamWebsocket(pinnedAuth)
			}
		}
		if upstreamMode == responsesWebsocketUpstreamModeWS && !nativeWebsocketContinuation &&
			requestRequiresCurrentUpstreamWebsocket {
			replayErr := cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
			wsTerminateErr = replayErr
			_, _, _ = writer.closeForUpstreamError(replayErr)
			return
		}

		allowIncrementalInputWithPreviousResponseID := nativeWebsocketContinuation
		if forcedTranscriptReplay || retryingTranscriptReplacement || forceTranscriptReplacement {
			allowIncrementalInputWithPreviousResponseID = false
		}

		var requestJSON []byte
		var errMsg *handlers.ErrorMessage
		switch {
		case retryingTranscriptReplacement:
			requestJSON, errMsg = normalizeResponseTranscriptReplacementRetry(payload, replacementRetryBaseline)
		case forceTranscriptReplacement:
			requestJSON, errMsg = normalizeResponseTranscriptReplacementRetry(payload, previousLastRequest)
		default:
			requestJSON, _, errMsg = normalizeResponsesWebsocketRequestWithIncrementalState(
				payload,
				normalizationPreviousRequest,
				previousLastResponseOutput,
				previousLastResponseID,
				previousLastResponsePendingToolCallIDs,
				allowIncrementalInputWithPreviousResponseID,
			)
		}
		if errMsg != nil {
			pinnedAuthID = framePinnedAuthID
			pinnedAuthSupportsContinuation = framePinnedAuthSupportsContinuation
			pinnedResponseID = framePinnedResponseID
			forceTranscriptReplayNextRequest = frameForceTranscriptReplay
			replayRequest = frameReplayRequest
			replacementRetryRequest = frameReplacementRetryRequest
			replacementRetryOwnerID = frameReplacementRetryOwnerID
			replacementRetryOwnerSupportsContinuation = frameReplacementRetryOwnerSupportsContinuation
			h.LoggingAPIResponseError(util.WithGinContext(context.Background(), c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(writer, errMsg)
			appendWebsocketEvent(wsTimelineLog, "response", errorPayload)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		requestJSON = h.prepareCodexMultiAgentV2Tools(c, requestJSON)
		toolCacheTurn.recordRequest(requestJSON)
		requestJSON = repairResponsesWebsocketToolCallsWithoutRecording(downstreamSessionKey, requestJSON)
		requestJSON = dedupeResponsesWebsocketInputItemsByID(requestJSON)
		requestPreviousResponseID := strings.TrimSpace(
			gjson.GetBytes(requestJSON, "previous_response_id").String(),
		)
		requestHasContinuationOwner := requestPreviousResponseID != ""
		requestLineage := responsesWebsocketLineageReplacement
		if !retryingTranscriptReplacement && !forceTranscriptReplacement {
			requestLineage = classifyResponsesWebsocketLineage(
				payload,
				previousLastRequest,
				previousLastResponseID,
				previousLastResponsePendingToolCallIDs,
				allowIncrementalInputWithPreviousResponseID,
				forcedTranscriptReplay,
			)
		}
		requestStartingPinnedAuthID := pinnedAuthID
		requestStartingPinnedAuthSupportsContinuation := pinnedAuthSupportsContinuation
		requestStartingPinnedResponseID := pinnedResponseID

		selectedAuthIDForRequest := requestStartingPinnedAuthID
		selectedAuthSupportsContinuationForRequest := requestStartingPinnedAuthSupportsContinuation
		selectedAuthSupportsWebsocketForRequest := false
		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		if nativeWebsocketContinuation && requestRequiresCurrentUpstreamWebsocket {
			cliCtx = cliproxyexecutor.WithRequiredUpstreamWebsocket(cliCtx)
		}
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		if unknownPreviousResponseOwner {
			cliCtx = handlers.WithRequireResponsesContinuation(cliCtx)
		}
		cliCtx = handlers.WithSelectedAuthInfoCallback(
			cliCtx, func(selectedAuth cliproxyexecutor.SelectedAuthInfo) {
				selectedAuthID := strings.TrimSpace(selectedAuth.ID)
				selectedAuthIDForRequest = selectedAuthID
				selectedAuthSupportsContinuationForRequest = selectedAuthID != "" &&
					selectedAuth.SupportsResponsesContinuation
				selectedAuthSupportsWebsocketForRequest = selectedAuthID != "" &&
					selectedAuth.SupportsIncrementalResponsesWebsocket
				pinnedAuthSupportsContinuation = selectedAuthSupportsContinuationForRequest
				if requestHasContinuationOwner || pinnedAuthSupportsContinuation {
					pinnedAuthID = selectedAuthID
					pinnedResponseID = requestPreviousResponseID
				}
			},
		)
		if pinnedAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
		} else if requestHasContinuationOwner {
			cliCtx = handlers.WithPinSelectedAuth(cliCtx)
		}
		dataChan, _, errChan := h.ExecuteStreamChunksWithAuthManager(
			cliCtx, h.HandlerType(), modelName, requestJSON, "",
		)
		replayPinnedAuthFailure := func(errMsg *handlers.ErrorMessage) bool {
			return nativeWebsocketContinuation && requestRequiresCurrentUpstreamWebsocket &&
				shouldReplayResponsesWebsocketPinnedAuthFailure(errMsg)
		}

		completedOutput, completedResponseID, completedPendingToolCallIDs, forwardErrMsg, errForward := h.forwardResponsesWebsocket(
			c,
			writer,
			cliCancel,
			dataChan,
			errChan,
			wsTimelineLog,
			passthroughSessionID,
			downstreamSessionKey,
			responsesWebsocketForwardOptions{
				toolCacheTurn: toolCacheTurn,
				suppressError: replayPinnedAuthFailure,
			},
		)
		rememberResponsesWebsocketResponseOwner(
			responseOwners,
			requestPreviousResponseID,
			selectedAuthIDForRequest,
			selectedAuthSupportsContinuationForRequest,
		)
		if errForward != nil {
			wsTerminateErr = errForward
			if forwardErrMsg != nil && forwardErrMsg.Error != nil {
				wsTerminateErr = forwardErrMsg.Error
			}
			switch {
			case errors.Is(errForward, websocket.ErrCloseSent):
			case isWebsocketConnectionClosedError(errForward):
				log.Debugf(
					"responses websocket: client closed during forward id=%s error=%v",
					passthroughSessionID,
					errForward,
				)
			default:
				log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			}
			return
		}
		if forwardErrMsg != nil {
			if replayPinnedAuthFailure(forwardErrMsg) {
				replayErr := cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
				wsTerminateErr = replayErr
				_, _, _ = writer.closeForUpstreamError(replayErr)
				return
			}
			if !shouldReleaseResponsesWebsocketPinnedAuth(forwardErrMsg) {
				if requestLineage != responsesWebsocketLineageExternal {
					pinnedAuthID = requestStartingPinnedAuthID
					pinnedAuthSupportsContinuation = requestStartingPinnedAuthSupportsContinuation
					pinnedResponseID = requestStartingPinnedResponseID
				}
				continue
			}

			switch requestLineage {
			case responsesWebsocketLineageReplacement:
				replacementRetryRequest = bytes.Clone(requestJSON)
				replacementRetryOwnerID = lastResponseOwnerAuthID
				replacementRetryOwnerSupportsContinuation = lastResponseOwnerSupportsContinuation
				pinnedAuthID = ""
				pinnedAuthSupportsContinuation = false
				pinnedResponseID = ""
				forceTranscriptReplayNextRequest = false
			case responsesWebsocketLineageExternal:
				forceTranscriptReplayNextRequest = false
			case responsesWebsocketLineageFullTranscript:
				pinnedAuthID = ""
				pinnedAuthSupportsContinuation = false
				pinnedResponseID = ""
				forceTranscriptReplayNextRequest = len(previousLastRequest) > 0
				replacementRetryRequest = nil
				replacementRetryOwnerID = ""
				replacementRetryOwnerSupportsContinuation = false
			default:
				canReleasePinnedAuth := len(replayRequest) > 0 ||
					(len(previousLastRequest) == 0 && !requestHasContinuationOwner)
				if canReleasePinnedAuth {
					pinnedAuthID = ""
					pinnedAuthSupportsContinuation = false
					pinnedResponseID = ""
					forceTranscriptReplayNextRequest = len(previousLastRequest) > 0
				} else {
					pinnedAuthID = requestStartingPinnedAuthID
					pinnedAuthSupportsContinuation = requestStartingPinnedAuthSupportsContinuation
					pinnedResponseID = requestStartingPinnedResponseID
					forceTranscriptReplayNextRequest = false
				}
			}
			continue
		}

		toolCacheTurn.commit()

		var nextReplayRequest []byte
		var errReplay error
		switch requestLineage {
		case responsesWebsocketLineageFullTranscript, responsesWebsocketLineageReplacement:
			nextReplayRequest = bytes.Clone(requestJSON)
		case responsesWebsocketLineageExternal:
			nextReplayRequest = nil
		default:
			nextReplayRequest, errReplay = buildResponsesWebsocketReplayRequest(
				requestJSON,
				replayRequest,
				previousLastResponseOutput,
				requestHasContinuationOwner,
			)
		}
		if errReplay != nil {
			nextReplayRequest = nil
			log.Warnf(
				"responses websocket: disable cross-auth transcript replay id=%s error=%v",
				passthroughSessionID,
				errReplay,
			)
		}

		rememberResponsesWebsocketResponseOwner(
			responseOwners,
			completedResponseID,
			selectedAuthIDForRequest,
			selectedAuthSupportsContinuationForRequest,
		)
		if selectedAuthSupportsWebsocketForRequest {
			upstreamMode = responsesWebsocketUpstreamModeWS
			upstreamWebsocketAuthID = selectedAuthIDForRequest
		} else {
			upstreamMode = responsesWebsocketUpstreamModeHTTP
			upstreamWebsocketAuthID = ""
		}
		lastRequest = bytes.Clone(requestJSON)
		lastResponseOutput = completedOutput
		lastResponseID = completedResponseID
		lastResponseOwnerAuthID = selectedAuthIDForRequest
		lastResponseOwnerSupportsContinuation = selectedAuthSupportsContinuationForRequest
		lastResponsePendingToolCallIDs = completedPendingToolCallIDs
		if pinnedAuthID != "" {
			pinnedResponseID = completedResponseID
		} else {
			pinnedResponseID = ""
		}
		replayRequest = nextReplayRequest
		forceTranscriptReplayNextRequest = false
		replacementRetryRequest = nil
		replacementRetryOwnerID = ""
		replacementRetryOwnerSupportsContinuation = false
	}
}

func classifyResponsesWebsocketLineage(
	rawJSON []byte,
	previousRequest []byte,
	previousResponseID string,
	previousPendingToolCallIDs []string,
	allowIncrementalInputWithPreviousResponseID bool,
	forcedTranscriptReplay bool,
) responsesWebsocketLineage {
	if forcedTranscriptReplay {
		return responsesWebsocketLineageFullTranscript
	}

	rawPreviousResponseID := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
	if rawPreviousResponseID != "" && rawPreviousResponseID != strings.TrimSpace(previousResponseID) {
		return responsesWebsocketLineageExternal
	}
	if len(previousRequest) == 0 {
		return responsesWebsocketLineageLocal
	}

	input := gjson.GetBytes(rawJSON, "input")
	if allowIncrementalInputWithPreviousResponseID && rawPreviousResponseID == "" &&
		!inputSatisfiesPendingToolCalls(input, previousPendingToolCallIDs) {
		return responsesWebsocketLineageReplacement
	}
	if !allowIncrementalInputWithPreviousResponseID {
		return responsesWebsocketLineageFullTranscript
	}
	return responsesWebsocketLineageLocal
}

func buildResponsesWebsocketReplayRequest(
	requestJSON []byte,
	replayRequest []byte,
	previousResponseOutput []byte,
	requestHasContinuationOwner bool,
) ([]byte, error) {
	if len(replayRequest) == 0 {
		if requestHasContinuationOwner {
			return nil, nil
		}
		return normalizeResponseTranscriptReplacement(requestJSON, requestJSON), nil
	}

	existingInput := gjson.GetBytes(replayRequest, "input")
	mergedInput, errMerge := mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(previousResponseOutput))
	if errMerge != nil {
		return nil, fmt.Errorf("merge previous response output: %w", errMerge)
	}
	nextInput := gjson.GetBytes(requestJSON, "input")
	mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, nextInput.Raw)
	if errMerge != nil {
		return nil, fmt.Errorf("merge next request input: %w", errMerge)
	}

	normalized := normalizeResponseTranscriptReplacement(requestJSON, replayRequest)
	normalized, errSet := sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, fmt.Errorf("set replay input: %w", errSet)
	}
	return bytes.Clone(normalized), nil
}

func responsesWebsocketClientIP(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return internallogging.ExtractClientRequestMetadata(c.Request).ClientIP
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func inputHasCodexLocalCompactionSummary(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}

	hasSummary := false
	for index, item := range input.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "additional_tools" {
			tools := item.Get("tools")
			if index != 0 || strings.TrimSpace(item.Get("role").String()) != "developer" || !tools.IsArray() {
				return false
			}
			for _, tool := range tools.Array() {
				if !tool.IsObject() || strings.TrimSpace(tool.Get("type").String()) == "" {
					return false
				}
			}
			continue
		}
		if itemType != "" && itemType != "message" {
			return false
		}

		role := strings.TrimSpace(item.Get("role").String())
		if role != "user" && role != "developer" {
			return false
		}
		if role == "user" && strings.HasPrefix(
			codexLocalCompactionMessageText(item), codexLocalCompactionSummaryPrefix+"\n",
		) {
			hasSummary = true
		}
	}
	return hasSummary
}

func codexLocalCompactionMessageText(message gjson.Result) string {
	content := message.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}

	var text strings.Builder
	for _, part := range content.Array() {
		if strings.TrimSpace(part.Get("type").String()) == "input_text" {
			text.WriteString(part.Get("text").String())
		}
	}
	return text.String()
}

func shouldReplaceCodexLocalCompactionTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	if gjson.GetBytes(rawJSON, "previous_response_id").Exists() {
		return false
	}
	return inputHasCodexLocalCompactionSummary(nextInput)
}

func responsesWebsocketRequestRequiresCurrentUpstream(payload []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" ||
		strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == wsRequestTypeAppend
}

func responsesWebsocketPreviousResponseNotFoundError() *handlers.ErrorMessage {
	return &handlers.ErrorMessage{
		StatusCode: http.StatusConflict,
		Error: errors.New(
			`{"error":{"message":"Previous response is not available on this websocket; resend the full conversation input without previous_response_id","type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`,
		),
	}
}

func responsesWebsocketAuthUsesUpstreamWebsocket(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "codex" && provider != "xai" {
		return false
	}
	if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
		if enabled, errParse := strconv.ParseBool(raw); errParse == nil {
			return enabled
		}
	}
	switch value := auth.Metadata["websockets"].(type) {
	case bool:
		return value
	case string:
		enabled, errParse := strconv.ParseBool(strings.TrimSpace(value))
		return errParse == nil && enabled
	default:
		return false
	}
}

func responsesWebsocketErrorStatus(errMsg *handlers.ErrorMessage) int {
	if errMsg == nil {
		return 0
	}
	if errMsg.StatusCode > 0 {
		return errMsg.StatusCode
	}
	return clienterror.HTTPStatusFromError(errMsg.Error)
}

// shouldExposeResponsesUpstreamError reports whether a terminal upstream error
// is caused by the request and can therefore be acted on by the client.
func shouldExposeResponsesUpstreamError(errMsg *handlers.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	return clienterror.IsRequestFault(responsesWebsocketErrorStatus(errMsg), errMsg.Error)
}

func shouldReplayResponsesWebsocketPinnedAuthFailure(errMsg *handlers.ErrorMessage) bool {
	switch responsesWebsocketErrorStatus(errMsg) {
	case http.StatusUnauthorized, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func normalizeResponsesWebsocketRequestWithMode(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	allowIncrementalInputWithPreviousResponseID bool,
) ([]byte, []byte, *handlers.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithLastResponseID(
		rawJSON, lastRequest, lastResponseOutput, "", allowIncrementalInputWithPreviousResponseID,
	)
}

func normalizeResponsesWebsocketRequestWithLastResponseID(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	lastResponseID string,
	allowIncrementalInputWithPreviousResponseID bool,
) ([]byte, []byte, *handlers.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithIncrementalState(
		rawJSON, lastRequest, lastResponseOutput, lastResponseID, nil, allowIncrementalInputWithPreviousResponseID,
	)
}

func normalizeResponsesWebsocketRequestWithIncrementalState(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	lastResponseID string,
	lastResponsePendingToolCallIDs []string,
	allowIncrementalInputWithPreviousResponseID bool,
) ([]byte, []byte, *handlers.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(
			rawJSON,
			lastRequest,
			lastResponseOutput,
			lastResponseID,
			lastResponsePendingToolCallIDs,
			allowIncrementalInputWithPreviousResponseID,
		)
	case wsRequestTypeAppend:
		return normalizeResponseSubsequentRequest(
			rawJSON,
			lastRequest,
			lastResponseOutput,
			lastResponseID,
			lastResponsePendingToolCallIDs,
			allowIncrementalInputWithPreviousResponseID,
		)
	default:
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *handlers.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, bytes.Clone(normalized), nil
}

func normalizeResponseSubsequentRequest(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	lastResponseID string,
	lastResponsePendingToolCallIDs []string,
	allowIncrementalInputWithPreviousResponseID bool,
) ([]byte, []byte, *handlers.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}
	if shouldReplaceCodexLocalCompactionTranscript(rawJSON, nextInput) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, bytes.Clone(normalized), nil
	}

	if allowIncrementalInputWithPreviousResponseID {
		prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
		if prev == "" {
			if !inputSatisfiesPendingToolCalls(nextInput, lastResponsePendingToolCallIDs) {
				normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
				return normalized, bytes.Clone(normalized), nil
			}
			prev = strings.TrimSpace(lastResponseID)
		}
		if prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			normalized, _ = sjson.SetBytes(normalized, "previous_response_id", prev)
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, bytes.Clone(normalized), nil
		}
	}

	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput, errMerge := mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(lastResponseOutput))
	if errMerge != nil {
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid previous response output: %w", errMerge),
		}
	}

	mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, nextInput.Raw)
	if errMerge != nil {
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid request input: %w", errMerge),
		}
	}
	if dedupedInput, errDedupe := dedupeFunctionCallsByCallID(mergedInput); errDedupe == nil {
		mergedInput = dedupedInput
	}
	if dedupedInput, errDedupe := dedupeInputItemsByID(mergedInput); errDedupe == nil {
		mergedInput = dedupedInput
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, bytes.Clone(normalized), nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}

	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}

	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func inputSatisfiesPendingToolCalls(input gjson.Result, pendingCallIDs []string) bool {
	if len(pendingCallIDs) == 0 {
		return true
	}
	if !input.IsArray() {
		return false
	}
	outputs := make(map[string]struct{}, len(pendingCallIDs))
	for _, item := range input.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				outputs[callID] = struct{}{}
			}
		}
	}
	for _, callID := range pendingCallIDs {
		callID = strings.TrimSpace(callID)
		if callID == "" {
			continue
		}
		if _, ok := outputs[callID]; !ok {
			return false
		}
	}
	return true
}

func normalizeResponseTranscriptReplacementRetry(
	rawJSON []byte,
	lastRequest []byte,
) ([]byte, *handlers.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return nil, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
	input := gjson.GetBytes(rawJSON, "input")
	if !input.Exists() || !input.IsArray() {
		return nil, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
	if strings.TrimSpace(gjson.GetBytes(normalized, "model").String()) == "" {
		return nil, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, nil
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return bytes.Clone(normalized)
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	seenCallIDs := make(map[string]struct{}, len(items))
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		if isResponsesToolCallType(itemType) {
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID != "" {
				if _, ok := seenCallIDs[callID]; ok {
					continue
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func dedupeResponsesWebsocketInputItemsByID(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}
	dedupedInput, errDedupe := dedupeInputItemsByID(input.Raw)
	if errDedupe != nil || dedupedInput == input.Raw {
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, "input", []byte(dedupedInput))
	if errSet != nil {
		return payload
	}
	return updated
}

func dedupeInputItemsByID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	type itemMetadata struct {
		itemType string
		id       string
		callID   string
	}
	meta := make([]itemMetadata, len(items))
	for i, item := range items {
		if len(item) == 0 {
			continue
		}
		res := gjson.GetManyBytes(item, "type", "id", "call_id")
		meta[i] = itemMetadata{
			itemType: strings.TrimSpace(res[0].String()),
			id:       strings.TrimSpace(res[1].String()),
			callID:   strings.TrimSpace(res[2].String()),
		}
	}

	referencedCallIDs := make(map[string]struct{}, len(items))
	for i := range items {
		switch meta[i].itemType {
		case "function_call_output", "custom_tool_call_output":
			if meta[i].callID != "" {
				referencedCallIDs[meta[i].callID] = struct{}{}
			}
		}
	}

	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for i := range items {
		itemID := meta[i].id
		if itemID == "" {
			continue
		}
		_, referenced := referencedCallIDs[meta[i].callID]
		referenced = referenced && meta[i].callID != ""
		if _, seen := keepIndexByID[itemID]; !seen {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
			continue
		}
		if referenced || !keepReferencedByID[itemID] {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
		}
	}

	filtered := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		if len(item) == 0 {
			continue
		}
		itemID := meta[i].id
		if itemID != "" && keepIndexByID[itemID] != i {
			continue
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

// sendWebsocketError logs, writes, and records an error event on the websocket connection.
// Returns the serialized error payload and any write error.
func (h *ResponsesAPIHandler) sendWebsocketError(
	c *gin.Context,
	writer *responsesWebsocketWriter,
	errMsg *handlers.ErrorMessage,
	wsTimelineLog *websocketTimelineRecorder,
	sessionID string,
) ([]byte, error) {
	h.LoggingAPIResponseError(util.WithGinContext(context.Background(), c), errMsg)
	markAPIResponseTimestamp(c)
	errorPayload, errWrite := writeResponsesWebsocketError(writer, errMsg)
	appendWebsocketEvent(wsTimelineLog, "response", errorPayload)
	log.Infof(
		"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
		sessionID,
		websocket.TextMessage,
		websocketPayloadEventType(errorPayload),
		websocketPayloadPreview(errorPayload),
	)
	return errorPayload, errWrite
}

type responsesWebsocketForwardOptions struct {
	toolCacheTurn *responsesWebsocketToolCacheTurn
	suppressError func(*handlers.ErrorMessage) bool
}

func (h *ResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	writer *responsesWebsocketWriter,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan cliproxyexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	wsTimelineLog *websocketTimelineRecorder,
	sessionID string,
	downstreamSessionKey string,
	options ...responsesWebsocketForwardOptions,
) ([]byte, string, []string, *handlers.ErrorMessage, error) {
	var opts responsesWebsocketForwardOptions
	if len(options) > 0 {
		opts = options[0]
	}
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	pendingToolCallIDs := map[string]struct{}{}

	terminateUpstreamError := func(errMsg *handlers.ErrorMessage, payload []byte) (bool, bool, error) {
		if errMsg == nil {
			return false, false, nil
		}
		h.LoggingAPIResponseError(util.WithGinContext(context.Background(), c), errMsg)
		if opts.suppressError != nil && opts.suppressError(errMsg) {
			return true, false, nil
		}
		markAPIResponseTimestamp(c)
		if matched, wrote, errClose := writer.closeForUpstreamError(errMsg.Error); matched {
			if errClose != nil && !isWebsocketConnectionClosedError(errClose) {
				return false, wrote, errClose
			}
			return false, wrote, websocket.ErrCloseSent
		}
		errorPayload, wrote, errTerminate := writeResponsesWebsocketTerminalError(
			writer, wsTimelineLog, errMsg, payload,
		)
		if wrote {
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				sessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
		}
		return false, wrote, errTerminate
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, c.Request.Context().Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg == nil {
				cancel(nil)
				return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, nil
			}
			suppressed, delivered, errTerminate := terminateUpstreamError(errMsg, nil)
			deliveryErr := errTerminate
			if suppressed || delivered {
				deliveryErr = nil
			}
			handlers.AcknowledgeStreamError(errMsg, deliveryErr)
			cancel(errMsg.Error)
			if suppressed {
				return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
			}
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errTerminate
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					if errMsg, hasErr := pendingResponsesWebsocketError(errs); hasErr {
						if errMsg == nil {
							cancel(nil)
							return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, nil
						}
						suppressed, delivered, errTerminate := terminateUpstreamError(errMsg, nil)
						deliveryErr := errTerminate
						if suppressed || delivered {
							deliveryErr = nil
						}
						handlers.AcknowledgeStreamError(errMsg, deliveryErr)
						cancel(errMsg.Error)
						if suppressed {
							return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
						}
						return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errTerminate
					}
					errMsg := &handlers.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					_, _, errTerminate := terminateUpstreamError(errMsg, nil)
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errTerminate
				}
				cancel(nil)
				return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk.Payload)
			for i := range payloads {
				collectResponsesWebsocketOutputItem(payloads[i], outputItemsByIndex, &outputItemsFallback)
				eventType := gjson.GetBytes(payloads[i], "type").String()
				if isResponsesWebsocketCompletionEvent(eventType) {
					payloads[i] = restoreResponsesWebsocketCompletionOutput(
						payloads[i], outputItemsByIndex, outputItemsFallback,
					)
				}
				if opts.toolCacheTurn != nil {
					opts.toolCacheTurn.recordResponse(payloads[i])
				} else {
					recordResponsesWebsocketToolCallsFromPayload(downstreamSessionKey, payloads[i])
				}
				recordPendingToolCallIDsFromPayload(pendingToolCallIDs, payloads[i])
				if eventType == wsEventTypeError {
					payloadErrMsg := responsesWebsocketErrorMessageFromPayload(payloads[i])
					suppressed, delivered, errTerminate := terminateUpstreamError(payloadErrMsg, payloads[i])
					if chunk.Acknowledge != nil {
						deliveryErr := errTerminate
						if suppressed || delivered {
							deliveryErr = nil
						}
						chunk.Acknowledge(deliveryErr)
					}
					cancel(payloadErrMsg.Error)
					if suppressed {
						return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), payloadErrMsg, nil
					}
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), payloadErrMsg, errTerminate
				}
				if isResponsesWebsocketCompletionEvent(eventType) {
					if eventType == wsEventTypeCompleted {
						payloads[i], _ = sjson.SetBytes(payloads[i], "type", wsEventTypeDone)
					}

					completed = true
					completedOutput = responseCompletedOutputFromPayload(
						payloads[i], outputItemsByIndex, outputItemsFallback,
					)
					completedResponseID = responseCompletedIDFromPayload(payloads[i])
				}
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(wsTimelineLog, "response", payloads[i])
				if errWrite := writer.writeText(payloads[i]); errWrite != nil {
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(errWrite)
					}
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, errWrite
				}
			}
			if chunk.Terminal && !completed {
				protocolErr := errors.New("responses stream ended before response.completed")
				errMsg := &handlers.ErrorMessage{
					StatusCode: http.StatusBadGateway,
					Error:      protocolErr,
				}
				suppressed, _, errTerminate := terminateUpstreamError(errMsg, nil)
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(errors.Join(protocolErr, errTerminate))
				}
				cancel(protocolErr)
				if suppressed {
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
				}
				return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errTerminate
			}
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
	}
}

func isResponsesWebsocketCompletionEvent(eventType string) bool {
	return eventType == wsEventTypeCompleted || eventType == wsEventTypeDone
}

type responsesWebsocketPayloadError struct {
	status  int
	payload []byte
}

func (e *responsesWebsocketPayloadError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.payload)
}

func (e *responsesWebsocketPayloadError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func responsesWebsocketErrorMessageFromPayload(payload []byte) *handlers.ErrorMessage {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) > 0 {
		return &handlers.ErrorMessage{
			StatusCode: status,
			Error: &responsesWebsocketPayloadError{
				status:  status,
				payload: bytes.Clone(trimmedPayload),
			},
		}
	}
	return &handlers.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", http.StatusText(status))}
}

func pendingResponsesWebsocketError(errs <-chan *handlers.ErrorMessage) (*handlers.ErrorMessage, bool) {
	if errs == nil {
		return nil, false
	}
	select {
	case errMsg, ok := <-errs:
		return errMsg, ok
	default:
		return nil, false
	}
}

func shouldReleaseResponsesWebsocketPinnedAuth(errMsg *handlers.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	if errMsg.Error != nil {
		var authErr *coreauth.Error
		if errors.As(errMsg.Error, &authErr) && authErr != nil {
			switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
			case "pinned_auth_not_found", "auth_removed":
				return true
			}
		}
	}
	switch responsesWebsocketErrorStatus(errMsg) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func collectResponsesWebsocketOutputItem(
	payload []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback *[][]byte,
) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	outputIndex := gjson.GetBytes(payload, "output_index")
	if outputIndex.Exists() {
		if outputIndex.Type != gjson.Number {
			return
		}
		index, errParse := strconv.ParseInt(outputIndex.Raw, 10, 64)
		if errParse != nil || index < 0 {
			return
		}
		outputItemsByIndex[index] = bytes.Clone([]byte(item.Raw))
		return
	}
	*outputItemsFallback = append(*outputItemsFallback, bytes.Clone([]byte(item.Raw)))
}

func restoreResponsesWebsocketCompletionOutput(
	payload []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		reconciledOutput, changed := reconcileResponsesWebsocketCompletionToolCalls(
			output, outputItemsByIndex, outputItemsFallback,
		)
		if !changed {
			return payload
		}
		restored, errSet := sjson.SetRawBytes(payload, "response.output", reconciledOutput)
		if errSet != nil {
			return payload
		}
		return restored
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return payload
	}

	restored, errSet := sjson.SetRawBytes(
		payload,
		"response.output",
		responseCompletedOutputFromPayload(payload, outputItemsByIndex, outputItemsFallback),
	)
	if errSet != nil {
		return payload
	}
	return restored
}

func reconcileResponsesWebsocketCompletionToolCalls(
	output gjson.Result,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
) ([]byte, bool) {
	collectedToolCalls := make(map[string]json.RawMessage)
	recordCollectedToolCall := func(raw []byte) {
		item := gjson.ParseBytes(raw)
		if !isCompleteResponsesWebsocketToolCall(item) {
			return
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		collectedToolCalls[callID] = append(json.RawMessage(nil), raw...)
	}

	indexes := make([]int64, 0, len(outputItemsByIndex))
	for index := range outputItemsByIndex {
		indexes = append(indexes, index)
	}
	slices.Sort(
		indexes,
	)
	for _, index := range indexes {
		recordCollectedToolCall(outputItemsByIndex[index])
	}
	for _, item := range outputItemsFallback {
		recordCollectedToolCall(item)
	}
	if len(collectedToolCalls) == 0 {
		return nil, false
	}

	items := output.Array()
	reconciled := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		raw := json.RawMessage(item.Raw)
		if isResponsesToolCallType(item.Get("type").String()) {
			callID := strings.TrimSpace(item.Get("call_id").String())
			if collected, ok := collectedToolCalls[callID]; ok {
				if reconciledItem, itemChanged := reconcileResponsesWebsocketCompletionToolCall(
					item, collected,
				); itemChanged {
					raw = reconciledItem
					changed = true
				}
			}
		}
		reconciled = append(reconciled, raw)
	}
	if !changed {
		return nil, false
	}

	marshaledOutput, errMarshal := json.Marshal(reconciled)
	if errMarshal != nil {
		return nil, false
	}
	return marshaledOutput, true
}

func reconcileResponsesWebsocketCompletionToolCall(
	completed gjson.Result,
	collected json.RawMessage,
) (json.RawMessage, bool) {
	if bytes.Equal([]byte(completed.Raw), collected) {
		return nil, false
	}
	collectedItem := gjson.ParseBytes(collected)
	completedType := strings.TrimSpace(completed.Get("type").String())
	if completedType != strings.TrimSpace(collectedItem.Get("type").String()) {
		return append(json.RawMessage(nil), collected...), true
	}

	fields := []string{"id", "call_id", "name", "status"}
	switch completedType {
	case "function_call":
		fields = append(fields, "arguments")
	case "custom_tool_call":
		fields = append(fields, "input")
	}

	merged := []byte(completed.Raw)
	for _, field := range fields {
		collectedField := collectedItem.Get(field)
		completedField := completed.Get(field)
		if !collectedField.Exists() || completedField.Raw == collectedField.Raw ||
			(completedField.Type == gjson.String && collectedField.Type == gjson.String &&
				completedField.String() == collectedField.String()) {
			continue
		}
		updated, errSet := sjson.SetRawBytes(merged, field, []byte(collectedField.Raw))
		if errSet != nil {
			return append(json.RawMessage(nil), collected...), true
		}
		merged = updated
	}
	if bytes.Equal(merged, []byte(completed.Raw)) {
		return nil, false
	}
	return merged, true
}

func responseCompletedOutputFromPayload(
	payload []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return bytes.Clone([]byte(output.Raw))
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return []byte("[]")
	}

	indexes := make([]int64, 0, len(outputItemsByIndex))
	for index := range outputItemsByIndex {
		indexes = append(indexes, index)
	}
	slices.Sort(
		indexes,
	)

	items := make([]json.RawMessage, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	appendCollectedItem := func(raw []byte) {
		item := gjson.ParseBytes(raw)
		if isResponsesToolCallType(item.Get("type").String()) && !isCompleteResponsesWebsocketToolCall(item) {
			return
		}
		items = append(items, append(json.RawMessage(nil), raw...))
	}
	for _, index := range indexes {
		appendCollectedItem(outputItemsByIndex[index])
	}
	for _, item := range outputItemsFallback {
		appendCollectedItem(item)
	}

	marshaledOutput, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return marshaledOutput
}

func responseCompletedIDFromPayload(payload []byte) string {
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func recordPendingToolCallIDsFromPayload(pending map[string]struct{}, payload []byte) {
	if pending == nil || len(payload) == 0 {
		return
	}
	updatePendingToolCallIDsFromItem(pending, gjson.GetBytes(payload, "item"))
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			updatePendingToolCallIDsFromItem(pending, item)
		}
	}
}

func updatePendingToolCallIDsFromItem(pending map[string]struct{}, item gjson.Result) {
	if pending == nil || !item.Exists() {
		return
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "function_call", "custom_tool_call":
		if !isCompleteResponsesWebsocketToolCall(item) {
			return
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		pending[callID] = struct{}{}
	case "function_call_output", "custom_tool_call_output":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			delete(pending, callID)
		}
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketTerminalError(
	writer *responsesWebsocketWriter,
	wsTimelineLog *websocketTimelineRecorder,
	errMsg *handlers.ErrorMessage,
	payload []byte,
) ([]byte, bool, error) {
	if !shouldExposeResponsesUpstreamError(errMsg) {
		if errMsg != nil && errMsg.Error != nil {
			appendWebsocketEvent(wsTimelineLog, "disconnect", []byte(errMsg.Error.Error()))
		}
		_, errClose := writer.closeWithoutError()
		if errClose != nil && !isWebsocketConnectionClosedError(errClose) {
			return nil, false, errClose
		}
		return nil, false, websocket.ErrCloseSent
	}

	if len(payload) == 0 {
		var errBuild error
		payload, errBuild = buildResponsesWebsocketErrorPayload(errMsg)
		if errBuild != nil {
			_, _ = writer.closeWithoutError()
			return nil, false, errBuild
		}
	}

	wrote, errClose := writer.closeWithPayload(payload)
	if wrote {
		appendWebsocketEvent(wsTimelineLog, "response", payload)
	}
	if errClose != nil && !isWebsocketConnectionClosedError(errClose) {
		return payload, wrote, errClose
	}
	return payload, wrote, websocket.ErrCloseSent
}

func buildResponsesWebsocketErrorPayload(errMsg *handlers.ErrorMessage) ([]byte, error) {
	status := responsesWebsocketErrorStatus(errMsg)
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	errText := http.StatusText(status)
	if errMsg != nil && errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		errText = errMsg.Error.Error()
	}

	body := handlers.BuildErrorResponseBody(status, errText)
	payload := map[string]any{
		"type":   wsEventTypeError,
		"status": status,
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := map[string]any{}
		for key, values := range errMsg.Addon {
			if len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}
		if len(headers) > 0 {
			payload["headers"] = headers
		}
	}

	if len(body) > 0 && json.Valid(body) {
		var decoded map[string]any
		if errDecode := json.Unmarshal(body, &decoded); errDecode == nil {
			if inner, ok := decoded["error"]; ok {
				payload["error"] = inner
			} else {
				payload["error"] = decoded
			}
		}
	}

	if _, ok := payload["error"]; !ok {
		payload["error"] = map[string]any{
			"type":    "server_error",
			"message": errText,
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func writeResponsesWebsocketError(writer *responsesWebsocketWriter, errMsg *handlers.ErrorMessage) ([]byte, error) {
	payload, errBuild := buildResponsesWebsocketErrorPayload(errMsg)
	if errBuild != nil {
		return nil, errBuild
	}
	return payload, writer.writeText(payload)
}

type websocketTimelineSource interface {
	AppendBytes([]byte) error
	HasPayload() bool
	WritePayloadTo(io.Writer) error
	Cleanup() error
}

type websocketTimelineRecorder struct {
	context      *gin.Context
	source       websocketTimelineSource
	sourceFailed bool
	cleanupQueue *internallogging.FileBodySourceCleanupQueue
	fallback     *internallogging.BoundedLogBuffer
	size         int
}

func newWebsocketTimelineRecorder(c *gin.Context) *websocketTimelineRecorder {
	recorder := &websocketTimelineRecorder{
		context:  c,
		fallback: internallogging.NewBoundedLogBuffer(),
	}
	if c == nil {
		return recorder
	}
	if value, exists := c.Get(internallogging.FileBodySourceCleanupQueueContextKey); exists {
		recorder.cleanupQueue, _ = value.(*internallogging.FileBodySourceCleanupQueue)
	}
	if recorder.cleanupQueue == nil {
		recorder.cleanupQueue = &internallogging.FileBodySourceCleanupQueue{}
		c.Set(internallogging.FileBodySourceCleanupQueueContextKey, recorder.cleanupQueue)
	}
	value, exists := c.Get(internallogging.WebsocketTimelineSourceContextKey)
	if !exists {
		return recorder
	}
	source, ok := value.(*internallogging.FileBodySource)
	if ok {
		recorder.source = source
	}
	return recorder
}

func appendWebsocketEvent(recorder *websocketTimelineRecorder, eventType string, payload []byte) {
	if recorder == nil {
		return
	}
	if recorder.fallback == nil {
		recorder.fallback = internallogging.NewBoundedLogBuffer()
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 || recorder.size >= 10<<20 {
		return
	}

	var event bytes.Buffer
	if recorder.size > 0 {
		event.WriteByte('\n')
	}
	event.WriteString("websocket.")
	event.WriteString(eventType)
	event.WriteByte('\n')
	event.Write(trimmedPayload)
	event.WriteByte('\n')

	if recorder.source != nil && !recorder.sourceFailed {
		if errAppend := recorder.source.AppendBytes(event.Bytes()); errAppend == nil {
			recorder.size += event.Len()
			return
		} else {
			log.WithError(errAppend).Warn("failed to append websocket timeline log source; using memory fallback")
		}
		recorder.moveSourceToFallback()
	}

	recorder.fallback.Append(event.Bytes())
	recorder.size += event.Len()
}

func (r *websocketTimelineRecorder) moveSourceToFallback() {
	if r == nil || r.source == nil {
		return
	}
	source := r.source
	if source.HasPayload() {
		migrated := internallogging.NewBoundedLogBuffer()
		if errWrite := source.WritePayloadTo(migrated); errWrite != nil {
			r.sourceFailed = true
			log.WithError(errWrite).Warn("failed to move websocket timeline log source; retaining source prefix")
			return
		}
		r.fallback.Append(migrated.Bytes())
	}
	r.source = nil
	if r.context != nil {
		r.context.Set(
			internallogging.WebsocketTimelineSourceContextKey,
			(*internallogging.FileBodySource)(nil),
		)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		if fileSource, ok := source.(*internallogging.FileBodySource); ok {
			r.cleanupQueue.Add(fileSource)
		}
		log.WithError(errCleanup).Warn("failed to clean up websocket timeline log source")
	}
}

func (r *websocketTimelineRecorder) String() string {
	if r == nil || r.fallback == nil {
		return ""
	}
	return r.fallback.String()
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	preview := trimmedPayload
	if len(preview) > wsPayloadLogMaxSize {
		preview = preview[:wsPayloadLogMaxSize]
	}
	previewText := strings.ReplaceAll(string(preview), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	if len(trimmedPayload) > wsPayloadLogMaxSize {
		return fmt.Sprintf("%s...(truncated,total=%d)", previewText, len(trimmedPayload))
	}
	return previewText
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(wsTimelineBodyKey, []byte(trimmedBody))
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
