package executor_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtime "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type auditImageArtifactStore struct{ puts atomic.Int32 }

func (s *auditImageArtifactStore) PutBytes(_ context.Context, data []byte, _ string) (
	handlers.ImageArtifactRecord,
	error,
) {
	if len(data) != 1 || data[0] != 0 {
		return handlers.ImageArtifactRecord{}, fmt.Errorf("unexpected synthetic image bytes: %v", data)
	}
	s.puts.Add(1)
	return handlers.ImageArtifactRecord{
		ID: "img_" + strings.Repeat(
			"a",
			64,
		) + ".png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Size: 1,
	}, nil
}

func (s *auditImageArtifactStore) PutDataURL(
	ctx context.Context,
	value string,
) (handlers.ImageArtifactRecord, error) {
	if value != "data:image/png;base64,AA==" {
		return handlers.ImageArtifactRecord{}, fmt.Errorf("unexpected synthetic image data URL: %s", value)
	}
	return s.PutBytes(ctx, []byte{0}, "image/png")
}

func (*auditImageArtifactStore) PutRemoteImage(
	context.Context,
	string,
) (handlers.ImageArtifactRecord, []byte, error) {
	return handlers.ImageArtifactRecord{}, nil, errors.New("synthetic test does not fetch remote images")
}

type auditImageArtifactURLBuilder struct{}

func (auditImageArtifactURLBuilder) ImageArtifactContentURL(
	_ *gin.Context,
	record handlers.ImageArtifactRecord,
) (string, error) {
	return "https://artifacts.synthetic.invalid/" + record.ID, nil
}

type auditImageEndpointExecutor struct {
	auth.ProviderExecutor
	calls atomic.Int32
}

func (*auditImageEndpointExecutor) Identifier() string { return "codex" }
func (e *auditImageEndpointExecutor) Execute(context.Context, *auth.Auth, core.Request, core.Options) (
	core.Response,
	error,
) {
	e.calls.Add(1)
	return core.Response{Payload: []byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`)}, nil
}
func (e *auditImageEndpointExecutor) ExecuteStream(
	context.Context,
	*auth.Auth,
	core.Request,
	core.Options,
) (*core.StreamResult, error) {
	e.calls.Add(1)
	ch := make(chan core.StreamChunk, 1)
	ch <- core.StreamChunk{
		Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"ty" +
			"pe\":\"image_generation_call\",\"result\":\"AA==\",\"output_format\":" +
			"\"png\"}]}}\n\n"),
	}
	close(ch)
	return &core.StreamResult{Chunks: ch}, nil
}

func TestAuditImageModesReachPublicEndpoints(t *testing.T) {
	for _, mode := range []config.DisableImageGenerationMode{
		config.DisableImageGenerationOff, config.DisableImageGenerationAll, config.DisableImageGenerationChat,
		config.DisableImageGenerationPassthrough,
	} {
		for _, endpoint := range []string{"generations", "edits"} {
			t.Run(
				mode.String()+"/"+endpoint, func(t *testing.T) {
					manager := auth.NewManager(nil, nil, nil)
					manager.SetConfig(&config.Config{})
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					execute := &auditImageEndpointExecutor{}
					manager.RegisterExecutor(execute)
					credential, err := manager.Register(
						t.Context(), &auth.Auth{
							ID: t.Name(), Provider: "codex",
							Attributes: map[string]string{"api_key": "synthetic-key", "auth_kind": "apikey"},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(
						credential.ID,
						"codex",
						[]*registry.ModelInfo{
							{ID: "gpt-image-1.5", SupportedOutputModalities: []string{"image"}},
						},
					)
					t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
					manager.RefreshSchedulerEntry(credential.ID)
					base := handlers.NewBaseAPIHandlers(
						&sdkconfig.SDKConfig{DisableImageGeneration: mode},
						manager,
					)
					store := &auditImageArtifactStore{}
					base.SetImageArtifactStore(store)
					base.SetImageArtifactURLBuilder(auditImageArtifactURLBuilder{})
					handler := openai.NewAPIHandler(base)
					body := `{"model":"gpt-image-1.5","prompt":"synthetic","response_format":` +
						`"b64_json","image":"data:image/png;base64,` +
						`AA=="}`
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(
						http.MethodPost,
						"/v1/images/"+endpoint,
						strings.NewReader(body),
					)
					c.Request.Header.Set("Content-Type", "application/json")
					if endpoint == "generations" {
						handler.ImagesGenerations(c)
					} else {
						handler.ImagesEdits(c)
					}
					if mode == config.DisableImageGenerationAll {
						if recorder.Code != http.StatusNotFound || execute.calls.Load() != 0 ||
							store.puts.Load() != 0 {
							t.Fatalf(
								"all status=%d calls=%d body=%s", recorder.Code, execute.calls.Load(),
								recorder.Body.String(),
							)
						}
					} else if recorder.Code != http.StatusOK || execute.calls.Load() != 1 || gjson.Get(
						recorder.Body.String(), "data.0.content_url",
					).String() != "https://artifacts.synthetic.invalid/img_"+strings.Repeat(
						"a", 64,
					)+".png" || store.puts.Load() != 1 || gjson.Get(
						recorder.Body.String(), "data.0.b64_json",
					).String() != "AA==" {
						t.Fatalf(
							"mode=%s status=%d calls=%d body=%s", mode, recorder.Code, execute.calls.Load(),
							recorder.Body.String(),
						)
					}
				},
			)
		}
	}
}

func TestAuditImageModesControlActualCodexTools(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_synthetic_policy","object":"` +
		`response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"` +
		`total_tokens":2}}}`
	for _, mode := range []config.DisableImageGenerationMode{
		config.DisableImageGenerationOff, config.DisableImageGenerationAll, config.DisableImageGenerationChat,
		config.DisableImageGenerationPassthrough,
	} {
		for _, clientTool := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				for _, transport := range []string{"http", "http-stream", "ws", "ws-stream"} {
					t.Run(
						fmt.Sprintf("%s/client-tool=%t/override=%t/%s", mode, clientTool, override, transport),
						func(t *testing.T) {
							received := make(chan []byte, 1)
							server := httptest.NewServer(
								http.HandlerFunc(
									func(w http.ResponseWriter, r *http.Request) {
										if websocket.IsWebSocketUpgrade(r) {
											peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
											if err != nil {
												t.Error(err)
												return
											}
											defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
											_, body, err := peer.ReadMessage()
											if err != nil {
												t.Error(err)
												return
											}
											received <- body
											if err = peer.WriteMessage(
												websocket.TextMessage, []byte(completed),
											); err != nil {
												t.Error(err)
											}
											return
										}
										body, err := io.ReadAll(r.Body)
										if err != nil {
											t.Error(err)
											return
										}
										received <- body
										w.Header().Set("Content-Type", "text/event-stream")
										_, _ = io.WriteString(w, "data: "+completed+"\n\n")
									},
								),
							)
							defer server.Close()
							cfg := &config.Config{DisableImageGeneration: mode}
							if override {
								cfg.Payload.Override = []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{Name: "gpt-5.4"},
										}, Params: map[string]any{
											"tools": []any{
												map[string]any{
													"type": "image_generation", "quality": "high",
												},
											}, "tool_choice": map[string]any{"type": "image_generation"},
										},
									},
								}
							}
							toolJSON := ""
							if clientTool {
								toolJSON = `,"tools":[{"type":"image_generation","quality":"low"}],` +
									`"tool_choice":{"type":` +
									`"image_generation"}`
							}
							request := core.Request{
								Model:   "gpt-5.4",
								Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"` + toolJSON + `}`),
							}
							credential := &auth.Auth{
								ID: t.Name(), Provider: "codex",
								Attributes: map[string]string{"base_url": server.URL, "auth_kind": "oauth"},
								Metadata: map[string]any{
									"access_token": "synthetic-access", "account_id": "synthetic-account",
								},
							}
							ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
							defer cancel()
							if strings.HasPrefix(transport, "ws") {
								ctx = core.WithDownstreamWebsocket(ctx)
							}
							opts := core.Options{
								SourceFormat:   translator.FormatOpenAIResponse,
								ResponseFormat: translator.FormatOpenAIResponse,
								Metadata:       map[string]any{core.RequestPathMetadataKey: "/v1/responses"},
							}
							execute := runtime.NewCodexAutoExecutor(cfg)
							var err error
							if strings.HasSuffix(transport, "stream") {
								var stream *core.StreamResult
								stream, err = execute.ExecuteStream(ctx, credential, request, opts)
								if err == nil {
									for chunk := range stream.Chunks {
										if chunk.Err != nil {
											err = chunk.Err
										}
									}
								}
							} else {
								_, err = execute.Execute(ctx, credential, request, opts)
							}
							if err != nil {
								t.Fatal(err)
							}
							wire := <-received
							var image gjson.Result
							for _, tool := range gjson.GetBytes(wire, "tools").Array() {
								if tool.Get("type").String() == "image_generation" {
									image = tool
								}
							}
							wantImage := override || mode == config.DisableImageGenerationOff ||
								(mode == config.DisableImageGenerationPassthrough && clientTool)
							if image.Exists() != wantImage {
								t.Fatalf(
									"mode=%s image=%t want=%t body=%s",
									mode,
									image.Exists(),
									wantImage,
									wire,
								)
							}
							if override && (image.Get("quality").String() != "high" || gjson.GetBytes(
								wire, "tool_choice.type",
							).String() != "image_generation") {
								t.Fatalf("explicit payload rule lost: %s", wire)
							}
							if !override && clientTool &&
								(mode ==
									config.DisableImageGenerationAll ||
									mode ==
										config.DisableImageGenerationChat) &&
								gjson.GetBytes(
									wire, "tool_choice",
								).Exists() {
								t.Fatalf("stripped tool choice survived: %s", wire)
							}
						},
					)
				}
			}
		}
	}
}
