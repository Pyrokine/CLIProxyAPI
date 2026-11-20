package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditAntigravityUsesSingleSelectedEndpoint(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, operation := range []string{"execute", "count", "stream"} {
			t.Run(
				fmt.Sprintf("custom=%t/%s", custom, operation), func(t *testing.T) {
					credential := &auth.Auth{
						ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{
							"access_token": "synthetic-access", "project_id": "synthetic-project",
							"expired": time.Now().Add(2 * time.Hour).Format(time.RFC3339),
						},
					}
					wantURL := antigravityBaseURLDaily
					if custom {
						wantURL = "https://custom-antigravity.synthetic.invalid"
						credential.Attributes = map[string]string{"base_url": wantURL + "/"}
					}
					var targets []string
					ctx := util.WithRoundTripper(
						t.Context(), antigravityRoundTripFunc(
							func(request *http.Request) (*http.Response, error) {
								targets = append(targets, request.URL.Scheme+"://"+request.URL.Host)
								return &http.Response{
									StatusCode: http.StatusServiceUnavailable, Header: make(http.Header),
									Body: io.NopCloser(
										strings.NewReader(`{"error":{"message":"synthetic unavailable"}}`),
									),
									Request: request,
								}, nil
							},
						),
					)
					execute := NewAntigravityExecutor(&config.Config{})
					request := core.Request{
						Model: "gemini-2.5-flash",
						Payload: []byte(
							`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}}`,
						),
					}
					opts := core.Options{
						SourceFormat: translator.FormatAntigravity, ResponseFormat: translator.FormatAntigravity,
					}
					var err error
					if operation == "count" {
						_, err = execute.CountTokens(ctx, credential, request, opts)
					} else {
						err = auditFinalUsageCall(ctx, execute, credential, request, opts, operation == "stream")
					}
					if err == nil || len(targets) != 1 || targets[0] != wantURL {
						t.Fatalf("error=%v targets=%v want=%s", err, targets, wantURL)
					}
				},
			)
		}
	}
}

func TestAuditNativeInteractionsKeepsCallIdentity(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		for _, nativeID := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				t.Run(
					fmt.Sprintf(
						"%s/native-id=%t/stream=%t",
						provider,
						nativeID,
						streaming,
					),
					func(t *testing.T) {
						wantID := "legacy-call"
						idField := ""
						if nativeID {
							wantID = "native-call"
							idField = `,"id":"native-call"`
						}
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), geminiInteractionsRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									calls++
									body, err := io.ReadAll(request.Body)
									if err != nil {
										t.Error(err)
									}
									inputs := gjson.GetBytes(body, "input").Array()
									if len(inputs) != 2 {
										t.Errorf("input=%s", body)
									} else if inputs[0].Get("id").String() !=
										wantID ||
										inputs[0].Get("call_id").Exists() ||
										inputs[1].Get("call_id").String() !=
											wantID ||
										!inputs[1].Get("is_error").Bool() ||
										inputs[0].Get("name").String() !=
											"lookup" ||
										inputs[1].Get("name").String() !=
											"lookup" {
										t.Errorf("call/result identity or error state changed: %s", body)
									}
									wire := `{"interaction":{"id":"synthetic","status":"completed","steps":[]}}`
									if streaming {
										wire = ("data: {\"event_type\":\"interaction.completed\"," +
											"\"interaction\":" +
											"{\"id") +
											"\":\"synthetic\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n"
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						var execute auth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
						if provider == "vertex" {
							execute = NewGeminiVertexExecutor(&config.Config{})
						}
						credential := &auth.Auth{
							ID: t.Name(), Provider: provider, Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": "https://interactions.synthetic.invalid",
								"interactions": "true",
							},
						}
						request := core.Request{
							Model: "gemini-2.5-flash",
							Payload: []byte(
								`{"model":"gemini-2.5-flash","input":[{"type":"function_call",` +
									`"call_id":"legacy-call"` +
									idField +
									`,"name":"lookup","arguments":{}},{"type":"function_result","call_id":"` +
									wantID +
									`","name":"lookup","is_error":true,"result":"synthetic failure"}]}`,
							),
						}
						err := auditFinalUsageCall(
							ctx, execute, credential, request, core.Options{
								SourceFormat:   translator.FormatInteractions,
								ResponseFormat: translator.FormatInteractions,
							}, streaming,
						)
						if err != nil || calls != 1 {
							t.Fatalf("error=%v calls=%d", err, calls)
						}
					},
				)
			}
		}
	}
}
