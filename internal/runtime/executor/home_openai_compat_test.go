package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type homeOpenAICompatDispatcher struct {
	payload []byte
}

func (*homeOpenAICompatDispatcher) HeartbeatOK() bool { return true }

func (d *homeOpenAICompatDispatcher) RPopAuth(
	_ context.Context, _, _ string, _ http.Header, _ int,
) ([]byte, error) {
	return d.payload, nil
}

func TestHomeOpenAICompatUsesSelectedCredentialModelOptions(t *testing.T) {
	for _, test := range []struct {
		name, key string
		stream    bool
		useMCT    bool
	}{
		{name: "completion-tokens", key: "max_completion_tokens", useMCT: true},
		{name: "legacy-tokens", key: "max_tokens"},
		{name: "stream-completion-tokens", key: "max_completion_tokens", stream: true, useMCT: true},
		{name: "stream-legacy-tokens", key: "max_tokens", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read upstream request: %v", errRead)
				}
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			cfg := &config.Config{
				Home: config.HomeConfig{Enabled: true},
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name: "local-compat", Models: []config.OpenAICompatibilityModel{{
						Name: "upstream-model", Alias: "client-model", UseMaxCompletionTokens: !test.useMCT,
					}},
				}},
			}
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewOpenAICompatExecutor("openai-compatibility", cfg))
			payload, errMarshal := json.Marshal(map[string]any{
				"model":      "upstream-model",
				"model_info": map[string]any{"id": "upstream-model", "context_length": 32768},
				"auth": &cliproxyauth.Auth{
					ID: "selected-compat", Provider: "custom-compat", Attributes: map[string]string{
						"api_key": "fixture-key", "base_url": server.URL + "/v1", "compat_name": "local-compat", "config_index": "0",
					},
					Metadata: map[string]any{"credential_options": map[string]any{
						"models": []config.OpenAICompatibilityModel{{
							Name: "upstream-model", Alias: "client-model", UseMaxCompletionTokens: test.useMCT,
						}},
					}},
				},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			manager.PublishHomeDispatch(&homeOpenAICompatDispatcher{payload: payload}, executionregistry.New(), 1)
			request := cliproxyexecutor.Request{
				Model:   "client-model",
				Payload: []byte(`{"model":"client-model","messages":[{"role":"user","content":"hello"}],"max_tokens":128}`),
			}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: test.stream}
			if test.stream {
				stream, errExecute := manager.ExecuteStream(context.Background(), []string{"custom-compat"}, request, options)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, errExecute := manager.Execute(context.Background(), []string{"custom-compat"}, request, options); errExecute != nil {
				t.Fatal(errExecute)
			}
			if got := gjson.GetBytes(gotBody, test.key).Int(); got != 128 {
				t.Fatalf("selected %s = %d in %s", test.key, got, gotBody)
			}
			other := "max_tokens"
			if !test.useMCT {
				other = "max_completion_tokens"
			}
			if gjson.GetBytes(gotBody, other).Exists() {
				t.Fatalf("local model option %s leaked into Home request %s", other, gotBody)
			}
		})
	}
}
