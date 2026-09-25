package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readTimeout          = 60 * time.Second
	writeTimeout         = 10 * time.Second
	maxInboundMessageLen = 64 << 20 // 64 MiB
	heartbeatInterval    = 30 * time.Second
	pendingMessageBuffer = 8
)

var (
	errClosed            = errors.New("websocket session closed")
	errPendingBufferFull = errors.New("websocket pending response buffer full")
)

type pendingRequest struct {
	ch            chan message
	terminal      chan message
	failed        chan error
	ctx           context.Context
	sessionClosed <-chan struct{}

	cancelMu      sync.Mutex
	stopCancel    func() bool
	cancelStopped bool
}

func newPendingRequest(ctx context.Context, sessionClosed <-chan struct{}) *pendingRequest {
	return &pendingRequest{
		ch:            make(chan message, pendingMessageBuffer),
		terminal:      make(chan message, 1),
		failed:        make(chan error, 1),
		ctx:           ctx,
		sessionClosed: sessionClosed,
	}
}

func (pr *pendingRequest) setStopCancel(stop func() bool) {
	if pr == nil || stop == nil {
		return
	}
	pr.cancelMu.Lock()
	if pr.cancelStopped {
		pr.cancelMu.Unlock()
		stop()
		return
	}
	pr.stopCancel = stop
	pr.cancelMu.Unlock()
}

func (pr *pendingRequest) stopCancellation() {
	if pr == nil {
		return
	}
	pr.cancelMu.Lock()
	if pr.cancelStopped {
		pr.cancelMu.Unlock()
		return
	}
	pr.cancelStopped = true
	stop := pr.stopCancel
	pr.stopCancel = nil
	pr.cancelMu.Unlock()
	if stop != nil {
		stop()
	}
}

func (pr *pendingRequest) deliver(msg message) error {
	if pr == nil {
		return errClosed
	}
	select {
	case <-pr.ctx.Done():
		return pr.ctx.Err()
	case <-pr.sessionClosed:
		return errClosed
	default:
	}
	if isTerminalMessage(msg.Type) {
		select {
		case <-pr.ctx.Done():
			return pr.ctx.Err()
		case <-pr.sessionClosed:
			return errClosed
		case pr.terminal <- msg:
			return nil
		default:
			return errPendingBufferFull
		}
	}
	select {
	case <-pr.ctx.Done():
		return pr.ctx.Err()
	case <-pr.sessionClosed:
		return errClosed
	case pr.ch <- msg:
		return nil
	default:
		return errPendingBufferFull
	}
}

func (pr *pendingRequest) fail(err error) {
	if pr == nil || err == nil {
		return
	}
	select {
	case pr.failed <- err:
	default:
	}
}

func (pr *pendingRequest) receive() (message, error) {
	if pr == nil {
		return message{}, errClosed
	}
	if err := pr.ctx.Err(); err != nil {
		return message{}, err
	}
	select {
	case msg := <-pr.ch:
		return msg, nil
	default:
	}
	select {
	case msg := <-pr.terminal:
		return msg, nil
	default:
	}
	select {
	case msg := <-pr.ch:
		return msg, nil
	case msg := <-pr.terminal:
		return msg, nil
	case err := <-pr.failed:
		return message{}, err
	case <-pr.ctx.Done():
		return message{}, pr.ctx.Err()
	case <-pr.sessionClosed:
		select {
		case msg := <-pr.ch:
			return msg, nil
		case msg := <-pr.terminal:
			return msg, nil
		default:
			return message{}, errClosed
		}
	}
}

type session struct {
	conn       *websocket.Conn
	manager    *Manager
	provider   string
	id         string
	closed     chan struct{}
	closeOnce  sync.Once
	writeMutex sync.Mutex
	pending    sync.Map // map[string]*pendingRequest
}

func newSession(conn *websocket.Conn, mgr *Manager, id string) *session {
	s := &session{
		conn:     conn,
		manager:  mgr,
		provider: "",
		id:       id,
		closed:   make(chan struct{}),
	}
	conn.SetReadLimit(maxInboundMessageLen)
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(
		func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
			return nil
		},
	)
	s.startHeartbeat()
	return s
}

func (s *session) startHeartbeat() {
	if s == nil || s.conn == nil {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				s.writeMutex.Lock()
				err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeTimeout))
				s.writeMutex.Unlock()
				if err != nil {
					s.cleanup(err)
					return
				}
			}
		}
	}()
}

func (s *session) run(_ context.Context) {
	defer s.cleanup(errClosed)
	for {
		var msg message
		if err := s.conn.ReadJSON(&msg); err != nil {
			s.cleanup(err)
			return
		}
		s.dispatch(msg)
	}
}

func (s *session) dispatch(msg message) {
	if msg.Type == messageTypePing {
		_ = s.send(context.Background(), message{ID: msg.ID, Type: messageTypePong})
		return
	}
	value, ok := s.pending.Load(msg.ID)
	if !ok {
		if isTerminalMessage(msg.Type) && s.manager != nil {
			s.manager.logDebugf(
				"wsrelay: received terminal message for unknown id %s (provider=%s)", msg.ID, s.provider,
			)
		}
		return
	}
	req := value.(*pendingRequest)
	if isTerminalMessage(msg.Type) {
		actual, loaded := s.pending.LoadAndDelete(msg.ID)
		if !loaded {
			return
		}
		req = actual.(*pendingRequest)
		req.stopCancellation()
	}
	if errDeliver := req.deliver(msg); errors.Is(errDeliver, errPendingBufferFull) {
		s.pending.CompareAndDelete(msg.ID, req)
		req.stopCancellation()
		req.fail(errDeliver)
	}
}

func isTerminalMessage(messageType string) bool {
	return messageType == MessageTypeHTTPResp || messageType == MessageTypeError || messageType == MessageTypeStreamEnd
}

func (s *session) send(_ context.Context, msg message) error {
	select {
	case <-s.closed:
		return errClosed
	default:
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if err := s.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

func (s *session) request(ctx context.Context, msg message) (*pendingRequest, error) {
	if ctx == nil {
		return nil, fmt.Errorf("wsrelay: request context is nil")
	}
	if msg.ID == "" {
		return nil, fmt.Errorf("wsrelay: message id is required")
	}
	req := newPendingRequest(ctx, s.closed)
	if _, loaded := s.pending.LoadOrStore(msg.ID, req); loaded {
		return nil, fmt.Errorf("wsrelay: duplicate message id %s", msg.ID)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		if s.pending.CompareAndDelete(msg.ID, req) {
			req.stopCancellation()
		}
	})
	req.setStopCancel(stopCancel)
	if err := s.send(ctx, msg); err != nil {
		s.pending.CompareAndDelete(msg.ID, req)
		req.stopCancellation()
		return nil, err
	}
	return req, nil
}

func (s *session) cleanup(cause error) {
	s.closeOnce.Do(
		func() {
			close(s.closed)
			s.pending.Range(
				func(key, value any) bool {
					if s.pending.CompareAndDelete(key, value) {
						value.(*pendingRequest).stopCancellation()
					}
					return true
				},
			)
			if s.conn != nil {
				_ = s.conn.Close()
			}
			if s.manager != nil {
				s.manager.handleSessionClosed(s, cause)
			}
		},
	)
}
