package executor

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiHomeModelThinkingCapabilitiesOverrideLocalRegistry(t *testing.T) {
	for _, test := range []struct {
		name   string
		stream bool
	}{
		{name: "non-streaming"},
		{name: "streaming", stream: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				var upstreamBody []byte
				ctx := util.WithRoundTripper(
					t.Context(), kimiTokenRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							var err error
							upstreamBody, err = io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							contentType := "application/json"
							response := `{"id":"chatcmpl_test","object":"chat.completion","created":1,"model":"` +
								`k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"` +
								`},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_toke` +
								`ns":1,"total_tokens":2}}`
							if test.stream {
								contentType = "text/event-stream"
								response = "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\"," +
									"\"created\":1,\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"" +
									"content\":\"ok\"},\"finish_reason\":null}]}\n\n" + "data: [DONE]\n\n"
							}
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": {contentType}},
								Body:       io.NopCloser(strings.NewReader(response)),
							}, nil
						},
					),
				)
				selection, err := json.Marshal(
					map[string]any{
						"model": "kimi-k3", "provider": "kimi",
						"model_info": map[string]any{
							"id": "kimi-k3", "type": "kimi", "context_length": 1048576,
							"thinking":     map[string]any{"levels": []string{"low", "high"}},
							"user_defined": false,
						},
						"auth": cliproxyauth.Auth{
							ID: "home-kimi-k3-auth", Provider: "kimi", Status: cliproxyauth.StatusActive,
							Attributes: map[string]string{},
							Metadata: map[string]any{
								"access_token": "token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
							},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{RequestRetry: 1, Home: config.HomeConfig{Enabled: true}}
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(NewKimiExecutor(cfg))
				manager.PublishHomeDispatch(&homeOpenAICompatDispatcher{payload: selection}, executionregistry.New(), 1)
				payload := []byte(
					`{"model":"kimi-k3","reasoning_effort":"max","messages":[{"role":"user","content":"hello"}]}`,
				)
				req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
				opts := cliproxyexecutor.Options{
					Stream: test.stream, SourceFormat: sdktranslator.FormatOpenAI,
					ResponseFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload,
				}
				if test.stream {
					result, err := manager.ExecuteStream(ctx, []string{"kimi"}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream chunk error = %v", chunk.Err)
						}
					}
				} else if _, err := manager.Execute(ctx, []string{"kimi"}, req, opts); err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != "enabled" {
					t.Fatalf("thinking.type = %q, want enabled; body=%s", got, upstreamBody)
				}
				if got := gjson.GetBytes(upstreamBody, "thinking.effort").String(); got != "high" {
					t.Fatalf("thinking.effort = %q, want high from Home model capabilities; body=%s", got, upstreamBody)
				}
				if effort := gjson.GetBytes(upstreamBody, "reasoning_effort"); effort.Exists() {
					t.Fatalf("reasoning_effort should be absent, got %s; body=%s", effort.Raw, upstreamBody)
				}
			},
		)
	}
}
