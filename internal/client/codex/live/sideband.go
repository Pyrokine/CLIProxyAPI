package live

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	xproxy "golang.org/x/net/proxy"
)

const (
	defaultSidebandAPIBaseURL = "wss://api.openai.com/v1"
	sessionLifetime           = time.Hour
	websocketControlTimeout   = time.Second
)

var (
	callIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	sidebandUpgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(*http.Request) bool {
			return true
		},
	}
)

type liveSession struct {
	callID        string
	authID        string
	model         string
	homeSelection *auth.HomeDispatchSelection
	media         mediaRelaySession
	resources     *liveSessionResources
	token         uint64
}

type liveSessionResources struct {
	mu      sync.Mutex
	closed  bool
	closers []func() error
}

type storedSession struct {
	session liveSession
	claimed bool
	timer   *time.Timer
}

type sessionStore struct {
	mu       sync.Mutex
	next     uint64
	lifetime time.Duration
	sessions map[string]*storedSession
}

type sessionClaim int

const (
	sessionClaimMissing sessionClaim = iota
	sessionClaimBusy
	sessionClaimAcquired
)

func newSessionStore() *sessionStore {
	return &sessionStore{
		lifetime: sessionLifetime,
		sessions: make(map[string]*storedSession),
	}
}

func (s *sessionStore) put(callID string, session liveSession) liveSession {
	if s == nil || !callIDPattern.MatchString(callID) {
		_ = endLiveSession(session, "invalid_call_id")
		return liveSession{}
	}

	if session.resources == nil {
		session.resources = &liveSessionResources{}
	}
	s.mu.Lock()
	s.next++
	session.callID = callID
	session.token = s.next
	previous := s.sessions[callID]
	entry := &storedSession{session: session}
	entry.timer = time.AfterFunc(
		s.expiryDuration(), func() {
			s.expire(callID, session.token)
		},
	)
	s.sessions[callID] = entry
	s.mu.Unlock()

	if previous != nil {
		if previous.timer != nil {
			previous.timer.Stop()
		}
		if previous.session.resources != nil && previous.session.resources != session.resources {
			_ = previous.session.resources.close()
		}
		if previous.session.media != nil && previous.session.media != session.media {
			if errClose := previous.session.media.CloseWithReason("session_replaced"); errClose != nil {
				log.WithError(errClose).Debug("codex live media: close replaced session")
			}
		}
		if previous.session.homeSelection != session.homeSelection {
			endHomeSelection(previous.session, "session_replaced")
		}
	}
	return session
}

func (s *sessionStore) claim(callID string) (liveSession, sessionClaim) {
	if s == nil || !callIDPattern.MatchString(callID) {
		return liveSession{}, sessionClaimMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.sessions[callID]
	if entry == nil {
		return liveSession{}, sessionClaimMissing
	}
	if entry.claimed {
		return liveSession{}, sessionClaimBusy
	}
	entry.claimed = true
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	return entry.session, sessionClaimAcquired
}

func (s *sessionStore) release(session liveSession) {
	if s == nil || session.callID == "" {
		return
	}
	s.mu.Lock()
	entry := s.sessions[session.callID]
	if entry == nil || entry.session.token != session.token || !entry.claimed {
		s.mu.Unlock()
		return
	}
	entry.claimed = false
	entry.timer = time.AfterFunc(
		s.expiryDuration(), func() {
			s.expire(session.callID, session.token)
		},
	)
	s.mu.Unlock()
}

func (s *sessionStore) complete(session liveSession, reason string) {
	if s == nil || session.callID == "" {
		_ = endLiveSession(session, reason)
		return
	}
	s.mu.Lock()
	entry := s.sessions[session.callID]
	if entry == nil || entry.session.token != session.token {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, session.callID)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	s.mu.Unlock()
	_ = endLiveSession(entry.session, reason)
}

func (s *sessionStore) closeAll(reason string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	entries := make([]*storedSession, 0, len(s.sessions))
	for callID, entry := range s.sessions {
		delete(s.sessions, callID)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entries = append(entries, entry)
	}
	s.mu.Unlock()
	closeErrors := make([]error, 0, len(entries))
	for _, entry := range entries {
		if errClose := endLiveSession(entry.session, reason); errClose != nil {
			closeErrors = append(closeErrors, errClose)
		}
	}
	return errors.Join(closeErrors...)
}

func (s *sessionStore) expiryDuration() time.Duration {
	if s.lifetime > 0 {
		return s.lifetime
	}
	return sessionLifetime
}

func (s *sessionStore) expire(callID string, token uint64) {
	s.mu.Lock()
	entry := s.sessions[callID]
	if entry == nil || entry.session.token != token || entry.claimed {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, callID)
	s.mu.Unlock()
	_ = endLiveSession(entry.session, "session_expired")
}

func (s *sessionStore) peek(callID string) (liveSession, bool) {
	if s == nil {
		return liveSession{}, false
	}
	s.mu.Lock()
	entry := s.sessions[callID]
	s.mu.Unlock()
	if entry == nil {
		return liveSession{}, false
	}
	return entry.session, true
}

func endLiveSession(session liveSession, reason string) error {
	var closeErrors []error
	if session.resources != nil {
		if errClose := session.resources.close(); errClose != nil {
			closeErrors = append(closeErrors, errClose)
		}
	}
	if session.media != nil {
		if errClose := session.media.CloseWithReason(reason); errClose != nil {
			log.WithError(errClose).Debug("codex live media: close stored session")
			closeErrors = append(closeErrors, errClose)
		}
	}
	endHomeSelection(session, reason)
	return errors.Join(closeErrors...)
}

func endHomeSelection(session liveSession, reason string) {
	if session.homeSelection != nil {
		session.homeSelection.End(reason)
	}
}

func (r *liveSessionResources) add(closers ...func() error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closers = append(r.closers, closers...)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	_ = closeSessionResources(closers)
}

func (r *liveSessionResources) close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	closers := r.closers
	r.closers = nil
	r.mu.Unlock()
	return closeSessionResources(closers)
}

func closeSessionResources(closers []func() error) error {
	closeErrors := make([]error, 0, len(closers))
	for _, closer := range closers {
		if closer == nil {
			continue
		}
		if errClose := closer(); errClose != nil && !isNormalWebsocketClose(errClose) {
			log.WithError(errClose).Debug("codex live: close session resource")
			closeErrors = append(closeErrors, errClose)
		}
	}
	return errors.Join(closeErrors...)
}

type sidebandStyle int

const (
	sidebandFrameless sidebandStyle = iota
	sidebandRealtimeCalls
	sidebandRealtimeQuery
)

// HandleSideband relays live session sideband WebSocket frames bidirectionally.
func (h *Handler) HandleSideband(c *gin.Context) {
	if h == nil || h.authManager == nil || h.sessions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex live sideband unavailable"})
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"error": "WebSocket upgrade required"})
		return
	}

	style, callID, ok := sidebandTarget(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid Codex live call ID"})
		return
	}
	session, claim := h.sessions.claim(callID)
	switch claim {
	case sessionClaimBusy:
		c.JSON(http.StatusConflict, gin.H{"error": "Codex live session already joining"})
		return
	case sessionClaimAcquired:
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex live session not found"})
		return
	}
	consumeSession := false
	defer func() {
		if consumeSession {
			h.sessions.complete(session, "session_closed")
			return
		}
		h.sessions.release(session)
	}()

	ctx := util.WithGinContext(c.Request.Context(), c)
	var selection *auth.HomeDispatchSelection
	var selected *auth.Auth
	var errSelect error
	if session.homeSelection != nil {
		if !session.homeSelection.Active() {
			consumeSession = true
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex live Home selection unavailable"})
			return
		}
		selection = session.homeSelection
		selected = selection.CloneAuth()
	} else {
		selectionOpts := coreexecutor.Options{
			Headers: c.Request.Header.Clone(),
			Metadata: map[string]any{
				coreexecutor.PinnedAuthMetadataKey:       session.authID,
				coreexecutor.ExecutionSessionMetadataKey: callID,
			},
		}
		selection, selected, errSelect = h.selectOAuth(ctx, session.model, selectionOpts)
	}
	if errSelect != nil {
		writeSelectionError(c, errSelect)
		return
	}
	if selected == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex auth unavailable"})
		return
	}

	if selection != nil {
		attemptCtx, releaseAttempt, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			consumeSession = true
			runtimeexecutor.RecordAPIWebsocketError(ctx, h.configSnapshot(), "attempt", errAttempt)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex live sideband session unavailable"})
			return
		}
		ctx = attemptCtx
		defer releaseAttempt()
	}
	logging.SetGinCPATraceID(c, selected.EnsureIndex())
	cfg := h.configSnapshot()

	upstreamURL := buildSidebandURL(h.sidebandAPIBaseURL, style, callID)
	upstreamHTTPURL := websocketHTTPURL(upstreamURL)
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, upstreamHTTPURL, nil)
	if errRequest != nil {
		runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "request", errRequest)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Codex live sideband request unavailable"})
		return
	}
	req.Header = protocolHeaders(c.Request.Header)
	setAccountHeader(req.Header, selected)
	if errPrepare := h.authManager.PrepareHttpRequest(ctx, selected, req); errPrepare != nil {
		runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "auth", errPrepare)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Codex live sideband auth unavailable"})
		return
	}

	runtimeexecutor.RecordAPIWebsocketRequest(
		ctx,
		cfg,
		upstreamURL,
		"WEBSOCKET",
		headersForLogging(req.Header),
		nil,
		"codex",
		selected,
	)

	dialer := newProxyAwareSidebandDialer(cfg, selected)
	dialer.Subprotocols = websocket.Subprotocols(c.Request)
	upstream, handshakeResponse, errDial := dialer.DialContext(ctx, upstreamURL, req.Header)
	if errDial != nil {
		handleSidebandDialError(c, ctx, cfg, handshakeResponse, errDial)
		return
	}
	if handshakeResponse != nil {
		runtimeexecutor.RecordAPIWebsocketHandshake(
			ctx,
			cfg,
			handshakeResponse.StatusCode,
			callResponseHeaders(handshakeResponse.Header),
		)
		if handshakeResponse.Body != nil {
			if errClose := handshakeResponse.Body.Close(); errClose != nil {
				log.Errorf("codex live sideband: close handshake response body error: %v", errClose)
			}
		}
	}

	closeUpstream := websocketCloseFunc("upstream", upstream)
	if selection != nil {
		if errBind := selection.Bind(closeUpstream); errBind != nil {
			consumeSession = true
			runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "bind", errBind)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex live sideband session unavailable"})
			return
		}
	} else {
		defer func() { _ = closeUpstream() }()
	}

	upgradeHeaders := make(http.Header)
	if subprotocol := upstream.Subprotocol(); subprotocol != "" {
		upgradeHeaders.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	downstream, errUpgrade := sidebandUpgrader.Upgrade(c.Writer, c.Request, upgradeHeaders)
	if errUpgrade != nil {
		_ = closeUpstream()
		return
	}
	closeDownstream := websocketCloseFunc("downstream", downstream)
	if selection != nil {
		if errBind := selection.Bind(closeDownstream); errBind != nil {
			consumeSession = true
			return
		}
	} else {
		defer func() { _ = closeDownstream() }()
	}
	if session.resources != nil {
		session.resources.add(closeUpstream, closeDownstream)
	}
	consumeSession = true

	if errRelay := relayWebsockets(downstream, upstream); errRelay != nil && !isNormalWebsocketClose(errRelay) {
		runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "relay", errRelay)
		log.WithError(errRelay).Debug("codex live sideband relay closed")
	}
}

func sidebandTarget(c *gin.Context) (sidebandStyle, string, bool) {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return sidebandFrameless, "", false
	}
	if callID := strings.TrimSpace(c.Param("call_id")); callID != "" {
		style := sidebandFrameless
		if strings.Contains(c.Request.URL.Path, "/realtime/calls/") {
			style = sidebandRealtimeCalls
		}
		return style, callID, callIDPattern.MatchString(callID)
	}
	callID := strings.TrimSpace(c.Query("call_id"))
	return sidebandRealtimeQuery, callID, callIDPattern.MatchString(callID)
}

func buildSidebandURL(baseURL string, style sidebandStyle, callID string) string {
	root := strings.TrimRight(baseURL, "/")
	switch style {
	case sidebandRealtimeCalls:
		return root + "/realtime/calls/" + callID
	case sidebandRealtimeQuery:
		return root + "/realtime?intent=quicksilver&call_id=" + url.QueryEscape(callID)
	default:
		return root + "/live/" + callID
	}
}

func websocketHTTPURL(rawURL string) string {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return rawURL
	}
	switch strings.ToLower(parsed.Scheme) {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	return parsed.String()
}

func callIDFromLocation(location string) string {
	location = strings.TrimSpace(location)
	if callIDPattern.MatchString(location) {
		return location
	}
	parsed, errParse := url.Parse(location)
	if errParse != nil {
		return ""
	}
	if callID := strings.TrimSpace(parsed.Query().Get("call_id")); callIDPattern.MatchString(callID) {
		return callID
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	callID := parts[len(parts)-1]
	previous := parts[len(parts)-2]
	if !callIDPattern.MatchString(callID) || (previous != "live" && previous != "calls") {
		return ""
	}
	return callID
}

func handleSidebandDialError(
	c *gin.Context,
	ctx context.Context,
	cfg *config.Config,
	response *http.Response,
	errDial error,
) {
	status := clienterror.HTTPStatusFromErrorOr(errDial, http.StatusBadGateway)
	if response != nil {
		if response.StatusCode > 0 {
			status = response.StatusCode
		}
		runtimeexecutor.RecordAPIWebsocketHandshake(
			ctx,
			cfg,
			response.StatusCode,
			callResponseHeaders(response.Header),
		)
		if response.Body != nil {
			if errClose := response.Body.Close(); errClose != nil {
				log.Errorf("codex live sideband: close rejected handshake body error: %v", errClose)
			}
		}
	}
	runtimeexecutor.RecordAPIWebsocketError(ctx, cfg, "dial", errDial)
	c.JSON(status, gin.H{"error": "Codex live sideband upstream unavailable"})
}

func websocketCloseFunc(name string, conn *websocket.Conn) func() error {
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(
			func() {
				closeErr = conn.Close()
				if closeErr != nil && !isNormalWebsocketClose(closeErr) {
					log.Debugf("codex live sideband: close %s websocket error: %v", name, closeErr)
				}
			},
		)
		return closeErr
	}
}

func relayWebsockets(downstream, upstream *websocket.Conn) error {
	configureControlFrameRelay(downstream, upstream)

	results := make(chan error, 2)
	go func() { results <- copyWebsocket(upstream, downstream) }()
	go func() { results <- copyWebsocket(downstream, upstream) }()

	firstErr := <-results
	closeCode, closeReason := websocketCloseDetails(firstErr)
	payload := websocket.FormatCloseMessage(closeCode, closeReason)
	_ = downstream.WriteControl(websocket.CloseMessage, payload, time.Now().Add(websocketControlTimeout))
	_ = upstream.WriteControl(websocket.CloseMessage, payload, time.Now().Add(websocketControlTimeout))
	_ = downstream.Close()
	_ = upstream.Close()
	<-results
	return firstErr
}

func configureControlFrameRelay(downstream, upstream *websocket.Conn) {
	downstream.SetPingHandler(controlFrameRelay(upstream, websocket.PingMessage))
	downstream.SetPongHandler(controlFrameRelay(upstream, websocket.PongMessage))
	upstream.SetPingHandler(controlFrameRelay(downstream, websocket.PingMessage))
	upstream.SetPongHandler(controlFrameRelay(downstream, websocket.PongMessage))
}

func controlFrameRelay(destination *websocket.Conn, messageType int) func(string) error {
	return func(payload string) error {
		return destination.WriteControl(messageType, []byte(payload), time.Now().Add(websocketControlTimeout))
	}
}

func copyWebsocket(destination, source *websocket.Conn) error {
	for {
		messageType, reader, errReader := source.NextReader()
		if errReader != nil {
			return errReader
		}
		writer, errWriter := destination.NextWriter(messageType)
		if errWriter != nil {
			return errWriter
		}
		_, errCopy := io.Copy(writer, reader)
		errClose := writer.Close()
		if errCopy != nil {
			return errCopy
		}
		if errClose != nil {
			return errClose
		}
	}
}

func websocketCloseDetails(err error) (int, string) {
	if closeErr, ok := errors.AsType[*websocket.CloseError](err); ok {
		switch closeErr.Code {
		case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseTLSHandshake:
			return websocket.CloseNormalClosure, ""
		default:
			return closeErr.Code, closeErr.Text
		}
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return websocket.CloseNormalClosure, ""
	}
	return websocket.CloseInternalServerErr, "relay closed"
}

func isNormalWebsocketClose(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return websocket.IsCloseError(
		err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	)
}

func newProxyAwareSidebandDialer(cfg *config.Config, selected *auth.Auth) *websocket.Dialer {
	return newSidebandDialer(proxyURLForAuth(cfg, selected))
}

func proxyURLForAuth(cfg *config.Config, selected *auth.Auth) string {
	if selected != nil && strings.TrimSpace(selected.ProxyURL) != "" {
		return strings.TrimSpace(selected.ProxyURL)
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func newSidebandDialer(proxyURL string) *websocket.Dialer {
	dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	if strings.TrimSpace(proxyURL) == "" {
		return dialer
	}

	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex live sideband: %v", errParse)
		return dialer
	}
	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer
	case proxyutil.ModeProxy:
	default:
		return dialer
	}

	switch setting.URL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *xproxy.Auth
		if setting.URL.User != nil {
			username := setting.URL.User.Username()
			password, _ := setting.URL.User.Password()
			proxyAuth = &xproxy.Auth{User: username, Password: password}
		}
		socksDialer, errSOCKS5 := xproxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, xproxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("codex live sideband: create SOCKS5 dialer failed: %v", errSOCKS5)
			return dialer
		}
		dialer.Proxy = nil
		if contextDialer, ok := socksDialer.(xproxy.ContextDialer); ok {
			dialer.NetDialContext = contextDialer.DialContext
		} else {
			dialer.NetDialContext = func(_ context.Context, network, address string) (net.Conn, error) {
				return socksDialer.Dial(network, address)
			}
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		log.Errorf("codex live sideband: unsupported proxy scheme: %s", setting.URL.Scheme)
	}
	return dialer
}
