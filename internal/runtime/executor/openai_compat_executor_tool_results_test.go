package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type openAICompatRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAICompatRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAICompatResponsesStreamFinalizesOnlyAfterFinishOrDone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		finish     string
		done       bool
		readErr    error
		wantEvent  string
		wantFailed bool
	}{
		{"EOF after stop and late usage", "stop", false, nil, "response.completed", false},
		{"EOF after length and late usage", "length", false, nil, "response.incomplete", false},
		{"DONE after stop", "stop", true, nil, "response.completed", false},
		{"EOF before finish", "", false, nil, "", true},
		{"read error after finish", "stop", false, io.ErrUnexpectedEOF, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finishReason := "null"
			if tc.finish != "" {
				finishReason = `"` + tc.finish + `"`
			}
			upstream := "data: {\"id\":\"chat-eof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"chat-eof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":" + finishReason + "}]}\n\n" +
				"data: {\"id\":\"chat-eof\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n"
			if tc.done {
				upstream += "data: [DONE]\n\n"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, upstream)
			}))
			defer server.Close()
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test"}}
			ctx := context.Background()
			if tc.readErr != nil {
				ctx = util.WithRoundTripper(ctx, openAICompatRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(&terminalErrorReader{data: []byte(upstream), err: tc.readErr}), Request: req}, nil
				}))
			}
			request := []byte(`{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)
			stream, err := NewOpenAICompatExecutor("openai-compatibility", &config.Config{}).ExecuteStream(ctx, auth,
				cliproxyexecutor.Request{Model: "test", Payload: request}, cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse,
					OriginalRequest: request, Stream: true,
				})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var streamErr error
			for chunk := range stream.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if (streamErr != nil) != tc.wantFailed {
				t.Fatalf("stream error = %v, want failure %v; output=%s", streamErr, tc.wantFailed, output.String())
			}
			if tc.readErr != nil && !errors.Is(streamErr, tc.readErr) {
				t.Fatalf("stream error = %v, want %v", streamErr, tc.readErr)
			}
			if tc.wantEvent != "" {
				if got := strings.Count(output.String(), `"type":"`+tc.wantEvent+`"`); got != 1 {
					t.Fatalf("terminal event count = %d, want 1; output=%s", got, output.String())
				}
				if !strings.Contains(output.String(), `"input_tokens":3`) || !strings.Contains(output.String(), `"output_tokens":1`) {
					t.Fatalf("late usage not included in terminal event: %s", output.String())
				}
			} else if strings.Contains(output.String(), `"type":"response.completed"`) {
				t.Fatalf("truncated stream reported completion: %s", output.String())
			}
		})
	}
}

func TestOpenAICompatExecutorToolResultContentByInputModalities(t *testing.T) {
	tests := []struct {
		name            string
		stream          bool
		inputModalities []string
		textOnly        bool
		source          sdktranslator.Format
	}{
		{name: "OpenAI non-stream text-only", inputModalities: []string{"text"}, textOnly: true, source: sdktranslator.FormatOpenAI},
		{name: "OpenAI stream text-only", stream: true, inputModalities: []string{"text"}, textOnly: true, source: sdktranslator.FormatOpenAI},
		{name: "OpenAI multimodal", inputModalities: []string{"text", "image"}, source: sdktranslator.FormatOpenAI},
		{name: "OpenAI unspecified", source: sdktranslator.FormatOpenAI},
		{name: "non-stream text-only", stream: false, inputModalities: []string{"text"}, textOnly: true},
		{name: "stream text-only", stream: true, inputModalities: []string{"text"}, textOnly: true},
		{name: "non-stream multimodal", stream: false, inputModalities: []string{"text", "image"}},
		{name: "non-stream unspecified", stream: false, inputModalities: nil},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				var gotBody []byte
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							gotBody, _ = io.ReadAll(r.Body)
							if tt.stream {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = w.Write([]byte("data: [DONE]\n\n"))
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
						},
					),
				)
				defer server.Close()

				executor := NewOpenAICompatExecutor(
					"openai-compatibility", &config.Config{
						OpenAICompatibility: []config.OpenAICompatibility{
							{
								Name: "compat",
								Models: []config.OpenAICompatibilityModel{
									{
										Name:            "mapped-model",
										Alias:           "claude-client",
										InputModalities: tt.inputModalities,
									},
								},
							},
						},
					},
				)
				auth := &cliproxyauth.Auth{
					Provider: "openai-compatibility",
					Attributes: map[string]string{
						"base_url":     server.URL + "/v1",
						"api_key":      "test",
						"compat_name":  "compat",
						"provider_key": "compat",
					},
				}
				payload := []byte(`{"model":"claude-client","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"inspect_image","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"image inspected"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`)
				source := tt.source
				if source == "" {
					source = sdktranslator.FormatClaude
				}
				if source == sdktranslator.FormatOpenAI {
					payload = []byte(`{"model":"claude-client","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"inspect_image","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"image inspected"},{"role":"user","content":[{"type":"text","text":"Images returned by the preceding tool call(s):"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`)
				}
				req := cliproxyexecutor.Request{Model: "mapped-model", Payload: payload}
				opts := cliproxyexecutor.Options{
					SourceFormat:   source,
					ResponseFormat: sdktranslator.FormatOpenAI,
					Stream:         tt.stream,
				}

				if tt.stream {
					result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
					if errExecute != nil {
						t.Fatalf("ExecuteStream error: %v", errExecute)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream chunk error: %v", chunk.Err)
						}
					}
				} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
					t.Fatalf("Execute error: %v", errExecute)
				}

				toolContent := gjson.GetBytes(gotBody, "messages.1.content")
				if toolContent.Type != gjson.String {
					t.Fatalf("tool content type = %s, want string; body=%s", toolContent.Type, string(gotBody))
				}
				wantToolContent := "image inspected"
				wantMessageCount := int64(3)
				if tt.textOnly {
					wantToolContent += "\n\n[image omitted: unsupported by upstream]"
					wantMessageCount = 2
				}
				if toolContent.String() != wantToolContent {
					t.Fatalf("tool content = %q, want %q", toolContent.String(), wantToolContent)
				}
				if got := gjson.GetBytes(gotBody, "messages.#").Int(); got != wantMessageCount {
					t.Fatalf("message count = %d, want %d; body=%s", got, wantMessageCount, string(gotBody))
				}
				if !tt.textOnly {
					relay := gjson.GetBytes(gotBody, "messages.2")
					if relay.Get("role").String() != "user" || relay.Get("content.1.type").String() != "image_url" {
						t.Fatalf("tool image relay missing; body=%s", string(gotBody))
					}
				}
			},
		)
	}
}
