package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const vertexInteractionFixture = `{"id":"fixture-interaction","model":"gemini-2.5-pro","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"native-answer"}]}],"usage":{"total_input_tokens":7,"total_output_tokens":3}}`

func vertexInteractionFixtureStream() string {
	return "event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":0,\n" +
		"data: \"delta\":{\"type\":\"text_delta\",\"text\":\"native-answer\"}}\n\n" +
		"data: {\"event_type\":\"interaction.completed\",\"interaction\":" + vertexInteractionFixture + "}\n\n" +
		"event: done\n\n"
}

func TestVertexNativeInteractionsRouting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attribute  string
		metadata   map[string]any
		source     sdktranslator.Format
		wantNative bool
	}{
		{name: "default", source: sdktranslator.FormatInteractions},
		{name: "attribute true", attribute: " TRUE ", source: sdktranslator.FormatInteractions, wantNative: true},
		{name: "attribute one", attribute: "1", source: sdktranslator.FormatInteractions, wantNative: true},
		{name: "attribute false", attribute: "false", source: sdktranslator.FormatInteractions},
		{name: "metadata interactions", metadata: map[string]any{"interactions": true}, source: sdktranslator.FormatInteractions, wantNative: true},
		{name: "metadata native", metadata: map[string]any{"native_interactions": true}, source: sdktranslator.FormatInteractions, wantNative: true},
		{name: "metadata string", metadata: map[string]any{"native_interactions": "true"}, source: sdktranslator.FormatInteractions},
		{name: "chat remains legacy", attribute: "true", source: sdktranslator.FormatOpenAI},
	} {
		for _, stream := range []bool{false, true} {
			name := "execute"
			if stream {
				name = "stream"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(t.Context(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"legacy-answer"}]},"finishReason":"STOP"}]}`
					if tc.wantNative {
						wantAlt := ""
						if stream {
							wantAlt = "sse"
						}
						if req.URL.Path != "/v1beta1/interactions" || req.URL.Query().Get("alt") != wantAlt {
							t.Fatalf("native URL = %s", req.URL)
						}
						if gjson.GetBytes(body, "previous_interaction_id").String() != "previous-fixture" ||
							gjson.GetBytes(body, "input.0.id").Exists() || gjson.GetBytes(body, "input.0.content.0.id").Exists() ||
							gjson.GetBytes(body, "stream").Bool() != stream {
							t.Fatalf("native request = %s", body)
						}
						if req.Header.Get("Api-Revision") != "fixture-revision" || req.Header.Get("X-Copied") != "fixture-header" {
							t.Fatalf("native headers = %v", req.Header)
						}
						response = vertexInteractionFixture
						if stream {
							response = vertexInteractionFixtureStream()
						}
					} else {
						if strings.Contains(req.URL.Path, "/interactions") || !gjson.GetBytes(body, "contents").IsArray() {
							t.Fatalf("legacy dispatch = %s body=%s", req.URL, body)
						}
						if stream {
							response = "data: " + response + "\n\n"
						}
					}
					if req.Header.Get("X-Goog-Api-Key") != "synthetic-key" || req.Header.Get("Authorization") != "" {
						t.Fatalf("credential headers = %v", req.Header)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}))
				auth := &cliproxyauth.Auth{
					Provider: "vertex", Metadata: tc.metadata,
					Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://vertex-format.invalid", "interactions": tc.attribute, "header:X-Copied": "$x-client-value"},
				}
				payload := []byte(`{"model":"gemini-2.5-pro","previous_interaction_id":"previous-fixture","input":[{"type":"user_input","id":"remove-step","content":[{"type":"text","id":"remove-part","text":"fixture-prompt"}]}]}`)
				if tc.source == sdktranslator.FormatOpenAI {
					payload = []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"fixture-prompt"}]}`)
				}
				req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: tc.source, ResponseFormat: sdktranslator.FormatInteractions, OriginalRequest: payload, Stream: stream, Headers: http.Header{"Api-Revision": {"fixture-revision"}, "X-Client-Value": {"fixture-header"}}}
				exec := NewGeminiVertexExecutor(&config.Config{})
				var output bytes.Buffer
				if stream {
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output.Write(chunk.Payload)
					}
				} else {
					result, err := exec.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output.Write(result.Payload)
				}
				if calls != 1 || tc.wantNative && !strings.Contains(output.String(), "native-answer") {
					t.Fatalf("calls=%d output=%s", calls, output.String())
				}
			})
		}
	}
}

func TestVertexNativeInteractionsServiceAccountAndResponseFormat(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	for _, stream := range []bool{false, true} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatClaude} {
			name := "execute/"
			if stream {
				name = "stream/"
			}
			t.Run(name+format.String(), func(t *testing.T) {
				tokenCalls, nativeCalls := 0, 0
				ctx := util.WithRoundTripper(t.Context(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
					response := `{"access_token":"synthetic-bearer","expires_in":3600,"token_type":"Bearer"}`
					if req.URL.Host == "vertex-token.invalid" {
						tokenCalls++
					} else {
						nativeCalls++
						if req.URL.Host != "vertex-format.invalid" || req.URL.Path != "/v1beta1/projects/fixture-project/locations/global/interactions" ||
							req.Header.Get("Authorization") != "Bearer synthetic-bearer" || req.Header.Get("X-Goog-Api-Key") != "" ||
							req.Header.Get("Api-Revision") != geminiInteractionsAPIRevision {
							t.Fatalf("service account dispatch = %s headers=%v", req.URL, req.Header)
						}
						response = vertexInteractionFixture
						if stream {
							response = vertexInteractionFixtureStream()
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}))
				auth := &cliproxyauth.Auth{Provider: "vertex", Attributes: map[string]string{"base_url": "https://vertex-format.invalid/"}, Metadata: map[string]any{
					"native_interactions": true, "project_id": "fixture-project", "location": "us-central1",
					"service_account": map[string]any{"type": "service_account", "project_id": "fixture-project", "client_email": "fixture@fixture.invalid", "private_key": privateKey, "token_uri": "https://vertex-token.invalid/token"},
				}}
				payload := []byte(`{"model":"gemini-2.5-pro","input":"fixture-prompt"}`)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: format, OriginalRequest: payload, Stream: stream}
				req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: payload}
				exec := NewGeminiVertexExecutor(&config.Config{})
				var output bytes.Buffer
				if stream {
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output.Write(chunk.Payload)
					}
				} else {
					result, err := exec.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output.Write(result.Payload)
				}
				if tokenCalls != 1 || nativeCalls != 1 || !strings.Contains(output.String(), "native-answer") {
					t.Fatalf("token/native calls=%d/%d output=%s", tokenCalls, nativeCalls, output.String())
				}
				if format == sdktranslator.FormatClaude {
					if stream && strings.Count(output.String(), "event: message_stop") != 1 || !stream && gjson.Get(output.String(), "type").String() != "message" {
						t.Fatalf("Claude response = %s", output.String())
					}
				}
			})
		}
	}
}

func TestVertexNativeInteractionsFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		status int
		auth   *cliproxyauth.Auth
	}{
		{name: "execute rejection", status: http.StatusForbidden},
		{name: "stream rejection", stream: true, status: http.StatusForbidden},
		{name: "truncated stream", stream: true, status: http.StatusOK},
		{name: "execute missing service account", auth: &cliproxyauth.Auth{Provider: "vertex", Metadata: map[string]any{"native_interactions": true}}},
		{name: "stream missing service account", stream: true, auth: &cliproxyauth.Auth{Provider: "vertex", Metadata: map[string]any{"native_interactions": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(t.Context(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body := `{"error":{"message":"fixture rejection"}}`
				if tc.status == http.StatusOK {
					body = "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			auth := tc.auth
			if auth == nil {
				auth = &cliproxyauth.Auth{Provider: "vertex", Attributes: map[string]string{"api_key": "synthetic-key", "interactions": "true", "base_url": "https://vertex-format.invalid"}}
			}
			exec := NewGeminiVertexExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"fixture-prompt"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, Stream: tc.stream}
			var err error
			if tc.stream {
				var result *cliproxyexecutor.StreamResult
				result, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = exec.Execute(ctx, auth, req, opts)
			}
			wantCalls := 1
			if tc.auth != nil {
				wantCalls = 0
			}
			if err == nil || calls != wantCalls {
				t.Fatalf("calls=%d want=%d error=%v", calls, wantCalls, err)
			}
			if tc.auth == nil {
				status, ok := err.(interface{ StatusCode() int })
				wantStatus := tc.status
				if tc.status == http.StatusOK {
					wantStatus = http.StatusBadGateway
				}
				if !ok || status.StatusCode() != wantStatus {
					t.Fatalf("error=%v, want HTTP %d", err, wantStatus)
				}
			}
		})
	}
}

func TestVertexNativeInteractionsAfterAuthFormat(t *testing.T) {
	for _, operation := range []string{"execute", "stream", "count"} {
		t.Run(operation, func(t *testing.T) {
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(NewGeminiVertexExecutor(&config.Config{}))
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "vertex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"api_key": "synthetic-key", "interactions": "true", "base_url": "https://vertex-format.invalid"}}
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			models := registry.GetGlobalRegistry()
			models.RegisterClient(auth.ID, "vertex", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
			t.Cleanup(func() { models.UnregisterClient(auth.ID) })
			manager.RefreshSchedulerEntry(auth.ID)
			ctx := util.WithRoundTripper(t.Context(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				body := vertexInteractionFixture
				if operation == "stream" {
					body = vertexInteractionFixtureStream()
				} else if operation == "count" {
					body = `{"totalTokens":7}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			intercepts := 0
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, Stream: operation == "stream"}
			opts.RequestAfterAuthInterceptor = func(_ context.Context, input cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
				intercepts++
				want := sdktranslator.FormatInteractions
				if operation == "count" {
					want = sdktranslator.FormatGemini
				}
				if input.ToFormat != want {
					t.Errorf("after-auth format = %s, want %s", input.ToFormat, want)
				}
				return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
			}
			req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"model":"gemini-2.5-pro","input":"fixture-prompt"}`)}
			var err error
			switch operation {
			case "execute":
				_, err = manager.Execute(ctx, []string{"vertex"}, req, opts)
			case "count":
				_, err = manager.ExecuteCount(ctx, []string{"vertex"}, req, opts)
			case "stream":
				var result *cliproxyexecutor.StreamResult
				result, err = manager.ExecuteStream(ctx, []string{"vertex"}, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						if chunk.Acknowledge != nil {
							chunk.Acknowledge(nil)
						}
					}
				}
			}
			if err != nil || intercepts != 1 {
				t.Fatalf("intercepts=%d error=%v", intercepts, err)
			}
		})
	}
}
