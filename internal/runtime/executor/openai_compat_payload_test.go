package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type openAICompatPayloadRoundTripper func(*http.Request) (*http.Response, error)

func (f openAICompatPayloadRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAICompatImagesPayloadRules(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "image-model", Protocol: "openai"}}
	for _, rule := range []struct {
		name        string
		config      config.PayloadConfig
		wantQuality string
		wantSize    string
		wantUser    bool
	}{
		{"control", config.PayloadConfig{}, "standard", "", true},
		{"default", config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"size": "1024x1024", "quality": "hd"}}}}, "standard", "1024x1024", true},
		{"default_raw", config.PayloadConfig{DefaultRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"size": `"1024x1024"`}}}}, "standard", "1024x1024", true},
		{"override", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"quality": "hd"}}}}, "hd", "", true},
		{"override_raw", config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"quality": `"hd"`}}}}, "hd", "", true},
		{"filter", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"quality", "user"}}}}, "", "", false},
		{"source_and_header_gate", config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "image-model", Protocol: "openai", FromProtocol: "openai-image", Headers: map[string]string{"X-Image-Rule": "enabled"}}}, Params: map[string]any{"quality": "hd"}}}}, "hd", "", true},
	} {
		for _, endpoint := range []string{"/v1/images/generations", "/v1/images/edits"} {
			for _, stream := range []bool{false, true} {
				for _, multipartBody := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream_%t/multipart_%t", rule.name, strings.TrimPrefix(endpoint, "/v1/images/"), stream, multipartBody), func(t *testing.T) {
						payload := []byte(`{"model":"alias-image","prompt":"fixture","quality":"standard","user":"fixture-user"}`)
						contentType := "application/json"
						fileBytes := []byte{0, 1, 2, 255, 0, 13, 10}
						if multipartBody {
							var body bytes.Buffer
							writer := multipart.NewWriter(&body)
							for key, value := range map[string]string{"model": "alias-image", "prompt": "fixture", "quality": "standard", "user": "fixture-user"} {
								if err := writer.WriteField(key, value); err != nil {
									t.Fatal(err)
								}
							}
							for _, value := range []string{"first", "second"} {
								if err := writer.WriteField("tag", value); err != nil {
									t.Fatal(err)
								}
							}
							part, err := writer.CreateFormFile("image", "fixture.bin")
							if err != nil {
								t.Fatal(err)
							}
							if _, err = part.Write(fileBytes); err != nil {
								t.Fatal(err)
							}
							if err = writer.Close(); err != nil {
								t.Fatal(err)
							}
							payload, contentType = body.Bytes(), writer.FormDataContentType()
						}
						calls := 0
						transport := openAICompatPayloadRoundTripper(func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.Host != "example.invalid" || req.URL.Path != endpoint {
								return nil, fmt.Errorf("unexpected URL: %s", req.URL)
							}
							body, err := io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							fields := make(map[string]string)
							if multipartBody {
								_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
								if err != nil {
									return nil, err
								}
								form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(32 << 20)
								if err != nil {
									return nil, err
								}
								defer form.RemoveAll()
								for key, values := range form.Value {
									if len(values) > 0 {
										fields[key] = values[0]
									}
								}
								if !reflect.DeepEqual(form.Value["tag"], []string{"first", "second"}) {
									t.Errorf("repeated fields = %v", form.Value["tag"])
								}
								if len(form.File["image"]) != 1 {
									return nil, fmt.Errorf("image count = %d", len(form.File["image"]))
								}
								file, err := form.File["image"][0].Open()
								if err != nil {
									return nil, err
								}
								gotFile, err := io.ReadAll(file)
								file.Close()
								if err != nil {
									return nil, err
								}
								if !bytes.Equal(gotFile, fileBytes) {
									t.Errorf("file bytes changed: %v", gotFile)
								}
							} else {
								if !gjson.ValidBytes(body) {
									return nil, fmt.Errorf("invalid outgoing JSON: %s", body)
								}
								gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool { fields[key.String()] = value.String(); return true })
							}
							if fields["model"] != "image-model" || fields["prompt"] != "fixture" {
								t.Errorf("model/prompt changed: %v", fields)
							}
							if fields["quality"] != rule.wantQuality || fields["size"] != rule.wantSize {
								t.Errorf("quality/size = %q/%q, want %q/%q", fields["quality"], fields["size"], rule.wantQuality, rule.wantSize)
							}
							if _, ok := fields["user"]; ok != rule.wantUser {
								t.Errorf("user present = %t, want %t", ok, rule.wantUser)
							}
							if stream && fields["stream"] != "true" {
								t.Errorf("stream = %q", fields["stream"])
							}
							if !stream {
								if _, ok := fields["stream"]; ok {
									t.Error("nonstream request retained stream")
								}
							}
							response := `{"data":[{"b64_json":"fixture"}]}`
							if stream {
								response = "data: {\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}\n\n"
							}
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
						})
						ctx := util.WithRoundTripper(context.Background(), http.RoundTripper(transport))
						exec := NewOpenAICompatExecutor("compat", &config.Config{Payload: rule.config})
						auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}}
						req := cliproxyexecutor.Request{Model: "image-model", Payload: payload}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image"), Stream: stream, OriginalRequest: payload, Headers: http.Header{"Content-Type": {contentType}, "X-Image-Rule": {"enabled"}}, Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: endpoint}}
						if stream {
							result, err := exec.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						} else {
							if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
								t.Fatal(err)
							}
						}
						if calls != 1 {
							t.Fatalf("dispatches = %d, want 1", calls)
						}
					})
				}
			}
		}
	}
}

func TestOpenAICompatStreamUsagePayloadRules(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "fixture", Protocol: "openai"}}
	for _, tc := range []struct {
		name        string
		payload     config.PayloadConfig
		wantOptions bool
		wantUsage   bool
		clientValue bool
	}{
		{"automatic", config.PayloadConfig{}, true, true, false},
		{"filter_parent", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"stream_options"}}}}, false, false, true},
		{"filter_child", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"stream_options.include_usage"}}}}, true, false, true},
		{"override_child", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"stream_options.include_usage": false}}}}, true, false, true},
		{"override_parent", config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"stream_options": `{"include_usage":false,"other":1}`}}}}, true, false, true},
		{"unmatched", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "other"}}, Params: []string{"stream_options"}}}}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), http.RoundTripper(openAICompatPayloadRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if got := gjson.GetBytes(body, "stream_options").Exists(); got != tc.wantOptions {
					t.Errorf("stream_options present = %t, want %t; body=%s", got, tc.wantOptions, body)
				}
				if got := gjson.GetBytes(body, "stream_options.include_usage").Bool(); got != tc.wantUsage {
					t.Errorf("include_usage = %t, want %t; body=%s", got, tc.wantUsage, body)
				}
				if tc.name == "filter_child" && gjson.GetBytes(body, "stream_options.include_usage").Exists() {
					t.Errorf("filtered child reappeared: %s", body)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), Request: req}, nil
			})))
			exec := NewOpenAICompatExecutor("compat", &config.Config{Payload: tc.payload})
			req := cliproxyexecutor.Request{Model: "fixture", Payload: []byte(fmt.Sprintf(`{"model":"fixture","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":%t}}`, tc.clientValue))}
			result, err := exec.ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}}, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
			if calls != 1 {
				t.Fatalf("dispatches = %d, want 1", calls)
			}
		})
	}
}

func TestOpenAICompatPayloadRulesRemainFinalForThinking(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "external-model", Protocol: "openai"}}
	for _, tc := range []struct {
		name    string
		payload config.PayloadConfig
		want    string
	}{
		{"control", config.PayloadConfig{}, "high"},
		{"override", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"reasoning_effort": "low"}}}}, "low"},
		{"filter", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"reasoning_effort"}}}}, ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var got []byte
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), openAICompatPayloadRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "compat.invalid" {
						return nil, fmt.Errorf("unexpected synthetic destination: %s", req.URL.Host)
					}
					var err error
					got, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					response := `{"id":"fixture","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
					if stream {
						response = "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}))
				executor := NewOpenAICompatExecutor("compat", &config.Config{Payload: tc.payload})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic", "base_url": "https://compat.invalid/v1"}}
				payload := []byte(`{"model":"external-model","messages":[{"role":"user","content":"hi"}]}`)
				before := bytes.Clone(payload)
				req := cliproxyexecutor.Request{Model: "external-model(high)", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("dispatches=%d, want one", calls)
				}
				if effort := gjson.GetBytes(got, "reasoning_effort").String(); effort != tc.want {
					t.Errorf("final reasoning_effort=%q, want %q from payload rules", effort, tc.want)
				}
				if !bytes.Equal(before, payload) {
					t.Error("caller payload was mutated")
				}
			})
		}
	}
}

func TestOpenAICompatCountUsesFinalConfiguredPayload(t *testing.T) {
	const model = "external-model"
	payload := []byte(`{"model":"external-model","messages":[{"role":"user","content":"hi"}]}`)
	configured := strings.Repeat("configured content ", 40)
	models := []config.PayloadModelRule{{Name: model, Protocol: "openai"}}
	for _, tc := range []struct {
		name, original, expected string
		payload                  config.PayloadConfig
	}{
		{"control", "", string(payload), config.PayloadConfig{}},
		{"override", "", fmt.Sprintf(`{"model":"external-model","messages":[{"role":"user","content":%q}]}`, configured),
			config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"messages.0.content": configured}}}}},
		{"filter", "", `{"model":"external-model","messages":[{"role":"user"}]}`,
			config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"messages.0.content"}}}}},
		{"default-original-omission", `{"model":"external-model","messages":[{"role":"user"}]}`, fmt.Sprintf(`{"model":"external-model","messages":[{"role":"user","content":%q}]}`, configured),
			config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"messages.0.content": configured}}}}},
	} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
			t.Run(tc.name+"/"+format.String(), func(t *testing.T) {
				encoder, err := tokenizerForModel(model)
				if err != nil {
					t.Fatal(err)
				}
				want, err := countOpenAIChatTokens(encoder, []byte(tc.expected))
				if err != nil {
					t.Fatal(err)
				}
				executor := NewOpenAICompatExecutor("compat", &config.Config{Payload: tc.payload})
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: format, OriginalRequest: []byte(tc.original)}
				response, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{Model: model, Payload: payload}, opts)
				if err != nil {
					t.Fatal(err)
				}
				wanted := sdktranslator.TranslateTokenCount(context.Background(), sdktranslator.FormatOpenAI, format, want, buildOpenAIUsageJSON(want))
				if !bytes.Equal(response.Payload, wanted) {
					t.Errorf("count response=%s, want %s for final configured fields", response.Payload, wanted)
				}
			})
		}
	}
}

func TestOpenAICompatFinalPayloadUsesSourceAndHeaderGates(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("matching=%t/stream=%t", enabled, stream), func(t *testing.T) {
				var body []byte
				ctx := util.WithRoundTripper(context.Background(), openAICompatPayloadRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host != "compat.invalid" {
						return nil, fmt.Errorf("unexpected synthetic destination: %s", req.URL.Host)
					}
					var err error
					body, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					response := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
					if stream {
						response = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}))
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "external-model", Protocol: "openai", FromProtocol: "openai", Headers: map[string]string{"X-Rule": "enabled"}}},
					Params: map[string]any{"configured_marker": true},
				}}}}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream, Headers: make(http.Header)}
				if enabled {
					opts.Headers.Set("X-Rule", "enabled")
				}
				executor := NewOpenAICompatExecutor("compat", cfg)
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic", "base_url": "https://compat.invalid/v1"}}
				req := cliproxyexecutor.Request{Model: "external-model", Payload: []byte(`{"model":"external-model","messages":[{"role":"user","content":"hi"}]}`)}
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(body, "configured_marker").Bool(); got != enabled {
					t.Errorf("configured marker=%t, want %t for source/header gate", got, enabled)
				}
			})
		}
	}
}
