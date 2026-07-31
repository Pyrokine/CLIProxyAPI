// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

// codexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type codexWebsocketsExecutor struct {
	*codexExecutor

	sessMu   sync.Mutex
	sessions map[string]*codexWebsocketSession
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string

	writeMu sync.Mutex

	activeMu     sync.Mutex
	activeConn   *websocket.Conn
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc

	readerConn *websocket.Conn
}

func newCodexWebsocketsExecutor(cfg *config.Config) *codexWebsocketsExecutor {
	return &codexWebsocketsExecutor{
		codexExecutor: newCodexExecutor(cfg),
		sessions:      make(map[string]*codexWebsocketSession),
	}
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	payload []byte
	err     error
}

func (s *codexWebsocketSession) setActive(conn *websocket.Conn, ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCancel != nil {
		s.activeCancel()
	}
	s.activeConn = conn
	s.activeCh = ch
	s.activeDone = nil
	s.activeCancel = nil
	if conn != nil && ch != nil {
		activeCtx, activeCancel := context.WithCancel(context.Background())
		s.activeDone = activeCtx.Done()
		s.activeCancel = activeCancel
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) clearActive(conn *websocket.Conn, ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeConn == conn && s.activeCh == ch {
		s.activeConn = nil
		s.activeCh = nil
		if s.activeCancel != nil {
			s.activeCancel()
		}
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) activeForConn(
	conn *websocket.Conn,
) (chan codexWebsocketRead, <-chan struct{}, bool) {
	if s == nil || conn == nil {
		return nil, nil, false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeConn != conn || s.activeCh == nil {
		return nil, nil, false
	}
	return s.activeCh, s.activeDone, true
}

func (s *codexWebsocketSession) writeMessage(conn *websocket.Conn, msgType int, payload []byte) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteMessage(msgType, payload)
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	conn.SetPingHandler(
		func(appData string) error {
			s.writeMu.Lock()
			defer s.writeMu.Unlock()
			// Reply pongs from the same write lock to avoid concurrent writes.
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		},
	)
}

func (e *codexWebsocketsExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return e.codexExecutor.executeCompact(ctx, auth, req, opts)
	}

	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, false)
	if err != nil {
		return resp, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload
	body, replayScope := applyCodexReasoningReplayCache(ctx, from, req, opts, body)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return resp, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())
	clientBody := body
	upstreamBody, identityState := applyCodexIdentityConfuseBody(e.cfg, auth, originalPayload, body)
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)
	applyModelHeaderOverrides(wsHeaders, baseModel)
	applyCodexIdentityConfuseHeaders(wsHeaders, &identityState)

	var authID string
	if auth != nil {
		authID = auth.ID
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		sess.reqMu.Lock()
		defer sess.reqMu.Unlock()
	}

	wsReqBody := buildCodexWebsocketRequestBody(upstreamBody)
	wsReqLog := codexWebsocketRequestLog(wsURL, wsHeaders, wsReqBody, e.Identifier(), auth)
	recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBody, e.Identifier(), auth)
	recordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			recordAPIWebsocketUpgradeRejection(
				ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr,
			)
		}
		if respHS == nil {
			recordAPIResponseError(ctx, e.cfg, errDial)
			recordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
			return resp, errDial
		}
		if respHS.StatusCode == http.StatusUpgradeRequired {
			return e.codexExecutor.Execute(ctx, auth, req, opts)
		}
		if respHS.StatusCode > 0 {
			return resp, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		recordAPIResponseError(ctx, e.cfg, errDial)
		recordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		return resp, errDial
	}
	recordCodexWebsocketHandshake(ctx, e.cfg, respHS)
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(conn, readCh)
		defer func() {
			sess.clearActive(conn, readCh)
		}()
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a fresh websocket connection. This is mainly to handle
			// upstream closing the socket between sequential requests within the same
			// execution session.
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry == nil && connRetry != nil {
				sess.setActive(connRetry, readCh)
				wsReqBodyRetry := buildCodexWebsocketRequestBody(upstreamBody)
				wsReqLogRetry := codexWebsocketRequestLog(wsURL, wsHeaders, wsReqBodyRetry, e.Identifier(), auth)
				recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBodyRetry, e.Identifier(), auth)
				recordAPIWebsocketRequest(ctx, e.cfg, wsReqLogRetry)
				recordCodexWebsocketHandshake(ctx, e.cfg, respHSRetry)
				if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry == nil {
					conn = connRetry
				} else {
					e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
					recordAPIResponseError(ctx, e.cfg, errSendRetry)
					recordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
					return resp, errSendRetry
				}
			} else {
				closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
				recordAPIResponseError(ctx, e.cfg, errDialRetry)
				recordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				return resp, errDialRetry
			}
		} else {
			recordAPIResponseError(ctx, e.cfg, errSend)
			recordAPIWebsocketError(ctx, e.cfg, "send", errSend)
			return resp, errSend
		}
	}

	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			errRead = mapCodexWebsocketReadError(errRead)
			recordAPIResponseError(ctx, e.cfg, errRead)
			recordAPIWebsocketError(ctx, e.cfg, "read", errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = fmt.Errorf("codex websockets executor: unexpected binary message")
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
				}
				recordAPIResponseError(ctx, e.cfg, err)
				recordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
				return resp, err
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		appendAPIResponseChunk(ctx, e.cfg, payload)
		appendAPIWebsocketResponse(ctx, e.cfg, payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			clearCodexReasoningReplayOnInvalidSignature(replayScope, statusErrCode(wsErr), []byte(wsErr.Error()))
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			recordAPIResponseError(ctx, e.cfg, wsErr)
			recordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexCompletionEvent(payload)
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" {
			cacheCodexReasoningReplayFromCompleted(replayScope, payload)
			if detail, ok := parseCodexUsage(payload); ok {
				reporter.publish(ctx, detail)
			}
			var param any
			out := sdktranslator.TranslateNonStream(
				ctx, to, from, req.Model, originalPayload, clientBody,
				applyCodexIdentityExposeResponsePayload(payload, identityState), &param,
			)
			resp = cliproxyexecutor.Response{Payload: out}
			return resp, nil
		}
	}
}

func (e *codexWebsocketsExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", auth.ID, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, true)
	if err != nil {
		return nil, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload
	body, replayScope := applyCodexReasoningReplayCache(ctx, from, req, opts, body)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(from, req, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())
	clientBody := body
	upstreamBody, identityState := applyCodexIdentityConfuseBody(e.cfg, auth, originalPayload, body)
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)
	applyModelHeaderOverrides(wsHeaders, baseModel)
	applyCodexIdentityConfuseHeaders(wsHeaders, &identityState)

	authID := auth.ID

	// noinspection GoDfaConstantCondition,GoMaybeNil — sess may or may not be nil depending on runtime path.
	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		if sess != nil {
			sess.reqMu.Lock()
		}
	}

	wsReqBody := buildCodexWebsocketRequestBody(upstreamBody)
	wsReqLog := codexWebsocketRequestLog(wsURL, wsHeaders, wsReqBody, e.Identifier(), auth)
	recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBody, e.Identifier(), auth)
	recordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			recordAPIWebsocketUpgradeRejection(
				ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr,
			)
		}
		if respHS == nil {
			recordAPIResponseError(ctx, e.cfg, errDial)
			recordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return nil, errDial
		}
		if respHS.StatusCode == http.StatusUpgradeRequired {
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return e.codexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if respHS.StatusCode > 0 {
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return nil, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		recordAPIResponseError(ctx, e.cfg, errDial)
		recordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errDial
	}
	recordCodexWebsocketHandshake(ctx, e.cfg, respHS)

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(conn, readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		recordAPIResponseError(ctx, e.cfg, errSend)
		recordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a new websocket connection for the same execution session.
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry != nil || connRetry == nil {
				closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
				recordAPIResponseError(ctx, e.cfg, errDialRetry)
				recordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				sess.clearActive(conn, readCh)
				sess.reqMu.Unlock()
				return nil, errDialRetry
			}
			sess.setActive(connRetry, readCh)
			wsReqBodyRetry := buildCodexWebsocketRequestBody(upstreamBody)
			wsReqLogRetry := codexWebsocketRequestLog(wsURL, wsHeaders, wsReqBodyRetry, e.Identifier(), auth)
			recordUpstreamRequest(ctx, e.cfg, wsURL, wsHeaders.Clone(), wsReqBodyRetry, e.Identifier(), auth)
			recordAPIWebsocketRequest(ctx, e.cfg, wsReqLogRetry)
			recordCodexWebsocketHandshake(ctx, e.cfg, respHSRetry)
			if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
				recordAPIResponseError(ctx, e.cfg, errSendRetry)
				recordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
				e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
				sess.clearActive(connRetry, readCh)
				sess.reqMu.Unlock()
				return nil, errSendRetry
			}
			conn = connRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(conn, readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		failContext := func() error {
			errContext := ctx.Err()
			if errContext == nil {
				errContext = context.Canceled
			}
			terminateReason = "context_done"
			terminateErr = errContext
			recordAPIResponseError(ctx, e.cfg, errContext)
			reporter.publishFailure(ctx, errContext)
			return errContext
		}

		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		for {
			if ctx != nil && ctx.Err() != nil {
				errContext := failContext()
				_ = send(cliproxyexecutor.StreamChunk{Err: errContext})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					errContext := failContext()
					_ = send(cliproxyexecutor.StreamChunk{Err: errContext})
					return
				}
				terminateReason = "read_error"
				errRead = mapCodexWebsocketReadError(errRead)
				terminateErr = errRead
				recordAPIResponseError(ctx, e.cfg, errRead)
				recordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.publishFailure(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = errBinary
					recordAPIResponseError(ctx, e.cfg, errBinary)
					recordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", errBinary)
					reporter.publishFailure(ctx, errBinary)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: errBinary})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			payload = applyCodexIdentityConfuseResponsePayload(payload, identityState)
			appendAPIResponseChunk(ctx, e.cfg, payload)
			appendAPIWebsocketResponse(ctx, e.cfg, payload)

			if !gjson.ValidBytes(payload) {
				errMalformed := statusErr{
					code: http.StatusBadGateway,
					msg:  "codex websockets executor: upstream returned malformed event data",
				}
				terminateReason = "upstream_error"
				terminateErr = errMalformed
				recordAPIResponseError(ctx, e.cfg, errMalformed)
				recordAPIWebsocketError(ctx, e.cfg, "upstream_error", errMalformed)
				reporter.publishFailure(ctx, errMalformed)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", errMalformed)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: errMalformed})
				return
			}

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				clearCodexReasoningReplayOnInvalidSignature(replayScope, statusErrCode(wsErr), []byte(wsErr.Error()))
				terminateReason = "upstream_error"
				terminateErr = wsErr
				recordAPIResponseError(ctx, e.cfg, wsErr)
				recordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.publishFailure(ctx, wsErr)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			eventType := gjson.GetBytes(payload, "type").String()
			isTerminalEvent := eventType == "response.completed" || eventType == "response.done" || eventType == "error"
			if cliproxyexecutor.DownstreamWebsocket(ctx) {
				chunk := cliproxyexecutor.StreamChunk{
					Payload: applyCodexIdentityExposeResponsePayload(payload, identityState),
				}
				var delivery <-chan error
				if isTerminalEvent {
					deliveryResult := make(chan error, 1)
					var acknowledgeOnce sync.Once
					chunk.Acknowledge = func(errDelivery error) {
						acknowledgeOnce.Do(func() {
							deliveryResult <- errDelivery
						})
					}
					delivery = deliveryResult
				}
				if !send(chunk) {
					failContext()
					return
				}
				if isTerminalEvent {
					var errDelivery error
					select {
					case errDelivery = <-delivery:
					case <-ctx.Done():
						errDelivery = ctx.Err()
					}
					detail, hasUsage := parseCodexUsage(payload)
					if errDelivery != nil {
						terminateReason = "downstream_delivery_error"
						terminateErr = errDelivery
						if hasUsage {
							reporter.publishFailureWithDetail(ctx, detail, errDelivery)
						} else {
							reporter.publishFailure(ctx, errDelivery)
						}
						return
					}
					if hasUsage {
						reporter.publish(context.WithoutCancel(ctx), detail)
					} else {
						reporter.ensurePublished(context.WithoutCancel(ctx))
					}
					return
				}
				continue
			}

			payload = normalizeCodexCompletionEvent(payload)
			eventType = gjson.GetBytes(payload, "type").String()
			var completedUsagePayload []byte
			switch eventType {
			case "response.output_item.done":
				collectCodexOutputItemDone(payload, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.done":
				payload = patchCodexCompletedOutput(payload, outputItemsByIndex, outputItemsFallback)
				cacheCodexReasoningReplayFromCompleted(replayScope, payload)
				completedUsagePayload = bytes.Clone(payload)
			}

			line := encodeCodexWebsocketAsSSE(payload)
			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, originalPayload, clientBody,
				applyCodexIdentityExposeResponsePayload(line, identityState), &param,
			)
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					failContext()
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" {
				if detail, ok := parseCodexUsage(completedUsagePayload); ok {
					reporter.publish(ctx, detail)
				} else {
					reporter.ensurePublished(ctx)
				}
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *codexWebsocketsExecutor) dialCodexWebsocket(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	wsURL string,
	headers http.Header,
) (*websocket.Conn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if conn != nil {
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
	}
	return conn, resp, err
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func mapCodexWebsocketReadError(err error) error {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return statusErr{
			code: http.StatusRequestEntityTooLarge,
			msg:  `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`,
		}
	}
	return err
}

func buildCodexWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}

	wsReqBody, errSet := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	fallback := bytes.Clone(body)
	fallback, _ = sjson.SetBytes(fallback, "type", "response.create")
	return fallback
}

func readCodexWebsocketMessage(
	ctx context.Context,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh chan codexWebsocketRead,
) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

func newProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL == "" {
		return dialer
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: parse proxy URL failed: %v", errParse)
		return dialer
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h":
		proxyDialer, errDialer := proxyutil.NewDialerForURL(parsedURL)
		if errDialer != nil {
			log.Errorf("codex websockets executor: create proxy dialer failed: %v", errDialer)
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return proxyDialer.Dial(network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(parsedURL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", parsedURL.Scheme)
	}

	return dialer
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

func applyCodexPromptCacheHeaders(from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte) (
	[]byte,
	http.Header,
) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}

	var cache codexCache
	if shouldUseImplicitCodexConversationCache(from, req.Model) {
		if cached, ok := codexPromptCacheFromClaudeRequest(req.Model, req.Payload); ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
		headers.Set("Conversation_id", cache.ID)
		headers.Set("Session_id", cache.ID)
	}

	return rawJSON, headers
}

func applyCodexWebsocketHeaders(
	ctx context.Context,
	headers http.Header,
	auth *cliproxyauth.Auth,
	token string,
	cfg *config.Config,
) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	var ginHeaders http.Header
	if ginCtx := ginContextFrom(ctx); ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = trustedCodexClientHeaders(ginCtx.Request.Header)
	}

	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "x-codex-beta-features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-metadata", "")
	misc.EnsureHeader(headers, ginHeaders, "x-responsesapi-include-timing-metrics", "")

	misc.EnsureHeader(headers, ginHeaders, "Version", codexClientVersion)
	betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	if betaHeader == "" && ginHeaders != nil {
		betaHeader = strings.TrimSpace(ginHeaders.Get("OpenAI-Beta"))
	}
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", betaHeader)
	ensureHeaderWithConfigPrecedence(headers, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") {
		ensureHeaderWithTargetPrecedence(headers, ginHeaders, "Session_id", uuid.NewString())
	}

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if !isAPIKey {
		headers.Set("Originator", codexOriginator)
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					setHeaderCasePreserved(headers, "ChatGPT-Account-ID", trimmed)
				}
			}
		}
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)

	return headers
}

type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func parseCodexWebsocketError(payload []byte) (error, bool) {
	err, ok := parseCodexEventError(payload)
	if !ok {
		return nil, false
	}
	headers := parseCodexWebsocketErrorHeaders(payload)
	if len(headers) == 0 {
		return err, true
	}
	return statusErrWithHeaders{
		statusErr: statusErr{code: statusErrCode(err), msg: err.Error()},
		headers:   headers,
	}, true
}

func parseCodexWebsocketErrorHeaders(payload []byte) http.Header {
	headersNode := gjson.GetBytes(payload, "headers")
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	mapped := make(http.Header)
	headersNode.ForEach(
		func(key, value gjson.Result) bool {
			name := strings.TrimSpace(key.String())
			if name == "" {
				return true
			}
			switch value.Type {
			case gjson.String:
				if v := strings.TrimSpace(value.String()); v != "" {
					mapped.Set(name, v)
				}
			case gjson.Number, gjson.True, gjson.False:
				if v := strings.TrimSpace(value.Raw); v != "" {
					mapped.Set(name, v)
				}
			default:
			}
			return true
		},
	)
	if len(mapped) == 0 {
		return nil
	}
	return mapped
}

func normalizeCodexCompletionEvent(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

func encodeCodexHTTPEvent(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	return encodeSSEDataFrame(payload)
}

func encodeCodexWebsocketAsSSE(payload []byte) []byte {
	return encodeCodexHTTPEvent(payload)
}

func codexWebsocketRequestLog(
	wsURL string,
	headers http.Header,
	body []byte,
	provider string,
	auth *cliproxyauth.Auth,
) upstreamRequestLog {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	return upstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   headers.Clone(),
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
}

func websocketUpgradeRequestLog(info upstreamRequestLog) upstreamRequestLog {
	upgradeInfo := info
	upgradeInfo.URL = websocketUpgradeRequestURL(info.URL)
	upgradeInfo.Method = http.MethodGet
	upgradeInfo.Body = nil
	upgradeInfo.Headers = info.Headers.Clone()
	if upgradeInfo.Headers == nil {
		upgradeInfo.Headers = make(http.Header)
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Connection")) == "" {
		upgradeInfo.Headers.Set("Connection", "Upgrade")
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Upgrade")) == "" {
		upgradeInfo.Headers.Set("Upgrade", "websocket")
	}
	return upgradeInfo
}

func recordCodexWebsocketHandshake(ctx context.Context, cfg *config.Config, resp *http.Response) {
	if resp == nil {
		return
	}
	recordAPIWebsocketHandshake(ctx, cfg, resp.StatusCode, resp.Header.Clone())
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
}

func websocketHandshakeBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
	if len(body) == 0 {
		return nil
	}
	return body
}

func closeHTTPResponseBody(resp *http.Response, logPrefix string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("%s: %v", logPrefix, errClose)
	}
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func (e *codexWebsocketsExecutor) getOrCreateSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess, ok := e.sessions[sessionID]; ok && sess != nil {
		return sess
	}
	sess := &codexWebsocketSession{sessionID: sessionID}
	e.sessions[sessionID] = sess
	return sess
}

func (e *codexWebsocketsExecutor) ensureUpstreamConn(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	authID string,
	wsURL string,
	headers http.Header,
) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	}

	sess.connMu.Lock()
	conn := sess.conn
	readerConn := sess.readerConn
	staleAuthID := sess.authID
	staleWSURL := sess.wsURL
	var staleConn *websocket.Conn
	if conn != nil && (staleAuthID != authID || staleWSURL != wsURL) {
		staleConn = conn
		sess.conn = nil
		if sess.readerConn == conn {
			sess.readerConn = nil
		}
		sess.authID = ""
		sess.wsURL = ""
		conn = nil
		readerConn = nil
	}
	sess.connMu.Unlock()
	if staleConn != nil {
		logCodexWebsocketDisconnected(sess.sessionID, staleAuthID, staleWSURL, "connection_identity_changed", nil)
		if errClose := staleConn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close stale websocket error: %v", errClose)
		}
	}
	if conn != nil {
		if readerConn != conn {
			sess.connMu.Lock()
			sess.readerConn = conn
			sess.connMu.Unlock()
			sess.configureConn(conn)
			go e.readUpstreamLoop(sess, conn)
		}
		return conn, nil, nil
	}

	conn, resp, errDial := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, resp, errDial
	}

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		sess.connMu.Unlock()
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		return previous, nil, nil
	}
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConn(conn)
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, resp, nil
}

func deliverCodexWebsocketTerminalRead(
	ch chan codexWebsocketRead,
	done <-chan struct{},
	event codexWebsocketRead,
) {
	select {
	case ch <- event:
	case <-done:
	}
}

func (e *codexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			if ch, done, ok := sess.activeForConn(conn); ok {
				deliverCodexWebsocketTerminalRead(ch, done, codexWebsocketRead{conn: conn, err: errRead})
				sess.clearActive(conn, ch)
			}
			e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				if ch, done, ok := sess.activeForConn(conn); ok {
					deliverCodexWebsocketTerminalRead(ch, done, codexWebsocketRead{conn: conn, err: errBinary})
					sess.clearActive(conn, ch)
				}
				e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				return
			}
			continue
		}

		ch, done, ok := sess.activeForConn(conn)
		if !ok {
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload}:
		case <-done:
		}
	}
}

func (e *codexWebsocketsExecutor) invalidateUpstreamConn(
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

	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func (e *codexWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil {
		return
	}
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.closeAllExecutionSessions("sessions_closed")
		return
	}

	e.sessMu.Lock()
	sess := e.sessions[sessionID]
	delete(e.sessions, sessionID)
	e.sessMu.Unlock()

	e.closeExecutionSession(sess, "session_closed")
}

func (e *codexWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil {
		return
	}

	e.sessMu.Lock()
	sessions := make([]*codexWebsocketSession, 0, len(e.sessions))
	for sessionID, sess := range e.sessions {
		delete(e.sessions, sessionID)
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	e.sessMu.Unlock()

	for i := range sessions {
		e.closeExecutionSession(sessions[i], reason)
	}
}

func (e *codexWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
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
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func logCodexWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Infof(
		"codex websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID),
		strings.TrimSpace(authID), strings.TrimSpace(wsURL),
	)
}

func logCodexWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		log.Infof(
			"codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v",
			strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL),
			strings.TrimSpace(reason), err,
		)
		return
	}
	log.Infof(
		"codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID),
		strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason),
	)
}

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets.
//
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	httpExec *codexExecutor
	wsExec   *codexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: newCodexExecutor(cfg),
		wsExec:   newCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}
