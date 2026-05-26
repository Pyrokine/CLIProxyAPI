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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
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
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
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
	if got := strings.Join(chunks, ""); got != "data: {\"ok\":true}\n\n" {
		t.Fatalf("stream payload = %q", got)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "stream").Bool(); !got {
		t.Fatalf("stream should be true: %s", gotBody)
	}
}
