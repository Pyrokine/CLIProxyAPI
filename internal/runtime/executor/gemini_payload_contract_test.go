package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type googlePayloadTransport func(*http.Request) (*http.Response, error)

func (f googlePayloadTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func googlePayloadResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req,
	}
}

func googlePayloadAuth(provider string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "payload-contract-" + provider, Provider: provider,
		Attributes: map[string]string{
			"api_key": "synthetic-key", "base_url": "https://payload-contract.invalid", "interactions": "true",
		},
		Metadata: map[string]any{
			"access_token": "synthetic-token", "project_id": "synthetic-project",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

// googlePayloadCall observes the bytes consumed by the HTTP transport, not an intermediate payload.
func googlePayloadCall(
	t *testing.T,
	provider, route, model string,
	cfg *config.Config,
	payload []byte,
	opts cliproxyexecutor.Options,
) []byte {
	t.Helper()
	var captured []byte
	calls := 0
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = util.WithRoundTripper(
		ctx, googlePayloadTransport(
			func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "payload-contract.invalid" {
					return nil, fmt.Errorf("unexpected upstream host %q", req.URL.Host)
				}
				var err error
				captured, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"fi` +
					`nishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidates` +
					`TokenCount":1,"totalTokenCount":3}}`
				if provider == "antigravity" {
					response = `{"response":` + response + `}`
				}
				if route == "count" {
					if !strings.HasSuffix(req.URL.Path, ":countTokens") {
						return nil, fmt.Errorf("count URL %s", req.URL)
					}
					response = `{"totalTokens":7}`
				} else if strings.Contains(req.URL.Path, "streamGenerateContent") {
					response = "data: " + response + "\n\n"
				}
				return googlePayloadResponse(req, response), nil
			},
		),
	)
	var exec cliproxyauth.ProviderExecutor
	switch provider {
	case "gemini":
		exec = NewGeminiExecutor(cfg)
	case "vertex":
		exec = NewGeminiVertexExecutor(cfg)
	case "antigravity":
		exec = NewAntigravityExecutor(cfg)
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	req := cliproxyexecutor.Request{Model: model, Payload: payload}
	snapshot := bytes.Clone(payload)
	var err error
	switch route {
	case "generate", "aggregate":
		_, err = exec.Execute(ctx, googlePayloadAuth(provider), req, opts)
	case "stream":
		var result *cliproxyexecutor.StreamResult
		result, err = exec.ExecuteStream(ctx, googlePayloadAuth(provider), req, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
	case "count":
		_, err = exec.CountTokens(ctx, googlePayloadAuth(provider), req, opts)
	default:
		t.Fatalf("unknown route %q", route)
	}
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.Equal(snapshot, payload) {
		t.Fatalf("calls=%d, caller payload unchanged=%t", calls, bytes.Equal(snapshot, payload))
	}
	return captured
}

func TestGeminiExecutorsEncodeAlt(t *testing.T) {
	const model = "gemini-2.5-flash"
	const alt = "json + profile"
	cfg := &config.Config{}
	serviceAccount := &cliproxyauth.Auth{
		Provider: "vertex", Metadata: map[string]any{
			"project_id": "fixture-project", "location": "europe-west12",
			"service_account": googleImagenServiceAccount(t),
		},
	}
	observed := errors.New("offline upstream request captured")
	for _, provider := range []struct {
		name, host, path string
		exec             cliproxyauth.ProviderExecutor
	}{
		{"gemini", "payload-contract.invalid", "/v1beta/models/" + model, NewGeminiExecutor(cfg)},
		{
			"vertex-api-key", "payload-contract.invalid", "/v1/publishers/google/models/" + model,
			NewGeminiVertexExecutor(cfg),
		},
		{
			"vertex-service-account", "europe-west12-aiplatform.googleapis.com",
			"/v1/projects/fixture-project/locations/europe-west12/publishers/google/models/" + model,
			NewGeminiVertexExecutor(cfg),
		},
		{"gemini-cli", "cloudcode-pa.googleapis.com", "/v1internal", NewGeminiCLIExecutor(cfg)},
	} {
		for _, tc := range []struct{ name, action, alt, key, value string }{
			{"generate", "generateContent", alt, "$alt", alt},
			{"stream-default", "streamGenerateContent", "", "alt", "sse"},
			{"stream-explicit", "streamGenerateContent", alt, "$alt", alt},
			{"count", "countTokens", alt, "$alt", alt},
		} {
			t.Run(
				provider.name+"/"+tc.name, func(t *testing.T) {
					credential := googlePayloadAuth(provider.exec.Identifier())
					if provider.name == "vertex-service-account" {
						credential = serviceAccount
					}
					ctx := util.WithRoundTripper(
						t.Context(), googlePayloadTransport(
							func(req *http.Request) (*http.Response, error) {
								if provider.name == "vertex-service-account" && req.URL.String() == testVertexTokenURL {
									return googlePayloadResponse(
										req, `{"access_token":"fixture-token","expires_in":3600}`,
									), nil
								}
								if req.URL.Host != provider.host || req.URL.Path != provider.path+":"+tc.action {
									t.Fatalf(
										"upstream URL = %s, want %s%s:%s", req.URL, provider.host, provider.path,
										tc.action,
									)
								}
								query := req.URL.Query()
								if tc.action == "countTokens" && provider.name != "gemini-cli" {
									if len(query) != 0 {
										t.Fatalf("unexpected countTokens query: %v", query)
									}
								} else if len(query) != 1 || len(query[tc.key]) != 1 || query.Get(tc.key) != tc.value {
									t.Fatalf("query = %v, want exactly %s=%q", query, tc.key, tc.value)
								}
								return nil, observed
							},
						),
					)
					req := cliproxyexecutor.Request{
						Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`),
					}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Alt: tc.alt}
					var err error
					switch tc.action {
					case "generateContent":
						_, err = provider.exec.Execute(ctx, credential, req, opts)
					case "streamGenerateContent":
						opts.Stream = true
						_, err = provider.exec.ExecuteStream(ctx, credential, req, opts)
					case "countTokens":
						_, err = provider.exec.CountTokens(ctx, credential, req, opts)
					}
					if !errors.Is(err, observed) {
						t.Fatalf("executor did not reach offline upstream: %v", err)
					}
				},
			)
		}
	}
}

func TestGeminiStringifiedToolMediaReachesEveryExecutionEntry(t *testing.T) {
	payload := []byte(`{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
		`l_id":"call_1","name":"screenshot","arguments":"{}"},{"type":"function` +
		`_call_output","call_id":"call_1","output":"[{\"type\":\"input_text\",\` +
		`"text\":\"done\"},{\"type\":\"input_image\",\"image_url\":\"data:image` +
		`/jpeg;base64,/9j/4AAQSkZJRg==\"}]"}]}`)
	for _, provider := range []string{"gemini", "vertex", "antigravity"} {
		for _, route := range []string{"generate", "stream", "count"} {
			t.Run(
				provider+"/"+route, func(t *testing.T) {
					body := googlePayloadCall(
						t, provider, route, "gemini-3.7-flash-high", &config.Config{}, payload,
						cliproxyexecutor.Options{
							SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload,
							Stream: route == "stream",
						},
					)
					prefix := ""
					if provider == "antigravity" {
						prefix = "request."
					}
					var responses []gjson.Result
					for _, content := range gjson.GetBytes(body, prefix+"contents").Array() {
						for _, part := range content.Get("parts").Array() {
							if response := part.Get("functionResponse"); response.Exists() {
								responses = append(responses, response)
							}
						}
					}
					if len(responses) != 1 || responses[0].Get("name").String() != "screenshot" ||
						responses[0].Get("response.result").String() != "done" {
						t.Fatalf("final tool output changed ownership or text: %s", body)
					}
					if provider == "vertex" {
						if responses[0].Get("id").Exists() {
							t.Fatalf("Vertex Responses output retained a tool ID: %s", body)
						}
					} else if responses[0].Get("id").String() != "call_1" {
						t.Fatalf("tool output ID changed: %s", body)
					}
					media := responses[0].Get("parts").Array()
					if len(media) != 1 || media[0].Get("inlineData.mimeType").String() != "image/jpeg" ||
						media[0].Get("inlineData.data").String() != "/9j/4AAQSkZJRg==" {
						t.Fatalf("final upstream body lost the tool image: %s", body)
					}
				},
			)
		}
	}
}

func TestGeminiPayloadContentIndices(t *testing.T) {
	for _, route := range []string{"generate", "stream"} {
		for _, role := range []string{"model", "user"} {
			t.Run(
				route+"/"+role, func(t *testing.T) {
					models := []config.PayloadModelRule{{Name: "gemini-2.5-flash", Protocol: "gemini"}}
					cfg := &config.Config{
						Payload: config.PayloadConfig{
							Default: []config.PayloadRule{
								{
									Models: models, Params: map[string]any{"generationConfig.temperature": 0.25},
								},
							},
							Override: []config.PayloadRule{
								{
									Models: models, Params: map[string]any{
										"contents.0.parts.0.text": "configured", "generationConfig.topP": 0.9,
									},
								},
							},
							Filter: []config.PayloadFilterRule{
								{
									Models: models, Params: []string{"generationConfig.topP"},
								},
							},
						},
					}
					payload := []byte(fmt.Sprintf(
						`{"contents":[{"role":%q,"parts":[{"text":"history"}]}],`+
							`"generationConfig":{"temperature":`+
							`0.4}}`,
						role,
					))
					body := googlePayloadCall(
						t, "gemini", route, "gemini-2.5-flash", cfg, payload,
						cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
					)
					if gjson.GetBytes(body, "contents.0.role").String() != "user" || gjson.GetBytes(
						body, "contents.0.parts.0.text",
					).String() != "configured" {
						t.Fatalf("final first user content = %s", body)
					}
					if role == "model" && (gjson.GetBytes(
						body, "contents.1.role",
					).String() != "model" || gjson.GetBytes(body, "contents.1.parts.0.text").String() != "history") {
						t.Fatalf("model history changed: %s", body)
					}
					if gjson.GetBytes(body, "generationConfig.temperature").Float() != 0.4 || gjson.GetBytes(
						body, "generationConfig.topP",
					).Exists() {
						t.Fatalf("default/filter semantics changed: %s", body)
					}
				},
			)
		}
	}
}

func TestGeminiPayloadSourceProtocol(t *testing.T) {
	for _, source := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"original"}]}]}`},
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"original"}]}`},
		{sdktranslator.FormatOpenAIResponse, `{"input":"original"}`},
	} {
		for _, route := range []string{"generate", "stream"} {
			for _, matching := range []bool{true, false} {
				t.Run(
					fmt.Sprintf("%s/%s/%t", source.format, route, matching), func(t *testing.T) {
						from := source.format.String()
						if !matching {
							from = "claude"
						}
						cfg := &config.Config{
							Payload: config.PayloadConfig{
								Override: []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{
												Name: "gemini-2.5-flash", Protocol: "gemini", FromProtocol: from,
												Headers: map[string]string{"X-Payload-Test": "match"},
											},
										},
										Params: map[string]any{"contents.0.parts.0.text": "configured"},
									},
								},
							},
						}
						body := googlePayloadCall(
							t,
							"gemini",
							route,
							"gemini-2.5-flash",
							cfg,
							[]byte(source.body),
							cliproxyexecutor.Options{
								SourceFormat: source.format, Headers: http.Header{"X-Payload-Test": {"match"}},
							},
						)
						want := "original"
						if matching {
							want = "configured"
						}
						if got := gjson.GetBytes(body, "contents.0.parts.0.text").String(); got != want {
							t.Fatalf("text=%q want=%q body=%s", got, want, body)
						}
					},
				)
			}
		}
	}
}

func TestGeminiTokenCountPayloadRules(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex"} {
		for _, kind := range []string{"override", "default", "filter", "none"} {
			t.Run(
				provider+"/"+kind, func(t *testing.T) {
					models := []config.PayloadModelRule{
						{
							Name: "gemini-2.5-flash", Protocol: "gemini", FromProtocol: "gemini",
						},
					}
					cfg := &config.Config{}
					payload := `{"contents":[{"role":"user","parts":[{"text":"original"}]}],"` +
						`systemInstruction":{"parts":[{"text":"original-system"}]},"generationConfig":{"` +
						`temperature":0.1}}`
					wantText, wantSystem := "original", "original-system"
					switch kind {
					case "override":
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{
									"contents.0.parts.0.text":        "configured",
									"systemInstruction.parts.0.text": "configured-system",
									"generationConfig.temperature":   0.2,
									"tools": []any{
										map[string]any{"googleSearch": map[string]any{}},
									},
									"safetySettings": []any{map[string]any{"category": "configured"}},
								},
							},
						}
						wantText, wantSystem = "configured", "configured-system"
					case "default":
						payload = `{"contents":[{"role":"user","parts":[{"text":"original"}]}]}`
						cfg.Payload.Default = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{
									"contents.0.parts.0.text":        "unused",
									"systemInstruction.parts.0.text": "configured-system",
								},
							},
						}
						wantSystem = "configured-system"
					case "filter":
						cfg.Payload.Filter = []config.PayloadFilterRule{
							{
								Models: models, Params: []string{"systemInstruction"},
							},
						}
						wantSystem = ""
					}
					body := googlePayloadCall(
						t, provider, "count", "gemini-2.5-flash", cfg, []byte(payload),
						cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
					)
					if got := gjson.GetBytes(body, "contents.0.parts.0.text").String(); got != wantText {
						t.Fatalf("text=%q want=%q body=%s", got, wantText, body)
					}
					if got := gjson.GetBytes(body, "systemInstruction.parts.0.text").String(); got != wantSystem {
						t.Fatalf("system=%q want=%q body=%s", got, wantSystem, body)
					}
					for _, field := range []string{"generationConfig", "tools", "safetySettings"} {
						if gjson.GetBytes(body, field).Exists() != (kind == "override") {
							t.Fatalf("count cleanup/config precedence for %s: %s", field, body)
						}
					}
					if kind == "override" &&
						(gjson.GetBytes(body, "generationConfig.temperature").Float() != 0.2 ||
							!gjson.GetBytes(body, "tools.0.googleSearch").Exists() || gjson.GetBytes(
							body, "safetySettings.0.category",
						).String() != "configured") {
						t.Fatalf("explicit count configuration lost before transport: %s", body)
					}
				},
			)
		}
	}
}
