package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityUsesPreparedRequestPayload(t *testing.T) {
	const response = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"prepared-format-answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}}`
	for _, method := range []string{"execute", "aggregated-execute", "stream", "count"} {
		t.Run(method, func(t *testing.T) {
			exec := NewAntigravityExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				ID: "offline-antigravity-prepared-" + method, Provider: "antigravity",
				Attributes: map[string]string{"base_url": "https://offline-format.invalid"},
				Metadata:   map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			requests := 0
			ctx = util.WithRoundTripper(ctx, apiKeyCompatRoundTripper(func(request *http.Request) (*http.Response, error) {
				requests++
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if !strings.Contains(string(body), "prepared-format-prompt") || strings.Contains(string(body), "original-format-prompt") {
					return nil, fmt.Errorf("prepared Chat request was replaced by the original Responses body: %s", body)
				}
				data := response
				contentType := "application/json"
				if method == "stream" || method == "aggregated-execute" {
					data = "data: " + response + "\n\n"
					contentType = "text/event-stream"
				} else if method == "count" {
					data = `{"totalTokens":7}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(data)), Request: request}, nil
			}))
			req := cliproxyexecutor.Request{Model: "gemini-3.6-flash-high", Payload: []byte(`{"messages":[{"role":"user","content":"prepared-format-prompt"}],"stream":true}`)}
			if method == "aggregated-execute" {
				req.Model = "gemini-3-pro"
			}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatClaude,
				OriginalRequest: []byte(`{"input":"original-format-prompt","stream":true}`),
			}
			if method == "stream" {
				opts.Stream = true
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var output strings.Builder
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output.Write(chunk.Payload)
				}
				if !strings.Contains(output.String(), "prepared-format-answer") || !strings.Contains(output.String(), "message_stop") {
					t.Fatalf("unexpected translated stream: %s", output.String())
				}
			} else {
				var result cliproxyexecutor.Response
				var err error
				if method == "count" {
					opts.CountTokens = true
					result, err = exec.CountTokens(ctx, auth, req, opts)
				} else {
					result, err = exec.Execute(ctx, auth, req, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if (method == "count" && gjson.GetBytes(result.Payload, "input_tokens").Int() != 7) ||
					(method != "count" && gjson.GetBytes(result.Payload, "content.0.text").String() != "prepared-format-answer") {
					t.Fatalf("unexpected translated response: %s", result.Payload)
				}
			}
			if requests != 1 {
				t.Fatalf("upstream requests = %d, want 1", requests)
			}
		})
	}
}
