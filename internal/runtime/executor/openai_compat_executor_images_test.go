package executor

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorImagesJSONPassthrough(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotContentType string
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotContentType = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"abc"}]}`))
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test-key",
		},
	}
	payload := []byte(`{"model":"alias-image","prompt":"draw","stream":true}`)
	resp, err := executor.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "upstream-image",
			Payload: payload,
		}, cliproxyexecutor.Options{
			Headers:      http.Header{"Content-Type": {"application/json"}},
			SourceFormat: sdktranslator.FromString("openai-image"),
			Metadata: map[string]any{
				cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/images/generations")
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content type = %q", gotContentType)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream should be removed for non-stream request: %s", gotBody)
	}
	if string(resp.Payload) != `{"created":1,"data":[{"b64_json":"abc"}]}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
}

func TestOpenAICompatExecutorImagesMultipartPassthrough(t *testing.T) {
	var gotPath string
	var gotModel string
	var gotPrompt string
	var gotStream string
	var gotFile string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if err := r.ParseMultipartForm(32 << 20); err != nil {
					t.Fatalf("ParseMultipartForm error: %v", err)
				}
				gotModel = r.FormValue("model")
				gotPrompt = r.FormValue("prompt")
				gotStream = r.FormValue("stream")
				file, _, err := r.FormFile("image")
				if err != nil {
					t.Fatalf("FormFile error: %v", err)
				}
				data, _ := io.ReadAll(file)
				if errClose := file.Close(); errClose != nil {
					t.Fatalf("close upload file: %v", errClose)
				}
				gotFile = string(data)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"abc"}]}`))
			},
		),
	)
	defer server.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "alias-image"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("stream", "true"); err != nil {
		t.Fatalf("write stream field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatalf("CreateFormFile error: %v", err)
	}
	_, _ = part.Write([]byte("png-data"))
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL + "/v1"}}
	_, err = executor.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "upstream-image",
			Payload: body.Bytes(),
		}, cliproxyexecutor.Options{
			Headers:      http.Header{"Content-Type": {writer.FormDataContentType()}},
			SourceFormat: sdktranslator.FromString("openai-image"),
			Metadata: map[string]any{
				cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits",
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/images/edits" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/images/edits")
	}
	if gotModel != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image", gotModel)
	}
	if gotPrompt != "edit" {
		t.Fatalf("prompt = %q, want edit", gotPrompt)
	}
	if gotStream != "" {
		t.Fatalf("stream should be removed for non-stream request, got %q", gotStream)
	}
	if gotFile != "png-data" {
		t.Fatalf("file = %q, want png-data", gotFile)
	}
}

func TestOpenAICompatExecutorImagesStreamPassthrough(t *testing.T) {
	for _, tt := range []struct{ name, payload string }{
		{name: "ordinary", payload: "data: {\"object\":\"chat.completion.chunk\",\"model\":\"image-served\"}\n\ndata: {\"service_tier\":\"priority\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}\n\n"},
		{name: "completed-model-is-final", payload: "data: {\"model\":\"image-served\",\"status\":\"completed\"}\n\ndata: {\"model\":\"late-metadata\"}\n\ndata: {\"service_tier\":\"priority\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}\n\n"},
		{name: "incomplete-model-is-final", payload: "data: {\"model\":\"image-served\",\"status\":\"incomplete\"}\n\ndata: {\"model\":\"late-metadata\"}\n\ndata: {\"service_tier\":\"priority\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {

			plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
			registerTestUsagePlugin(t, plugin)
			var gotBody []byte
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						gotBody, _ = io.ReadAll(r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(
							[]byte(tt.payload),
						)
					},
				),
			)
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL + "/v1"}}
			stream, err := executor.ExecuteStream(
				context.Background(), auth, cliproxyexecutor.Request{
					Model:   "upstream-image",
					Payload: []byte(`{"model":"alias-image","prompt":"draw"}`),
				}, cliproxyexecutor.Options{
					Headers:      http.Header{"Content-Type": {"application/json"}},
					SourceFormat: sdktranslator.FromString("openai-image"),
					Metadata: map[string]any{
						cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
					},
				},
			)
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			var chunks []string
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error: %v", chunk.Err)
				}
				chunks = append(chunks, string(chunk.Payload))
			}
			wantStream := tt.payload
			if got := strings.Join(chunks, ""); got != wantStream {
				t.Fatalf("stream payload = %q", got)
			}
			select {
			case record := <-plugin.records:
				if record.Model != "upstream-image" || record.ResponseModel != "image-served" {
					t.Fatalf("image stream model = %q/%q", record.Model, record.ResponseModel)
				}
				if record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 5 {
					t.Fatalf("usage detail = %+v", record.Detail)
				}
				if record.ResponseServiceTier != "priority" || record.Detail.ResponseServiceTier != "priority" {
					t.Fatalf(
						"response tiers = (%q, %q), want priority",
						record.ResponseServiceTier,
						record.Detail.ResponseServiceTier,
					)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for image stream usage record")
			}
			if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
				t.Fatalf("model = %q, want upstream-image; body=%s", got, gotBody)
			}
			if got := gjson.GetBytes(gotBody, "stream").Bool(); !got {
				t.Fatalf("stream should be true: %s", gotBody)
			}

		})
	}
}

func TestOpenAICompatExecutorVideoRequestTranslation(t *testing.T) {
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, stream := range []bool{false, true} {
			name := string(source) + "/nonstream"
			if stream {
				name = string(source) + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				type capturedRequest struct {
					path string
					body []byte
				}
				requests := make(chan capturedRequest, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read outbound video request: %v", err)
						return
					}
					requests <- capturedRequest{path: r.URL.Path, body: body}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"id\":\"video-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"described\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"video-1","choices":[{"index":0,"message":{"role":"assistant","content":"described"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				payload := []byte(`{
					"model":"video-model","messages":[{"role":"user","content":[
						{"type":"text","text":"Describe clips"},
						{"type":"video_url","video_url":{"url":"https://example.test/clip.mp4","processing":"agentic"}},
						{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAECAwQ="}}
					]}]
				}`)
				if source == sdktranslator.FormatOpenAIResponse {
					payload = []byte(`{
						"model":"video-model","input":[{"role":"user","content":[
							{"type":"input_text","text":"Describe clips"},
							{"type":"input_video","video_url":"https://example.test/clip.mp4","processing":"agentic"},
							{"type":"input_video","video_url":"data:video/mp4;base64,AAECAwQ="}
						]}]
					}`)
				}
				executor := NewOpenAICompatExecutor("compat", &config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{
					"base_url": server.URL + "/v1", "api_key": "test",
				}}
				req := cliproxyexecutor.Request{Model: "video-model", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: source, Stream: stream}
				var received []byte
				if stream {
					result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatalf("ExecuteStream: %v", err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream: %v", chunk.Err)
						}
						received = append(received, chunk.Payload...)
					}
				} else {
					result, err := executor.Execute(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatalf("Execute: %v", err)
					}
					received = result.Payload
				}
				if !bytes.Contains(received, []byte("described")) {
					t.Fatalf("upstream response missing: %s", received)
				}
				select {
				case captured := <-requests:
					if captured.path != "/v1/chat/completions" || gjson.GetBytes(captured.body, "model").String() != "video-model" {
						t.Fatalf("upstream route/model changed: %s %s", captured.path, captured.body)
					}
					parts := gjson.GetBytes(captured.body, "messages.0.content").Array()
					if len(parts) != 3 ||
						parts[0].Get("text").String() != "Describe clips" ||
						parts[1].Get("video_url.url").String() != "https://example.test/clip.mp4" ||
						parts[1].Get("video_url.processing").String() != "agentic" ||
						parts[2].Get("video_url.url").String() != "data:video/mp4;base64,AAECAwQ=" {
						t.Fatalf("video parts changed: %s", captured.body)
					}
				default:
					t.Fatal("upstream video request was not captured")
				}
			})
		}
	}
}
