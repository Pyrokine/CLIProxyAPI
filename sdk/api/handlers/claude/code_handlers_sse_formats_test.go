package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type claudeFormatStreamExecutor struct {
	claudeEmptyStreamExecutor
	payloads []string
}

func (e *claudeFormatStreamExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, len(e.payloads)+1)
	for _, payload := range e.payloads {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(payload)}
	}
	chunks <- coreexecutor.StreamChunk{Terminal: true}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func runClaudeFormatConsumer(t *testing.T, payloads []string) string {
	t.Helper()
	const model = "fixture-claude-sse-format"
	executor := &claudeFormatStreamExecutor{payloads: payloads}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: model, Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	engine := gin.New()
	engine.POST("/v1/messages", handler.ClaudeMessages)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(
			`{"model":"fixture-claude-sse-format","stream":true,"max_tokens":16,`+
				`"messages":[{"role":"user","content":`+
				`"fixture"}]}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Claude stream status=%d, body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

func TestClaudeConsumerRecognizesLegalSSELineFormats(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		lineEnd   string
		dataField string
		multiline bool
		fragment  bool
	}{
		{name: "canonical LF", lineEnd: "\n", dataField: "data: "},
		{name: "data without space", lineEnd: "\n", dataField: "data:"},
		{name: "CRLF split across chunks", lineEnd: "\r\n", dataField: "data: ", fragment: true},
		{name: "CR only", lineEnd: "\r", dataField: "data: "},
		{name: "multiline data", lineEnd: "\n", dataField: "data: ", multiline: true},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				var stream strings.Builder
				for _, event := range []string{"message_start", "message_delta", "message_stop"} {
					payload := `{"type":"` + event + `"}`
					if scenario.multiline {
						payload = `{"type":` + scenario.lineEnd + scenario.dataField + `"` + event + `"}`
					}
					stream.WriteString(
						"event: " +
							event +
							scenario.lineEnd +
							scenario.dataField +
							payload +
							scenario.lineEnd +
							scenario.lineEnd,
					)
				}
				want := stream.String()
				payloads := []string{want}
				if scenario.fragment {
					payloads = nil
					for _, value := range []byte(want) {
						payloads = append(payloads, string([]byte{value}))
					}
				}
				if got := runClaudeFormatConsumer(t, payloads); got != want {
					t.Fatalf("stream changed or gained another terminal: got=%q, want=%q", got, want)
				}
			},
		)
	}
}

func TestClaudeConsumerSuppressesEventsAfterTerminalInOnePayload(t *testing.T) {
	for _, terminal := range []string{"message_stop", "error"} {
		t.Run(
			terminal, func(t *testing.T) {
				prefix := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: mes" +
					"sage_delta\ndata: {\"type\":\"message_delta\"}\n\n" +
					"event: " + terminal + "\ndata: {\"type\":\"" + terminal + "\"}\n\n"
				late := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"," +
					"\"delta\":{\"text\":\"fixture-late-text\"}}\n\n"
				if got := runClaudeFormatConsumer(t, []string{prefix + late}); got != prefix {
					t.Fatalf("post-terminal payload was forwarded: got=%q, want=%q", got, prefix)
				}
			},
		)
	}
}
