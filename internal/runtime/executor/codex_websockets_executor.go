// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/client/grokbuild"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

// codexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for WebSocket upgrade failures.
type codexWebsocketsExecutor struct {
	*codexExecutor

	sessMu   sync.Mutex
	sessions map[string]*codexWebsocketSession
}

type codexModelHeadersContextKey struct{}

type websocketConnectionCloser struct {
	responseURL string
	conn        *websocket.Conn
	once        sync.Once
	err         error
}

func newWebsocketConnectionCloser(conn *websocket.Conn, responses ...*http.Response) *websocketConnectionCloser {
	if conn == nil {
		return nil
	}
	closer := &websocketConnectionCloser{conn: conn}
	if len(responses) > 0 && responses[0] != nil && responses[0].Request != nil && responses[0].Request.URL != nil {
		closer.responseURL = util.SanitizeUpstreamURL(responses[0].Request.URL.String())
	}
	return closer
}

func (c *websocketConnectionCloser) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.once.Do(func() { c.err = c.conn.Close() })
	return c.err
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu                    sync.Mutex
	conn                      *websocket.Conn
	connCloser                *websocketConnectionCloser
	lifecycleBindMu           sync.Mutex
	lifecycle                 cliproxyexecutor.ExecutionLifecycle
	wsURL                     string
	authID                    string
	credentialSHA256          string
	proxyURL                  string
	multiAgentV2OptimizedConn *websocket.Conn
	homeLifecycle             *cliproxyauth.HomeDispatchSelection
	modelHeaders              map[string]string

	writeMu sync.Mutex

	activeMu     sync.Mutex
	activeConn   *websocket.Conn
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc

	responseStateConn   *websocket.Conn
	terminalResponseIDs map[string]struct{}

	readerConn *websocket.Conn

	upstreamDisconnectOnce    sync.Once
	upstreamDisconnectCh      chan error
	upstreamDisconnectErrMu   sync.RWMutex
	upstreamDisconnectErrConn *websocket.Conn
	upstreamDisconnectErr     error
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

func (s *codexWebsocketSession) clearActive(conn *websocket.Conn, ch chan codexWebsocketRead) bool {
	if s == nil {
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeConn != conn || s.activeCh != ch {
		return false
	}
	s.activeConn = nil
	s.activeCh = nil
	if s.activeCancel != nil {
		s.activeCancel()
	}
	s.activeCancel = nil
	s.activeDone = nil
	return true
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

func (s *codexWebsocketSession) idleHomeOwner(conn *websocket.Conn) *cliproxyauth.HomeDispatchSelection {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != conn {
		return nil
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeConn == conn && s.activeCh != nil {
		return nil
	}
	return s.homeLifecycle
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
	if err := s.upstreamDisconnectError(conn); err != nil {
		return err
	}
	return conn.WriteMessage(msgType, payload)
}

func (s *codexWebsocketSession) setMultiAgentV2Optimized(conn *websocket.Conn, optimized bool) {
	if s == nil || conn == nil {
		return
	}
	s.connMu.Lock()
	if s.conn == conn {
		if optimized {
			s.multiAgentV2OptimizedConn = conn
		} else {
			s.multiAgentV2OptimizedConn = nil
		}
	}
	s.connMu.Unlock()
}

func (s *codexWebsocketSession) isMultiAgentV2Optimized(conn *websocket.Conn) bool {
	if s == nil || conn == nil {
		return false
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn == conn && s.multiAgentV2OptimizedConn == conn
}

func (s *codexWebsocketSession) resetUpstreamDisconnectError(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.upstreamDisconnectErrMu.Lock()
	s.upstreamDisconnectErrConn = conn
	s.upstreamDisconnectErr = nil
	s.upstreamDisconnectErrMu.Unlock()
}

func (s *codexWebsocketSession) setUpstreamDisconnectError(conn *websocket.Conn, err error) {
	if s == nil || conn == nil || err == nil {
		return
	}
	s.upstreamDisconnectErrMu.Lock()
	if s.upstreamDisconnectErrConn == conn && s.upstreamDisconnectErr == nil {
		s.upstreamDisconnectErr = err
	}
	s.upstreamDisconnectErrMu.Unlock()
}

func (s *codexWebsocketSession) upstreamDisconnectError(conn *websocket.Conn) error {
	if s == nil || conn == nil {
		return nil
	}
	s.upstreamDisconnectErrMu.RLock()
	defer s.upstreamDisconnectErrMu.RUnlock()
	if s.upstreamDisconnectErrConn != conn {
		return nil
	}
	return s.upstreamDisconnectErr
}

func (s *codexWebsocketSession) bindExecutionLifecycle(
	ctx context.Context, opts cliproxyexecutor.Options, conn *websocket.Conn, closer *websocketConnectionCloser,
) error {
	if closer == nil {
		return fmt.Errorf("websocket connection closer is nil")
	}
	if s == nil {
		return cliproxyexecutor.BindExecutionResource(opts, closer)
	}
	lifecycle := opts.ExecutionLifecycle
	if lifecycle == nil {
		return nil
	}
	s.lifecycleBindMu.Lock()
	defer s.lifecycleBindMu.Unlock()
	selection, homeOwned := lifecycle.(*cliproxyauth.HomeDispatchSelection)
	retainedHome := homeOwned && cliproxyexecutor.DownstreamWebsocket(ctx)
	// reflect.Value 按接口实例比较，也支持含不可比较字段的生命周期实现
	identity := reflect.ValueOf(lifecycle)
	s.connMu.Lock()
	if s.conn == conn && s.connCloser == nil {
		s.connCloser = closer
	}
	if s.conn != conn || s.connCloser != closer {
		s.connMu.Unlock()
		return fmt.Errorf("websocket connection closed before lifecycle bind")
	}
	if reflect.ValueOf(s.lifecycle) == identity {
		s.connMu.Unlock()
		if retainedHome {
			selection.Retain()
		}
		return nil
	}
	previous := s.lifecycle
	s.lifecycle = lifecycle
	if retainedHome {
		s.homeLifecycle = selection
	} else {
		s.homeLifecycle = nil
	}
	s.connMu.Unlock()
	closeOwnedConnection := func() error {
		s.connMu.Lock()
		owned := reflect.ValueOf(s.lifecycle) == identity && (retainedHome || s.conn == conn)
		ownedCloser := closer
		if owned {
			if retainedHome {
				ownedCloser = s.connCloser
			}
			ownedConn := s.conn
			s.conn = nil
			s.connCloser = nil
			s.lifecycle = nil
			s.homeLifecycle = nil
			s.modelHeaders = nil
			s.multiAgentV2OptimizedConn = nil
			s.proxyURL = ""
			if s.readerConn == ownedConn {
				s.readerConn = nil
			}
		} else if s.conn == conn {
			ownedCloser = nil
		}
		s.connMu.Unlock()
		if owned {
			s.notifyUpstreamDisconnect(nil)
			// End 等待资源回调结束后返回，异步结束可避免重复等待当前回调
			go lifecycle.End("websocket_closed")
		}
		return ownedCloser.Close()
	}
	if errBind := lifecycle.Bind(closeOwnedConnection); errBind != nil {
		return errors.Join(errBind, closeOwnedConnection())
	}
	s.connMu.Lock()
	bound := s.conn == conn && s.connCloser == closer && reflect.ValueOf(s.lifecycle) == identity
	s.connMu.Unlock()
	if !bound {
		return fmt.Errorf("websocket connection closed during lifecycle bind")
	}
	if previous != nil {
		previous.End("websocket_owner_replaced")
	}
	if retainedHome {
		selection.Retain()
	}
	return nil
}

func closeWebsocketAfterBindFailure(
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	closer *websocketConnectionCloser,
) {
	if sess != nil {
		sess.connMu.Lock()
		if sess.conn == conn {
			sess.conn = nil
			sess.connCloser = nil
			sess.lifecycle = nil
			sess.homeLifecycle = nil
			sess.modelHeaders = nil
			sess.multiAgentV2OptimizedConn = nil
			sess.proxyURL = ""
			if sess.readerConn == conn {
				sess.readerConn = nil
			}
		}
		sess.connMu.Unlock()
	}
	if errClose := closer.Close(); errClose != nil {
		log.Errorf("websockets executor: close lifecycle bind failure connection error: %v", errClose)
	}
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.resetUpstreamDisconnectError(conn)
	s.activeMu.Lock()
	if s.responseStateConn != conn {
		s.responseStateConn = conn
		s.terminalResponseIDs = make(map[string]struct{})
	}
	s.activeMu.Unlock()
	conn.SetPingHandler(
		func(appData string) error {
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		},
	)
	defaultCloseHandler := conn.CloseHandler()
	conn.SetCloseHandler(
		func(code int, text string) error {
			s.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: code, Text: text})
			return defaultCloseHandler(code, text)
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
	ctx = reporter.withContext(ctx)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(
		ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, false, true,
	)
	if err != nil {
		return resp, err
	}
	finalize := codexPayloadFinalizer(ctx, e.cfg, &prepared, &req, opts, e.Identifier())
	body, err := finalize(prepared.Body)
	if err != nil {
		return resp, err
	}
	originalPayload := prepared.OriginalPayload
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, auth, from, req, opts, body)
	if errReplay != nil {
		return resp, errReplay
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return resp, err
	}

	body, wsHeaders := applyCodexPromptCacheHeadersWithContext(ctx, from, req, body, opts.Headers)
	upstreamBody, identityState := applyCodexIdentityConfuseBody(
		e.cfg, auth, originalPayload, body, req.Metadata[codexConfiguredPromptCacheMetadataKey] == true,
	)
	body = upstreamBody
	reporter.setTranslatedReasoningEffort(body, to.String())
	clientBody := body
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg, prepared.Native, opts.Headers)
	applyCodexIdentityConfuseHeaders(wsHeaders, &identityState)
	applyCodexRoutingHint(ctx, wsHeaders, auth, gjson.GetBytes(body, "model").String(), body, opts.Headers)
	modelHeaders := applyModelHeaderOverrides(wsHeaders, gjson.GetBytes(body, "model").String(), e.Identifier())

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

	var conn *websocket.Conn
	var connCloser *websocketConnectionCloser
	var respHS *http.Response
	var errDial error
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		conn = existingCodexWebsocketSessionConn(
			sess, authID, wsURL, cliproxyauth.AccessTokenSHA256(auth), effectiveProxyURL(ctx, e.cfg, auth),
		)
		if conn != nil {
			sess.connMu.Lock()
			connCloser = sess.connCloser
			sess.connMu.Unlock()
		}
		if conn == nil {
			return resp, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
	} else {
		conn, connCloser, respHS, errDial = e.ensureUpstreamConn(
			context.WithValue(ctx, codexModelHeadersContextKey{}, modelHeaders), auth, sess, authID, wsURL, wsHeaders,
		)
	}
	if errDial != nil {
		bodyErr, normalizedDialErr := codexWebsocketDialError(respHS, errDial)
		if respHS != nil {
			recordAPIWebsocketUpgradeRejection(
				ctx,
				e.cfg,
				websocketUpgradeRequestLog(wsReqLog),
				respHS.StatusCode,
				respHS.Header.Clone(),
				bodyErr,
				respHS,
			)
		}
		recordAPIResponseError(ctx, e.cfg, normalizedDialErr)
		recordAPIWebsocketError(ctx, e.cfg, "dial", normalizedDialErr)
		return resp, normalizedDialErr
	}
	recordCodexWebsocketHandshake(ctx, e.cfg, respHS, connCloser)
	if errBind := sess.bindExecutionLifecycle(ctx, opts, conn, connCloser); errBind != nil {
		closeWebsocketAfterBindFailure(sess, conn, connCloser)
		return resp, errBind
	}
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := connCloser.Close(); errClose != nil {
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
	restoreMultiAgentV2 := prepared.OptimizedMultiAgentV2
	if sess != nil {
		restoreMultiAgentV2 = !prepared.MultiAgentV2Conflict &&
			(prepared.OptimizedMultiAgentV2 || sess.isMultiAgentV2Optimized(conn))
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		errSend = mapCodexWebsocketWriteError(sess, conn, errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)
		}
		recordAPIResponseError(ctx, e.cfg, errSend)
		recordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		return resp, errSend
	}
	if sess != nil && (prepared.OptimizedMultiAgentV2 || prepared.MultiAgentV2Conflict) {
		sess.setMultiAgentV2Optimized(
			conn,
			prepared.OptimizedMultiAgentV2 && !prepared.MultiAgentV2Conflict,
		)
	}

	responseStarted := false
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh, connCloser)
		if errRead != nil {
			errRead = mapCodexWebsocketReadError(errRead)
			if responseStarted {
				errRead = markResponseStarted(errRead)
			}
			recordAPIResponseError(ctx, e.cfg, errRead)
			recordAPIWebsocketError(ctx, e.cfg, "read", errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = markResponseStarted(fmt.Errorf("codex websockets executor: unexpected binary message"))
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
		helps.EmitWebSocketResponseEvent(ctx, opts, auth, e.Identifier(), req.Model, payload)
		responseStarted = true
		appendAPIResponseChunk(ctx, e.cfg, payload)
		appendAPIWebsocketResponse(ctx, e.cfg, payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
				ctx,
				replayScope,
				statusErrCode(wsErr),
				[]byte(wsErr.Error()),
			); errClearReplay != nil {
				log.Warn("codex reasoning replay cache delete failed")
			}
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			recordAPIResponseError(ctx, e.cfg, wsErr)
			recordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexCompletionEvent(payload)
		payload = helps.RestoreCodexMultiAgentV2Response(payload, restoreMultiAgentV2)
		eventType := gjson.GetBytes(payload, "type").String()
		switch eventType {
		case "response.output_item.done":
			collectCodexOutputItemDone(payload, outputItemsByIndex, &outputItemsFallback)
			continue
		case "response.completed", "response.incomplete":
			if !prepared.Native {
				payload = patchCodexCompletedOutput(payload, outputItemsByIndex, outputItemsFallback)
			}
			var param any
			out := sdktranslator.TranslateNonStream(
				ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, clientBody,
				applyCodexIdentityExposeResponsePayload(payload, identityState), &param,
			)
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil ||
				len(out) == 0 {
				if errTranslate == nil {
					errTranslate = markResponseStarted(
						statusErr{
							code: http.StatusBadGateway, msg: "upstream response translation returned an empty payload",
						},
					)
				}
				recordAPIResponseError(ctx, e.cfg, errTranslate)
				detail, _ := parseCodexUsage(payload)
				reporter.publishFailureWithDetail(ctx, detail, errTranslate)
				return resp, errTranslate
			}
			if eventType == "response.completed" {
				cacheCodexReasoningReplayFromCompleted(ctx, replayScope, payload)
			}
			observeResponseTokenEvent(ctx, payload)
			reporter.observeResponseModel(payload)
			if detail, ok := parseCodexUsage(payload); ok {
				reporter.publish(ctx, detail)
			}
			reporter.ensurePublished(ctx)
			publishCodexImageToolUsage(ctx, reporter, clientBody, payload)
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
		return nil, statusErr{
			code: http.StatusBadRequest,
			msg:  "streaming not supported for /responses/compact",
		}
	}

	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	ctx = reporter.withContext(ctx)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(
		ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, true, true,
	)
	if err != nil {
		return nil, err
	}
	finalize := codexPayloadFinalizer(ctx, e.cfg, &prepared, &req, opts, e.Identifier())
	body, err := finalize(prepared.Body)
	if err != nil {
		return nil, err
	}
	originalPayload := prepared.OriginalPayload
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, auth, from, req, opts, body)
	if errReplay != nil {
		return nil, errReplay
	}
	isGrokClient := grokbuild.IsGrokClientContext(ctx, opts.Headers)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders := applyCodexPromptCacheHeadersWithContext(ctx, from, req, body, opts.Headers)
	upstreamBody, identityState := applyCodexIdentityConfuseBody(
		e.cfg, auth, originalPayload, body, req.Metadata[codexConfiguredPromptCacheMetadataKey] == true,
	)
	body = upstreamBody
	reporter.setTranslatedReasoningEffort(body, to.String())
	clientBody := body
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg, prepared.Native, opts.Headers)
	applyCodexIdentityConfuseHeaders(wsHeaders, &identityState)
	applyCodexRoutingHint(ctx, wsHeaders, auth, gjson.GetBytes(body, "model").String(), body, opts.Headers)
	modelHeaders := applyModelHeaderOverrides(wsHeaders, gjson.GetBytes(body, "model").String(), e.Identifier())

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

	var conn *websocket.Conn
	var connCloser *websocketConnectionCloser
	var respHS *http.Response
	var errDial error
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		if sess == nil {
			return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
		conn = existingCodexWebsocketSessionConn(
			sess, authID, wsURL, cliproxyauth.AccessTokenSHA256(auth), effectiveProxyURL(ctx, e.cfg, auth),
		)
		if conn == nil {
			sess.reqMu.Unlock()
			return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
		sess.connMu.Lock()
		connCloser = sess.connCloser
		sess.connMu.Unlock()
	} else {
		conn, connCloser, respHS, errDial = e.ensureUpstreamConn(
			context.WithValue(ctx, codexModelHeadersContextKey{}, modelHeaders), auth, sess, authID, wsURL, wsHeaders,
		)
	}
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr, normalizedDialErr := codexWebsocketDialError(respHS, errDial)
		if respHS != nil {
			recordAPIWebsocketUpgradeRejection(
				ctx,
				e.cfg,
				websocketUpgradeRequestLog(wsReqLog),
				respHS.StatusCode,
				respHS.Header.Clone(),
				bodyErr,
				respHS,
			)
		}
		recordAPIResponseError(ctx, e.cfg, normalizedDialErr)
		recordAPIWebsocketError(ctx, e.cfg, "dial", normalizedDialErr)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, normalizedDialErr
	}
	recordCodexWebsocketHandshake(ctx, e.cfg, respHS, connCloser)
	if errBind := sess.bindExecutionLifecycle(ctx, opts, conn, connCloser); errBind != nil {
		closeWebsocketAfterBindFailure(sess, conn, connCloser)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errBind
	}

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(conn, readCh)
	}
	restoreMultiAgentV2 := prepared.OptimizedMultiAgentV2
	if sess != nil {
		restoreMultiAgentV2 = !prepared.MultiAgentV2Conflict &&
			(prepared.OptimizedMultiAgentV2 || sess.isMultiAgentV2Optimized(conn))
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		errSend = mapCodexWebsocketWriteError(sess, conn, errSend)
		recordAPIResponseError(ctx, e.cfg, errSend)
		recordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)
			sess.clearActive(conn, readCh)
			sess.reqMu.Unlock()
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := connCloser.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}
		return nil, errSend
	}
	if sess != nil && (prepared.OptimizedMultiAgentV2 || prepared.MultiAgentV2Conflict) {
		sess.setMultiAgentV2Optimized(
			conn,
			prepared.OptimizedMultiAgentV2 && !prepared.MultiAgentV2Conflict,
		)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if terminateErr != nil && opts.ExecutionLifecycle != nil {
				opts.ExecutionLifecycle.End(terminateReason)
			}
			if sess != nil {
				sess.clearActive(conn, readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := connCloser.Close(); errClose != nil {
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
		inputTokensHandled := from != sdktranslator.FormatClaude ||
			cliproxyexecutor.ResponseFormatOrSource(opts) != sdktranslator.FormatClaude
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		responseStarted := false
		for {
			if ctx != nil && ctx.Err() != nil {
				errContext := failContext()
				_ = send(cliproxyexecutor.StreamChunk{Err: errContext})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh, connCloser)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					errContext := failContext()
					_ = send(cliproxyexecutor.StreamChunk{Err: errContext})
					return
				}
				terminateReason = "read_error"
				errRead = mapCodexWebsocketReadError(errRead)
				if responseStarted {
					errRead = markResponseStarted(errRead)
				}
				terminateErr = errRead
				recordAPIResponseError(ctx, e.cfg, errRead)
				recordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.publishFailure(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					errBinary := markResponseStarted(
						fmt.Errorf("codex websockets executor: unexpected binary message"),
					)
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
			helps.EmitWebSocketResponseEvent(ctx, opts, auth, e.Identifier(), req.Model, payload)
			responseStarted = true
			appendAPIWebsocketResponse(ctx, e.cfg, payload)
			payload = applyCodexIdentityConfuseResponsePayload(payload, identityState)
			payload = helps.RestoreCodexMultiAgentV2Response(payload, restoreMultiAgentV2)
			appendAPIResponseChunk(ctx, e.cfg, payload)

			if !gjson.ValidBytes(payload) {
				errMalformed := markResponseStarted(
					statusErr{
						code: http.StatusBadGateway,
						msg:  "codex websockets executor: upstream returned malformed event data",
					},
				)
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
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
					ctx,
					replayScope,
					statusErrCode(wsErr),
					[]byte(wsErr.Error()),
				); errClearReplay != nil {
					log.Warn("codex reasoning replay cache delete failed")
				}
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
			switch eventType {
			case "response.output_item.done":
				collectCodexOutputItemDone(payload, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.done", "response.incomplete":
				if !prepared.Native {
					payload = patchCodexCompletedOutput(payload, outputItemsByIndex, outputItemsFallback)
				}
			}
			isTerminalEvent := eventType == "response.completed" || eventType == "response.done" ||
				eventType == "response.incomplete" || eventType == "error"
			if cliproxyexecutor.DownstreamWebsocket(ctx) {
				chunk := cliproxyexecutor.StreamChunk{
					Payload: helps.EnsureResponsesUsageDetails(
						applyCodexIdentityExposeResponsePayload(
							payload, identityState,
						),
					),
				}
				var delivery <-chan error
				if isTerminalEvent && cliproxyexecutor.StreamDeliveryAcknowledgement(ctx) {
					deliveryResult := make(chan error, 1)
					var acknowledgeOnce sync.Once
					chunk.Acknowledge = func(errDelivery error) {
						acknowledgeOnce.Do(
							func() {
								deliveryResult <- errDelivery
							},
						)
					}
					delivery = deliveryResult
				}
				if !send(chunk) {
					_ = failContext()
					return
				}
				if isTerminalEvent {
					var errDelivery error
					if delivery != nil {
						select {
						case errDelivery = <-delivery:
						case <-ctx.Done():
							errDelivery = ctx.Err()
						}
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
					usageCtx := context.WithoutCancel(ctx)
					observeResponseTokenEvent(ctx, payload)
					reporter.observeResponseModel(payload)
					if hasUsage {
						reporter.publish(usageCtx, detail)
					} else {
						reporter.ensurePublished(usageCtx)
					}
					publishCodexImageToolUsage(usageCtx, reporter, clientBody, payload)
					return
				}
				continue
			}

			payload = normalizeCodexCompletionEvent(payload)
			eventType = gjson.GetBytes(payload, "type").String()
			var completedUsagePayload []byte
			if eventType == "response.completed" || eventType == "response.incomplete" {
				completedUsagePayload = bytes.Clone(payload)
			}

			line := encodeCodexWebsocketAsSSE(payload)
			if transformed, ok := grokbuild.TransformKeepaliveSSEFrame(line, isGrokClient); ok {
				line = transformed
			}
			chunks := sdktranslator.TranslateStream(
				ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, clientBody,
				applyCodexIdentityExposeResponsePayload(line, identityState), &param,
			)
			chunks = estimateClaudeStreamInputTokens(ctx, chunks, originalPayload, &inputTokensHandled)
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
				errTranslate = markResponseStarted(errTranslate)
				terminateReason = "translation_error"
				terminateErr = errTranslate
				recordAPIResponseError(ctx, e.cfg, errTranslate)
				recordAPIWebsocketError(ctx, e.cfg, "translation_error", errTranslate)
				reporter.publishFailure(ctx, errTranslate)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "translation_error", errTranslate)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: errTranslate})
				return
			}
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					_ = failContext()
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" ||
				eventType == "response.incomplete" {
				if eventType == "response.completed" {
					cacheCodexReasoningReplayFromCompleted(ctx, replayScope, completedUsagePayload)
				}
				observeResponseTokenEvent(ctx, completedUsagePayload)
				reporter.observeResponseModel(completedUsagePayload)
				if detail, ok := parseCodexUsage(completedUsagePayload); ok {
					reporter.publish(ctx, detail)
				} else {
					reporter.ensurePublished(ctx)
				}
				publishCodexImageToolUsage(ctx, reporter, clientBody, completedUsagePayload)
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
	dialer, errDialer := newProxyAwareWebsocketDialer(ctx, e.cfg, auth)
	if errDialer != nil {
		return nil, nil, errDialer
	}
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

func mapCodexWebsocketWriteError(sess *codexWebsocketSession, conn *websocket.Conn, err error) error {
	if err == nil || sess == nil || conn == nil {
		return err
	}
	upstreamErr := sess.upstreamDisconnectError(conn)
	var closeErr *websocket.CloseError
	if !errors.As(upstreamErr, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		return err
	}
	return mapCodexWebsocketReadError(upstreamErr)
}

type codexWebsocketMessageTooBigError struct {
	statusErr
}

func (codexWebsocketMessageTooBigError) IsRequestScoped() bool {
	return true
}

func mapCodexWebsocketReadError(err error) error {
	if err == nil {
		return nil
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return codexWebsocketMessageTooBigError{
			code: http.StatusRequestEntityTooLarge,
			msg: `{"error":{"message":"upstream websocket message too big","type":` +
				`"invalid_request_error","code":` +
				`"message_too_big"}}`,
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
	connCloser *websocketConnectionCloser,
) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return 0, nil, errCtx
		}
		stopCancel := context.AfterFunc(ctx, func() { _ = connCloser.Close() })
		defer stopCancel()
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errCtx := ctx.Err(); errCtx != nil {
			return 0, nil, errCtx
		}
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

func newProxyAwareWebsocketDialer(
	ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth,
) (*websocket.Dialer, error) {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := effectiveProxyURL(ctx, cfg, auth)
	setting, err := proxyutil.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("codex websockets executor: invalid proxy URL: %w", err)
	}
	switch setting.Mode {
	case proxyutil.ModeInherit:
		return dialer, nil
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
	case proxyutil.ModeProxy:
		if !proxyutil.ValidRequestProxy(proxyURL) {
			return nil, fmt.Errorf("codex websockets executor: invalid proxy URL")
		}
		if setting.URL.Scheme == "http" {
			dialer.Proxy = http.ProxyURL(setting.URL)
			break
		}
		proxyDialer, errDialer := proxyutil.NewDialerForURL(setting.URL)
		if errDialer != nil {
			return nil, fmt.Errorf("codex websockets executor: create proxy dialer: %w", errDialer)
		}
		contextDialer, ok := proxyDialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf(
				"codex websockets executor: proxy dialer does not support context cancellation",
			)
		}
		dialer.Proxy = nil
		dialer.NetDialContext = contextDialer.DialContext
	default:
		return nil, fmt.Errorf("codex websockets executor: unsupported proxy mode")
	}

	return dialer, nil
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("codex websockets executor: upstream URL has no host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("codex websockets executor: unsupported upstream URL scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

func applyCodexPromptCacheHeaders(
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	rawJSON []byte,
) ([]byte, http.Header) {
	return applyCodexPromptCacheHeadersWithContext(context.Background(), from, req, rawJSON, nil)
}

func applyCodexPromptCacheHeadersWithContext(
	ctx context.Context,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	rawJSON []byte,
	requestHeaders http.Header,
) ([]byte, http.Header) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}
	if req.Metadata[codexConfiguredPromptCacheMetadataKey] == true {
		if cacheID := cliproxysession.NormalizeExplicitID(
			gjson.GetBytes(rawJSON, "prompt_cache_key").String(),
		); cacheID != "" {
			headers.Set("Conversation_id", cacheID)
			setCodexSessionHeader(headers, cacheID)
		}
		return rawJSON, headers
	}
	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" {
		modelName = req.Model
	}
	var cache codexCache
	modelScoped := false
	if shouldUseImplicitCodexConversationCache(from, modelName) {
		cache, modelScoped = codexPromptCacheFromClaudeRequest(ctx, modelName, req.Payload, requestHeaders)
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) ||
		sourceFormatEqual(from, sdktranslator.FormatOpenAI) || sourceFormatEqual(from, sdktranslator.FormatCodex) {
		cache.ID = cliproxysession.NormalizeExplicitID(gjson.GetBytes(req.Payload, "prompt_cache_key").String())
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}
	if cache.ID == "" && sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		cache, _ = codexPromptCacheFromOpenAIAPIKey(ctx)
	}
	if cache.ID == "" {
		cache.ID = cliproxysession.NormalizeExplicitID(gjson.GetBytes(rawJSON, "prompt_cache_key").String())
	}
	if cache.ID != "" {
		if !modelScoped && codexPromptCacheModelName(modelName) != "" {
			cache.ID = uuid.NewSHA1(
				uuid.NameSpaceOID, []byte(strings.Join(
					[]string{
						"cli-proxy-api", "codex", "prompt-cache", codexPromptCacheModelName(modelName), cache.ID,
					}, "\x00",
				)),
			).String()
		}
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
		headers.Set("Conversation_id", cache.ID)
		setCodexSessionHeader(headers, cache.ID)
	}
	return rawJSON, headers
}

func applyCodexWebsocketHeaders(
	ctx context.Context,
	headers http.Header,
	auth *cliproxyauth.Auth,
	token string,
	cfg *config.Config,
	nativeRequest bool,
	clientHeaders ...http.Header,
) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeaders(headers, "Authorization", bearer)

	var identityHeaders http.Header
	var customHeaderSource http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		customHeaderSource = clientHeaders[0]
		identityHeaders = trustedCodexClientHeaders(customHeaderSource, codexCloakingDisabled(cfg, auth))
	} else if ginCtx := ginContextFrom(ctx); ginCtx != nil && ginCtx.Request != nil {
		customHeaderSource = ginCtx.Request.Header
		identityHeaders = trustedCodexClientHeaders(customHeaderSource, codexCloakingDisabled(cfg, auth))
	}

	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, identityHeaders, "X-Codex-Beta-Features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, identityHeaders, "X-Codex-Turn-State", "")
	misc.EnsureHeader(headers, identityHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(headers, identityHeaders, "X-Client-Request-Id", "")
	misc.EnsureHeader(headers, identityHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(headers, identityHeaders, "Thread-Id", "")
	misc.EnsureHeader(headers, identityHeaders, "X-ResponsesAPI-Include-Timing-Metrics", "")
	if nativeRequest {
		misc.EnsureHeader(headers, identityHeaders, codexResponsesLiteHeader, "")
		misc.EnsureHeader(headers, identityHeaders, "Conversation_id", "")
		misc.EnsureHeader(headers, identityHeaders, "X-Codex-Routing-Hint", "")
	}

	misc.EnsureHeader(headers, identityHeaders, "Version", "")
	betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	if betaHeader == "" && identityHeaders != nil {
		betaHeader = strings.TrimSpace(identityHeaders.Get("OpenAI-Beta"))
	}
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", betaHeader)
	ensureHeaderWithConfigPrecedence(headers, identityHeaders, "User-Agent", cfgUserAgent, codexUserAgent)
	if nativeRequest && codexCloakingDisabled(cfg, auth) {
		setCodexSessionHeader(headers, "")
		ensureCodexSessionHeader(headers, identityHeaders, "")
		for key := range headers {
			if strings.EqualFold(key, "Conversation_id") {
				delete(headers, key)
			}
		}
		if conversationID := strings.TrimSpace(
			headerValueCaseInsensitive(
				identityHeaders, "Conversation_id",
			),
		); conversationID != "" {
			setHeaderCasePreserved(headers, "Conversation_id", conversationID)
		}
	} else {
		sessionFallback := ""
		if strings.Contains(headers.Get("User-Agent"), "Mac OS") {
			sessionFallback = uuid.NewString()
		}
		ensureCodexSessionHeader(headers, identityHeaders, sessionFallback)
	}

	isAPIKey := auth.AuthKind() == cliproxyauth.AuthKindAPIKey
	if originator := strings.TrimSpace(identityHeaders.Get("Originator")); originator != "" {
		headers.Set("Originator", originator)
	} else if !isAPIKey {
		headers.Set("Originator", codexOriginator)
	}
	if !isAPIKey && auth != nil && auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok {
			if trimmed := strings.TrimSpace(accountID); trimmed != "" {
				setHeaderCasePreserved(headers, "ChatGPT-Account-ID", trimmed)
			}
		}
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	customHeaderRequest := &http.Request{Header: headers}
	if ctx != nil {
		customHeaderRequest = customHeaderRequest.WithContext(ctx)
	}
	util.ApplyCustomHeadersFromAttrs(customHeaderRequest, attrs, customHeaderSource)
	headers.Del("Content-Length")
	applyCodexCloakingHeaders(headers, cfg, auth)

	return headers
}

type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func parseCodexWebsocketError(payload []byte) (error, bool) {
	return parseCodexEventError(payload)
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
	payload = helps.EnsureResponsesUsageDetails(payload)
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

func recordCodexWebsocketHandshake(
	ctx context.Context,
	cfg *config.Config,
	resp *http.Response,
	closers ...*websocketConnectionCloser,
) {
	if resp == nil {
		endpoint := ""
		if len(closers) > 0 && closers[0] != nil {
			endpoint = closers[0].responseURL
		}
		usageReporterFromContext(ctx).observeResponseURL(endpoint, "ws_connection_handshake_url", "connection_reused")
		return
	}
	recordAPIWebsocketHandshake(ctx, cfg, resp.StatusCode, resp.Header.Clone(), resp)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
}

func codexWebsocketDialError(resp *http.Response, dialErr error) ([]byte, error) {
	body := websocketHandshakeBody(resp)
	if resp != nil && resp.StatusCode > 0 {
		return body, newCodexStatusErr(resp.StatusCode, body, resp.Header)
	}
	return body, dialErr
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
	sess := &codexWebsocketSession{sessionID: sessionID, upstreamDisconnectCh: make(chan error, 1)}
	e.sessions[sessionID] = sess
	return sess
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *codexWebsocketsExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil {
		return nil
	}
	sess := e.getOrCreateSession(sessionID)
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectCh
}

func (s *codexWebsocketSession) notifyUpstreamDisconnect(err error) {
	if s == nil || s.upstreamDisconnectCh == nil {
		return
	}
	if disposition, ok := errors.AsType[cliproxyexecutor.ExecutionDispositionError](err); ok {
		directive := disposition.ExecutionDisposition().RetryDirective
		if directive != "" && directive != cliproxyexecutor.RetryDirectiveNoRetry {
			return
		}
	}
	s.upstreamDisconnectOnce.Do(
		func() {
			s.upstreamDisconnectCh <- err
			close(s.upstreamDisconnectCh)
		},
	)
}

func existingCodexWebsocketSessionConn(
	sess *codexWebsocketSession,
	authID string,
	wsURL string,
	credentialSHA256 string,
	proxyURL string,
) *websocket.Conn {
	if sess == nil {
		return nil
	}
	sess.connMu.Lock()
	conn := sess.conn
	matches := conn != nil && sess.authID == authID && sess.wsURL == wsURL &&
		sess.credentialSHA256 == credentialSHA256 && sess.proxyURL == proxyURL
	sess.connMu.Unlock()
	if !matches || sess.upstreamDisconnectError(conn) != nil {
		return nil
	}
	return conn
}

func (e *codexWebsocketsExecutor) ensureUpstreamConn(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	authID string,
	wsURL string,
	headers http.Header,
) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	if sess == nil {
		conn, resp, err := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
		return conn, newWebsocketConnectionCloser(conn, resp), resp, err
	}

	var modelOverrides map[string]string
	hasModelHeaders := false
	if ctx != nil {
		modelOverrides, hasModelHeaders = ctx.Value(codexModelHeadersContextKey{}).(map[string]string)
	}
	var modelHeaders map[string]string
	if hasModelHeaders && len(modelOverrides) > 0 {
		modelHeaders = make(map[string]string, len(modelOverrides))
		for name := range modelOverrides {
			modelHeaders[http.CanonicalHeaderKey(name)] = headers.Get(name)
		}
	}
	credentialSHA256 := cliproxyauth.AccessTokenSHA256(auth)
	proxyURL := effectiveProxyURL(ctx, e.cfg, auth)
	sess.connMu.Lock()
	conn := sess.conn
	closer := sess.connCloser
	readerConn := sess.readerConn
	staleAuthID := sess.authID
	staleWSURL := sess.wsURL
	staleCredentialSHA256 := sess.credentialSHA256
	staleProxyURL := sess.proxyURL
	var staleConn *websocket.Conn
	var staleLifecycle cliproxyexecutor.ExecutionLifecycle
	if conn != nil && (staleAuthID != authID || staleWSURL != wsURL ||
		staleCredentialSHA256 != credentialSHA256 || staleProxyURL != proxyURL ||
		(hasModelHeaders && !maps.Equal(sess.modelHeaders, modelHeaders))) {
		staleConn = conn
		sess.conn = nil
		sess.connCloser = nil
		if sess.homeLifecycle == nil {
			staleLifecycle = sess.lifecycle
			sess.lifecycle = nil
		}
		sess.modelHeaders = nil
		sess.multiAgentV2OptimizedConn = nil
		if sess.readerConn == conn {
			sess.readerConn = nil
		}
		sess.authID = ""
		sess.wsURL = ""
		sess.credentialSHA256 = ""
		sess.proxyURL = ""
		conn = nil
		readerConn = nil
	}
	sess.connMu.Unlock()
	if staleConn != nil {
		logCodexWebsocketDisconnected(
			sess.sessionID,
			staleAuthID,
			staleWSURL,
			"connection_identity_changed",
			nil,
		)
		if errClose := closer.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close stale websocket error: %v", errClose)
		}
		if staleLifecycle != nil {
			staleLifecycle.End("connection_identity_changed")
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
		return conn, closer, nil, nil
	}

	conn, resp, errDial := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, nil, resp, errDial
	}
	closer = newWebsocketConnectionCloser(conn, resp)

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		previousCloser := sess.connCloser
		sess.connMu.Unlock()
		if errClose := closer.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		return previous, previousCloser, nil, nil
	}
	sess.conn = conn
	sess.connCloser = closer
	sess.multiAgentV2OptimizedConn = nil
	sess.wsURL = wsURL
	sess.authID = authID
	sess.credentialSHA256 = credentialSHA256
	sess.proxyURL = proxyURL
	sess.modelHeaders = modelHeaders
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConn(conn)
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, closer, resp, nil
}

// sendTerminalWebsocketRead reports whether it invalidated a full channel's connection before waiting.
func sendTerminalWebsocketRead(
	ch chan<- codexWebsocketRead,
	done <-chan struct{},
	event codexWebsocketRead,
	invalidate func(),
) bool {
	select {
	case ch <- event:
		return false
	case <-done:
		return false
	default:
	}

	invalidated := invalidate != nil
	if invalidated {
		invalidate()
	}
	select {
	case ch <- event:
	case <-done:
	}
	return invalidated
}

func deliverCodexWebsocketTerminalRead(
	ch chan codexWebsocketRead,
	done <-chan struct{},
	event codexWebsocketRead,
) {
	sendTerminalWebsocketRead(ch, done, event, nil)
}

func (e *codexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			sess.setUpstreamDisconnectError(conn, errRead)
			idleOwner := sess.idleHomeOwner(conn)
			invalidate := func() {
				e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
				if idleOwner != nil {
					idleOwner.End("upstream_disconnected")
				}
			}
			invalidated := false
			if ch, done, ok := sess.activeForConn(conn); ok {
				invalidated = sendTerminalWebsocketRead(
					ch, done, codexWebsocketRead{conn: conn, err: errRead}, invalidate,
				)
				sess.clearActive(conn, ch)
			}
			if !invalidated {
				invalidate()
			}
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				sess.setUpstreamDisconnectError(conn, errBinary)
				idleOwner := sess.idleHomeOwner(conn)
				invalidate := func() {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
					if idleOwner != nil {
						idleOwner.End("unexpected_binary")
					}
				}
				invalidated := false
				if ch, done, ok := sess.activeForConn(conn); ok {
					invalidated = sendTerminalWebsocketRead(
						ch, done, codexWebsocketRead{conn: conn, err: errBinary}, invalidate,
					)
					sess.clearActive(conn, ch)
				}
				if !invalidated {
					invalidate()
				}
				return
			}
			continue
		}

		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" || eventType == "response.done" ||
			eventType == "response.incomplete" || eventType == "response.failed" {
			responseID := gjson.GetBytes(payload, "response.id")
			if responseID.Type == gjson.String && responseID.String() != "" {
				sess.activeMu.Lock()
				if sess.responseStateConn == conn {
					sess.terminalResponseIDs[responseID.String()] = struct{}{}
				}
				sess.activeMu.Unlock()
			}
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
	closer := sess.connCloser
	var lifecycle cliproxyexecutor.ExecutionLifecycle
	if sess.homeLifecycle == nil {
		lifecycle = sess.lifecycle
		sess.lifecycle = nil
	}
	sess.conn = nil
	sess.connCloser = nil
	sess.modelHeaders = nil
	sess.multiAgentV2OptimizedConn = nil
	sess.proxyURL = ""
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	sess.notifyUpstreamDisconnect(err)
	if errClose := closer.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
	if lifecycle != nil {
		lifecycle.End(reason)
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

func (e *codexWebsocketsExecutor) CloseExecutionSessionsForAuth(authID string) {
	authID = strings.TrimSpace(authID)
	if e == nil || authID == "" {
		return
	}

	e.sessMu.Lock()
	sessions := make([]*codexWebsocketSession, 0)
	for sessionID, sess := range e.sessions {
		if sess == nil {
			continue
		}
		sess.connMu.Lock()
		matches := strings.TrimSpace(sess.authID) == authID
		sess.connMu.Unlock()
		if matches {
			delete(e.sessions, sessionID)
			sessions = append(sessions, sess)
		}
	}
	e.sessMu.Unlock()

	for _, sess := range sessions {
		e.closeExecutionSession(sess, "auth_replaced")
	}
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
	closer := sess.connCloser
	lifecycle := sess.lifecycle
	sess.lifecycle = nil
	sess.homeLifecycle = nil
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	sess.connCloser = nil
	sess.modelHeaders = nil
	sess.multiAgentV2OptimizedConn = nil
	sess.proxyURL = ""
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()
	if lifecycle != nil {
		lifecycle.End(reason)
	}
	sess.notifyUpstreamDisconnect(nil)

	if conn == nil {
		return
	}
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := closer.Close(); errClose != nil {
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
		"codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s",
		strings.TrimSpace(sessionID),
		strings.TrimSpace(authID),
		strings.TrimSpace(wsURL),
		strings.TrimSpace(reason),
	)
}

// CodexAutoExecutor routes Codex requests to the WebSocket transport only when:
//  1. The downstream transport is WebSocket, and
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

func (e *CodexAutoExecutor) SupportsApplyPatch() bool {
	return e != nil && e.httpExec != nil && e.wsExec != nil &&
		e.httpExec.SupportsApplyPatch() && e.wsExec.SupportsApplyPatch()
}

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
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) &&
		!codexPublicImageRequest(req, opts) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
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
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) &&
		!codexPublicImageRequest(req, opts) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
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

// InterruptExecutionSession sends a control frame on the bound Codex socket.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *CodexAutoExecutor) InterruptExecutionSession(
	ctx context.Context,
	sessionID string,
	payload []byte,
) error {
	if e == nil || e.wsExec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	return e.wsExec.InterruptExecutionSession(ctx, sessionID, payload)
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *codexWebsocketsExecutor) InterruptExecutionSession(
	ctx context.Context,
	sessionID string,
	payload []byte,
) error {
	if e == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	e.sessMu.Lock()
	sess := e.sessions[sessionID]
	e.sessMu.Unlock()
	if sess == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	sess.connMu.Unlock()
	if conn == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	// Recheck the response state after the auth callback, which may outlive the current turn.
	authEnabled := cliproxyexecutor.WebsocketAuthEnabled(ctx, authID)
	sess.connMu.Lock()
	if sess.conn != conn {
		sess.connMu.Unlock()
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sess.activeMu.Lock()
	responseID := gjson.GetBytes(payload, "response_id").String()
	_, alreadyTerminal := sess.terminalResponseIDs[responseID]
	alreadyTerminal = alreadyTerminal && sess.responseStateConn == conn
	active := sess.activeConn == conn && sess.activeCh != nil
	disconnectErr := sess.upstreamDisconnectError(conn)
	sess.activeMu.Unlock()
	sess.connMu.Unlock()
	if disconnectErr != nil {
		return disconnectErr
	}
	if alreadyTerminal {
		return nil
	}
	if !active {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if !authEnabled {
		return fmt.Errorf("websocket credential is no longer enabled")
	}
	if err := writeCodexWebsocketMessage(sess, conn, payload); err != nil {
		return err
	}
	log.WithField("outcome", "forwarded").Debug("codex websockets: response.interrupt handled")
	return nil
}

// UpstreamDisconnectChan 返回当前会话的首次上游断连通知
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) CloseExecutionSessionsForAuth(authID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSessionsForAuth(authID)
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
