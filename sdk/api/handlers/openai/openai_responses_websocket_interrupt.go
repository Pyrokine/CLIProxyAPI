package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responsesLocalInterrupt struct {
	mu         sync.Mutex
	active     bool
	responseID string
	pendingID  string
	frames     chan []byte
}

func newResponsesLocalInterrupt() *responsesLocalInterrupt {
	return &responsesLocalInterrupt{frames: make(chan []byte, 1)}
}

func (s *responsesLocalInterrupt) begin() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = true
	s.responseID = ""
	s.pendingID = ""
	s.mu.Unlock()
}

func (s *responsesLocalInterrupt) setResponseID(responseID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.active {
		s.responseID = strings.TrimSpace(responseID)
	}
	s.mu.Unlock()
}

func (s *responsesLocalInterrupt) end() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = false
	s.responseID = ""
	s.pendingID = ""
	select {
	case <-s.frames:
	default:
	}
	s.mu.Unlock()
}

func (s *responsesLocalInterrupt) deliver(payload []byte) error {
	if s == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	responseID := strings.TrimSpace(gjson.GetBytes(payload, "response_id").String())
	if s.responseID == "" || responseID != s.responseID {
		return fmt.Errorf("response.interrupt response_id does not match the active response")
	}
	if s.pendingID != "" {
		if s.pendingID == responseID {
			return nil
		}
		return fmt.Errorf("another response.interrupt is pending")
	}
	select {
	case s.frames <- bytes.Clone(payload):
		s.pendingID = responseID
		return nil
	default:
		return fmt.Errorf("response.interrupt queue is full")
	}
}

func (s *responsesLocalInterrupt) framesChan() <-chan []byte {
	if s == nil {
		return nil
	}
	return s.frames
}

// readResponsesWebsocketInput owns the downstream socket reader so interrupts bypass the turn queue.
func readResponsesWebsocketInput(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	conn *websocket.Conn,
	interrupt func([]byte) error,
	local *responsesLocalInterrupt,
	writer *responsesWebsocketWriter,
	timeline *websocketTimelineRecorder,
) (<-chan []byte, <-chan struct{}) {
	input := make(chan []byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(input)
		for {
			kind, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				cancel(errRead)
				return
			}
			if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
				continue
			}
			if json.Valid(payload) && gjson.GetBytes(payload, "type").String() == "response.interrupt" {
				appendResponsesInterruptDiagnostic(timeline, payload, "")
				errInterrupt := interrupt(payload)
				outcome := "handled"
				if errors.Is(errInterrupt, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
					errInterrupt = local.deliver(payload)
					outcome = "local_http"
				}
				if errInterrupt == nil {
					appendResponsesInterruptDiagnostic(timeline, payload, outcome)
					continue
				}
				appendResponsesInterruptDiagnostic(timeline, payload, "rejected")
				_, errWrite := writeResponsesWebsocketError(writer, &handlers.ErrorMessage{
					StatusCode: http.StatusBadRequest,
					Error:      errInterrupt,
				})
				if errWrite != nil {
					cancel(errWrite)
					return
				}
				continue
			}
			select {
			case input <- payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return input, done
}

func appendResponsesInterruptDiagnostic(timeline *websocketTimelineRecorder, payload []byte, outcome string) {
	if timeline == nil {
		return
	}
	idHash := sha256.Sum256([]byte(gjson.GetBytes(payload, "response_id").String()))
	diagnostic := struct {
		Type           string `json:"type"`
		ResponseIDHash string `json:"response_id_hash"`
		Outcome        string `json:"outcome,omitempty"`
	}{Type: "response.interrupt", ResponseIDHash: fmt.Sprintf("%x", idHash[:8]), Outcome: outcome}
	data, err := json.Marshal(diagnostic)
	if err != nil {
		return
	}
	event := "interrupt"
	if outcome != "" {
		event = "interrupt.outcome"
	}
	appendWebsocketEvent(timeline, event, data)
}

func (h *ResponsesAPIHandler) forwardResponsesWebsocketInterrupt(ctx context.Context, sessionID string, payload []byte) error {
	responseID := gjson.GetBytes(payload, "response_id")
	if responseID.Type != gjson.String || strings.TrimSpace(responseID.String()) == "" {
		return fmt.Errorf("response.interrupt requires response_id")
	}
	if h == nil || h.AuthManager == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	executor, ok := h.AuthManager.Executor("codex")
	if !ok || executor == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sender, ok := executor.(interface {
		InterruptExecutionSession(context.Context, string, []byte) error
	})
	if !ok {
		return fmt.Errorf("upstream executor does not support response.interrupt")
	}
	ctx = cliproxyexecutor.WithWebsocketAuthCheck(ctx, func(authID string) bool {
		auth, found := h.AuthManager.GetByID(authID)
		if !found || auth == nil {
			auth, found = h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID)
		}
		return found && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
	})
	return sender.InterruptExecutionSession(ctx, sessionID, payload)
}

func completeResponsesWebsocketLocalInterrupt(
	c *gin.Context,
	writer *responsesWebsocketWriter,
	wsTimelineLog *websocketTimelineRecorder,
	cancel handlers.APIHandlerCancelFunc,
	payload []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
	pendingToolCallIDs map[string]struct{},
) ([]byte, string, []string, *handlers.ErrorMessage, error) {
	responseID := strings.TrimSpace(gjson.GetBytes(payload, "response_id").String())
	output := responseCompletedOutputFromPayload([]byte(`{"response":{"output":[]}}`), outputItemsByIndex, outputItemsFallback)
	incomplete := []byte(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"interrupted"}}}`)
	var err error
	incomplete, err = sjson.SetBytes(incomplete, "response.id", responseID)
	if err == nil {
		incomplete, err = sjson.SetRawBytes(incomplete, "response.output", output)
	}
	if err != nil {
		cancel(err)
		return output, responseID, sortedStringSet(pendingToolCallIDs), nil, err
	}
	markAPIResponseTimestamp(c)
	appendWebsocketEvent(wsTimelineLog, "response", incomplete)
	if errWrite := writer.writeText(incomplete); errWrite != nil {
		cancel(errWrite)
		return output, responseID, sortedStringSet(pendingToolCallIDs), nil, errWrite
	}
	cancel(context.Canceled)
	return output, responseID, sortedStringSet(pendingToolCallIDs), nil, nil
}
