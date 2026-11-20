package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const originalLiveAntigravityMessage = `{"type": "message_start", "message": {"id": "UVM7aqirB-npz7IP8rfZuQQ"` +
	`, "type": "message", "role": "assistant", "content": [], "model": "gemini-3-flash", "stop_reason": null,` +
	` "stop_sequence": null, "usage": {"input_tokens": 2, "output_tokens": 1}}}`
const originalLiveKimiChat = `{"id":"chatcmpl-McAG6QS2WmxRKmMxSjWvbgWB","object":"chat.completion.chunk","` +
	`created":1782272842,"model":"kimi-k2.5","choices":[{"index":0,"delta":{"role":"assistant","content":""},` +
	`"finish_reason":null}],"system_fingerprint":"fpv0_b30801d4"}`
const originalLiveKimiMessage = `{"type":"message_start","message":{"id":"msg_iFEkPDty2KtvlbdThqOBsN25"` +
	`,"type":"message","role":"assistant","content":[],"model":"kimi-k2.5",` +
	`"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1263,"` +
	`cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tok` +
	`ens":0,"service_tier":"standard","inference_geo":"not_available","prom` +
	`pt_tokens":1263,"cached_tokens":0}}}`
const originalLiveXAIMessage = `{"type":"message_start","message":{"id":"4aeb964a-1190-98f6-9978-a8a75` +
	`48848d8","type":"message","role":"assistant","model":"grok-4.3","stop_` +
	`sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":` +
	`[],"stop_reason":null}}`

type originalForceMappingExecutor struct {
	*forceMappingExecutor
	unknownCapacity  bool
	creditsRequested []bool
}

func (e *originalForceMappingExecutor) Execute(
	ctx context.Context,
	_ *Auth,
	req core.Request,
	_ core.Options,
) (core.Response, error) {
	e.mu.Lock()
	e.executeModels = append(e.executeModels, req.Model)
	if e.unknownCapacity {
		e.creditsRequested = append(e.creditsRequested, AntigravityCreditsRequested(ctx))
	}
	e.mu.Unlock()
	if e.unknownCapacity {
		if !AntigravityCreditsRequested(ctx) {
			return core.Response{}, &Error{
				HTTPStatus: http.StatusServiceUnavailable, Message: "MODEL_CAPACITY_EXHAUSTED",
			}
		}
		return core.Response{
			Payload: []byte(`{"model":"` + req.Model + `","message":{"model":"` + req.Model + `"}}`),
		}, nil
	}
	payload := forceMappingNonStreamUpstreamPayload(e.id, req.Model)
	switch e.id {
	case "antigravity":
		payload = strings.Replace(originalLiveAntigravityMessage, "gemini-3-flash", req.Model, 1)
	case "xai":
		payload = `{"type":"message","role":"assistant","model":"` +
			req.Model +
			`","content":[{"type":"text","text":"hi"}]}`
	}
	return core.Response{Payload: []byte(payload)}, nil
}

func (e *originalForceMappingExecutor) ExecuteStream(
	ctx context.Context,
	_ *Auth,
	req core.Request,
	_ core.Options,
) (*core.StreamResult, error) {
	e.mu.Lock()
	e.streamModels = append(e.streamModels, req.Model)
	if e.unknownCapacity {
		e.creditsRequested = append(e.creditsRequested, AntigravityCreditsRequested(ctx))
	}
	e.mu.Unlock()
	if e.unknownCapacity {
		chunks := make(chan core.StreamChunk, 1)
		if !AntigravityCreditsRequested(ctx) {
			chunks <- core.StreamChunk{
				Err: &Error{
					HTTPStatus: http.StatusServiceUnavailable, Message: "MODEL_CAPACITY_EXHAUSTED",
				},
			}
		} else {
			chunks <- core.StreamChunk{Payload: []byte(`data: {"message":{"model":"` + req.Model + `"}}` + "\n\n")}
		}
		close(chunks)
		return &core.StreamResult{Chunks: chunks}, nil
	}
	payloads := forceMappingStreamUpstreamChunks(e.id, req.Model)
	switch e.id {
	case "antigravity":
		payloads = [][]byte{
			[]byte("event: message_start\n"),
			[]byte(
				"data: " + strings.Replace(
					originalLiveAntigravityMessage,
					"gemini-3-flash",
					req.Model,
					1,
				) + "\n\n",
			),
		}
	case "kimi":
		payloads = [][]byte{
			[]byte("event:message_start\n"),
			[]byte("data:" + strings.Replace(originalLiveKimiMessage, "kimi-k2.5", req.Model, 1) + "\n\n"),
			[]byte("data: " + strings.Replace(originalLiveKimiChat, "kimi-k2.5", req.Model, 1) + "\n\n"),
		}
	case "xai":
		payloads = [][]byte{
			[]byte("event: message_start\n"),
			[]byte("data: " + strings.Replace(originalLiveXAIMessage, "grok-4.3", req.Model, 1) + "\n\n"),
		}
	}
	chunks := make(chan core.StreamChunk, len(payloads))
	for _, payload := range payloads {
		chunks <- core.StreamChunk{Payload: payload}
	}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}

func originalForceMappingPayload(t *testing.T, manager *Manager, provider, model string, stream bool) string {
	t.Helper()
	if !stream {
		response, err := manager.Execute(
			context.Background(), []string{provider}, core.Request{Model: model}, core.Options{},
		)
		if err != nil {
			t.Fatalf("Execute() error=%v", err)
		}
		return string(response.Payload)
	}
	result, err := manager.ExecuteStream(
		context.Background(), []string{provider}, core.Request{Model: model}, core.Options{},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error=%v", err)
	}
	return readOpenAICompatStreamPayload(t, result)
}

func TestOriginalOAuthForceMappingPreservesWireResponses(t *testing.T) {
	for _, tc := range []struct{ provider, upstream, alias string }{
		{"antigravity", "gemini-3-flash-preview", "claude-haiku-4-5-20251001"},
		{"antigravity", "gemini-3-flash", "claude-haiku-4-5-20251001"},
		{"codex", "gpt-5.4", "gpt-5.4-fast"},
		{"kimi", "kimi-k2.5", "k2.5"},
		{"xai", "grok-4.3", "grok-latest"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/%s/stream=%t", tc.provider, tc.upstream, stream), func(t *testing.T) {
					manager, base := setupForceMappingManager(t, tc.provider, tc.upstream, tc.alias)
					provider := &originalForceMappingExecutor{forceMappingExecutor: base}
					manager.RegisterExecutor(provider)
					payload := originalForceMappingPayload(t, manager, tc.provider, tc.alias, stream)
					models := provider.ExecuteModels()
					if stream {
						models = provider.StreamModels()
					}
					if len(models) != 1 || models[0] != tc.upstream || !strings.Contains(payload, tc.alias) ||
						forceMappingPayloadLeaksUpstream(payload, tc.upstream) {
						t.Fatalf(
							"models=%v payload=%s, want one target=%s and alias=%s",
							models,
							payload,
							tc.upstream,
							tc.alias,
						)
					}
				},
			)
		}
	}
}

func TestOriginalAPIKeyForceMappingPreservesWireResponses(t *testing.T) {
	for _, tc := range []struct{ provider, key, upstream, alias string }{
		{"claude", "claude-api-key", "glm-5.2", "claude-sonnet-latest"},
		{"codex", "codex-api-key", "gpt-5.5", "claude-sonnet-4-5"},
		{"xai", "xai-api-key", "grok-4.5", "grok-latest"},
		{"vertex", "vertex-api-key", "gemini-3-pro", "claude-opus-4-5"},
		{"openai-compatibility", "openai-compatibility", "deepseek-v3.1", "claude-opus-4.66"},
		{"gemini", "gemini-api-key", "gemini-3-pro", "public-gemini"},
		{"gemini-interactions", "interactions-api-key", "gemini-3-pro", "public-interactions"},
	} {
		for _, force := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, suffix := range []string{"", "(high)"} {
					t.Run(
						fmt.Sprintf("%s/force=%t/stream=%t/suffix=%s", tc.provider, force, stream, suffix),
						func(t *testing.T) {
							apiKey := tc.provider + "-key"
							entry := map[string]any{
								"api-key": apiKey, "models": []map[string]any{
									{
										"name": tc.upstream, "alias": tc.alias, "force-mapping": force,
									},
								},
							}
							if tc.provider == "openai-compatibility" {
								delete(entry, "api-key")
								entry["name"] = tc.provider
							}
							raw, err := json.Marshal(map[string]any{tc.key: []map[string]any{entry}})
							if err != nil {
								t.Fatal(err)
							}
							cfg := &internalconfig.Config{}
							if err := json.Unmarshal(raw, cfg); err != nil {
								t.Fatal(err)
							}
							manager := NewManager(nil, nil, nil)
							manager.SetConfig(cfg)
							provider := &originalForceMappingExecutor{
								forceMappingExecutor: &forceMappingExecutor{id: tc.provider},
							}
							manager.RegisterExecutor(provider)
							auth := &Auth{
								ID: tc.provider + "-api-key-force-mapping-auth", Provider: tc.provider,
								Attributes: map[string]string{"api_key": apiKey},
							}
							if tc.provider == "openai-compatibility" {
								auth.Attributes["compat_name"],
									auth.Attributes["provider_key"] = tc.provider, tc.provider
							}
							if _, err := manager.Register(context.Background(), auth); err != nil {
								t.Fatal(err)
							}
							registerProjectionTestClient(
								t, auth.ID, tc.provider,
								&registry.ModelInfo{ID: tc.alias}, &registry.ModelInfo{ID: tc.upstream},
							)
							manager.RefreshSchedulerEntry(auth.ID)
							payload := originalForceMappingPayload(
								t,
								manager,
								tc.provider,
								tc.alias+suffix,
								stream,
							)
							models := provider.ExecuteModels()
							if stream {
								models = provider.StreamModels()
							}
							upstream := tc.upstream + suffix
							want := upstream
							if force {
								want = tc.alias
							}
							if len(models) != 1 || models[0] != upstream || !strings.Contains(payload, want) ||
								force && forceMappingPayloadLeaksUpstream(payload, upstream) {
								t.Fatalf(
									"models=%v payload=%s, want target=%s client model=%s",
									models,
									payload,
									upstream,
									want,
								)
							}
						},
					)
				}
			}
		}
	}
}

func TestOriginalUnknownCapacityDoesNotEnterForceMappedCredits(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				const upstream, alias = "gemini-3-flash-preview", "claude-haiku-4-5-20251001"
				manager, _ := setupForceMappingCreditsFallbackManager(t, upstream, alias)
				provider := &originalForceMappingExecutor{
					forceMappingExecutor: &forceMappingExecutor{id: "antigravity"}, unknownCapacity: true,
				}
				manager.RegisterExecutor(provider)
				var err error
				if stream {
					var result *core.StreamResult
					result, err = manager.ExecuteStream(
						context.Background(),
						[]string{"antigravity"},
						core.Request{Model: alias},
						core.Options{},
					)
					if err == nil {
						for chunk := range result.Chunks {
							if len(chunk.Payload) != 0 {
								t.Fatalf("unknown capacity returned payload=%q", chunk.Payload)
							}
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = manager.Execute(
						context.Background(), []string{"antigravity"}, core.Request{Model: alias}, core.Options{},
					)
				}
				models := provider.ExecuteModels()
				if stream {
					models = provider.StreamModels()
				}
				provider.mu.Lock()
				flags := append([]bool(nil), provider.creditsRequested...)
				provider.mu.Unlock()
				if statusCodeFromError(err) != http.StatusServiceUnavailable || err == nil ||
					!strings.Contains(
						err.Error(), "MODEL_CAPACITY_EXHAUSTED",
					) || len(models) != 1 || models[0] != upstream ||
					len(flags) != 1 || flags[0] {
					t.Fatalf("unknown capacity error=%v models=%v credits=%v", err, models, flags)
				}
			},
		)
	}
}
