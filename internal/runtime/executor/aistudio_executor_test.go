package executor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAIStudioTranslateRequestNormalizesThinkingLevel(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model: "gemini-3.7-flash",
			Payload: []byte(`{
				"contents":[{"role":"user","parts":[{"text":"hi"}]}],
				"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	if got := gjson.GetBytes(payload, "generationConfig.thinkingConfig.thinkingLevel").String(); got != "HIGH" {
		t.Fatalf("thinkingLevel = %q, want HIGH; payload=%s", got, payload)
	}
	if !gjson.GetBytes(payload, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("includeThoughts was lost: %s", payload)
	}
}

func TestNormalizeAIStudioThinkingLevel(t *testing.T) {
	tests := []struct {
		name      string
		levelJSON string
		want      string
		unchanged bool
	}{
		{name: "minimal", levelJSON: `"minimal"`, want: "MINIMAL"},
		{name: "low", levelJSON: `"low"`, want: "LOW"},
		{name: "medium", levelJSON: `"medium"`, want: "MEDIUM"},
		{name: "mixed case high", levelJSON: `"hIgH"`, want: "HIGH"},
		{name: "already uppercase", levelJSON: `"HIGH"`, want: "HIGH", unchanged: true},
		{name: "unsupported max", levelJSON: `"max"`, want: "max", unchanged: true},
		{name: "non-string", levelJSON: `1`, want: "1", unchanged: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":%s}},"marker":true}`, test.levelJSON))
			output := normalizeAIStudioThinkingLevel(input)
			if got := gjson.GetBytes(output, "generationConfig.thinkingConfig.thinkingLevel").String(); got != test.want {
				t.Fatalf("thinkingLevel = %q, want %q; payload=%s", got, test.want, output)
			}
			if test.unchanged && string(output) != string(input) {
				t.Fatalf("payload changed from %s to %s", input, output)
			}
		})
	}
}

func TestAIStudioTranslateRequestNormalizesGeminiBoundaryTurns(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model:   "gemini-3.7-flash",
			Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	assertGeminiBoundaryRoles(t, payload, []string{"user", "model", "user"})
}

func TestAIStudioTranslateRequestCountTokensPreservesTrailingModelTurn(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model: "gemini-3.7-flash",
			Payload: []byte(`{"contents":[` +
				`{"role":"user","parts":[{"text":"hello"}]},` +
				`{"role":"model","parts":[{"text":"answer"}]}` +
				`]}`),
			Metadata: map[string]any{"action": "countTokens"},
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	assertGeminiBoundaryRoles(t, payload, []string{"user", "model"})
}

func TestGeminiFamilyPayloadRulesApplyAfterPreparation(t *testing.T) {
	for _, provider := range []string{"aistudio", "vertex"} {
		for _, stream := range []bool{false, true} {
			for _, action := range []string{"generateContent", "countTokens"} {
				for _, match := range []string{"matched", "wrong-source", "wrong-header"} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/%s", provider, stream, action, match), func(t *testing.T) {
						rule := config.PayloadModelRule{Name: "gemini-3.7-flash", Protocol: "gemini", FromProtocol: "gemini",
							Headers: map[string]string{"X-Fixture": "enabled"}}
						if match == "wrong-source" {
							rule.FromProtocol = "claude"
						}
						cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{rule},
							Params: map[string]any{"generationConfig.maxOutputTokens": 24, "session_id": "configured-session"}}}}}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: http.Header{"X-Fixture": {"enabled"}},
							Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/responses"}}
						if match == "wrong-header" {
							opts.Headers.Set("X-Fixture", "disabled")
						}
						req := cliproxyexecutor.Request{Model: "gemini-3.7-flash",
							Payload:  []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"answer"}]}],"generationConfig":{"maxOutputTokens":64},"session_id":"source-session"}`),
							Metadata: map[string]any{"action": action}}
						var payload []byte
						var err error
						if provider == "aistudio" {
							payload, _, err = NewAIStudioExecutor(cfg, "aistudio", nil).translateRequest(context.Background(), req, opts, stream)
						} else {
							var prepared *vertexGeminiBody
							prepared, err = NewGeminiVertexExecutor(cfg).prepareVertexGeminiBody(context.Background(), req, opts, req.Model, stream)
							if prepared != nil {
								payload = prepared.body
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						if match == "matched" {
							if gjson.GetBytes(payload, "generationConfig.maxOutputTokens").Int() != 24 ||
								gjson.GetBytes(payload, "session_id").String() != "configured-session" {
								t.Errorf("payload rules lost after preparation: %s", payload)
							}
						} else if gjson.GetBytes(payload, "session_id").Exists() || gjson.GetBytes(payload, "generationConfig.maxOutputTokens").Int() == 24 {
							t.Errorf("unmatched source/header rule applied: %s", payload)
						}
						roles := []string{"user", "model"}
						if action != "countTokens" {
							roles = append(roles, "user")
						}
						assertGeminiBoundaryRoles(t, payload, roles)
					})
				}
			}
		}
	}
}

func TestGeminiFamilyPayloadRulesReachTransport(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for _, provider := range []string{"aistudio", "vertex-api-key", "vertex-service-account"} {
		for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
			for _, match := range []string{"matched", "wrong-source", "wrong-header"} {
				for _, requestPath := range []string{"/v1/responses", "/v1/images/generations"} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", provider, action, match, requestPath), func(t *testing.T) {
						rule := config.PayloadModelRule{Name: "gemini-3.7-flash", Protocol: "gemini", FromProtocol: "gemini",
							Headers: map[string]string{"X-Fixture": "enabled"}}
						if match == "wrong-source" {
							rule.FromProtocol = "claude"
						}
						cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationChat,
							Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{rule},
								Params: map[string]any{"generationConfig.maxOutputTokens": 24, "session_id": "configured-session"}}}}}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini,
							Headers:  http.Header{"X-Fixture": {"enabled"}},
							Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: requestPath}}
						if match == "wrong-header" {
							opts.Headers.Set("X-Fixture", "disabled")
						}
						req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Metadata: map[string]any{"action": "generateContent", "fixture": true},
							Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"answer"}]}],"generationConfig":{"maxOutputTokens":64},"session_id":"source-session","tools":[{"type":"image_generation"}]}`)}
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						upstreamBody := make(chan []byte, 1)
						upstreamURL := make(chan string, 1)
						responseBody := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
						if action == "countTokens" {
							responseBody = `{"totalTokens":4}`
						}
						var exec cliproxyauth.ProviderExecutor
						auth := &cliproxyauth.Auth{ID: t.Name()}
						if provider == "aistudio" {
							manager := wsrelay.NewManager(wsrelay.Options{ProviderFactory: func(*http.Request) (string, error) { return auth.ID, nil }})
							server := httptest.NewServer(manager.Handler())
							t.Cleanup(server.Close)
							t.Cleanup(func() { _ = manager.Stop(context.Background()) })
							client, response, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+manager.Path(), nil)
							if response != nil && response.Body != nil {
								_ = response.Body.Close()
							}
							if errDial != nil {
								t.Fatal(errDial)
							}
							t.Cleanup(func() { _ = client.Close() })
							clientDone := make(chan error, 1)
							go func() {
								var envelope json.RawMessage
								if errRead := client.ReadJSON(&envelope); errRead != nil {
									clientDone <- errRead
									return
								}
								upstreamBody <- []byte(gjson.GetBytes(envelope, "payload.body").String())
								upstreamURL <- gjson.GetBytes(envelope, "payload.url").String()
								clientDone <- client.WriteJSON(map[string]any{"id": gjson.GetBytes(envelope, "id").String(),
									"type": wsrelay.MessageTypeHTTPResp, "payload": map[string]any{"status": http.StatusOK, "body": responseBody}})
							}()
							t.Cleanup(func() {
								select {
								case errClient := <-clientDone:
									if errClient != nil {
										t.Error(errClient)
									}
								case <-time.After(time.Second):
									t.Error("relay fixture did not complete")
								}
							})
							exec = NewAIStudioExecutor(cfg, "aistudio", manager)
						} else {
							exec = NewGeminiVertexExecutor(cfg)
							if provider == "vertex-api-key" {
								auth.Attributes = map[string]string{"api_key": "fixture-key", "base_url": "https://vertex.fixture"}
							} else {
								auth.Metadata = map[string]any{"project_id": "fixture-project", "location": "global",
									"service_account": map[string]any{"type": "service_account", "project_id": "fixture-project",
										"client_email": "fixture@example.invalid", "private_key": privateKey, "token_uri": "https://token.fixture/oauth2/token"}}
							}
							ctx = util.WithRoundTripper(ctx, vertexUsageRoundTripFunc(func(httpReq *http.Request) (*http.Response, error) {
								body := responseBody
								if httpReq.URL.Host == "token.fixture" {
									if cliproxyexecutor.RequestProxyURL(httpReq.Context()) != "" {
										t.Error("token exchange retained the execution-scoped proxy")
									}
									body = `{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`
								} else {
									payload, errRead := io.ReadAll(httpReq.Body)
									if errRead != nil {
										return nil, errRead
									}
									upstreamBody <- payload
									upstreamURL <- httpReq.URL.String()
									if action == "streamGenerateContent" {
										body = "data: " + body + "\n\n"
									}
								}
								return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
									Body: io.NopCloser(strings.NewReader(body)), Request: httpReq}, nil
							}))
						}
						switch action {
						case "countTokens":
							response, errCount := exec.CountTokens(ctx, auth, req, opts)
							if errCount != nil {
								t.Fatal(errCount)
							}
							if gjson.GetBytes(response.Payload, "totalTokens").Int() != 4 {
								t.Fatalf("unexpected token count: %s", response.Payload)
							}
						case "streamGenerateContent":
							stream, errStream := exec.ExecuteStream(ctx, auth, req, opts)
							if errStream != nil {
								t.Fatal(errStream)
							}
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						default:
							if _, errExecute := exec.Execute(ctx, auth, req, opts); errExecute != nil {
								t.Fatal(errExecute)
							}
						}
						if req.Metadata["action"] != "generateContent" || req.Metadata["fixture"] != true {
							t.Fatalf("request metadata was mutated: %#v", req.Metadata)
						}
						var payload []byte
						select {
						case payload = <-upstreamBody:
						case <-ctx.Done():
							t.Fatal("no upstream request captured")
						}
						if endpoint := <-upstreamURL; !strings.Contains(endpoint, ":"+action) {
							t.Fatalf("endpoint = %q, want action %s", endpoint, action)
						}
						if match == "matched" {
							if gjson.GetBytes(payload, "generationConfig.maxOutputTokens").Int() != 24 || gjson.GetBytes(payload, "session_id").String() != "configured-session" {
								t.Errorf("configured payload lost before transport: %s", payload)
							}
						} else if gjson.GetBytes(payload, "session_id").Exists() || gjson.GetBytes(payload, "generationConfig.maxOutputTokens").Int() == 24 {
							t.Errorf("unmatched rule applied before transport: %s", payload)
						}
						wantImage := action != "countTokens" && requestPath == "/v1/images/generations"
						if gjson.GetBytes(payload, `tools.#(type=="image_generation")`).Exists() != wantImage {
							t.Errorf("request path filter lost before transport: %s", payload)
						}
						roles := []string{"user", "model"}
						if action != "countTokens" {
							roles = append(roles, "user")
						}
						assertGeminiBoundaryRoles(t, payload, roles)
					})
				}
			}
		}
	}
}

func TestVertexAccessTokenClearsExecutionProxy(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount, err := json.Marshal(map[string]any{
		"type": "service_account", "project_id": "fixture-project", "client_email": "fixture@example.invalid",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"token_uri":   "https://token.fixture/oauth2/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), "http://execution-proxy.fixture:8080")
	requests := 0
	ctx = util.WithRoundTripper(ctx, vertexUsageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Host != "token.fixture" || cliproxyexecutor.RequestProxyURL(req.Context()) != "" {
			t.Error("token exchange did not isolate the execution-scoped proxy")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`)), Request: req}, nil
	}))
	token, err := vertexAccessToken(ctx, nil, nil, serviceAccount)
	if err != nil {
		t.Fatal(err)
	}
	if token != "fixture-access" || requests != 1 {
		t.Fatalf("token exchange result = %q, requests = %d", token, requests)
	}
	if cliproxyexecutor.RequestProxyURL(ctx) != "http://execution-proxy.fixture:8080" {
		t.Fatal("token exchange mutated the model request context")
	}
}

func assertGeminiBoundaryRoles(t *testing.T, payload []byte, want []string) {
	t.Helper()
	contents := gjson.GetBytes(payload, "contents").Array()
	if len(contents) != len(want) {
		t.Fatalf("contents len = %d, want %d; payload=%s", len(contents), len(want), payload)
	}
	for index, role := range want {
		if got := contents[index].Get("role").String(); got != role {
			t.Fatalf("contents[%d].role = %q, want %q; payload=%s", index, got, role, payload)
		}
	}
}
