package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexLiteOnlyNativeFidelity(t *testing.T) {
	for _, source := range []translator.Format{
		translator.FormatCodex, translator.FormatOpenAIResponse, translator.FormatClaude, translator.FormatOpenAI,
	} {
		for _, transport := range []string{"http", "websocket"} {
			for _, lite := range []string{"", "header", "metadata"} {
				t.Run(
					fmt.Sprintf("%s/%s/lite=%s", source, transport, lite), func(t *testing.T) {
						metadata := `{"type":"codex.response.metadata","headers":{"x-models-etag":"models-v1",` +
							`"x-codex-turn-state":"turn-1","x-codex-safety-buffering-enabled":"true",` +
							`"x-codex-safety-buffering-faster-model":"fixture-model"},"future":{"ok":true}}`
						completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
							(`"output":[],"future":{"ok":true},"usage":{"input_tokens":1,` +
								`"output_tokens":1,"total_tokens":` +
								`2}}}`)
						events := []string{
							metadata, `{"type":"response.output_item.done","output_index":0,` +
								`"item":{"id":"msg_1","type":"message","role":"assistant",` +
								`"content":[{"type":"output_text","text":"ok"}]}}`, completed,
						}
						captured := make(chan []byte, 1)
						capturedHeaders := make(chan http.Header, 1)
						server := httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, r *http.Request) {
									capturedHeaders <- r.Header.Clone()
									if transport == "websocket" {
										peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
										if err != nil {
											t.Error(err)
											return
										}
										defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
										_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
										_, body, err := peer.ReadMessage()
										if err != nil {
											t.Error(err)
											return
										}
										captured <- body
										for _, event := range events {
											if err := peer.WriteMessage(
												websocket.TextMessage, []byte(event),
											); err != nil {
												t.Error(err)
												return
											}
										}
										return
									}
									body, err := io.ReadAll(r.Body)
									if err != nil {
										t.Error(err)
										return
									}
									captured <- body
									w.Header().Set("Content-Type", "text/event-stream")
									for _, event := range events {
										if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
											t.Error(err)
											return
										}
									}
								},
							),
						)
						t.Cleanup(server.Close)
						payload := []byte(`{"model":"gpt-5.6-sol","input":[],"parallel_tool_calls":false}`)
						headers := http.Header{"Session-Id": {"session-1"}, "Thread-Id": {"thread-1"}}
						if lite == "header" {
							headers.Set(codexResponsesLiteHeader, "true")
						} else if lite == "metadata" {
							payload = []byte(`{"model":"gpt-5.6-sol","input":[],"parallel_tool_calls":false,` +
								(`"client_metadata":{"ws_request_header_x_openai_internal_codex_response` +
									`s_lite":"true"}}`))
						}
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						t.Cleanup(cancel)
						cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}
						execute := newCodexExecutor(cfg).ExecuteStream
						if transport == "websocket" {
							execute = newCodexWebsocketsExecutor(cfg).ExecuteStream
						}
						auth := &cliproxyauth.Auth{
							Attributes: map[string]string{
								"base_url": server.URL, "api_key": "test",
							},
						}
						result, err := execute(
							ctx, auth, core.Request{Model: "gpt-5.6-sol", Payload: payload}, core.Options{
								SourceFormat: source, ResponseFormat: translator.FormatCodex, Headers: headers,
								Stream: true,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						var terminal []byte
						var metadataEvents []string
						for chunk := range result.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							for line := range bytes.SplitSeq(chunk.Payload, []byte("\n")) {
								data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
								switch gjson.GetBytes(data, "type").String() {
								case "codex.response.metadata":
									metadataEvents = append(metadataEvents, string(data))
								case "response.completed":
									terminal = bytes.Clone(data)
								}
							}
						}
						body := <-captured
						upstreamHeaders := <-capturedHeaders
						native := lite != "" &&
							(source == translator.FormatCodex || source == translator.FormatOpenAIResponse)
						wantLite := ""
						if lite == "header" && (transport == "http" || native) {
							wantLite = "true"
						}
						if got := upstreamHeaders.Get(codexResponsesLiteHeader); got != wantLite {
							t.Errorf("upstream Lite header = %q, want %q", got, wantLite)
						}
						wantSession := "session-1"
						if cacheID := gjson.GetBytes(body, "prompt_cache_key").String(); cacheID != "" {
							wantSession = cacheID
						}
						if got := codexSessionHeaderValue(upstreamHeaders); wantSession == "" ||
							got != wantSession {
							t.Errorf("canonical session = %q, want %q", got, wantSession)
						}
						if len(metadataEvents) != 1 || metadataEvents[0] != metadata {
							t.Errorf("metadata event changed or duplicated: %q", metadataEvents)
						}
						if native {
							expected := completed
							for _, path := range []string{
								"response.usage.input_tokens_details.cached_tokens",
								"response.usage.output_tokens_details.reasoning_tokens",
							} {
								var err error
								expected, err = sjson.Set(expected, path, 0)
								if err != nil {
									t.Fatal(err)
								}
							}
							if gjson.GetBytes(body, "instructions").Exists() || string(terminal) != expected {
								t.Errorf(
									"native request or completion changed beyond explicit usage zeros: %s; %s",
									body,
									terminal,
								)
							}
						} else if gjson.GetBytes(body, "instructions").Type != gjson.String ||
							gjson.GetBytes(terminal, "response.output.0.id").String() != "msg_1" {
							t.Errorf("compatibility normalization or output hydration was lost: %s; %s", body, terminal)
						}
					},
				)
			}
		}
	}
}

func TestCodexWebsocketLiteHeaderWithoutSessionHeaders(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("native=%t/disabled=%t", native, disabled), func(t *testing.T) {
					cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: disabled}}
					headers := make(http.Header)
					headers.Set(codexResponsesLiteHeader, "true")
					got := applyCodexWebsocketHeaders(
						t.Context(),
						nil,
						nil,
						"fixture-token",
						cfg,
						native,
						headers,
					)
					want := ""
					if native {
						want = "true"
					}
					if value := got.Get(codexResponsesLiteHeader); value != want {
						t.Errorf("Lite header = %q, want %q", value, want)
					}
				},
			)
		}
	}
}

func TestCodexDisabledCloakingPreservesCallerIdentity(t *testing.T) {
	for _, test := range []struct {
		name, keyOverride string
		globalDisabled    bool
		wantCaller        bool
	}{
		{name: "enabled"},
		{name: "global disabled", globalDisabled: true, wantCaller: true},
		{name: "key disabled", keyOverride: "true", wantCaller: true},
		{name: "key enables over global", keyOverride: "false", globalDisabled: true},
	} {
		for _, fromGin := range []bool{false, true} {
			for _, transport := range []string{"http", "websocket"} {
				t.Run(
					fmt.Sprintf("%s/gin=%t/%s", test.name, fromGin, transport), func(t *testing.T) {
						caller := http.Header{
							"User-Agent": {"caller-agent/1.0"}, "Originator": {"caller-origin"},
							"Version": {"caller-version"}, "Session-Id": {"caller-session"},
							"Thread-Id":              {"caller-thread"},
							codexResponsesLiteHeader: {"true"}, "Authorization": {"Bearer caller-key"},
						}
						auth := &cliproxyauth.Auth{
							Attributes: map[string]string{"api_key": "selected-upstream"},
						}
						if test.keyOverride != "" {
							auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking] = test.keyOverride
						}
						cfg := &config.Config{
							Codex: config.CodexConfig{DisableCodexCloaking: test.globalDisabled},
						}
						ctx := t.Context()
						explicit := caller
						if fromGin {
							ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
							ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
							ginCtx.Request.Header = caller
							ctx = util.WithGinContext(ctx, ginCtx)
							explicit = nil
						}
						var got http.Header
						if transport == "websocket" {
							got = applyCodexWebsocketHeaders(
								ctx,
								nil,
								auth,
								"selected-upstream",
								cfg,
								false,
								explicit,
							)
						} else {
							req, err := http.NewRequestWithContext(
								ctx, http.MethodPost, "https://caller-header.invalid", nil,
							)
							if err != nil {
								t.Fatal(err)
							}
							applyCodexHeaders(req, auth, "selected-upstream", true, cfg, explicit)
							got = req.Header
						}
						for _, name := range []string{
							"User-Agent", "Originator", "Version", "Session-Id", "Thread-Id",
						} {
							if equal := got.Get(name) == caller.Get(name); equal != test.wantCaller {
								t.Errorf(
									"%s = %q, caller=%q, preserve=%t", name, got.Get(name), caller.Get(name),
									test.wantCaller,
								)
							}
						}
						if got.Get("Authorization") != "Bearer selected-upstream" {
							t.Error("caller identity headers changed the selected upstream credential")
						}
					},
				)
			}
		}
	}
}
