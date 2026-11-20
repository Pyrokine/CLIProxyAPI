package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexMultiAgentOriginalExecutionContracts(t *testing.T) {
	const modelID = "codex-executor-spawn-agent-test-model"
	models := registry.GetGlobalRegistry()
	models.RegisterClient(
		t.Name(), "codex", []*registry.ModelInfo{
			{
				ID: modelID, Description: "Executor test model.",
				Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
			},
		},
	)
	t.Cleanup(func() { models.UnregisterClient(t.Name()) })
	for _, sample := range []struct {
		name, requested, selected           string
		enabled, compat, home, dotted, long bool
	}{
		{name: "direct native enabled", requested: "gpt-5.4", selected: "gpt-5.4", enabled: true},
		{name: "direct native disabled", requested: "gpt-5.4", selected: "gpt-5.4"},
		{
			name: "direct compat enabled", requested: "deepseek-v4-flash", selected: "deepseek-v4-flash", enabled: true,
			compat: true,
		},
		{name: "direct compat disabled", requested: "deepseek-v4-flash", selected: "deepseek-v4-flash", compat: true},
		{name: "dotted response", requested: "gpt-5.4", selected: "gpt-5.4", enabled: true, dotted: true},
		{name: "long native enabled", requested: "gpt-5.4", selected: "gpt-5.4", enabled: true, long: true},
		{name: "long native disabled", requested: "gpt-5.4", selected: "gpt-5.4", long: true},
		{
			name: "long compat enabled", requested: "deepseek-v4-flash", selected: "deepseek-v4-flash", enabled: true,
			compat: true, long: true,
		},
		{
			name: "long compat disabled", requested: "deepseek-v4-flash", selected: "deepseek-v4-flash", compat: true,
			long: true,
		},
		{name: "native alias", requested: "tenant/chosen", selected: "gpt-5.4", enabled: true, home: true},
		{
			name: "compat alias", requested: "tenant/chosen", selected: "gpt-5.4", enabled: true, compat: true,
			home: true,
		},
		{name: "native suffix", requested: "tenant/chosen(high)", selected: "gpt-5.4(high)", enabled: true, home: true},
		{
			name: "compat suffix", requested: "tenant/chosen(high)", selected: "gpt-5.4(high)", enabled: true,
			compat: true, home: true,
		},
	} {
		for _, mode := range []string{"execute", "stream", "compact", "websocket execute", "websocket stream"} {
			t.Run(
				sample.name+"/"+mode, func(t *testing.T) {
					captured := make(chan []byte, 1)
					response := func(payload []byte) []byte {
						namespace := gjson.GetBytes(payload, "input.0.tools.0.name").String()
						name, encodedNamespace := "spawn_agent", fmt.Sprintf("%q", namespace)
						if sample.dotted {
							name, encodedNamespace = namespace+".spawn_agent", "null"
						}
						return []byte(fmt.Sprintf(
							`{"type":"response.completed","response":{"id":"resp_1","object":"response","`+
								`status":"completed","output":[{"type":"function_call","name":%q,"namespace":`+
								`%s,"arguments":"{}","call_id":"call_1"}],"usage":{"input_tokens":1,"`+
								`output_tokens":1,"total_tokens":2}}}`,
							name, encodedNamespace,
						))
					}
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, request *http.Request) {
								if websocket.IsWebSocketUpgrade(request) {
									peer, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
									if err != nil {
										t.Error(err)
										return
									}
									defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
									_, payload, err := peer.ReadMessage()
									if err != nil {
										t.Error(err)
										return
									}
									captured <- payload
									if err := peer.WriteMessage(websocket.TextMessage, response(payload)); err != nil {
										t.Error(err)
									}
									_, _, _ = peer.ReadMessage()
									return
								}
								payload, err := io.ReadAll(request.Body)
								if err != nil {
									t.Error(err)
									return
								}
								captured <- payload
								if strings.HasSuffix(request.URL.Path, "/responses/compact") {
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, gjson.GetBytes(response(payload), "response").Raw)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "data: %s\n\n", response(payload))
							},
						),
					)
					defer server.Close()
					cfg := &config.Config{
						Home:                   config.HomeConfig{Enabled: sample.home},
						Codex:                  config.CodexConfig{OptimizeMultiAgentV2: sample.enabled},
						DisableImageGeneration: config.DisableImageGenerationAll,
					}
					credential := &auth.Auth{
						ID: t.Name(), Provider: "codex", Prefix: "tenant",
						Attributes: map[string]string{
							"base_url": server.URL, "api_key": "synthetic-key", "auth_kind": auth.AuthKindAPIKey,
						},
						Metadata: map[string]any{
							"credential_options": map[string]any{
								"models": []config.CodexModel{
									{Name: "gpt-5.4", Alias: "other", IsCompat: !sample.compat},
									{Name: sample.selected, Alias: "chosen", IsCompat: sample.compat},
								},
							},
						},
					}
					selectedInfo := &registry.ModelInfo{
						ID: strings.TrimSuffix(sample.selected, "(high)"), IsCompat: sample.compat,
						ContextLength: 32768, Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
					}
					var execute auth.ProviderExecutor = newCodexExecutor(cfg)
					if strings.HasPrefix(mode, "websocket") {
						ws := newCodexWebsocketsExecutor(cfg)
						defer ws.CloseExecutionSession(t.Name())
						execute = ws
					}
					manager := auth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.RegisterExecutor(execute)
					var homeDispatcher *auditWebsocketHomeDispatcher
					if sample.home {
						envelope, err := json.Marshal(
							map[string]any{
								"model": sample.selected, "model_info": selectedInfo, "auth": credential,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						homeDispatcher = &auditWebsocketHomeDispatcher{payload: envelope}
						manager.PublishHomeDispatch(homeDispatcher, executionregistry.New(), 1)
						defer manager.CloseExecutionSession(t.Name())
					}
					ctx, cancel := context.WithTimeout(codexSpawnAgentTestContext(t), 5*time.Second)
					defer cancel()
					payload := codexSpawnAgentTestPayload()
					if sample.long {
						input := gjson.GetBytes(payload, "input").Array()
						history := []string{input[0].Raw}
						for index := range 128 {
							//goland:noinspection SpellCheckingInspection
							message, err := sjson.Set(input[1].Raw, "id", fmt.Sprintf("amsg_%d", index))
							if err != nil {
								t.Fatal(err)
							}
							history = append(history, message)
						}
						var err error
						payload, err = sjson.SetRawBytes(payload, "input", []byte("["+strings.Join(history, ",")+"]"))
						if err != nil {
							t.Fatal(err)
						}
					}
					before := bytes.Clone(payload)
					request := core.Request{Model: sample.requested, Payload: payload}
					if !sample.home {
						request.Metadata = map[string]any{"cliproxy.resolved_api_key_model_info": selectedInfo}
					}
					opts := core.Options{
						SourceFormat: translator.FormatOpenAIResponse,
						Headers:      http.Header{"User-Agent": {"overridden-client/1.0"}},
						Metadata:     map[string]any{core.ExecutionSessionMetadataKey: t.Name()},
					}
					if mode == "compact" {
						opts.Alt = "responses/compact"
					}
					var clientPayload []byte
					if strings.HasSuffix(mode, "stream") {
						var result *core.StreamResult
						var err error
						if sample.home {
							result, err = manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
						} else {
							result, err = execute.ExecuteStream(ctx, credential, request, opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(nil)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							clientPayload = append(clientPayload, chunk.Payload...)
						}
					} else {
						var result core.Response
						var err error
						if sample.home {
							result, err = manager.Execute(ctx, []string{"codex"}, request, opts)
						} else {
							result, err = execute.Execute(ctx, credential, request, opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						clientPayload = result.Payload
					}
					var wire []byte
					select {
					case wire = <-captured:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if !bytes.Equal(request.Payload, before) {
						t.Fatal("executor changed caller-owned request bytes")
					}
					if sample.home && homeDispatcher.calls.Load() != 1 {
						t.Fatalf("Home dispatches=%d, want 1", homeDispatcher.calls.Load())
					}
					if sample.home {
						if gjson.GetBytes(wire, "input.0.tools.0.name").String() != "collaboration-optimize" ||
							gjson.GetBytes(
								wire, "input.0.tools.0.tools.0.parameters.properties.message.encrypted",
							).Exists() {
							t.Fatalf("Home request did not optimize tool namespace and schema: %s", wire)
						}
						if strings.Contains(
							gjson.GetBytes(wire, "input.0.tools.0.tools.0.description").String(), modelID,
						) {
							t.Fatalf("Home request used the local model catalog: %s", wire)
						}
					} else {
						assertCodexSpawnAgentOptimization(t, wire, modelID, sample.enabled)
					}
					if gjson.GetBytes(wire, "model").String() != strings.TrimSuffix(sample.selected, "(high)") ||
						(strings.HasSuffix(sample.selected, "(high)") && gjson.GetBytes(
							wire, "reasoning.effort",
						).String() != "high") {
						t.Fatalf("selected model or reasoning suffix changed: %s", wire)
					}
					input := gjson.GetBytes(wire, "input").Array()
					if len(input) != len(gjson.GetBytes(before, "input").Array()) {
						t.Fatalf("delegated history length changed: %s", wire)
					}
					if sample.compat {
						for _, message := range input[1:] {
							if message.Get("type").String() != "message" || message.Get("role").String() != "user" ||
								message.Get("content.1.type").
									String() !=
									"input_text" || message.Get("content.1.text").String() != "delegated task" {
								t.Fatalf("compat conversion changed task content: %s", wire)
							}
							for _, field := range []string{
								"author", "recipient", "internal_chat_message_metadata_passthrough",
							} {
								if message.Get(field).Exists() {
									t.Fatalf("compat message retained %s: %s", field, wire)
								}
							}
						}
					} else {
						assertCodexSpawnAgentRequestMessage(t, wire, sample.enabled)
					}
					assertCodexSpawnAgentClientNamespace(t, clientPayload)
				},
			)
		}
	}
}

func codexSpawnAgentTestPayload() []byte {
	//goland:noinspection SpellCheckingInspection
	return []byte(`{"model":"gpt-5.4","input":[{"type":"additional_tools","role":"develop` +
		`er","tools":[{"type":"namespace","name":"collaboration","tools":[{"typ` +
		`e":"function","name":"spawn_agent","description":"Available model over` +
		`rides (optional; inherited parent model is preferred):\n- old-model\nS` +
		`pawns an agent.","parameters":{"type":"object","properties":{"message"` +
		`:{"type":"string","encrypted":true}}}}]}]},{"type":"agent_message","id` +
		`":"amsg_1","author":"/root","recipient":"/root/worker","content":[{"ty` +
		`pe":"input_text","text":"Payload:\n"},{"type":"encrypted_content","enc` +
		`rypted_content":"delegated task"}],"internal_chat_message_metadata_pas` +
		`sthrough":{"turn_id":"turn_1"}}]}`)
}

func codexSpawnAgentTestContext(t *testing.T) context.Context {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("User-Agent", "codex-tui/0.154.0")
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = request
	return util.WithGinContext(t.Context(), ginCtx)
}

func assertCodexSpawnAgentClientNamespace(t *testing.T, payload []byte) {
	t.Helper()
	if bytes.Contains(payload, []byte("collaboration-optimize")) || !bytes.Contains(
		payload, []byte(`"namespace":"collaboration"`),
	) {
		t.Fatalf("client namespace was not restored: %s", payload)
	}
	found := false
	for line := range bytes.SplitSeq(payload, []byte("\n")) {
		for _, path := range []string{"output", "response.output"} {
			for _, item := range gjson.GetBytes(jsonPayload(line), path).Array() {
				if item.Get("type").String() != "function_call" {
					continue
				}
				found = true
				if item.Get("name").String() != "spawn_agent" || item.Get("namespace").String() != "collaboration" ||
					item.Get("arguments").String() != "{}" || item.Get("call_id").String() != "call_1" {
					t.Fatalf("client tool identity or arguments changed: %s", item.Raw)
				}
			}
		}
	}
	if !found {
		t.Fatalf("completed response omitted the original tool call: %s", payload)
	}
}

func assertCodexSpawnAgentRequestMessage(t *testing.T, payload []byte, enabled bool) {
	t.Helper()
	for _, message := range gjson.GetBytes(payload, "input").Array()[1:] {
		if message.Get("type").String() != "agent_message" || message.Get("role").Exists() ||
			message.Get("author").String() != "/root" || message.Get("recipient").String() != "/root/worker" ||
			message.Get("internal_chat_message_metadata_passthrough.turn_id").String() != "turn_1" {
			t.Fatalf("native agent message identity changed: %s", payload)
		}
		if enabled {
			if message.Get("content.1.type").String() != "input_text" ||
				message.Get("content.1.text").String() != "delegated task" ||
				message.Get("content.1.encrypted_content").Exists() {
				t.Fatalf("enabled optimization did not normalize agent content: %s", payload)
			}
		} else if message.Get("content.1.type").String() != "encrypted_content" ||
			message.Get("content.1.encrypted_content").String() != "delegated task" {
			t.Fatalf("disabled optimization changed encrypted content: %s", payload)
		}
	}
}

func assertCodexSpawnAgentOptimization(t *testing.T, payload []byte, modelID string, enabled bool) {
	t.Helper()
	namespace := gjson.GetBytes(payload, "input.0.tools.0.name").String()
	description := gjson.GetBytes(payload, "input.0.tools.0.tools.0.description").String()
	encrypted := gjson.GetBytes(payload, "input.0.tools.0.tools.0.parameters.properties.message.encrypted")
	if enabled {
		want := "- `" + modelID + "`: Executor test model. Reasoning efforts: low, medium (default), high."
		if namespace != "collaboration-optimize" || !strings.Contains(description, want) || encrypted.Exists() {
			t.Fatalf("enabled optimization lost namespace/model list/schema contract: %s", payload)
		}
		return
	}
	if namespace != "collaboration" || !strings.Contains(description, "- old-model") || !encrypted.Bool() {
		t.Fatalf("disabled optimization changed namespace/model list/schema: %s", payload)
	}
}
