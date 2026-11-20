package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesTerminalFailureIsVisibleWithoutPrivateDetails(t *testing.T) {
	for _, client := range []string{"standard SSE", "Codex SSE", "websocket"} {
		t.Run(
			client, func(t *testing.T) {
				failure := &handlers.ErrorMessage{
					StatusCode: http.StatusInternalServerError,
					Error: errors.New(
						"private upstream failure; access_token=SYNTHETIC_RESPONSE_FAILURE_SECRET",
					),
				}
				var body string
				if client == "websocket" {
					payloads, result, _ := runResponsesWebsocketForwardError(t, nil, failure)
					if len(payloads) != 1 || !errors.Is(result.forward, websocket.ErrCloseSent) {
						t.Fatalf("websocket failure payloads=%d forward=%v", len(payloads), result.forward)
					}
					body = string(payloads[0])
					if gjson.GetBytes(
						payloads[0], "status",
					).Int() != http.StatusBadGateway || result.errMsg != failure {
						t.Fatal("generic public failure changed its internal cause or HTTP status")
					}
				} else {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					if client == "Codex SSE" {
						ctx.Request.Header.Set("Originator", "Codex Desktop")
					}
					if err := writeResponsesStreamTerminalError(ctx, failure); err != nil {
						t.Fatal(err)
					}
					body = recorder.Body.String()
				}
				if !strings.Contains(body, http.StatusText(http.StatusBadGateway)) ||
					strings.Contains(body, "private upstream failure") || strings.Contains(
					body, "SYNTHETIC_RESPONSE_FAILURE_SECRET",
				) {
					t.Fatalf("safe terminal failure missing or private details exposed: %s", body)
				}
			},
		)
	}
}

func TestResponsesProtocolFailureFlushesBeforeAcknowledgement(t *testing.T) {
	writeFailure := errors.New("synthetic protocol-error write failure")
	flushFailure := errors.New("synthetic protocol-error flush failure")
	for _, codex := range []bool{false, true} {
		for _, delivery := range []struct {
			name     string
			writeErr error
			flushErr error
		}{
			{name: "delivered"},
			{name: "write failure", writeErr: writeFailure},
			{name: "flush failure", flushErr: flushFailure},
		} {
			client := "standard/"
			if codex {
				client = "Codex/"
			}
			t.Run(
				client+delivery.name, func(t *testing.T) {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					if codex {
						ctx.Request.Header.Set("Originator", "codex-tui")
					}
					var events []string
					ctx.Writer = &openAIStreamDeliveryWriter{
						ResponseWriter: ctx.Writer, events: &events,
						writeErr: delivery.writeErr, flushErr: delivery.flushErr,
					}
					data := make(chan coreexecutor.StreamChunk, 1)
					var acknowledged, canceled error
					acks := 0
					data <- coreexecutor.StreamChunk{
						Payload: []byte(("event: response.completed\ndata: {\"private\":" +
							"SYNTHETIC_RESPONSE_FAILURE_SECRET\n\n")),
						Acknowledge: func(err error) {
							if delivery.writeErr == nil && (len(events) == 0 || events[len(events)-1] != "flush") {
								t.Errorf("protocol failure acknowledged before flush: %v", events)
							}
							acknowledged = err
							acks++
						},
					}
					close(data)
					handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
					handler.forwardResponsesStreamChunks(
						ctx, ctx.Writer, func(err error) { canceled = err }, nil, data, nil,
					)
					if acks != 1 || acknowledged == nil || canceled == nil ||
						!strings.Contains(acknowledged.Error(), "malformed data") || !strings.Contains(
						canceled.Error(), "malformed data",
					) {
						t.Fatalf(
							"protocol failure lost: acknowledgements=%d ack=%v cancel=%v",
							acks,
							acknowledged,
							canceled,
						)
					}
					for _, expected := range []error{delivery.writeErr, delivery.flushErr} {
						if expected != nil &&
							(!errors.Is(acknowledged, expected) || !errors.Is(canceled, expected)) {
							t.Fatalf(
								"delivery failure lost: ack=%v cancel=%v want=%v",
								acknowledged,
								canceled,
								expected,
							)
						}
					}
					body := recorder.Body.String()
					if delivery.writeErr == nil && (strings.Count(body, "event:") != 1 ||
						!strings.Contains(body, http.StatusText(http.StatusBadGateway))) {
						t.Fatalf("expected one generic failure event: %q", body)
					}
					if strings.Contains(body, "SYNTHETIC_RESPONSE_FAILURE_SECRET") || strings.Contains(
						body, "malformed data",
					) {
						t.Fatalf("protocol details exposed: %q", body)
					}
				},
			)
		}
	}
}

func TestStandardResponsesEmptyStreamReportsVisibleFailure(t *testing.T) {
	const model = "responses-empty-visible-failure"
	router := newResponsesStreamTestRouter(t, model, &responsesStreamTestExecutor{closeWithoutPayload: true})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder, httptest.NewRequest(
			http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"`+model+`","stream":true,"input":"synthetic"}`),
		),
	)
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(recorder.Body.String(), "event: error") ||
		!strings.Contains(recorder.Body.String(), http.StatusText(http.StatusBadGateway)) {
		t.Fatalf("empty stream failure HTTP=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
