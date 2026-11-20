package executor

import (
	"context"
	"fmt"
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

func TestOpenAICompatExecutorResponsesStreamFailsOnEOFWithoutDone(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte((`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":17` +
						`73896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"r` +
						`ole":"assistant","content":"partial"},"finish_reason":null}]}`) + "\n\n"),
				)
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}
	request := []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`)
	result, err := executor.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: request,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			ResponseFormat:  sdktranslator.FormatOpenAIResponse,
			OriginalRequest: request,
			Stream:          true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamed strings.Builder
	var streamErr error
	for chunk := range result.Chunks {
		streamed.Write(chunk.Payload)
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if !strings.Contains(streamed.String(), "response.output_text.delta") {
		t.Fatalf("stream did not forward partial assistant output: %q", streamed.String())
	}
	if strings.Contains(streamed.String(), "response.completed") {
		t.Fatalf("clean EOF without [DONE] was finalized as response.completed: %q", streamed.String())
	}
	if streamErr == nil {
		t.Fatal("clean EOF without [DONE] did not produce a terminal stream error")
	}
	statusErr, ok := streamErr.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusBadGateway {
		t.Fatalf("stream error status = %v, want %d", streamErr, http.StatusBadGateway)
	}
	if !strings.Contains(streamErr.Error(), "closed before a completed response") {
		t.Fatalf("stream error does not explain the missing terminal marker: %v", streamErr)
	}
}

func TestOpenAICompatExecutorResponsesStreamPreservesUpstreamDataError(t *testing.T) {
	for _, withDone := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("with_done=%t", withDone), func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write(
								[]byte(
									(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":17` +
										`73896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"r` +
										`ole":"assistant","content":"partial"},"finish_reason":null}]}`) + "\n\n",
								),
							)
							_, _ = w.Write(
								[]byte(
									(`data: {"error":{"type":"server_error","code":"upstream_failed",` +
										`"message":"upstream ` +
										`failed"}}`) +
										"\n\n",
								),
							)
							if withDone {
								_, _ = w.Write([]byte("data: [DONE]\n\n"))
							}
						},
					),
				)
				defer server.Close()

				executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
				auth := &cliproxyauth.Auth{
					Attributes: map[string]string{
						"base_url": server.URL + "/v1",
						"api_key":  "test",
					},
				}
				request := []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`)
				result, err := executor.ExecuteStream(
					context.Background(), auth, cliproxyexecutor.Request{
						Model:   "deepseek-v4-flash",
						Payload: request,
					}, cliproxyexecutor.Options{
						SourceFormat:    sdktranslator.FormatOpenAIResponse,
						ResponseFormat:  sdktranslator.FormatOpenAIResponse,
						OriginalRequest: request,
						Stream:          true,
					},
				)
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}

				var streamed strings.Builder
				var streamErr error
				for chunk := range result.Chunks {
					streamed.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if strings.Contains(streamed.String(), "response.completed") {
					t.Fatalf("upstream data error was finalized as response.completed: %q", streamed.String())
				}
				if streamErr == nil || !strings.Contains(streamErr.Error(), "upstream failed") {
					t.Fatalf("terminal stream error = %v, want original upstream failure", streamErr)
				}
			},
		)
	}
}

func TestOpenAICompatExecutorResponsesStreamPreservesNamedErrorEvent(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte((`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":17` +
						`73896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"r` +
						`ole":"assistant","content":"partial"},"finish_reason":null}]}`) + "\n\n"),
				)
				_, _ = w.Write([]byte("event: error\n"))
				_, _ = w.Write([]byte(`data: {"code":"upstream_failed",` + "\n"))
				_, _ = w.Write([]byte(`data: "message":"upstream failed"}` + "\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}
	request := []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`)
	result, err := executor.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: request,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			ResponseFormat:  sdktranslator.FormatOpenAIResponse,
			OriginalRequest: request,
			Stream:          true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamed strings.Builder
	var streamErr error
	for chunk := range result.Chunks {
		streamed.Write(chunk.Payload)
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if strings.Contains(streamed.String(), "response.completed") {
		t.Fatalf("named upstream error event was finalized as response.completed: %q", streamed.String())
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "upstream failed") {
		t.Fatalf("terminal stream error = %v, want named upstream failure", streamErr)
	}
}

func TestOpenAICompatExecutorResponsesStreamHandlesAdditionalErrorShapes(t *testing.T) {
	tests := []struct {
		name    string
		lines   []string
		wantErr string
	}{
		{
			name: "response failed payload",
			lines: []string{
				(`data: {"type":"response.failed","response":{"error":{"type":"server_er` +
					`ror","code":"upstream_failed","message":"response failed upstream"}}}`) + "\n\n",
				"data: [DONE]\n\n",
			},
			wantErr: "response failed upstream",
		},
		{
			name: "data before named error",
			lines: []string{
				`data: {"detail":"data before event failure"}` + "\n",
				"event: error\n\n",
				"data: [DONE]\n\n",
			},
			wantErr: "data before event failure",
		},
		{
			name: "done after incomplete error data",
			lines: []string{
				"event: error\n",
				`data: {"message":"incomplete upstream failure"` + "\n",
				"data: [DONE]\n\n",
			},
			wantErr: "incomplete data before [DONE]",
		},
		{
			name: "done immediately after error event",
			lines: []string{
				"event: error\n",
				"data: [DONE]\n\n",
			},
			wantErr: "error event ended before [DONE]",
		},
		{
			name: "incomplete data cannot cross frame boundary",
			lines: []string{
				"data: {\n\n",
				`data: "id":"chatcmpl_2","object":"chat.completion.chunk","choices":[]}` + "\n\n",
				"data: [DONE]\n\n",
			},
			wantErr: "incomplete SSE data frame",
		},
	}

	for _, tc := range tests {
		t.Run(
			tc.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write(
								[]byte(
									(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":17` +
										`73896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"r` +
										`ole":"assistant","content":"partial"},"finish_reason":null}]}`) + "\n\n",
								),
							)
							for _, line := range tc.lines {
								_, _ = w.Write([]byte(line))
							}
						},
					),
				)
				defer server.Close()

				executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
				auth := &cliproxyauth.Auth{
					Attributes: map[string]string{
						"base_url": server.URL + "/v1", "api_key": "test",
					},
				}
				request := []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`)
				result, err := executor.ExecuteStream(
					context.Background(),
					auth,
					cliproxyexecutor.Request{Model: "deepseek-v4-flash", Payload: request},
					cliproxyexecutor.Options{
						SourceFormat:   sdktranslator.FormatOpenAIResponse,
						ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: request, Stream: true,
					},
				)
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}

				var streamed strings.Builder
				var streamErr error
				for chunk := range result.Chunks {
					streamed.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if strings.Contains(streamed.String(), "response.completed") {
					t.Fatalf("upstream error was finalized as response.completed: %q", streamed.String())
				}
				if streamErr == nil || !strings.Contains(streamErr.Error(), tc.wantErr) {
					t.Fatalf("terminal stream error = %v, want %q", streamErr, tc.wantErr)
				}
			},
		)
	}
}

func TestOpenAICompatExecutorStreamDropsChunksAfterDone(t *testing.T) {
	// Some OpenAI-compatible upstreams (e.g. OpenCode zen) append non-spec
	// metadata after data: [DONE]. Those trailing events must not be forwarded,
	// otherwise clients that treat every pre-[DONE] data line as a chat chunk
	// fail to deserialize (e.g. missing required "id").
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, _ := w.(http.Flusher)
				_, _ = w.Write(
					[]byte(("data: {\"id\":\"c1a4ba22\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"" +
						"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")),
				)
				_, _ = w.Write(
					[]byte(("data: {\"id\":\"c1a4ba22\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"" +
						"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1," +
						"\"completion_tokens\":1,\"total_tokens\":2}}\n\n")),
				)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				_, _ = w.Write([]byte(`data: {"choices":[],"cost":"0"}` + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}
	result, err := executor.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model: "deepseek-v4-flash-free",
			Payload: []byte(
				`{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var payloads []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		if len(chunk.Payload) == 0 {
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	if len(payloads) != 2 {
		t.Fatalf(
			"got %d payloads %v, want 2 (content + finish; no post-DONE cost chunk)",
			len(payloads),
			payloads,
		)
	}
	for _, p := range payloads {
		if strings.Contains(p, `"cost"`) {
			t.Fatalf("post-DONE cost chunk was forwarded: %s", p)
		}
		if !gjson.Get(p, "id").Exists() {
			t.Fatalf("chunk missing id: %s", p)
		}
	}
	if gjson.Get(payloads[0], "choices.0.delta.content").String() != "hi" {
		t.Fatalf("first chunk = %s", payloads[0])
	}
	if gjson.Get(payloads[1], "choices.0.finish_reason").String() != "stop" {
		t.Fatalf("second chunk = %s", payloads[1])
	}
}
