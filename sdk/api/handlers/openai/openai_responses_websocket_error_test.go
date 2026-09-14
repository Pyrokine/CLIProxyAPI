package openai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type responsesWebsocketForwardTestResult struct {
	errMsg   *handlers.ErrorMessage
	forward  error
	ackCount int
	ackErr   error
}

func runResponsesWebsocketForwardError(
	t *testing.T,
	payload []byte,
	errMsg *handlers.ErrorMessage,
	blockWriter ...bool,
) ([][]byte, responsesWebsocketForwardTestResult, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	resultCh := make(chan responsesWebsocketForwardTestResult, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if errUpgrade != nil {
					resultCh <- responsesWebsocketForwardTestResult{forward: errUpgrade}
					return
				}
				defer func() { _ = conn.Close() }()

				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = request
				data := make(chan cliproxyexecutor.StreamChunk, 1)
				result := responsesWebsocketForwardTestResult{}
				terminal := len(blockWriter) > 1 && blockWriter[1]
				if len(payload) > 0 || terminal {
					data <- cliproxyexecutor.StreamChunk{
						Payload:  payload,
						Terminal: terminal,
						Acknowledge: func(err error) {
							result.ackCount++
							result.ackErr = err
						},
					}
				}
				close(data)
				errs := make(chan *handlers.ErrorMessage, 1)
				if errMsg != nil {
					errs <- errMsg
				}
				close(errs)

				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				writer := newResponsesWebsocketWriter(conn)
				if len(blockWriter) > 0 && blockWriter[0] {
					writer.writeMu.Lock()
					defer writer.writeMu.Unlock()
				}
				_, _, _, result.errMsg, result.forward = handler.forwardResponsesWebsocket(
					ginCtx,
					writer,
					func(...any) {},
					data,
					errs,
					nil,
					"test-session",
					"test-downstream",
				)
				resultCh <- result
			},
		),
	)
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	if errDeadline := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
		t.Fatalf("set read deadline: %v", errDeadline)
	}

	var payloads [][]byte
	var readErr error
	for {
		messageType, message, errRead := conn.ReadMessage()
		if errRead != nil {
			readErr = errRead
			break
		}
		if messageType == websocket.TextMessage {
			payloads = append(payloads, append([]byte(nil), message...))
		}
	}

	select {
	case result := <-resultCh:
		return payloads, result, readErr
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for websocket forward result")
		return nil, responsesWebsocketForwardTestResult{}, nil
	}
}

func TestMarkResponsesWebsocketTransportFailure(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		prior       internallogging.RequestOutcomeKind
		wantFailed  bool
		wantOutcome internallogging.RequestOutcomeKind
	}{
		{
			name:        "abnormal transport error",
			err:         io.ErrUnexpectedEOF,
			wantFailed:  true,
			wantOutcome: internallogging.RequestOutcomeWebsocketError,
		},
		{
			name: "normal close",
			err:  &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "done"},
		},
		{
			name:        "existing upstream failure",
			err:         io.ErrUnexpectedEOF,
			prior:       internallogging.RequestOutcomeUpstreamError,
			wantFailed:  true,
			wantOutcome: internallogging.RequestOutcomeUpstreamError,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				state := internallogging.NewRequestOutcomeState()
				if test.prior != "" {
					state.MarkFailure(test.prior, "upstream", "upstream failed")
				}
				ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
				request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				request = request.WithContext(internallogging.WithRequestOutcomeState(request.Context(), state))
				ginContext.Request = request

				markResponsesWebsocketTransportFailure(ginContext, test.err)

				outcome := state.Snapshot()
				if outcome.Failed != test.wantFailed || outcome.Kind != test.wantOutcome && test.wantFailed {
					t.Fatalf(
						"request outcome = %+v, want failed=%t kind=%q", outcome, test.wantFailed, test.wantOutcome,
					)
				}
			},
		)
	}
}

func TestForwardResponsesWebsocketExposesOnlyClientFaults(t *testing.T) {
	tests := []struct {
		name        string
		errMsg      *handlers.ErrorMessage
		wantExposed bool
		wantCode    string
	}{
		{
			name: "bad request",
			errMsg: &handlers.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      errors.New("bad request"),
			},
			wantExposed: true,
		},
		{
			name: "cyber policy behind bad gateway",
			errMsg: &handlers.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error: errors.New(
					`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
				),
			},
			wantExposed: true,
			wantCode:    "cyber_policy",
		},
		{
			name: "authentication",
			errMsg: &handlers.ErrorMessage{
				StatusCode: http.StatusUnauthorized,
				Error:      errors.New("invalid token"),
			},
		},
		{
			name: "quota",
			errMsg: &handlers.ErrorMessage{
				StatusCode: http.StatusTooManyRequests,
				Error:      errors.New("quota exhausted"),
			},
		},
		{
			name: "transport",
			errMsg: &handlers.ErrorMessage{
				StatusCode: http.StatusInternalServerError,
				Error:      errors.New("unexpected EOF"),
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				payloads, result, _ := runResponsesWebsocketForwardError(t, nil, test.errMsg)
				if got := len(payloads); got != boolToInt(test.wantExposed) {
					t.Fatalf("text payload count = %d, want %d", got, boolToInt(test.wantExposed))
				}
				if !errors.Is(result.forward, websocket.ErrCloseSent) {
					t.Fatalf("forward error = %v, want %v", result.forward, websocket.ErrCloseSent)
				}
				if result.errMsg == nil {
					t.Fatal("forward error message = nil")
				}
				if test.wantExposed {
					if got := gjson.GetBytes(payloads[0], "type").String(); got != wsEventTypeError {
						t.Fatalf("payload type = %q, want %q: %s", got, wsEventTypeError, payloads[0])
					}
					if test.wantCode != "" {
						if got := gjson.GetBytes(payloads[0], "error.code").String(); got != test.wantCode {
							t.Fatalf("error code = %q, want %q: %s", got, test.wantCode, payloads[0])
						}
					}
				}
			},
		)
	}
}

func TestForwardResponsesWebsocketClassifiesErrorPayloads(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		blockWriter   bool
		wantText      bool
		wantCloseCode int
		wantAckError  bool
	}{
		{
			name: "context length behind internal error",
			payload: `{"type":"error","status":500,` +
				`"error":{"type":"server_error","code":"context_length_exceeded","message":"too long"}}`,
			wantText: true,
		},
		{
			name: "client fault blocked by active writer",
			payload: `{"type":"error","status":500,` +
				`"error":{"type":"server_error","code":"context_length_exceeded","message":"too long"}}`,
			blockWriter:  true,
			wantAckError: true,
		},
		{
			name: "quota",
			payload: `{"type":"error","status":429,` +
				`"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"quota"}}`,
			wantAckError: true,
		},
		{
			name: "message too big",
			payload: `{"type":"error","status":413,` +
				`"error":{"type":"invalid_request_error","code":"message_too_big","message":"too large"}}`,
			wantCloseCode: websocket.CloseMessageTooBig,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				payloads, result, readErr := runResponsesWebsocketForwardError(
					t, []byte(test.payload), nil, test.blockWriter,
				)
				if got := len(payloads); got != boolToInt(test.wantText) {
					t.Fatalf("text payload count = %d, want %d", got, boolToInt(test.wantText))
				}
				if !errors.Is(result.forward, websocket.ErrCloseSent) {
					t.Fatalf("forward error = %v, want %v", result.forward, websocket.ErrCloseSent)
				}
				if result.errMsg == nil {
					t.Fatal("forward error message = nil")
				}
				if result.ackCount != 1 {
					t.Fatalf("ack count = %d, want 1", result.ackCount)
				}
				if (result.ackErr != nil) != test.wantAckError {
					t.Fatalf("ack error = %v, want error %t", result.ackErr, test.wantAckError)
				}
				if test.wantText && string(payloads[0]) != test.payload {
					t.Fatalf("terminal payload changed:\n got: %s\nwant: %s", payloads[0], test.payload)
				}
				if test.wantCloseCode > 0 {
					var closeErr *websocket.CloseError
					if !errors.As(readErr, &closeErr) || closeErr.Code != test.wantCloseCode {
						t.Fatalf("read error = %v, want close code %d", readErr, test.wantCloseCode)
					}
				}
			},
		)
	}
}

func TestForwardResponsesWebsocketValidatesTransportTerminal(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		wantAckError string
		wantType     string
	}{
		{
			name:         "missing completion",
			payload:      `{"type":"response.created","response":{"id":"resp_missing"}}`,
			wantAckError: "before response.completed",
		},
		{
			name:     "response completed",
			payload:  `{"type":"response.completed","response":{"id":"resp_completed","output":[]}}`,
			wantType: wsEventTypeDone,
		},
		{
			name:     "response done",
			payload:  `{"type":"response.done","response":{"id":"resp_done","output":[]}}`,
			wantType: wsEventTypeDone,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				payloads, result, _ := runResponsesWebsocketForwardError(
					t, []byte(test.payload), nil, false, true,
				)
				if result.ackCount != 1 {
					t.Fatalf("ack count = %d, want 1", result.ackCount)
				}
				if test.wantAckError != "" {
					if result.ackErr == nil || !strings.Contains(result.ackErr.Error(), test.wantAckError) {
						t.Fatalf("ack error = %v, want %q", result.ackErr, test.wantAckError)
					}
					if result.errMsg == nil || result.errMsg.Error == nil ||
						!strings.Contains(result.errMsg.Error.Error(), test.wantAckError) {
						t.Fatalf("forward error message = %#v, want %q", result.errMsg, test.wantAckError)
					}
					return
				}
				if result.ackErr != nil || result.errMsg != nil || result.forward != nil {
					t.Fatalf(
						"successful terminal result = ack:%v message:%#v forward:%v",
						result.ackErr,
						result.errMsg,
						result.forward,
					)
				}
				if len(payloads) != 1 || gjson.GetBytes(payloads[0], "type").String() != test.wantType {
					t.Fatalf("payloads = %q, want one %q event", payloads, test.wantType)
				}
			},
		)
	}
}

func TestResponsesWebsocketWriterTimesOutBlockedWrite(t *testing.T) {
	const writeTimeout = 10 * time.Millisecond
	startWrite := make(chan struct{})
	serverResult := make(chan error, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if errUpgrade != nil {
					serverResult <- errUpgrade
					return
				}
				defer func() { _ = conn.Close() }()
				tcpConn, ok := conn.NetConn().(*net.TCPConn)
				if !ok {
					serverResult <- errors.New("websocket connection is not TCP")
					return
				}
				if errBuffer := tcpConn.SetWriteBuffer(1024); errBuffer != nil {
					serverResult <- errBuffer
					return
				}
				writer := newResponsesWebsocketWriter(conn)
				writer.writeTimeout = writeTimeout
				<-startWrite
				serverResult <- writer.writeText(make([]byte, 1<<20))
			},
		),
	)
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		close(startWrite)
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	tcpConn, ok := conn.NetConn().(*net.TCPConn)
	if !ok {
		close(startWrite)
		t.Fatal("websocket client connection is not TCP")
	}
	if errBuffer := tcpConn.SetReadBuffer(1024); errBuffer != nil {
		close(startWrite)
		t.Fatalf("set websocket read buffer: %v", errBuffer)
	}
	close(startWrite)

	select {
	case errWrite := <-serverResult:
		var timeoutErr net.Error
		if !errors.As(errWrite, &timeoutErr) || !timeoutErr.Timeout() {
			t.Fatalf("write error = %v, want timeout", errWrite)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("websocket write did not respect its deadline")
	}
}

func TestResponsesWebsocketWriterCloseWithoutErrorDoesNotWaitForWriter(t *testing.T) {
	serverResult := make(chan error, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if errUpgrade != nil {
					serverResult <- errUpgrade
					return
				}
				writer := newResponsesWebsocketWriter(conn)
				writer.writeMu.Lock()
				closed := make(chan error, 1)
				go func() {
					_, errClose := writer.closeWithoutError()
					closed <- errClose
				}()
				select {
				case errClose := <-closed:
					writer.writeMu.Unlock()
					serverResult <- errClose
				case <-time.After(time.Second):
					writer.writeMu.Unlock()
					serverResult <- errors.New("closeWithoutError waited for the active writer")
				}
			},
		),
	)
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	if errClose := <-serverResult; errClose != nil && !isWebsocketConnectionClosedError(errClose) {
		t.Fatalf("closeWithoutError: %v", errClose)
	}
}

func TestResponsesWebsocketErrorStatusMapsContextErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		msg  *handlers.ErrorMessage
		want int
	}{
		{
			name: "canceled",
			msg:  &handlers.ErrorMessage{Error: context.Canceled},
			want: clienterror.StatusClientClosedRequest,
		},
		{
			name: "deadline",
			msg:  &handlers.ErrorMessage{Error: context.DeadlineExceeded},
			want: http.StatusGatewayTimeout,
		},
		{
			name: "explicit status wins",
			msg: &handlers.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      context.Canceled,
			},
			want: http.StatusBadGateway,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if got := responsesWebsocketErrorStatus(test.msg); got != test.want {
					t.Fatalf("responsesWebsocketErrorStatus() = %d, want %d", got, test.want)
				}
				payload, errBuild := buildResponsesWebsocketErrorPayload(test.msg)
				if errBuild != nil {
					t.Fatalf("buildResponsesWebsocketErrorPayload(): %v", errBuild)
				}
				if got := int(gjson.GetBytes(payload, "status").Int()); got != test.want {
					t.Fatalf("payload status = %d, want %d: %s", got, test.want, payload)
				}
			},
		)
	}
}

func TestResponsesWebsocketPinnedAuthUsesSharedErrorStatus(t *testing.T) {
	if shouldReleaseResponsesWebsocketPinnedAuth(&handlers.ErrorMessage{Error: context.Canceled}) {
		t.Fatal("context cancellation released pinned auth")
	}
	if !shouldReleaseResponsesWebsocketPinnedAuth(
		&handlers.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New("rate limited"),
		},
	) {
		t.Fatal("429 did not release pinned auth")
	}
}

func TestIsWebsocketConnectionClosedError(t *testing.T) {
	for _, err := range []error{
		net.ErrClosed,
		websocket.ErrCloseSent,
		errors.New("write tcp: use of closed network connection"),
	} {
		if !isWebsocketConnectionClosedError(err) {
			t.Fatalf("%v was not classified as a closed connection", err)
		}
	}
	if isWebsocketConnectionClosedError(errors.New("unexpected EOF")) {
		t.Fatal("unexpected EOF was classified as a closed connection")
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
