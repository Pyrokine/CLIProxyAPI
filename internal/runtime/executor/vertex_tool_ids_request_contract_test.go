package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestVertexFinalRequestsNormalizeConfiguredToolIDs(t *testing.T) {
	serviceAccount := googleImagenServiceAccount(t)
	const contents = `[{"role":"user","parts":[{"text":"lookup"}]},{"role":"model","parts":[` +
		`{"functionCall":{"id":"call_1","name":"lookup","args":{"id":9007199254` +
		`740993}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_1"` +
		`,"name":"lookup","response":{"id":"keep"}}}]}]`
	for _, credential := range []string{"API key", "service account"} {
		for _, stream := range []bool{false, true} {
			for _, source := range []sdktranslator.Format{
				sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini,
			} {
				t.Run(
					fmt.Sprintf("%s/stream_%t/%s", credential, stream, source), func(t *testing.T) {
						cfg := &config.Config{
							Payload: config.PayloadConfig{
								OverrideRaw: []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{
												Name: "gemini-2.5-flash", Protocol: "gemini",
											},
										},
										Params: map[string]any{"contents": contents},
									},
								},
							},
						}
						tokenCalls, modelCalls := 0, 0
						var outgoing []byte
						ctx := util.WithRoundTripper(
							t.Context(), googlePayloadTransport(
								func(request *http.Request) (*http.Response, error) {
									if request.URL.Host == "imagen-token.invalid" &&
										request.URL.Path == "/token" {
										tokenCalls++
										response := googlePayloadResponse(
											request,
											`{"access_token":"synthetic-token","expires_in":3600,`+
												`"token_type":`+
												`"Bearer"}`,
										)
										response.Header.Set("Content-Type", "application/json")
										return response, nil
									}
									if !strings.HasSuffix(
										request.URL.Path, ":generateContent",
									) && !strings.HasSuffix(request.URL.Path, ":streamGenerateContent") {
										return nil, fmt.Errorf("unexpected fixture request: %s", request.URL)
									}
									modelCalls++
									var err error
									outgoing, err = io.ReadAll(request.Body)
									response := googlePayloadResponse(
										request, `{"error":{"message":"synthetic final request captured"}}`,
									)
									response.StatusCode = http.StatusBadRequest
									return response, err
								},
							),
						)
						auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "vertex"}
						if credential == "API key" {
							auth.Attributes = map[string]string{"api_key": "synthetic-key"}
						} else {
							auth.Metadata = map[string]any{
								"project_id": "synthetic-project", "location": "global",
								"service_account": serviceAccount,
							}
						}
						payload := []byte(`{"model":"gemini-2.5-flash","input":"lookup"}`)
						if source == sdktranslator.FormatGemini {
							payload = []byte(`{"contents":[{"role":"user","parts":[{"text":"lookup"}]}]}`)
						}
						executor := NewGeminiVertexExecutor(cfg)
						request := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload}
						options := cliproxyexecutor.Options{
							SourceFormat: source, ResponseFormat: sdktranslator.FormatGemini, Stream: stream,
						}
						var err error
						if stream {
							_, err = executor.ExecuteStream(ctx, auth, request, options)
						} else {
							_, err = executor.Execute(ctx, auth, request, options)
						}
						wantTokenCalls := 0
						if credential == "service account" {
							wantTokenCalls = 1
						}
						if err == nil || modelCalls != 1 || tokenCalls != wantTokenCalls {
							t.Fatalf("captured model=%d token=%d error=%v", modelCalls, tokenCalls, err)
						}
						wantID := source != sdktranslator.FormatOpenAIResponse
						call := gjson.GetBytes(outgoing, "contents.1.parts.0.functionCall")
						result := gjson.GetBytes(outgoing, "contents.2.parts.0.functionResponse")
						if call.Get("id").Exists() != wantID || result.Get("id").Exists() != wantID {
							t.Errorf("final tool IDs differ from source format policy: %s", outgoing)
						}
						if call.Get("name").String() != "lookup" ||
							call.Get("args.id").Raw != "9007199254740993" ||
							result.Get("response.id").String() != "keep" {
							t.Errorf("tool identity or nested business values changed: %s", outgoing)
						}
						if gjson.Get(
							cfg.Payload.OverrideRaw[0].Params["contents"].(string), "1.parts.0.functionCall.id",
						).String() != "call_1" {
							t.Fatal("request normalization changed the configured input")
						}
					},
				)
			}
		}
	}
}
