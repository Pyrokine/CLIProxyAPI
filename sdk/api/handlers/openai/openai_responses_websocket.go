package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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
	wsRequestTypeCreate  = "response.create"
	wsRequestTypeAppend  = "response.append"
	wsEventTypeError     = "error"
	wsEventTypeCompleted = "response.completed"
	wsEventTypeDone      = "response.done"
	wsDoneMarker         = "[DONE]"
	wsTurnStateHeader    = "x-codex-turn-state"
	wsTimelineBodyKey    = "WEBSOCKET_TIMELINE_OVERRIDE"
	wsPayloadLogMaxSize  = 2048
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

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *ResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	conn.SetReadLimit(10 << 20) // 10MB per message
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	retainResponsesWebsocketToolCaches(downstreamSessionKey)
	passthroughSessionID := uuid.NewString()
	clientRemoteAddr := responsesWebsocketClientIP(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientRemoteAddr)
	var wsTerminateErr error
	wsTimelineLog := newWebsocketTimelineRecorder(c)
	defer func() {
		releaseResponsesWebsocketToolCaches(downstreamSessionKey)
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
		if errClose := conn.Close(); errClose != nil {
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
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
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

		allowIncrementalInputWithPreviousResponseID := pinnedAuthSupportsContinuation ||
			unknownPreviousResponseOwner
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
			errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
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
		requestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, requestJSON)
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
		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		if unknownPreviousResponseOwner {
			cliCtx = handlers.WithRequireResponsesContinuation(cliCtx)
		}
		if pinnedAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
		} else {
			if requestHasContinuationOwner {
				cliCtx = handlers.WithPinSelectedAuth(cliCtx)
			}
			cliCtx = handlers.WithSelectedAuthInfoCallback(
				cliCtx, func(selectedAuth cliproxyexecutor.SelectedAuthInfo) {
					selectedAuthID := strings.TrimSpace(selectedAuth.ID)
					selectedAuthIDForRequest = selectedAuthID
					selectedAuthSupportsContinuationForRequest = selectedAuthID != "" &&
						selectedAuth.SupportsResponsesContinuation
					pinnedAuthSupportsContinuation = selectedAuthSupportsContinuationForRequest
					if requestHasContinuationOwner || pinnedAuthSupportsContinuation {
						pinnedAuthID = selectedAuthID
						pinnedResponseID = requestPreviousResponseID
					}
				},
			)
		}
		dataChan, _, errChan := h.ExecuteStreamChunksWithAuthManager(
			cliCtx, h.HandlerType(), modelName, requestJSON, "",
		)

		completedOutput, completedResponseID, completedPendingToolCallIDs, forwardErrMsg, errForward := h.forwardResponsesWebsocket(
			c, conn, cliCancel, dataChan, errChan, wsTimelineLog, passthroughSessionID, downstreamSessionKey,
		)
		rememberResponsesWebsocketResponseOwner(
			responseOwners,
			requestPreviousResponseID,
			selectedAuthIDForRequest,
			selectedAuthSupportsContinuationForRequest,
		)
		if errForward != nil {
			wsTerminateErr = errForward
			appendWebsocketEvent(wsTimelineLog, "disconnect", []byte(errForward.Error()))
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		if forwardErrMsg != nil {
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
	if clientIP := strings.TrimSpace(c.ClientIP()); clientIP != "" {
		return clientIP
	}
	return strings.TrimSpace(c.Request.RemoteAddr)
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
	conn *websocket.Conn,
	errMsg *handlers.ErrorMessage,
	wsTimelineLog *websocketTimelineRecorder,
	sessionID string,
) ([]byte, error) {
	h.LoggingAPIResponseError(util.WithGinContext(context.Background(), c), errMsg)
	markAPIResponseTimestamp(c)
	errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
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

func (h *ResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan cliproxyexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	wsTimelineLog *websocketTimelineRecorder,
	sessionID string,
	downstreamSessionKey string,
) ([]byte, string, []string, *handlers.ErrorMessage, error) {
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	pendingToolCallIDs := map[string]struct{}{}

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
			if errMsg != nil {
				_, errWrite := h.sendWebsocketError(c, conn, errMsg, wsTimelineLog, sessionID)
				if errWrite != nil {
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					if errMsg, hasErr := pendingResponsesWebsocketError(errs); hasErr {
						if errMsg != nil {
							_, errWrite := h.sendWebsocketError(c, conn, errMsg, wsTimelineLog, sessionID)
							if errWrite != nil {
								cancel(errMsg.Error)
								return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errWrite
							}
							cancel(errMsg.Error)
							return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
						}
						cancel(nil)
						return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, nil
					}
					errMsg := &handlers.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					_, errWrite := h.sendWebsocketError(c, conn, errMsg, wsTimelineLog, sessionID)
					if errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s error=%v",
							sessionID,
							errWrite,
						)
						cancel(errMsg.Error)
						return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
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
				recordResponsesWebsocketToolCallsFromPayload(downstreamSessionKey, payloads[i])
				recordPendingToolCallIDsFromPayload(pendingToolCallIDs, payloads[i])
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
				if errWrite := conn.WriteMessage(websocket.TextMessage, payloads[i]); errWrite != nil {
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
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
	}
}

func isResponsesWebsocketCompletionEvent(eventType string) bool {
	return eventType == wsEventTypeCompleted || eventType == wsEventTypeDone
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
	status := errMsg.StatusCode
	if status <= 0 && errMsg.Error != nil {
		if se, ok := errMsg.Error.(interface{ StatusCode() int }); ok && se != nil {
			status = se.StatusCode()
		}
	}
	switch status {
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
		outputItemsByIndex[outputIndex.Int()] = bytes.Clone([]byte(item.Raw))
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
		return payload
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return payload
	}

	restored, err := sjson.SetRawBytes(
		payload,
		"response.output",
		responseCompletedOutputFromPayload(payload, outputItemsByIndex, outputItemsFallback),
	)
	if err != nil {
		return payload
	}
	return restored
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
	sort.Slice(indexes, func(i, j int) bool {
		return indexes[i] < indexes[j]
	})

	items := make([]json.RawMessage, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	for _, index := range indexes {
		items = append(items, json.RawMessage(outputItemsByIndex[index]))
	}
	for _, item := range outputItemsFallback {
		items = append(items, json.RawMessage(item))
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
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			pending[callID] = struct{}{}
		}
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

func writeResponsesWebsocketError(conn *websocket.Conn, errMsg *handlers.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
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
	return data, conn.WriteMessage(websocket.TextMessage, data)
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
