package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type latestMediaCapture struct {
	imageCaptureExecutor
	provider string
	response []byte
	options  core.Options
}

func (e *latestMediaCapture) Identifier() string { return e.provider }
func (e *latestMediaCapture) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	_, err := e.imageCaptureExecutor.Execute(ctx, auth, req, opts)
	e.options = opts
	return core.Response{Payload: e.response}, err
}
func (e *latestMediaCapture) ExecuteStream(
	ctx context.Context,
	auth *coreauth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	_, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: e.response}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}

func latestMediaHandler(t *testing.T, provider, model string, response []byte) (*APIHandler, *latestMediaCapture) {
	t.Helper()
	executor := &latestMediaCapture{provider: provider, response: response}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: provider, Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "apikey"},
	}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	info := registry.LookupStaticModelInfo(model)
	if info == nil {
		info = &registry.ModelInfo{ID: model}
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{info})
	manager.RefreshSchedulerEntry(auth.ID)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	return NewAPIHandler(base), executor
}

type svgMediaTestStore struct{ testImageArtifactStore }

func (s *svgMediaTestStore) PutBytes(_ context.Context, data []byte, mime string) (
	handlers.ImageArtifactRecord,
	error,
) {
	record := handlers.ImageArtifactRecord{ID: "synthetic.svg", MIMEType: mime, Size: int64(len(data))}
	s.records = append(s.records, record)
	return record, nil
}

func TestLatestMediaImageHandlersPreserveNativeParameters(t *testing.T) {
	for _, test := range []struct{ provider, model string }{
		{"codex", "gpt-image-2.6-flare"}, {"codex", "gpt-image-2.6-sunburst"}, {"meta", "muse-image-1.0"},
	} {
		t.Run(
			test.model, func(t *testing.T) {
				h, executor := latestMediaHandler(
					t, test.provider, test.model,
					[]byte(`{"created":1,"data":[{"b64_json":"`+testPNGBase64+`"}],"output_format":"png"}`),
				)
				body := `{"model":"` + test.model + `","prompt":"synthetic","complexity":"low","safety_identifier":"opaque-end-user"}`
				if test.provider == "meta" {
					body = `{"model":"muse-image-1.0","prompt":"synthetic","reasoning_strength":"high","tool_enablement":{"enable_image_search":false},"user":"opaque-end-user"}`
				}
				response := audit68OriginalPerformImagesEndpointRequest(
					t, imagesGenerationsPath, "application/json", strings.NewReader(body), h.ImagesGenerations,
				)
				if response.Code != 200 || executor.sourceFormat != "openai-image" || executor.requestPath != imagesGenerationsPath || !gjson.GetBytes(
					response.Body.Bytes(), "data.0.artifact_id",
				).Exists() {
					t.Fatalf(
						"route status=%d response=%s captured=%+v", response.Code, response.Body.String(), executor,
					)
				}
				if test.provider == "meta" {
					if !gjson.GetBytes(
						executor.payload, "tool_enablement.enable_image_search",
					).Exists() || gjson.GetBytes(
						executor.payload, "tool_enablement.enable_image_search",
					).Bool() || gjson.GetBytes(executor.payload, "user").String() != "opaque-end-user" {
						t.Fatalf("Meta request changed: %s", executor.payload)
					}
				} else if gjson.GetBytes(
					executor.payload, "complexity",
				).String() != "low" || gjson.GetBytes(
					executor.payload, "safety_identifier",
				).String() != "opaque-end-user" {
					t.Fatalf("OpenAI request changed: %s", executor.payload)
				}
			},
		)
	}
}

func TestImage26SVGHandlerInheritsTopLevelOutputFormat(t *testing.T) {
	data := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`)
	h, _ := latestMediaHandler(
		t, "codex", "gpt-image-2.6-flare",
		[]byte(`{"created":1,"output_format":"svg","data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(data)+`"}]}`),
	)
	store := &svgMediaTestStore{}
	h.SetImageArtifactStore(store)
	response := audit68OriginalPerformImagesEndpointRequest(
		t, imagesGenerationsPath, "application/json",
		strings.NewReader(`{"model":"gpt-image-2.6-flare","prompt":"synthetic","output_format":"svg"}`),
		h.ImagesGenerations,
	)
	if response.Code != 200 || len(store.records) != 1 || store.records[0].MIMEType != "image/svg+xml" || gjson.GetBytes(
		response.Body.Bytes(), "data.0.b64_json",
	).String() != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("SVG response=%s records=%+v", response.Body.String(), store.records)
	}
}

func TestMetaImageEditNumberedMultipartReachesExecutor(t *testing.T) {
	h, executor := latestMediaHandler(
		t, "meta", "muse-image-1.0",
		[]byte(`{"created":1,"data":[{"b64_json":"`+testPNGBase64+`"}],"output_format":"png"}`),
	)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"model": "muse-image-1.0", "prompt": "synthetic", "reasoning_strength": "low",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image[0]", "synthetic.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, "synthetic"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := audit68OriginalPerformImagesEndpointRequest(
		t, imagesEditsPath, writer.FormDataContentType(), &body, h.ImagesEdits,
	)
	if response.Code != 200 || executor.requestPath != imagesEditsPath || !bytes.Contains(
		executor.payload, []byte(`name="image[0]"`),
	) {
		t.Fatalf(
			"numbered edit status=%d response=%s request=%s", response.Code, response.Body.String(), executor.payload,
		)
	}
}

func TestMetaTranscriptionHandlerUsesNativePolicyExecution(t *testing.T) {
	for _, accept := range []string{"application/json", "text/plain", "text/event-stream"} {
		t.Run(
			accept, func(t *testing.T) {
				wire := []byte(`{"transcript":"synthetic","audioDurationMs":1000}`)
				if accept == "text/plain" {
					wire = []byte("synthetic")
				}
				if accept == "text/event-stream" {
					wire = []byte("data: {\"transcript\":\"synthetic\"}\n\n")
				}
				h, executor := latestMediaHandler(t, "meta", "muse-voice-transcribe-1.0", wire)
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				if err := writer.WriteField(
					"request", `{"model":"muse-voice-transcribe-1.0","mode":"DIARIZATION","audioEncoding":"WAV"}`,
				); err != nil {
					t.Fatal(err)
				}
				part, err := writer.CreateFormFile("audio", "synthetic.wav")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = io.WriteString(part, "RIFFsyntheticWAVE"); err != nil {
					t.Fatal(err)
				}
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
				gin.SetMode(gin.TestMode)
				router := gin.New()
				router.POST("/v1/asr/transcribe", h.MetaAudioTranscribe)
				request := httptest.NewRequest(
					http.MethodPost, "/v1/asr/transcribe?sessionId=synthetic", bytes.NewReader(body.Bytes()),
				)
				request.Header.Set("Content-Type", writer.FormDataContentType())
				request.Header.Set("Accept", accept)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 200 || response.Body.String() != string(wire) || executor.sourceFormat != "meta-audio" || executor.options.Query.Get("sessionId") != "synthetic" || !bytes.Equal(
					executor.payload, body.Bytes(),
				) {
					t.Fatalf(
						"transcription status=%d response=%s format=%s opts=%+v", response.Code, response.Body.String(),
						executor.sourceFormat, executor.options,
					)
				}
			},
		)
	}
}

type mediaTransportExecutor struct {
	coreauth.ProviderExecutor
	transport http.RoundTripper
}

func (e *mediaTransportExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.ProviderExecutor.Execute(util.WithRoundTripper(ctx, e.transport), auth, req, opts)
}

func (e *mediaTransportExecutor) ExecuteStream(
	ctx context.Context,
	auth *coreauth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	return e.ProviderExecutor.ExecuteStream(util.WithRoundTripper(ctx, e.transport), auth, req, opts)
}

func TestImage26ResponsesSelectsAPIKeyAmongMixedCodexCredentials(t *testing.T) {
	for _, test := range []struct{ stream, apiKey bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		stream := test.stream
		t.Run(
			fmt.Sprintf("stream=%t/api-key=%t", stream, test.apiKey), func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				executor := &mediaTransportExecutor{ProviderExecutor: runtimeexecutor.NewCodexAutoExecutor(&config.Config{})}
				manager.RegisterExecutor(executor)
				for _, auth := range []*coreauth.Auth{
					{
						ID: t.Name() + "-oauth", Provider: "codex", Status: coreauth.StatusActive,
						Attributes: map[string]string{"auth_kind": "oauth", "priority": "10"},
						Metadata:   map[string]any{"access_token": "synthetic-oauth"},
					},
					{
						ID: t.Name() + "-apikey", Provider: "codex", Status: coreauth.StatusActive,
						Attributes: map[string]string{"auth_kind": "apikey", "api_key": "synthetic-image-key"},
					},
				} {
					if !test.apiKey && auth.AuthKind() == coreauth.AuthKindAPIKey {
						continue
					}
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-6.1-sol"}},
					)
					manager.RefreshSchedulerEntry(auth.ID)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				}
				h := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				router := gin.New()
				router.POST("/v1/responses", h.Responses)
				payload := fmt.Sprintf(
					`{"model":"gpt-6.1-sol","input":"synthetic","stream":%t,"tools":[{"type":"image_generation","model":"gpt-image-2.6-flare"}]}`,
					stream,
				)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				calls := 0
				executor.transport = mediaRegressionTransport(
					func(r *http.Request) (*http.Response, error) {
						calls++
						if r.Header.Get("Authorization") != "Bearer synthetic-image-key" || r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/responses" {
							return nil, fmt.Errorf("unexpected media credential or endpoint: %s", r.URL)
						}
						wire := `{"id":"resp_mixed","object":"response","status":"completed","model":"gpt-6.1-sol","output":[{"type":"image_generation_call","id":"ig_mixed","status":"completed","result":"QUFB"}]}`
						mime := "application/json"
						if stream {
							wire = "data: " + `{"type":"response.completed","response":` + wire + "}\n\n"
							mime = "text/event-stream"
						}
						return &http.Response{
							StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {mime}},
							Body: io.NopCloser(strings.NewReader(wire)),
						}, nil
					},
				)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if !test.apiKey {
					if response.Code != http.StatusServiceUnavailable || calls != 0 || !strings.Contains(
						response.Body.String(), "API-key",
					) {
						t.Fatalf(
							"missing API key response=%d calls=%d body=%s", response.Code, calls, response.Body.String(),
						)
					}
					return
				}
				if response.Code != 200 || calls != 1 || !bytes.Contains(response.Body.Bytes(), []byte("QUFB")) {
					t.Fatalf(
						"mixed credential media response=%d calls=%d body=%s", response.Code, calls,
						response.Body.String(),
					)
				}
			},
		)
	}
}

func TestGrokVideo15LiteCreatesAndRetrievesWithExactModel(t *testing.T) {
	capture := &videoAuthCaptureExecutor{requestID: "synthetic-lite"}
	handler := newVideoAuthBindingTestHandler(t, capture)
	for _, id := range []string{"synthetic-lite-auth-a", "synthetic-lite-auth-b"} {
		registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: xaiVideos15LiteModel}})
		handler.AuthManager.RefreshSchedulerEntry(id)
	}
	created := performVideosEndpointRequest(
		t, http.MethodPost, openAIVideosPath, "application/json",
		strings.NewReader(`{"model":"grok-imagine-video-1.5-lite","prompt":"synthetic","seconds":"15","resolution":"1080p","image":{"url":"data:image/png;base64,AA=="}}`),
		handler.VideosCreate,
	)
	if created.Code != 200 || gjson.GetBytes(created.Body.Bytes(), "model").String() != xaiVideos15LiteModel {
		t.Fatalf("Lite create: %d %s", created.Code, created.Body.String())
	}
	retrieved := performVideosRouteRequest(
		t, http.MethodGet, openAIVideosPath+"/:video_id", openAIVideosPath+"/synthetic-lite", "", nil,
		handler.VideosRetrieve,
	)
	if retrieved.Code != 200 {
		t.Fatalf("Lite retrieve: %d %s", retrieved.Code, retrieved.Body.String())
	}
	if ids := capture.AuthIDs(); len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("Lite polling credential changed: %v", ids)
	}
	for _, model := range capture.models {
		if model != xaiVideos15LiteModel {
			t.Fatalf("Lite converted to another model: %s", model)
		}
	}
	body, meta, err := buildXAIVideosCreateRequest(
		[]byte(`{"prompt":"synthetic","seconds":"15","resolution":"1080p"}`), xaiVideos15LiteModel,
	)
	if err != nil || gjson.GetBytes(body, "model").String() != xaiVideos15LiteModel || gjson.GetBytes(
		body, "resolution",
	).String() != "1080p" || meta.RoutingModel != xaiVideos15LiteModel {
		t.Fatalf("Lite native payload=%s meta=%+v error=%v", body, meta, err)
	}
}
