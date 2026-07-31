package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type XAIWebsocketsExecutor struct {
	*XAIExecutor

	store   *xaiWebsocketSessionStore
	idStore *xaiWebsocketIDStateStore
}

type xaiWebsocketSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*codexWebsocketSession
}

type xaiWebsocketIDStateStore struct {
	mu       sync.Mutex
	sessions map[string]*xaiWebsocketIDState
}

type xaiWebsocketIDState struct {
	mu                   sync.Mutex
	downstreamToUpstream map[string]string
	sequence             int
	transcriptInput      []json.RawMessage
}

type xaiWebsocketRequestIDMapper struct {
	state                *xaiWebsocketIDState
	downstreamPreviousID string
	upstreamPreviousID   string
	upstreamResponseID   string
	downstreamResponseID string
}

var globalXAIWebsocketSessionStore = &xaiWebsocketSessionStore{
	sessions: make(map[string]*codexWebsocketSession),
}

var globalXAIWebsocketIDStates = &xaiWebsocketIDStateStore{
	sessions: make(map[string]*xaiWebsocketIDState),
}

const (
	maxXAIWebsocketTranscriptItems = 128
	maxXAIWebsocketTranscriptBytes = 1024 * 1024
)

func NewXAIWebsocketsExecutor(cfg *config.Config) *XAIWebsocketsExecutor {
	return &XAIWebsocketsExecutor{
		XAIExecutor: NewXAIExecutor(cfg),
		store:       globalXAIWebsocketSessionStore,
		idStore:     globalXAIWebsocketIDStates,
	}
}

func (e *XAIWebsocketsExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if e == nil || e.XAIExecutor == nil {
		return nil, fmt.Errorf("xai websockets executor: executor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	stateSessionID := xaiExecutionSessionID(req, opts)
	if stateSessionID == "" {
		stateSessionID = executionSessionID
	}
	idMapper := newXAIWebsocketRequestIDMapper(e.idStore, stateSessionID, req.Payload)
	if xaiInputHasItemType(req.Payload, "compaction_trigger") {
		return e.executeCompactionTriggerFromWebsocketContext(ctx, auth, req, opts, idMapper)
	}

	token, baseURL := xaiCreds(auth)
	if baseURL == "" {
		baseURL = xaiauth.DefaultAPIBaseURL
	}

	prepared, err := e.prepareResponsesWebsocketRequest(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	prepared.body, err = idMapper.upstreamRequestPayload(prepared.body)
	if err != nil {
		return nil, err
	}

	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	wsHeaders := applyXAIWebsocketHeaders(auth, token, prepared.sessionID)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	var sess *codexWebsocketSession
	var readCh chan codexWebsocketRead
	if executionSessionID != "" {
		sess = e.store.getOrCreate(executionSessionID)
		if sess != nil {
			sess.reqMu.Lock()
			readCh = make(chan codexWebsocketRead, 4096)
		}
	}
	releaseSessionRequest := func(conn *websocket.Conn) {
		if sess != nil {
			sess.clearActive(conn, readCh)
			sess.reqMu.Unlock()
		}
	}

	wsReqBody := buildXAIWebsocketRequestBody(prepared.body)
	wsReqLog := xaiWebsocketRequestLog(wsURL, wsHeaders, wsReqBody, e.Identifier(), auth)
	recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBody, e.Identifier(), auth)
	recordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if errDial != nil {
		releaseSessionRequest(conn)
		bodyErr, normalizedDialErr := xaiWebsocketDialError(respHS, errDial)
		if respHS != nil {
			recordAPIWebsocketUpgradeRejection(
				ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr,
			)
		}
		recordAPIResponseError(ctx, e.cfg, normalizedDialErr)
		recordAPIWebsocketError(ctx, e.cfg, "dial", normalizedDialErr)
		return nil, normalizedDialErr
	}
	recordXAIWebsocketHandshake(ctx, e.cfg, respHS)
	if sess == nil {
		logXAIWebsocketConnected(executionSessionID, authID, wsURL)
	} else {
		sess.setActive(conn, readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		clearActiveOnReturn := true
		defer func() {
			if clearActiveOnReturn {
				releaseSessionRequest(conn)
			}
		}()
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry == nil && connRetry != nil {
				sess.setActive(connRetry, readCh)
				wsReqLogRetry := xaiWebsocketRequestLog(wsURL, wsHeaders, wsReqBody, e.Identifier(), auth)
				recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBody, e.Identifier(), auth)
				recordAPIWebsocketRequest(ctx, e.cfg, wsReqLogRetry)
				recordXAIWebsocketHandshake(ctx, e.cfg, respHSRetry)
				if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBody); errSendRetry == nil {
					conn = connRetry
					clearActiveOnReturn = false
				} else {
					e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
					recordAPIResponseError(ctx, e.cfg, errSendRetry)
					recordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
					return nil, errSendRetry
				}
			} else {
				bodyErr, normalizedDialErr := xaiWebsocketDialError(respHSRetry, errDialRetry)
				if respHSRetry != nil {
					recordAPIWebsocketUpgradeRejection(
						ctx,
						e.cfg,
						websocketUpgradeRequestLog(wsReqLog),
						respHSRetry.StatusCode,
						respHSRetry.Header.Clone(),
						bodyErr,
					)
				}
				recordAPIResponseError(ctx, e.cfg, normalizedDialErr)
				recordAPIWebsocketError(ctx, e.cfg, "dial_retry", normalizedDialErr)
				return nil, normalizedDialErr
			}
		} else {
			recordAPIResponseError(ctx, e.cfg, errSend)
			recordAPIWebsocketError(ctx, e.cfg, "send", errSend)
			return nil, errSend
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go e.streamXAIWebsocketResponses(ctx, sess, conn, readCh, out, req, prepared, reporter, idMapper, wsReqBody)
	return &cliproxyexecutor.StreamResult{Headers: websocketStreamResponseHeaders(), Chunks: out}, nil
}

func (e *XAIWebsocketsExecutor) prepareResponsesWebsocketRequest(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*xaiPreparedRequest, error) {
	prepared, err := e.prepareResponsesRequestWithContext(ctx, req, opts, true, sdktranslator.FormatCodex)
	if err != nil {
		return nil, err
	}
	if previousID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()); previousID != "" {
		prepared.body, _ = sjson.SetBytes(prepared.body, "previous_response_id", previousID)
	}
	return prepared, nil
}

func (e *XAIWebsocketsExecutor) streamXAIWebsocketResponses(
	ctx context.Context,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh chan codexWebsocketRead,
	out chan<- cliproxyexecutor.StreamChunk,
	req cliproxyexecutor.Request,
	prepared *xaiPreparedRequest,
	reporter *usageReporter,
	idMapper *xaiWebsocketRequestIDMapper,
	requestPayload []byte,
) {
	defer close(out)
	if sess == nil && conn != nil {
		defer func() {
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("xai websockets executor: close websocket error: %v", errClose)
			}
		}()
	}
	if sess != nil {
		defer func() {
			sess.clearActive(conn, readCh)
			sess.reqMu.Unlock()
		}()
	}
	var param any
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	generateFalse := gjson.GetBytes(requestPayload, "generate").Exists() && !gjson.GetBytes(
		requestPayload, "generate",
	).Bool()

	for {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			recordAPIWebsocketError(ctx, e.cfg, "read", errRead)
			reporter.publishFailure(ctx, errRead)
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "read_error", errRead)
			}
			sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errRead})
			return
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("xai websockets executor: unexpected binary message")
				recordAPIResponseError(ctx, e.cfg, errBinary)
				recordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", errBinary)
				reporter.publishFailure(ctx, errBinary)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				}
				sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errBinary})
				return
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		appendAPIResponseChunk(ctx, e.cfg, payload)
		appendAPIWebsocketResponse(ctx, e.cfg, payload)

		if wsErr, ok := parseXAIWebsocketError(payload); ok {
			recordAPIResponseError(ctx, e.cfg, wsErr)
			recordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
			reporter.publishFailure(ctx, wsErr)
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: wsErr})
			return
		}

		for _, eventPayload := range xaiNormalizeReasoningSummaryDataEvents(payload) {
			eventType := gjson.GetBytes(eventPayload, "type").String()
			switch eventType {
			case "response.output_item.done":
				xaiCollectOutputItemDone(eventPayload, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.done":
				eventPayload = xaiPatchCompletedOutput(eventPayload, outputItemsByIndex, outputItemsFallback)
				eventPayload = xaiNormalizeReasoningSummaryData(eventPayload)
				eventType = gjson.GetBytes(eventPayload, "type").String()
				if errTranscript := idMapper.recordTranscriptTurn(requestPayload, eventPayload); errTranscript != nil {
					recordAPIResponseError(ctx, e.cfg, errTranscript)
					recordAPIWebsocketError(ctx, e.cfg, "transcript_limit", errTranscript)
					reporter.publishFailure(ctx, errTranscript)
					sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTranscript})
					return
				}
				cacheXAIReasoningReplayFromCompleted(prepared.reasoningReplayScope, eventPayload)
				if detail, ok := parseCodexUsage(eventPayload); ok {
					reporter.publish(ctx, detail)
				}
			}
			clientPayload := idMapper.downstreamResponsePayload(eventPayload)

			if cliproxyexecutor.DownstreamWebsocket(ctx) {
				if !sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: clientPayload}) {
					return
				}
				if eventType == "response.completed" || eventType == "response.done" || eventType == "error" {
					return
				}
				if generateFalse && eventType == "response.created" {
					completedPayload := idMapper.downstreamResponsePayload(xaiGenerateFalseCompletedPayload(eventPayload))
					_ = sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: completedPayload})
					return
				}
				continue
			}

			line := append([]byte("data: "), clientPayload...)
			chunks := sdktranslator.TranslateStream(
				ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, line,
				&param,
			)
			for i := range chunks {
				if !sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" || eventType == "error" {
				return
			}
			if generateFalse && eventType == "response.created" {
				completedPayload := idMapper.downstreamResponsePayload(xaiGenerateFalseCompletedPayload(eventPayload))
				completedLine := append([]byte("data: "), completedPayload...)
				completedChunks := sdktranslator.TranslateStream(
					ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body,
					completedLine, &param,
				)
				for i := range completedChunks {
					if !sendXAIWebsocketChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: completedChunks[i]}) {
						return
					}
				}
				return
			}
		}
	}
}

func sendXAIWebsocketChunk(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	chunk cliproxyexecutor.StreamChunk,
) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *XAIWebsocketsExecutor) executeCompactionTriggerFromWebsocketContext(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	idMapper *xaiWebsocketRequestIDMapper,
) (*cliproxyexecutor.StreamResult, error) {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return e.XAIExecutor.executeCompactionTriggerStream(ctx, auth, req, opts)
	}
	if idMapper != nil && idMapper.state != nil {
		req.Payload = buildXAIWebsocketCompactionPayload(req.Payload, idMapper.state.snapshotTranscriptInput())
	}
	prepared, data, headers, err := e.XAIExecutor.executeCompactRequest(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	outPayloads := xaiWebsocketPayloadsFromSSEFrames(xaiBuildCompactionTriggerStreamChunks(prepared, data))
	out := make(chan cliproxyexecutor.StreamChunk, len(outPayloads))
	for _, payload := range outPayloads {
		out <- cliproxyexecutor.StreamChunk{Payload: idMapper.downstreamResponsePayload(payload)}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{Headers: websocketStreamResponseHeadersFrom(headers), Chunks: out}, nil
}

func buildXAIWebsocketCompactionPayload(payload []byte, transcript []byte) []byte {
	out := xaiRemoveInputItemsByType(bytes.Clone(payload), "compaction_trigger")
	if len(transcript) > 0 {
		out, _ = sjson.SetRawBytes(out, "input", transcript)
	}
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	return out
}

func xaiWebsocketPayloadsFromSSEFrames(frames [][]byte) [][]byte {
	out := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, xaiDataTag) {
				continue
			}
			payload := bytes.TrimSpace(line[len(xaiDataTag):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			out = append(out, bytes.Clone(payload))
		}
	}
	return out
}

func buildXAIWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	wsReqBody := bytes.Clone(body)
	wsReqBody, _ = sjson.SetBytes(wsReqBody, "type", "response.create")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "stream")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "stream_options")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "background")
	wsReqBody, _ = sjson.SetBytes(wsReqBody, "store", true)
	if strings.TrimSpace(gjson.GetBytes(wsReqBody, "previous_response_id").String()) != "" {
		wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "instructions")
	}
	return wsReqBody
}

func applyXAIWebsocketHeaders(auth *cliproxyauth.Auth, token string, sessionID string) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		headers.Set("x-grok-conv-id", sessionID)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	return headers
}

func xaiWebsocketRequestLog(
	url string,
	headers http.Header,
	body []byte,
	provider string,
	auth *cliproxyauth.Auth,
) upstreamRequestLog {
	info := upstreamRequestLog{
		URL:      url,
		Method:   http.MethodPost,
		Headers:  headers.Clone(),
		Body:     bytes.Clone(body),
		Provider: provider,
	}
	if auth != nil {
		info.AuthID = auth.ID
		info.AuthLabel = auth.Label
	}
	return info
}

func (e *XAIWebsocketsExecutor) ensureUpstreamConn(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	authID string,
	wsURL string,
	headers http.Header,
) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
		conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			return nil, resp, err
		}
		(&codexWebsocketSession{}).configureConn(conn)
		return conn, resp, nil
	}

	sess.connMu.Lock()
	if sess.conn != nil && sess.wsURL == wsURL && sess.authID == authID {
		conn := sess.conn
		sess.connMu.Unlock()
		return conn, nil, nil
	}
	oldConn := sess.conn
	oldURL := sess.wsURL
	oldAuthID := sess.authID
	sess.conn = nil
	if sess.readerConn == oldConn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	if oldConn != nil {
		logXAIWebsocketDisconnected(sess.sessionID, oldAuthID, oldURL, "replaced", nil)
		if errClose := oldConn.Close(); errClose != nil {
			log.Errorf("xai websockets executor: close replaced websocket error: %v", errClose)
		}
	}

	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, resp, err
	}
	sess.configureConn(conn)

	sess.connMu.Lock()
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	sess.connMu.Unlock()

	logXAIWebsocketConnected(sess.sessionID, authID, wsURL)
	go e.readSessionLoop(sess, conn)
	return conn, resp, nil
}

func (e *XAIWebsocketsExecutor) readSessionLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if sess == nil || conn == nil {
		return
	}
	for {
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			sess.connMu.Lock()
			current := sess.conn
			authID := sess.authID
			wsURL := sess.wsURL
			if current == conn {
				sess.conn = nil
			}
			if sess.readerConn == conn {
				sess.readerConn = nil
			}
			sessionID := sess.sessionID
			sess.connMu.Unlock()
			if current == conn {
				logXAIWebsocketDisconnected(sessionID, authID, wsURL, "read_error", errRead)
			}
			sess.activeMu.Lock()
			ch := sess.activeCh
			done := sess.activeDone
			sess.activeMu.Unlock()
			if ch != nil {
				select {
				case ch <- codexWebsocketRead{conn: conn, err: errRead}:
				case <-done:
				}
			}
			return
		}

		sess.activeMu.Lock()
		ch := sess.activeCh
		done := sess.activeDone
		sess.activeMu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload}:
		case <-done:
		}
	}
}

func (e *XAIWebsocketsExecutor) invalidateUpstreamConn(
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	reason string,
	err error,
) {
	if sess == nil || conn == nil {
		return
	}
	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		return
	}
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	logXAIWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("xai websockets executor: close websocket error: %v", errClose)
	}
}

func (e *XAIWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil {
		return
	}
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.closeAllExecutionSessions("executor_replaced")
		return
	}
	sess := e.store.take(sessionID)
	e.idStore.delete(sessionID)
	e.closeExecutionSession(sess, "session_closed")
}

func (e *XAIWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil || e.store == nil {
		return
	}
	sessions := e.store.takeAll()
	e.idStore.deleteAll()
	for _, sess := range sessions {
		e.closeExecutionSession(sess, reason)
	}
}

func (e *XAIWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()

	if conn == nil {
		return
	}
	logXAIWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("xai websockets executor: close websocket error: %v", errClose)
	}
}

func (s *xaiWebsocketSessionStore) getOrCreate(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess := s.sessions[sessionID]; sess != nil {
		return sess
	}
	sess := &codexWebsocketSession{sessionID: sessionID}
	s.sessions[sessionID] = sess
	return sess
}

func (s *xaiWebsocketSessionStore) take(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	return sess
}

func (s *xaiWebsocketSessionStore) takeAll() []*codexWebsocketSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*codexWebsocketSession, 0, len(s.sessions))
	for sessionID, sess := range s.sessions {
		delete(s.sessions, sessionID)
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func (s *xaiWebsocketSessionStore) closeForAuthID(authID string, reason string) []*codexWebsocketSession {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*codexWebsocketSession, 0)
	for sessionID, sess := range s.sessions {
		if sess == nil {
			delete(s.sessions, sessionID)
			continue
		}
		sess.connMu.Lock()
		match := sess.authID == authID
		sess.connMu.Unlock()
		if match {
			delete(s.sessions, sessionID)
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func CloseXAIWebsocketSessionsForAuthID(authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "auth_removed"
	}
	exec := NewXAIWebsocketsExecutor(nil)
	for _, sess := range globalXAIWebsocketSessionStore.closeForAuthID(authID, reason) {
		exec.closeExecutionSession(sess, reason)
	}
}

func (s *xaiWebsocketIDStateStore) get(sessionID string) *xaiWebsocketIDState {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*xaiWebsocketIDState)
	}
	state := s.sessions[sessionID]
	if state == nil {
		state = &xaiWebsocketIDState{downstreamToUpstream: make(map[string]string)}
		s.sessions[sessionID] = state
	}
	return state
}

func (s *xaiWebsocketIDStateStore) delete(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *xaiWebsocketIDStateStore) deleteAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sessions = make(map[string]*xaiWebsocketIDState)
	s.mu.Unlock()
}

func newXAIWebsocketRequestIDMapper(
	store *xaiWebsocketIDStateStore,
	sessionID string,
	payload []byte,
) *xaiWebsocketRequestIDMapper {
	mapper := &xaiWebsocketRequestIDMapper{
		downstreamPreviousID: strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()),
	}
	if store == nil || strings.TrimSpace(sessionID) == "" {
		mapper.upstreamPreviousID = mapper.downstreamPreviousID
		return mapper
	}
	mapper.state = store.get(sessionID)
	mapper.upstreamPreviousID = mapper.state.upstreamIDForDownstream(mapper.downstreamPreviousID)
	if mapper.upstreamPreviousID == "" {
		mapper.upstreamPreviousID = mapper.downstreamPreviousID
	}
	return mapper
}

func (m *xaiWebsocketRequestIDMapper) upstreamRequestPayload(payload []byte) ([]byte, error) {
	if m == nil {
		return payload, nil
	}
	out := bytes.Clone(payload)
	if m.downstreamPreviousID != "" && m.upstreamPreviousID != m.downstreamPreviousID {
		out, _ = sjson.SetBytes(out, "previous_response_id", m.upstreamPreviousID)
	}
	if m.state != nil && strings.TrimSpace(gjson.GetBytes(out, "previous_response_id").String()) != "" {
		return m.state.prependTranscriptInput(out)
	}
	return out, nil
}

func (m *xaiWebsocketRequestIDMapper) downstreamResponsePayload(payload []byte) []byte {
	if m == nil || len(payload) == 0 {
		return payload
	}
	upstreamResponseID := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
	if upstreamResponseID == "" {
		upstreamResponseID = strings.TrimSpace(gjson.GetBytes(payload, "id").String())
	}
	if upstreamResponseID != "" {
		m.upstreamResponseID = upstreamResponseID
		if m.state != nil {
			m.downstreamResponseID = m.state.downstreamIDForResponse(
				upstreamResponseID, m.upstreamPreviousID, m.downstreamPreviousID,
			)
		} else {
			m.downstreamResponseID = upstreamResponseID
		}
	}
	return rewriteXAIWebsocketDownstreamIDs(
		payload, m.upstreamResponseID, m.downstreamResponseID, m.upstreamPreviousID, m.downstreamPreviousID,
	)
}

func (m *xaiWebsocketRequestIDMapper) recordTranscriptTurn(requestPayload []byte, completedPayload []byte) error {
	if m == nil || m.state == nil {
		return nil
	}
	return m.state.recordTranscriptTurn(requestPayload, completedPayload)
}

func (s *xaiWebsocketIDState) upstreamIDForDownstream(downstreamID string) string {
	downstreamID = strings.TrimSpace(downstreamID)
	if s == nil || downstreamID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.downstreamToUpstream[downstreamID])
}

func (s *xaiWebsocketIDState) downstreamIDForResponse(
	upstreamID string,
	upstreamPreviousID string,
	downstreamPreviousID string,
) string {
	upstreamID = strings.TrimSpace(upstreamID)
	if s == nil || upstreamID == "" {
		return upstreamID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downstreamToUpstream == nil {
		s.downstreamToUpstream = make(map[string]string)
	}
	if downstreamPreviousID != "" && upstreamID == upstreamPreviousID {
		s.sequence++
		downstreamID := fmt.Sprintf("%s_ws_%d", upstreamID, s.sequence)
		s.downstreamToUpstream[downstreamID] = upstreamID
		return downstreamID
	}
	for downstreamID, mappedUpstreamID := range s.downstreamToUpstream {
		if mappedUpstreamID == upstreamID {
			return downstreamID
		}
	}
	s.downstreamToUpstream[upstreamID] = upstreamID
	return upstreamID
}

func (s *xaiWebsocketIDState) snapshotTranscriptInput() []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.transcriptInput) == 0 {
		return nil
	}
	return xaiMarshalRawMessages(s.transcriptInput)
}

func (s *xaiWebsocketIDState) prependTranscriptInput(payload []byte) ([]byte, error) {
	if s == nil || len(payload) == 0 {
		return payload, nil
	}
	s.mu.Lock()
	prefix := make([]json.RawMessage, 0, len(s.transcriptInput))
	for _, item := range s.transcriptInput {
		prefix = append(prefix, bytes.Clone(item))
	}
	s.mu.Unlock()
	if len(prefix) == 0 {
		return payload, nil
	}
	current := xaiJSONRawMessages(gjson.GetBytes(payload, "input"))
	merged := append(prefix, current...)
	if errValidate := validateXAIWebsocketTranscript(merged); errValidate != nil {
		return nil, errValidate
	}
	out, errSet := sjson.SetRawBytes(payload, "input", xaiMarshalRawMessages(merged))
	if errSet != nil {
		return payload, nil
	}
	return out, nil
}

func (s *xaiWebsocketIDState) recordTranscriptTurn(requestPayload []byte, completedPayload []byte) error {
	if s == nil || len(requestPayload) == 0 || len(completedPayload) == 0 {
		return nil
	}
	inputItems := xaiJSONRawMessages(gjson.GetBytes(requestPayload, "input"))
	outputItems := xaiJSONRawMessages(gjson.GetBytes(completedPayload, "response.output"))
	if len(inputItems) == 0 && len(outputItems) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(gjson.GetBytes(requestPayload, "previous_response_id").String()) == "" {
		s.transcriptInput = nil
	}
	next := make([]json.RawMessage, 0, len(s.transcriptInput)+len(inputItems)+len(outputItems))
	next = append(next, s.transcriptInput...)
	next = append(next, inputItems...)
	next = append(next, outputItems...)
	if errValidate := validateXAIWebsocketTranscript(next); errValidate != nil {
		return errValidate
	}
	s.transcriptInput = next
	return nil
}

func validateXAIWebsocketTranscript(items []json.RawMessage) error {
	if len(items) > maxXAIWebsocketTranscriptItems {
		return fmt.Errorf("xai websocket transcript exceeds maximum item count of %d", maxXAIWebsocketTranscriptItems)
	}
	totalBytes := 0
	for _, item := range items {
		totalBytes += len(item)
		if totalBytes > maxXAIWebsocketTranscriptBytes {
			return fmt.Errorf(
				"xai websocket transcript exceeds maximum size of %d bytes", maxXAIWebsocketTranscriptBytes,
			)
		}
	}
	return nil
}

func xaiJSONRawMessages(result gjson.Result) []json.RawMessage {
	if !result.IsArray() {
		return nil
	}
	items := result.Array()
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Raw) == "" {
			continue
		}
		out = append(out, json.RawMessage(item.Raw))
	}
	return out
}

func xaiMarshalRawMessages(items []json.RawMessage) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	out, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return out
}

func rewriteXAIWebsocketDownstreamIDs(
	payload []byte,
	upstreamResponseID string,
	downstreamResponseID string,
	upstreamPreviousID string,
	downstreamPreviousID string,
) []byte {
	upstreamResponseID = strings.TrimSpace(upstreamResponseID)
	downstreamResponseID = strings.TrimSpace(downstreamResponseID)
	upstreamPreviousID = strings.TrimSpace(upstreamPreviousID)
	downstreamPreviousID = strings.TrimSpace(downstreamPreviousID)
	if len(payload) == 0 || (upstreamResponseID == downstreamResponseID && upstreamPreviousID == downstreamPreviousID) {
		return payload
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return payload
	}
	if !rewriteXAIWebsocketDownstreamIDValue(
		value, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID,
	) {
		return payload
	}
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return payload
	}
	return out
}

func rewriteXAIWebsocketDownstreamIDValue(
	value any,
	upstreamResponseID string,
	downstreamResponseID string,
	upstreamPreviousID string,
	downstreamPreviousID string,
) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for key, item := range typed {
			if text, ok := item.(string); ok {
				next := text
				if key == "previous_response_id" {
					next = rewriteXAIWebsocketPreviousIDString(text, upstreamPreviousID, downstreamPreviousID)
				} else {
					next = rewriteXAIWebsocketDownstreamIDString(
						text, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID,
					)
				}
				if next != text {
					typed[key] = next
					changed = true
				}
				continue
			}
			if rewriteXAIWebsocketDownstreamIDValue(
				item, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID,
			) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i, item := range typed {
			if text, ok := item.(string); ok {
				next := rewriteXAIWebsocketDownstreamIDString(
					text, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID,
				)
				if next != text {
					typed[i] = next
					changed = true
				}
				continue
			}
			if rewriteXAIWebsocketDownstreamIDValue(
				item, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID,
			) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func rewriteXAIWebsocketPreviousIDString(value string, upstreamPreviousID string, downstreamPreviousID string) string {
	if upstreamPreviousID == "" || downstreamPreviousID == "" {
		return value
	}
	return strings.ReplaceAll(value, upstreamPreviousID, downstreamPreviousID)
}

func rewriteXAIWebsocketDownstreamIDString(
	value string,
	upstreamResponseID string,
	downstreamResponseID string,
	upstreamPreviousID string,
	downstreamPreviousID string,
) string {
	out := value
	if upstreamResponseID != "" && downstreamResponseID != "" {
		out = strings.ReplaceAll(out, upstreamResponseID, downstreamResponseID)
	}
	if upstreamPreviousID != "" && downstreamPreviousID != "" {
		out = strings.ReplaceAll(out, upstreamPreviousID, downstreamPreviousID)
	}
	return out
}

func xaiGenerateFalseCompletedPayload(createdPayload []byte) []byte {
	completed := bytes.Clone(createdPayload)
	completed, _ = sjson.SetBytes(completed, "type", "response.completed")
	completed, _ = sjson.SetBytes(completed, "response.status", "completed")
	if !gjson.GetBytes(completed, "response.output").Exists() {
		completed, _ = sjson.SetRawBytes(completed, "response.output", []byte("[]"))
	}
	return completed
}

func xaiWebsocketDialError(resp *http.Response, dialErr error) ([]byte, error) {
	body := websocketHandshakeBody(resp)
	if resp != nil && resp.StatusCode > 0 {
		return body, xaiStatusErr(resp.StatusCode, body)
	}
	return body, dialErr
}

func parseXAIWebsocketError(payload []byte) (error, bool) {
	if err, ok := parseCodexWebsocketError(payload); ok {
		return err, true
	}
	errNode := gjson.GetBytes(payload, "error")
	if !errNode.Exists() {
		return nil, false
	}
	message := strings.TrimSpace(errNode.Get("message").String())
	if message == "" {
		message = strings.TrimSpace(errNode.String())
	}
	if message == "" {
		message = errNode.Raw
	}
	code := http.StatusInternalServerError
	if strings.Contains(strings.ToLower(message), "invalid") {
		code = http.StatusBadRequest
	}
	return statusErr{code: code, msg: message}, true
}

func websocketStreamResponseHeaders() http.Header {
	return websocketStreamResponseHeadersFrom(nil)
}

func websocketStreamResponseHeadersFrom(headers http.Header) http.Header {
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	out.Set("Content-Type", "application/json")
	return out
}

func recordXAIWebsocketHandshake(ctx context.Context, cfg *config.Config, resp *http.Response) {
	if resp == nil {
		return
	}
	recordAPIWebsocketHandshake(ctx, cfg, resp.StatusCode, resp.Header.Clone())
	closeHTTPResponseBody(resp, "xai websockets executor: close handshake response body error")
}

func logXAIWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Infof(
		"xai websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID),
		strings.TrimSpace(authID), strings.TrimSpace(wsURL),
	)
}

func logXAIWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		log.Infof(
			"xai websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v",
			strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL),
			strings.TrimSpace(reason), err,
		)
		return
	}
	log.Infof(
		"xai websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID),
		strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason),
	)
}

// XAIAutoExecutor routes xAI requests to the websocket transport only when the client is connected over websocket
// and the selected auth enables websocket transport.
type XAIAutoExecutor struct {
	httpExec *XAIExecutor
	wsExec   *XAIWebsocketsExecutor
}

func NewXAIAutoExecutor(cfg *config.Config) *XAIAutoExecutor {
	return &XAIAutoExecutor{
		httpExec: NewXAIExecutor(cfg),
		wsExec:   NewXAIWebsocketsExecutor(cfg),
	}
}

func (e *XAIAutoExecutor) Identifier() string { return "xai" }

func (e *XAIAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *XAIAutoExecutor) HttpRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *XAIAutoExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai auto executor: executor is nil")
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("xai auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && xaiWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *XAIAutoExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func xaiWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	return codexWebsocketsEnabled(auth)
}
