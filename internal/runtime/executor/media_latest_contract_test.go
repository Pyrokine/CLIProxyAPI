package executor

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestImage26PublicAPIContracts(t *testing.T) {
	for _, model := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		for _, operation := range []string{"generate", "edit", "responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/%s/stream=%t", model, operation, stream), func(t *testing.T) {
						path := "/v1/images/generations"
						upstreamPath := "/v1/images/generations"
						requestModel := model
						payload := fmt.Sprintf(
							`{"model":%q,"prompt":"synthetic","complexity":"medium","safety_identifier":"test-subject","output_format":"png"}`,
							model,
						)
						if operation == "edit" {
							path, upstreamPath = "/v1/images/edits", "/v1/images/edits"
							payload = fmt.Sprintf(
								`{"model":%q,"prompt":"edit","images":[{"file_id":"source"}],"markup":{"file_id":"markup"},"complexity":"medium","safety_identifier":"test-subject","output_format":"png"}`,
								model,
							)
						}
						opts := codexOpenAIImageTestOptions(path, stream)
						if operation == "responses" {
							upstreamPath, requestModel = "/v1/responses", "gpt-6.1-sol"
							opts.SourceFormat = translator.FormatOpenAIResponse
							payload = fmt.Sprintf(
								`{"model":"gpt-6.1-sol","input":"synthetic","previous_response_id":"resp_previous","safety_identifier":"test-subject","tools":[{"type":"image_generation","model":%q,"complexity":"medium","output_format":"png"}]}`,
								model,
							)
						}
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(r *http.Request) (*http.Response, error) {
									calls++
									body, err := io.ReadAll(r.Body)
									if err != nil {
										return nil, err
									}
									if r.URL.Host != "api.openai.com" || r.URL.Path != upstreamPath || r.Header.Get("Authorization") != "Bearer synthetic-key" {
										t.Fatalf("wrong public channel: %s headers=%v", r.URL, r.Header)
									}
									if gjson.GetBytes(body, "model").String() != requestModel || gjson.GetBytes(
										body, "safety_identifier",
									).String() != "test-subject" || gjson.GetBytes(body, "stream").Bool() != stream {
										t.Fatalf("public image request changed: %s", body)
									}
									if operation == "responses" {
										if gjson.GetBytes(
											body, "tools.0.model",
										).String() != model || gjson.GetBytes(
											body, "tools.0.complexity",
										).String() != "medium" || gjson.GetBytes(
											body, "previous_response_id",
										).String() != "resp_previous" {
											t.Fatalf("Responses tool contract lost: %s", body)
										}
									} else if gjson.GetBytes(body, "complexity").String() != "medium" {
										t.Fatalf("complexity lost: %s", body)
									}
									wire := `{"created":1,"data":[{"b64_json":"AA=="}],"output_format":"png","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
									if operation == "responses" {
										wire = `{"id":"resp_image","object":"response","status":"completed","output":[{"type":"image_generation_call","result":"AA==","output_format":"png"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
									}
									mime := "application/json"
									if stream {
										mime = "text/event-stream"
										if operation == "responses" {
											wire = "data: " + `{"type":"response.completed","response":` + wire + "}\n\n"
										} else {
											wire = "data: " + `{"type":"image_generation.completed","b64_json":"AA=="}` + "\n\n"
										}
									}
									return &http.Response{
										StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {mime}},
										Body: io.NopCloser(strings.NewReader(wire)),
									}, nil
								},
							),
						)
						ctx = core.WithDownstreamWebsocket(ctx)
						auth := &coreauth.Auth{
							Provider: "codex", Attributes: map[string]string{
								"auth_kind": "apikey", "api_key": "synthetic-key", "websockets": "true",
							},
						}
						executor := NewCodexAutoExecutor(&config.Config{})
						req := core.Request{Model: requestModel, Payload: []byte(payload)}
						var output []byte
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, req, opts)
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
							result, err := executor.Execute(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							output = result.Payload
						}
						if calls != 1 || !bytes.Contains(output, []byte("AA==")) {
							t.Fatalf("calls=%d output=%s", calls, output)
						}
					},
				)
			}
		}
	}
}

func TestImage26RejectsCodexOAuthBeforeTransport(t *testing.T) {
	for _, stream := range []bool{false, true} {
		ctx := util.WithRoundTripper(
			t.Context(), devinTestRoundTripper(
				func(*http.Request) (*http.Response, error) {
					t.Fatal("OAuth request reached public image transport")
					return nil, nil
				},
			),
		)
		auth := &coreauth.Auth{
			Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth", "websockets": "true"},
			Metadata: map[string]any{"access_token": "synthetic-oauth"},
		}
		ctx = core.WithDownstreamWebsocket(ctx)
		for _, responses := range []bool{false, true} {
			req := core.Request{
				Model: "gpt-image-2.6-flare", Payload: []byte(`{"model":"gpt-image-2.6-flare","prompt":"synthetic"}`),
			}
			opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
			if responses {
				req.Model = "gpt-6.1-sol"
				req.Payload = []byte(`{"model":"gpt-6.1-sol","input":"synthetic","tools":[{"type":"image_generation","model":"gpt-image-2.6-flare"}]}`)
				opts.SourceFormat = translator.FormatOpenAIResponse
			}
			executor := NewCodexAutoExecutor(&config.Config{})
			var err error
			if stream {
				_, err = executor.ExecuteStream(ctx, auth, req, opts)
			} else {
				_, err = executor.Execute(ctx, auth, req, opts)
			}
			if err == nil || statusErrCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "API-key") {
				t.Fatalf("OAuth boundary error=%v", err)
			}
		}
	}
}

func mediaSyntheticAudio(t *testing.T) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="request"`)
	header.Set("Content-Type", "application/json")
	settings, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(
		settings,
		`{"model":"muse-voice-transcribe-1.0","mode":"DIARIZATION","audioEncoding":"WAV","keywords":["synthetic"]}`,
	); err != nil {
		t.Fatal(err)
	}
	audio, err := writer.CreateFormFile("audio", "synthetic.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(audio, "RIFF\x24\x00\x00\x00WAVEsynthetic-audio"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func TestMetaDedicatedImageAndTranscriptionContracts(t *testing.T) {
	for _, mode := range []string{"image", "edit", "audio-json", "audio-text", "audio-stream"} {
		t.Run(
			mode, func(t *testing.T) {
				model := "muse-image-1.0"
				payload := []byte(`{"model":"muse-image-1.0","prompt":"synthetic","reasoning_strength":"low","tool_enablement":{"enable_web_search":false},"output_format":"webp"}`)
				opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, false)
				path := "/v1/images/generations"
				mime := "application/json"
				response := `{"created":1,"data":[{"b64_json":"AA=="}],"output_format":"webp"}`
				if mode == "edit" {
					path = "/v1/images/edits"
					opts = codexOpenAIImageTestOptions(codexImagesEditsPath, false)
					payload = []byte(`{"model":"muse-image-1.0","prompt":"synthetic","images":[{"image_url":"data:image/png;base64,AA=="}],"reasoning_strength":"low"}`)
				}
				if strings.HasPrefix(mode, "audio") {
					model = "muse-voice-transcribe-1.0"
					path = "/v1/asr/transcribe"
					payload, mime = mediaSyntheticAudio(t)
					opts = core.Options{
						SourceFormat: translator.FromString(metaAudioFormat),
						Headers:      http.Header{"Content-Type": {mime}, "Accept": {"application/json"}},
						Query:        url.Values{"sessionId": {"correlation"}},
					}
					response = `{"sessionId":"correlation","transcript":"synthetic speech","audioDurationMs":8240,"turns":[{"startMs":1,"endMs":8240,"speaker":"A","transcript":"synthetic speech"}]}`
					if mode == "audio-text" {
						opts.Headers.Set("Accept", "text/plain")
						response = "synthetic speech"
					}
					if mode == "audio-stream" {
						opts.Headers.Set("Accept", "text/event-stream")
						opts.Stream = true
						response = "data: {\"transcript\":\"synthetic speech\"}\n\n"
					}
				}
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(r *http.Request) (*http.Response, error) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								return nil, err
							}
							if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer minted-synthetic" {
								t.Fatalf("Meta endpoint=%s headers=%v", r.URL, r.Header)
							}
							if strings.HasPrefix(mode, "audio") {
								if !bytes.Equal(body, payload) || r.URL.Query().Get("sessionId") != "correlation" {
									t.Fatal("native transcription bytes or query changed")
								}
							} else if gjson.GetBytes(body, "reasoning_strength").String() != "low" {
								t.Fatalf("Meta image settings lost: %s", body)
							}
							accept := r.Header.Get("Accept")
							if accept == "" {
								accept = "application/json"
							}
							return &http.Response{
								StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {accept}},
								Body: io.NopCloser(strings.NewReader(response)),
							}, nil
						},
					),
				)
				auth := &coreauth.Auth{
					Provider: "meta",
					Metadata: map[string]any{"access_token": "minted-synthetic", "base_url": "https://api.meta.ai/v1"},
				}
				executor := NewMetaExecutor(&config.Config{})
				req := core.Request{Model: model, Payload: payload}
				var output []byte
				if opts.Stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
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
					result, err := executor.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output = result.Payload
				}
				if string(output) != response {
					t.Fatalf("native media response changed: %s", output)
				}
				if auth.Attributes != nil {
					t.Fatal("Meta media changed selected credential")
				}
			},
		)
	}
}

func TestMetaTranscriptionRejectsMalformedMultipart(t *testing.T) {
	body, contentType := mediaSyntheticAudio(t)
	if model, err := MetaAudioRequestModel(body, contentType); err != nil || model != "muse-voice-transcribe-1.0" {
		t.Fatalf("model=%s error=%v", model, err)
	}
	for _, invalid := range [][]byte{
		[]byte("not multipart"), bytes.TrimSuffix(body, []byte("--\r\n")),
		bytes.ReplaceAll(body, []byte("muse-voice-transcribe-1.0"), []byte("muse-spark-1.3")),
	} {
		if _, err := MetaAudioRequestModel(invalid, contentType); err == nil {
			t.Fatal("malformed native transcription accepted")
		}
	}
}

func TestImage26MultipartAnimationParametersKeepTypes(t *testing.T) {
	var input bytes.Buffer
	writer := multipart.NewWriter(&input)
	for key, value := range map[string]string{
		"model": "gpt-image-2.6-sunburst", "prompt": "animate", "output_format": "gif", "frame_count": "9",
		"frame_duration_ms": "200", "complexity": "low", "markup[file_id]": "file-markup",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	image, err := writer.CreateFormFile("image[]", "source.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(image, "synthetic"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	opts := codexOpenAIImageTestOptions(codexImagesEditsPath, false)
	opts.Headers = http.Header{"Content-Type": {writer.FormDataContentType()}}
	payload, _, _, err := newCodexExecutor(&config.Config{}).prepareDirectOpenAIImageBody(
		&coreauth.Auth{
			Attributes: map[string]string{
				"auth_kind": "apikey", "api_key": "synthetic",
			},
		}, core.Request{Model: "gpt-image-2.6-sunburst", Payload: input.Bytes()}, opts, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "frame_count").Type != gjson.Number || gjson.GetBytes(
		payload, "frame_duration_ms",
	).Int() != 200 || gjson.GetBytes(payload, "markup.file_id").String() != "file-markup" || gjson.GetBytes(
		payload, "images.0.image_url",
	).String() == "" {
		t.Fatalf("multipart fields lost: %s", payload)
	}
}
