package openai_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsesHandlerConsumesPatchTranslationFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/invalid=%t", stream, invalid), func(t *testing.T) {
				const model = "offline-patch-handler"
				const input = "*** Begin Patch\n中文🧪\n*** End Patch"
				arguments, err := sjson.Set(`{}`, "input", input)
				if err != nil {
					t.Fatal(err)
				}
				if invalid {
					arguments = `{"input":42}`
				}
				response := `{"id":"chat_patch","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
				response, err = sjson.Set(response, "choices.0.message.tool_calls.0.function.arguments", arguments)
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					chunk := `{"id":"chat_patch","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`
					chunk, err = sjson.Set(chunk, "choices.0.delta.tool_calls.0.function.arguments", arguments)
					if err != nil {
						t.Fatal(err)
					}
					response = "data: " + chunk + "\n\ndata: {\"id\":\"chat_patch\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n"
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					body, errRead := io.ReadAll(req.Body)
					if errRead != nil {
						http.Error(writer, errRead.Error(), http.StatusBadRequest)
						return
					}
					if gjson.GetBytes(body, "tools.0.function.parameters.properties.input.type").String() != "string" {
						http.Error(writer, "handler request did not reach patch converter", http.StatusBadRequest)
						return
					}
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					writer.Header().Set("Content-Type", contentType)
					_, _ = io.WriteString(writer, response)
				}))
				t.Cleanup(server.Close)
				exec := executor.NewOpenAICompatExecutor("offline-patch-handler", &config.Config{})
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(exec)
				auth := &coreauth.Auth{ID: t.Name(), Provider: exec.Identifier(), Status: coreauth.StatusActive,
					Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "synthetic-key"}}
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
				models := registry.GetGlobalRegistry()
				models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { models.UnregisterClient(auth.ID) })
				handler := openai.NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				payload := `{"model":"` + model + `","tools":[{"type":"custom","name":"apply_patch"}],"input":[{"role":"user","content":[{"type":"input_text","text":"offline patch"}]}]}`
				payload, err = sjson.Set(payload, "stream", stream)
				if err != nil {
					t.Fatal(err)
				}
				writer := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(writer)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
				handler.Responses(ctx)
				if calls.Load() != 1 {
					t.Fatalf("upstream requests = %d, want 1; response=%s", calls.Load(), writer.Body.String())
				}
				body := writer.Body.String()
				if invalid {
					if (!stream && writer.Code != http.StatusBadGateway) || !strings.Contains(body, "invalid apply_patch tool arguments") || strings.Contains(body, `"type":"response.completed"`) {
						t.Fatalf("invalid tool response was not rejected: status=%d body=%s", writer.Code, body)
					}
					return
				}
				if writer.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", writer.Code, body)
				}
				if !stream {
					if gjson.Get(body, "output.0.type").String() != "custom_tool_call" || gjson.Get(body, "output.0.input").String() != input {
						t.Fatalf("handler did not deliver freeform call: %s", body)
					}
				} else if strings.Count(body, `"type":"response.completed"`) != 1 || !strings.Contains(body, `"type":"response.custom_tool_call_input.done"`) {
					t.Fatalf("handler did not deliver completed freeform stream: %s", body)
				}
			})
		}
	}
}

func TestOpenAIHandlersRejectEmptiedResponsesAttachments(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, part := range []string{`{"type":"input_file","file_url":"https://example.test/file.pdf"}`, `{"type":"input_audio","input_audio":{"format":"wav"}}`} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", endpoint, stream, part), func(t *testing.T) {
					const model = "offline-attachment-handler"
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
						calls.Add(1)
						http.Error(writer, "invalid attachment reached upstream", http.StatusInternalServerError)
					}))
					t.Cleanup(server.Close)
					exec := executor.NewOpenAICompatExecutor("offline-attachment-handler", &config.Config{})
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(exec)
					auth := &coreauth.Auth{ID: t.Name(), Provider: exec.Identifier(), Status: coreauth.StatusActive,
						Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "synthetic-key"}}
					if _, err := manager.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					models := registry.GetGlobalRegistry()
					models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { models.UnregisterClient(auth.ID) })
					base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
					writer := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(writer)
					payload := fmt.Sprintf(`{"model":%q,"stream":%t,"instructions":"system","input":[{"role":"user","content":"history"},{"role":"assistant","content":"reply"},{"role":"user","content":[%s]}]}`, model, stream, part)
					ctx.Request = httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(payload))
					if endpoint == "/v1/responses" {
						openai.NewResponsesAPIHandler(base).Responses(ctx)
					} else {
						openai.NewAPIHandler(base).ChatCompletions(ctx)
					}
					if writer.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(writer.Body.String(), "unsupported content part:") {
						t.Fatalf("empty attachment turn was not rejected: status=%d calls=%d body=%s", writer.Code, calls.Load(), writer.Body.String())
					}
				})
			}
		}
	}
}

func TestResponsesHandlerConsumesDeferredPatchIdentityFailure(t *testing.T) {
	const model = "offline-patch-identity-handler"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: "+`{"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"","arguments":""}}]}}]}`+"\n\ndata: "+`{"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"","arguments":"{\"input\":\"complete\"}"}}]}}]}`+"\n\ndata: "+`{"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"apply_patch","arguments":""}}]}}]}`+"\n\ndata: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	exec := executor.NewOpenAICompatExecutor("offline-patch-identity-handler", &config.Config{})
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: t.Name(), Provider: exec.Identifier(), Status: coreauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "synthetic-key"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { models.UnregisterClient(auth.ID) })
	handler := openai.NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","stream":true,"tools":[{"type":"custom","name":"apply_patch"}],"input":"offline"}`))
	ctx.Request.Header.Set("Originator", "Codex Desktop")
	handler.Responses(ctx)
	body := writer.Body.String()
	if calls.Load() != 1 || !strings.Contains(body, "event: response.failed") || !strings.Contains(body, http.StatusText(http.StatusBadGateway)) || strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, "tool identity changed") {
		t.Fatalf("identity failure was not delivered as a generic terminal: status=%d calls=%d body=%s", writer.Code, calls.Load(), body)
	}
}
