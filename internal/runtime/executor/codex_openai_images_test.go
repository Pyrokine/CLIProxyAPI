package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newCodexOpenAIImageTestAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": serverURL,
			"api_key":  "codex-token",
		},
	}
}

func codexOpenAIImageTestOptions(path string, stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(codexOpenAIImageSourceFormat),
		Stream:       stream,
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: path,
		},
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	ginCtx.Request = req
	return util.WithGinContext(context.Background(), ginCtx)
}

func TestCodexExecutorWrappedOpenAIImageAcceptsResponseDone(t *testing.T) {
	responseDoneFrame := []byte(
		"event: response.done\n" +
			"data: {\"type\":\"response.done\",\"response\":{\"created_at\":1713833628," +
			"\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\",\"output_format\":\"png\"}]}}\n\n",
	)
	request := cliproxyexecutor.Request{
		Model:   "gpt-image-1",
		Payload: []byte(`{"model":"gpt-image-1","prompt":"A cute baby sea otter"}`),
	}

	t.Run(
		"non-stream", func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						gotPath = r.URL.Path
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(responseDoneFrame)
					},
				),
			)
			defer server.Close()

			executor := &codexExecutor{cfg: &config.Config{}}
			resp, errExecute := executor.Execute(
				context.Background(),
				newCodexOpenAIImageTestAuth(server.URL),
				request,
				codexOpenAIImageTestOptions(codexImagesGenerationsPath, false),
			)
			if errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			if gotPath != "/responses" {
				t.Fatalf("path = %q, want /responses", gotPath)
			}
			if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "AA==" {
				t.Fatalf("data.0.b64_json = %q, want AA==; payload=%s", got, string(resp.Payload))
			}
		},
	)

	t.Run(
		"stream", func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						gotPath = r.URL.Path
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(responseDoneFrame)
					},
				),
			)
			defer server.Close()

			executor := &codexExecutor{cfg: &config.Config{}}
			stream, errStream := executor.ExecuteStream(
				context.Background(),
				newCodexOpenAIImageTestAuth(server.URL),
				request,
				codexOpenAIImageTestOptions(codexImagesGenerationsPath, true),
			)
			if errStream != nil {
				t.Fatalf("ExecuteStream() error = %v", errStream)
			}
			var combined bytes.Buffer
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
				combined.Write(chunk.Payload)
			}
			if gotPath != "/responses" {
				t.Fatalf("path = %q, want /responses", gotPath)
			}
			eventData, hasData, valid := sseDataPayload(combined.Bytes())
			if !hasData || !valid {
				t.Fatalf("invalid completed image SSE frame: %q", combined.String())
			}
			if got := gjson.GetBytes(eventData, "type").String(); got != "image_generation.completed" {
				t.Fatalf("event type = %q, want image_generation.completed; frame=%q", got, combined.String())
			}
			if got := gjson.GetBytes(eventData, "b64_json").String(); got != "AA==" {
				t.Fatalf("b64_json = %q, want AA==; frame=%q", got, combined.String())
			}
		},
	)
}

func TestCodexExecutorWrappedOpenAIImageUsesExplicitDynamicHeaderSource(t *testing.T) {
	responseDoneFrame := []byte(
		"event: response.done\n" +
			"data: {\"type\":\"response.done\",\"response\":{\"created_at\":1713833628," +
			"\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\",\"output_format\":\"png\"}]}}\n\n",
	)
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var gotHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(responseDoneFrame)
			}))
			defer server.Close()

			auth := newCodexOpenAIImageTestAuth(server.URL)
			auth.Attributes["header:X-Copied"] = "$x-client-value"
			auth.Attributes["header:X-CPA-Session"] = "$CPA-SESSION-ID"
			auth.Attributes["header:Authorization"] = "must-not-replace"
			opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
			opts.Headers = http.Header{"X-Client-Value": []string{"from-options"}}
			ctx := util.WithSessionID(context.Background(), "image-session")
			executor := &codexExecutor{cfg: &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}}
			request := cliproxyexecutor.Request{
				Model:   "gpt-image-1",
				Payload: []byte(`{"model":"gpt-image-1","prompt":"draw"}`),
			}

			if stream {
				result, errStream := executor.ExecuteStream(ctx, auth, request, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
				}
			} else if _, errExecute := executor.Execute(ctx, auth, request, opts); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}

			if got := gotHeaders.Get("X-Copied"); got != "from-options" {
				t.Fatalf("X-Copied = %q, want from-options", got)
			}
			if got := gotHeaders.Get("X-CPA-Session"); got != "image-session" {
				t.Fatalf("X-CPA-Session = %q, want image-session", got)
			}
			if got := gotHeaders.Get("Authorization"); got != "Bearer codex-token" {
				t.Fatalf("Authorization = %q, want Bearer codex-token", got)
			}
		})
	}
}

func TestCodexOpenAIImage25ModelsPreserveDirectAndResponsesToolSelection(t *testing.T) {
	for _, model := range []string{
		"gpt-image-2.5-flare",
		"gpt-image-2.5-flare-2026-09-08",
		"gpt-image-2.5-sunburst",
		"gpt-image-2.5-sunburst-2026-09-08",
	} {
		t.Run(
			model, func(t *testing.T) {
				payload := []byte(`{"model":"` + model + `","prompt":"draw","quality":"xhigh"}`)
				req := cliproxyexecutor.Request{Model: "codex/" + model, Payload: payload}
				if directModel := codexDirectOpenAIImageRouteModel(req, codexOpenAIImageTestOptions(codexImagesGenerationsPath, false)); directModel != model {
					t.Fatalf("direct model = %q, want %q", directModel, model)
				}
				prepared, errPrepare := codexPrepareOpenAIImageGenerationJSON(payload, model)
				if errPrepare != nil {
					t.Fatalf("codexPrepareOpenAIImageGenerationJSON() error = %v", errPrepare)
				}
				if toolModel := gjson.GetBytes(prepared.Body, "tools.0.model").String(); toolModel != model {
					t.Fatalf("tool model = %q, want %q; body=%s", toolModel, model, prepared.Body)
				}
				if toolModel := codexImageGenerationToolModel(prepared.Body); toolModel != model {
					t.Fatalf("usage tool model = %q, want %q", toolModel, model)
				}
			},
		)
	}

	if model := codexDirectOpenAIImageRouteModel(
		cliproxyexecutor.Request{Model: "gpt-image-2.5", Payload: []byte(`{"model":"gpt-image-2.5"}`)},
		codexOpenAIImageTestOptions(codexImagesGenerationsPath, false),
	); model != "" {
		t.Fatalf("plain gpt-image-2.5 alias classified as direct model %q", model)
	}
}

func TestCodexOpenAIImage25PreservesSupportedQualityValues(t *testing.T) {
	for _, quality := range []string{"low", "medium", "high", "xhigh", "max", "auto"} {
		t.Run(
			quality, func(t *testing.T) {
				payload := []byte(
					`{"model":"gpt-image-2.5-flare","prompt":"draw","quality":"` + quality + `"}`,
				)
				prepared, errPrepare := codexPrepareOpenAIImageGenerationJSON(payload, "gpt-image-2.5-flare")
				if errPrepare != nil {
					t.Fatalf("codexPrepareOpenAIImageGenerationJSON() error = %v", errPrepare)
				}
				if got := gjson.GetBytes(prepared.Body, "tools.0.quality").String(); got != quality {
					t.Fatalf("quality = %q, want %q; body=%s", got, quality, prepared.Body)
				}
			},
		)
	}
}

func TestCodexDirectImageUsesResolvedUpstreamModel(t *testing.T) {
	cases := []struct {
		name       string
		resolved   string
		configured string
		want       string
	}{
		{name: "configured prefix", resolved: "openai/gpt-image-2", configured: "openai/gpt-image-2", want: "openai/gpt-image-2"},
		{name: "configured alias", resolved: "openai/gpt-image-2-cpr", configured: "openai/gpt-image-2-cpr", want: "openai/gpt-image-2-cpr"},
		{name: "configured case", resolved: "OpenAI/GPT-Image-2", configured: "OpenAI/GPT-Image-2", want: "OpenAI/GPT-Image-2"},
		{name: "legacy routing prefix", resolved: "codex/gpt-image-2(high)", want: "gpt-image-2"},
		{name: "unconfigured alias", resolved: "openai/gpt-image-2-cpr", want: "openai/gpt-image-2-cpr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				name := "non-stream"
				if stream {
					name = "stream"
				}
				t.Run(name, func(t *testing.T) {
					for _, multipartBody := range []bool{false, true} {
						format := "json"
						if multipartBody {
							format = "multipart"
						}
						t.Run(format, func(t *testing.T) {
							var gotModel, gotPath string
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								gotPath = r.URL.Path
								body, errRead := io.ReadAll(r.Body)
								if errRead != nil {
									t.Errorf("read request body: %v", errRead)
									return
								}
								gotModel = gjson.GetBytes(body, "model").String()
								if stream {
									w.Header().Set("Content-Type", "text/event-stream")
									_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")
								} else {
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, `{"created":1713833628,"data":[{"b64_json":"AA=="}]}`)
								}
							}))
							defer server.Close()
							cfg := &config.Config{}
							if tc.configured != "" {
								cfg.CodexKey = []config.CodexKey{{
									APIKey: "codex-token", BaseURL: server.URL,
									Models: []config.CodexModel{{Name: tc.configured, Alias: "gpt-image-2"}},
								}}
							}
							path := codexImagesGenerationsPath
							payload := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
							opts := codexOpenAIImageTestOptions(path, stream)
							opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = "gpt-image-2"
							if multipartBody {
								path = codexImagesEditsPath
								opts = codexOpenAIImageTestOptions(path, stream)
								opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = "gpt-image-2"
								var form bytes.Buffer
								writer := multipart.NewWriter(&form)
								if errWrite := writer.WriteField("model", "gpt-image-2"); errWrite != nil {
									t.Fatal(errWrite)
								}
								if errWrite := writer.WriteField("prompt", "draw"); errWrite != nil {
									t.Fatal(errWrite)
								}
								if errClose := writer.Close(); errClose != nil {
									t.Fatal(errClose)
								}
								payload = form.Bytes()
								opts.Headers = http.Header{"Content-Type": []string{writer.FormDataContentType()}}
							}
							request := cliproxyexecutor.Request{Model: tc.resolved, Payload: payload}
							correctPath := "/images/generations"
							if multipartBody {
								correctPath = "/images/edits"
							}
							executor := &codexExecutor{cfg: cfg}
							auth := newCodexOpenAIImageTestAuth(server.URL)
							if tc.configured != "" {
								auth.Attributes[cliproxyauth.AttributeConfigIndex] = "0"
							}
							if stream {
								result, errStream := executor.ExecuteStream(context.Background(), auth, request, opts)
								if errStream != nil {
									t.Fatal(errStream)
								}
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
								}
							} else if _, errExecute := executor.Execute(context.Background(), auth, request, opts); errExecute != nil {
								t.Fatal(errExecute)
							}
							if gotPath != correctPath || gotModel != tc.want {
								t.Errorf("upstream path = %q, model = %q; want path %q, model %q", gotPath, gotModel, correctPath, tc.want)
							}
						})
					}
				})
			}
		})
	}
}

func TestCodexDirectImageAppliesPayloadOverrides(t *testing.T) {
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			mode := "nonstream"
			if stream {
				mode = "stream"
			}
			t.Run(path+"/"+mode, func(t *testing.T) {
				var gotBody []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var errRead error
					gotBody, errRead = io.ReadAll(r.Body)
					if errRead != nil {
						t.Errorf("read body: %v", errRead)
						return
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"data":[{"b64_json":"AA=="}]}`)
					}
				}))
				defer server.Close()

				payload := []byte(`{"model":"gpt-image-2.5-flare","prompt":"draw","quality":"low"}`)
				opts := codexOpenAIImageTestOptions(path, stream)
				if path == codexImagesEditsPath {
					var form bytes.Buffer
					writer := multipart.NewWriter(&form)
					for key, value := range map[string]string{"model": "gpt-image-2.5-flare", "prompt": "edit", "quality": "low"} {
						if errWrite := writer.WriteField(key, value); errWrite != nil {
							t.Fatal(errWrite)
						}
					}
					if errClose := writer.Close(); errClose != nil {
						t.Fatal(errClose)
					}
					payload = form.Bytes()
					opts.Headers = http.Header{"Content-Type": []string{writer.FormDataContentType()}}
				}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "gpt-image-2.5-flare", Protocol: "openai"}},
					Params: map[string]any{"quality": "high"},
				}}}}
				executor := &codexExecutor{cfg: cfg}
				request := cliproxyexecutor.Request{Model: "gpt-image-2.5-flare", Payload: payload}
				if stream {
					result, errStream := executor.ExecuteStream(context.Background(), newCodexOpenAIImageTestAuth(server.URL), request, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, errExecute := executor.Execute(context.Background(), newCodexOpenAIImageTestAuth(server.URL), request, opts); errExecute != nil {
					t.Fatal(errExecute)
				}
				if got := gjson.GetBytes(gotBody, "quality").String(); got != "high" {
					t.Fatalf("upstream quality = %q, want high; body=%s", got, gotBody)
				}
			})
		}
	}
}

func TestCodexDirectImageConfigRouteIdentity(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "first", BaseURL: "https://codex.example", Models: []config.CodexModel{{Name: "tenant/gpt-image-2"}},
	}}}
	executor := &codexExecutor{cfg: cfg}
	request := cliproxyexecutor.Request{Model: "tenant/gpt-image-2", Payload: []byte(`{"model":"gpt-image-2"}`)}
	opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, false)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key": "first", "base_url": "https://codex.example",
	}}
	if got := executor.directOpenAIImageModel(auth, request, opts); got != "tenant/gpt-image-2" {
		t.Fatalf("unique credential mapping = %q", got)
	}
	auth.Attributes[cliproxyauth.AttributeConfigIndex] = "0"
	if got := executor.directOpenAIImageModel(auth, request, opts); got != "tenant/gpt-image-2" {
		t.Fatalf("indexed mapping = %q", got)
	}
	auth.Attributes["api_key"] = "other"
	if got := executor.directOpenAIImageModel(auth, request, opts); got != "gpt-image-2" {
		t.Fatalf("mismatched credential = %q", got)
	}
}

func TestCodexExecutorDirectOpenAIImageGenerationUsesImagesEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotAccept string
	var gotUA string
	var gotVersion string
	var gotOriginator string
	var gotBody []byte
	upstreamBody := []byte(`{"created":1713833628,"data":[{"b64_json":"AA=="}],"usage":{"total_tokens":100,"input_tokens":50,"output_tokens":50}}`)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotAccept = r.Header.Get("Accept")
				gotUA = r.Header.Get("User-Agent")
				gotVersion = r.Header.Get("Version")
				gotOriginator = r.Header.Get("Originator")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(upstreamBody)
			},
		),
	)
	defer server.Close()

	ctx := contextWithGinHeaders(
		map[string]string{
			"User-Agent":            "codex_cli_rs/0.135.0",
			"Version":               "0.135.0",
			"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
			"X-Client-Request-Id":   "client-request-1",
			"Originator":            "Codex Desktop",
		},
	)
	executor := &codexExecutor{
		cfg: &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}},
	}
	resp, errExecute := executor.Execute(
		ctx, newCodexOpenAIImageTestAuth(server.URL), cliproxyexecutor.Request{
			Model: "codex/gpt-image-1.5",
			Payload: []byte(
				`{"model":"codex/gpt-image-1.5","prompt":"A cute baby sea otter","n":1,` +
					`"size":"1024x1024","quality":"high","background":"opaque","output_format":"jpeg",` +
					`"output_compression":70,"moderation":"low","extra":{"preserve":true},"stream":false}`,
			),
		}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, false),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if gotPath != "/images/generations" {
		t.Fatalf("path = %q, want /images/generations", gotPath)
	}
	if gotAuth != "Bearer codex-token" {
		t.Fatalf("Authorization = %q, want Bearer codex-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if gotUA != codexUserAgent {
		t.Fatalf("User-Agent = %q, want codex default %q", gotUA, codexUserAgent)
	}
	if gotVersion != "0.135.0" {
		t.Fatalf("Version = %q, want %q", gotVersion, "0.135.0")
	}
	if gotOriginator != "Codex Desktop" {
		t.Fatalf("Originator = %q", gotOriginator)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-image-1.5" {
		t.Fatalf("model = %q, want gpt-image-1.5; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "extra.preserve").Bool(); !got {
		t.Fatalf("extra.preserve missing from body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "output_compression").Int(); got != 70 {
		t.Fatalf("output_compression = %d, want 70; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream should be removed for non-stream execution: %s", string(gotBody))
	}
	if !bytes.Equal(resp.Payload, upstreamBody) {
		t.Fatalf("payload = %s, want %s", string(resp.Payload), string(upstreamBody))
	}
}

func TestCodexExecutorDirectOpenAIImageGenerationStreamsImagesEndpoint(t *testing.T) {
	var gotPath string
	var gotAccept string
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAccept = r.Header.Get("Accept")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte("event: image_generation.partial_image\n" +
						"data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"AA==\"}\n\n"),
				)
				_, _ = w.Write(
					[]byte("event: image_generation.completed\n" +
						"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"BB==\"}\n\n"),
				)
			},
		),
	)
	defer server.Close()

	executor := &codexExecutor{cfg: &config.Config{}}
	stream, errStream := executor.ExecuteStream(
		context.Background(), newCodexOpenAIImageTestAuth(server.URL), cliproxyexecutor.Request{
			Model: "gpt-image-2.5-sunburst",
			Payload: []byte(
				`{"model":"gpt-image-2.5-sunburst","prompt":"A cute baby sea otter","quality":"max","partial_images":2}`,
			),
		}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, true),
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}

	var combined bytes.Buffer
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if gotPath != "/images/generations" {
		t.Fatalf("path = %q, want /images/generations", gotPath)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", gotAccept)
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("stream flag missing from upstream body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "partial_images").Int(); got != 2 {
		t.Fatalf("partial_images = %d, want 2; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-image-2.5-sunburst" {
		t.Fatalf("model = %q, want gpt-image-2.5-sunburst; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "quality").String(); got != "max" {
		t.Fatalf("quality = %q, want max; body=%s", got, string(gotBody))
	}
	out := combined.String()
	if !strings.Contains(out, "event: image_generation.partial_image") ||
		!strings.Contains(out, "event: image_generation.completed") {
		t.Fatalf("stream output missing image events: %q", out)
	}
}

func TestCodexExecutorDirectOpenAIImageEditUsesImagesEditEndpointForJSON(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1713833628,"data":[{"b64_json":"AA=="}],"usage":{"total_tokens":10}}`))
			},
		),
	)
	defer server.Close()

	executor := &codexExecutor{cfg: &config.Config{}}
	_, errExecute := executor.Execute(
		context.Background(), newCodexOpenAIImageTestAuth(server.URL), cliproxyexecutor.Request{
			Model: "gpt-image-2",
			Payload: []byte(
				`{"model":"gpt-image-2","prompt":"Replace the background",` +
					`"images":[{"file_id":"file-abc123"}],"mask":{"file_id":"file-mask123"},` +
					`"size":"1024x1024","quality":"high","output_format":"png",` +
					`"output_compression":100,"stream":false}`,
			),
		}, codexOpenAIImageTestOptions(codexImagesEditsPath, false),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if gotPath != "/images/edits" {
		t.Fatalf("path = %q, want /images/edits", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "images.0.file_id").String(); got != "file-abc123" {
		t.Fatalf("images.0.file_id = %q, want file-abc123; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "mask.file_id").String(); got != "file-mask123" {
		t.Fatalf("mask.file_id = %q, want file-mask123; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream should be removed for non-stream execution: %s", string(gotBody))
	}
}

func TestCodexExecutorDirectOpenAIImageEditUsesImagesEditEndpointForMultipart(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "codex/gpt-image-1.5"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "Create a lovely gift basket"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	if errWrite := writer.WriteField("output_format", "webp"); errWrite != nil {
		t.Fatalf("write output_format field: %v", errWrite)
	}
	if errWrite := writer.WriteField("n", "2"); errWrite != nil {
		t.Fatalf("write n field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	imagePart, errCreate := writer.CreateFormFile("image[]", "source.png")
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := imagePart.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image data: %v", errWrite)
	}
	maskPart, errCreateMask := writer.CreateFormFile("mask", "mask.png")
	if errCreateMask != nil {
		t.Fatalf("create mask field: %v", errCreateMask)
	}
	if _, errWrite := maskPart.Write([]byte("mask-data")); errWrite != nil {
		t.Fatalf("write mask data: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	var gotPath string
	var gotContentType string
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotContentType = r.Header.Get("Content-Type")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1713833628,"data":[{"b64_json":"AA=="}]}`))
			},
		),
	)
	defer server.Close()

	opts := codexOpenAIImageTestOptions(codexImagesEditsPath, false)
	opts.Headers = http.Header{"Content-Type": []string{writer.FormDataContentType()}}
	executor := &codexExecutor{cfg: &config.Config{}}
	_, errExecute := executor.Execute(
		context.Background(), newCodexOpenAIImageTestAuth(server.URL), cliproxyexecutor.Request{
			Model:   "codex/gpt-image-1.5",
			Payload: body.Bytes(),
		}, opts,
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if gotPath != "/images/edits" {
		t.Fatalf("path = %q, want /images/edits", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if !json.Valid(gotBody) {
		t.Fatalf("body is not valid JSON: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-image-1.5" {
		t.Fatalf("model = %q, want gpt-image-1.5; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "prompt").String(); got != "Create a lovely gift basket" {
		t.Fatalf("prompt = %q", got)
	}
	if got := gjson.GetBytes(gotBody, "output_format").String(); got != "webp" {
		t.Fatalf("output_format = %q, want webp; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream should be removed for non-stream execution: %s", string(gotBody))
	}
	imageURL := gjson.GetBytes(gotBody, "images.0.image_url").String()
	if !strings.Contains(imageURL, ";base64,cG5nLWRhdGE=") {
		t.Fatalf("images.0.image_url = %q, want png-data data URL; body=%s", imageURL, string(gotBody))
	}
	maskURL := gjson.GetBytes(gotBody, "mask.image_url").String()
	if !strings.Contains(maskURL, ";base64,bWFzay1kYXRh") {
		t.Fatalf("mask.image_url = %q, want mask-data data URL; body=%s", maskURL, string(gotBody))
	}
}

func TestCodexImageDefaultsUseOriginalClientFields(t *testing.T) {
	for _, tc := range []struct {
		name, original, wantQuality string
		multipart                   bool
	}{
		{"json-control", "", "low", false},
		{"multipart-explicit-quality", "", "low", true},
		{"json-omitted-before-preparation", `{"model":"gpt-image-2.5-flare","prompt":"edit"}`, "high", false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				outgoing := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					outgoing <- body
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"data":[{"b64_json":"AA=="}]}`)
					}
				}))
				defer server.Close()
				payload := []byte(`{"model":"gpt-image-2.5-flare","prompt":"edit","quality":"low"}`)
				opts := codexOpenAIImageTestOptions(codexImagesEditsPath, stream)
				if tc.multipart {
					var body bytes.Buffer
					writer := multipart.NewWriter(&body)
					for key, value := range map[string]string{"model": "gpt-image-2.5-flare", "prompt": "edit", "quality": "low"} {
						if err := writer.WriteField(key, value); err != nil {
							t.Fatal(err)
						}
					}
					part, err := writer.CreateFormFile("image[]", "input.png")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = part.Write([]byte{0, 255, 13, 10}); err != nil {
						t.Fatal(err)
					}
					if err = writer.Close(); err != nil {
						t.Fatal(err)
					}
					payload = body.Bytes()
					opts.Headers = http.Header{"Content-Type": {writer.FormDataContentType()}}
				}
				if tc.original != "" {
					opts.OriginalRequest = []byte(tc.original)
				}
				before := bytes.Clone(payload)
				cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "gpt-image-2.5-flare", Protocol: "openai"}},
					Params: map[string]any{"quality": "high"},
				}}}}
				executor := &codexExecutor{cfg: cfg}
				request := cliproxyexecutor.Request{Model: "gpt-image-2.5-flare", Payload: payload}
				if stream {
					result, err := executor.ExecuteStream(context.Background(), newCodexOpenAIImageTestAuth(server.URL), request, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(context.Background(), newCodexOpenAIImageTestAuth(server.URL), request, opts); err != nil {
					t.Fatal(err)
				}
				body := <-outgoing
				if got := gjson.GetBytes(body, "quality").String(); got != tc.wantQuality {
					t.Errorf("outgoing quality=%q, want %q from original client fields", got, tc.wantQuality)
				}
				if tc.multipart && gjson.GetBytes(body, "images.0.image_url").String() != "data:application/octet-stream;base64,AP8NCg==" {
					t.Errorf("uploaded image changed: %s", body)
				}
				if !bytes.Equal(before, payload) {
					t.Error("caller payload was mutated")
				}
			})
		}
	}
}

func TestCodexImageOriginalPayloadErrorsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct{ name, original, contentType string }{
		{"missing-boundary", "not json", "application/json"},
		{"malformed-multipart", "--fixture\r\nContent-Disposition: form-data; name=quality\r\n", "multipart/form-data; boundary=fixture"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected dispatch")
				}))
				executor := &codexExecutor{cfg: &config.Config{}}
				opts := codexOpenAIImageTestOptions(codexImagesEditsPath, stream)
				opts.OriginalRequest = []byte(tc.original)
				opts.Headers = http.Header{"Content-Type": {tc.contentType}}
				req := cliproxyexecutor.Request{Model: "gpt-image-2.5-flare", Payload: []byte(`{"model":"gpt-image-2.5-flare","prompt":"edit"}`)}
				var err error
				if stream {
					_, err = executor.ExecuteStream(ctx, newCodexOpenAIImageTestAuth("https://fixture.invalid"), req, opts)
				} else {
					_, err = executor.Execute(ctx, newCodexOpenAIImageTestAuth("https://fixture.invalid"), req, opts)
				}
				var status statusErr
				if !errors.As(err, &status) || status.code != http.StatusBadRequest || !status.requestScoped || status.dispatchPhase != cliproxyexecutor.DispatchPhaseNotSent || calls != 0 {
					t.Errorf("error=%v dispatches=%d, want request-scoped 400/NotSent without dispatch", err, calls)
				}
			})
		}
	}
}

func TestCodexImageClosingMarkerMustBeRealBoundary(t *testing.T) {
	for _, tc := range []struct{ name, original, contentType string }{
		{"missing-boundary-control", "not json", "application/json"},
		{"marker-in-incomplete-header", "--fixture\r\n--fixture--: synthetic-header\r\n", "multipart/form-data; boundary=fixture"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected dispatch")
				}))
				executor := &codexExecutor{cfg: &config.Config{}}
				opts := codexOpenAIImageTestOptions(codexImagesEditsPath, stream)
				opts.OriginalRequest = []byte(tc.original)
				opts.Headers = http.Header{"Content-Type": {tc.contentType}}
				req := cliproxyexecutor.Request{Model: "gpt-image-2.5-flare", Payload: []byte(`{"model":"gpt-image-2.5-flare","prompt":"edit"}`)}
				var err error
				if stream {
					_, err = executor.ExecuteStream(ctx, newCodexOpenAIImageTestAuth("https://fixture.invalid"), req, opts)
				} else {
					_, err = executor.Execute(ctx, newCodexOpenAIImageTestAuth("https://fixture.invalid"), req, opts)
				}
				var status statusErr
				if !errors.As(err, &status) || status.code != http.StatusBadRequest || !status.requestScoped || status.dispatchPhase != cliproxyexecutor.DispatchPhaseNotSent || calls != 0 {
					t.Errorf("error=%v dispatches=%d, want request-scoped 400/NotSent without dispatch", err, calls)
				}
			})
		}
	}
}

func TestCodexImageOriginalMultipartClosingLines(t *testing.T) {
	for _, tc := range []struct{ name, newline, suffix string }{
		{"crlf-eof", "\r\n", ""},
		{"crlf-final-line", "\r\n", "\r\n"},
		{"crlf-whitespace-and-epilogue", "\r\n", " \t\r\nsynthetic epilogue"},
		{"lf-final-line", "\n", "\n"},
		{"lf-epilogue", "\n", "\nsynthetic epilogue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-image-2.5-flare","quality":"low"}`)
			httpReq, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/images/edits", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			opts := codexOpenAIImageTestOptions(codexImagesEditsPath, false)
			opts.Headers = http.Header{"Content-Type": {"multipart/form-data; boundary=fixture"}}
			opts.OriginalRequest = []byte("--fixture" + tc.newline + "Content-Disposition: form-data; name=\"quality\"" + tc.newline + tc.newline + "low" + tc.newline + "--fixture--" + tc.suffix)
			executor := &codexExecutor{cfg: &config.Config{}}
			actual, err := executor.applyDirectOpenAIImagePayloadConfig(httpReq, body, cliproxyexecutor.Request{Model: "gpt-image-2.5-flare", Payload: body}, opts, "gpt-image-2.5-flare")
			if err != nil || !bytes.Equal(actual, body) {
				t.Fatalf("valid closing line was rejected or changed: body=%s error=%v", actual, err)
			}
		})
	}
}

func TestCodexWrappedImageResponseModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			for _, terminal := range []string{"response.completed", "response.done"} {
				t.Run(terminal, func(t *testing.T) {
					for _, tc := range []struct {
						name, early, final, want string
					}{
						{name: "no-model"},
						{name: "early-model-only", early: "image-core-first", want: "image-core-first"},
						{name: "terminal-model", early: "image-core-first", final: "image-core-final", want: "image-core-final"},
					} {
						t.Run(tc.name, func(t *testing.T) {
							calls := 0
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								calls++
								if r.URL.Path != "/responses" {
									t.Errorf("path=%q, want /responses", r.URL.Path)
								}
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"model\":%q}}\n\n", tc.early)
								_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"response\":{\"model\":%q,\"created_at\":1713833628,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\",\"output_format\":\"png\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n", terminal, tc.final)
							}))
							defer server.Close()
							ctx, finish := xaiOutcomeContext(t)
							executor := newCodexExecutor(&config.Config{})
							request := cliproxyexecutor.Request{Model: "gpt-image-1", Payload: []byte(`{"model":"gpt-image-1","prompt":"synthetic image"}`)}
							opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
							var output []byte
							if stream {
								result, err := executor.ExecuteStream(ctx, newCodexOpenAIImageTestAuth(server.URL), request, opts)
								if err != nil {
									t.Fatal(err)
								}
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
									output = append(output, chunk.Payload...)
								}
							} else {
								response, err := executor.Execute(ctx, newCodexOpenAIImageTestAuth(server.URL), request, opts)
								if err != nil {
									t.Fatal(err)
								}
								output = response.Payload
							}
							records := finish()
							if calls != 1 || !bytes.Contains(output, []byte("AA==")) || len(records) != 1 {
								t.Fatalf("requests=%d output=%s records=%+v", calls, output, records)
							}
							record := records[0]
							if record.Failed || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
								t.Fatalf("image outcome changed: %+v", record)
							}
							if record.Model != executor.resolveGPTImage2BaseModel() || record.ResponseModel != tc.want {
								t.Fatalf("usage model=%q responseModel=%q, want %q/%q", record.Model, record.ResponseModel, executor.resolveGPTImage2BaseModel(), tc.want)
							}
						})
					}
				})
			}
		})
	}
}

func TestCodexWrappedImageFinalPayload(t *testing.T) {
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"none", "override", "filter"} {
				for _, enabled := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/enabled=%t", path, stream, mode, enabled), func(t *testing.T) {
						models := []config.PayloadModelRule{{Name: "*", Protocol: "codex", FromProtocol: codexOpenAIImageSourceFormat, Headers: map[string]string{"X-Image-Payload": "enabled"}}}
						cfg := &config.Config{}
						if mode == "override" {
							cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{
								"model": "configured-image-core", "service_tier": "configured-tier", "prompt_cache_key": "configured-cache",
							}}}
						}
						if mode == "filter" {
							cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"model", "service_tier", "prompt_cache_key"}}}
						}
						calls := 0
						var sent []byte
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls++
							var err error
							sent, err = io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							if r.URL.Path != "/responses" {
								t.Errorf("path=%q, want /responses", r.URL.Path)
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"error":{"message":"synthetic request capture"}}`)
						}))
						defer server.Close()
						payload := []byte(`{"model":"gpt-image-1","prompt":"synthetic image","service_tier":"priority","images":[{"image_url":"data:image/png;base64,AA=="}]}`)
						original := bytes.Clone(payload)
						request := cliproxyexecutor.Request{Model: "gpt-image-1", Payload: payload, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "synthetic-image-session"}}
						opts := codexOpenAIImageTestOptions(path, stream)
						opts.Headers = http.Header{"X-Image-Payload": {"disabled"}}
						if enabled {
							opts.Headers.Set("X-Image-Payload", "enabled")
						}
						executor := newCodexExecutor(cfg)
						var executeErr error
						if stream {
							_, executeErr = executor.ExecuteStream(t.Context(), newCodexOpenAIImageTestAuth(server.URL), request, opts)
						} else {
							_, executeErr = executor.Execute(t.Context(), newCodexOpenAIImageTestAuth(server.URL), request, opts)
						}
						if calls != 1 || executeErr == nil || !bytes.Equal(request.Payload, original) {
							t.Fatalf("requests=%d error=%v input=%s", calls, executeErr, request.Payload)
						}
						wantModel, wantTier := executor.resolveGPTImage2BaseModel(), "priority"
						if enabled && mode == "override" {
							wantModel, wantTier = "configured-image-core", "configured-tier"
						}
						if enabled && mode == "filter" {
							wantModel, wantTier = "", ""
						}
						if got := gjson.GetBytes(sent, "model").String(); got != wantModel {
							t.Errorf("final model=%q, want %q; body=%s", got, wantModel, sent)
						}
						if got := gjson.GetBytes(sent, "service_tier").String(); got != wantTier {
							t.Errorf("final service_tier=%q, want %q; body=%s", got, wantTier, sent)
						}
						cache := gjson.GetBytes(sent, "prompt_cache_key")
						if enabled && mode == "override" && cache.String() != "configured-cache" {
							t.Errorf("configured cache changed: %s", sent)
						}
						if enabled && mode == "filter" && cache.Exists() {
							t.Errorf("filtered cache was restored: %s", sent)
						}
						if (!enabled || mode == "none") && cache.String() == "" {
							t.Errorf("control lost automatic cache: %s", sent)
						}
						if !gjson.GetBytes(sent, "stream").Bool() {
							t.Errorf("wrapped image transport lost SSE: %s", sent)
						}
					})
				}
			}
		}
	}
}
