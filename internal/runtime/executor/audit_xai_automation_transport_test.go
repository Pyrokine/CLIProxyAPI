package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditXAIAutomationSchemaReachesActualTransport(t *testing.T) {
	for _, kind := range []string{"qualified", "namespace", "unrelated", "namespace-case", "custom"} {
		for _, union := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/union=%t/stream=%t", kind, union, streaming), func(t *testing.T) {
						parameters := `{"type":"object","properties":{"mode":{"type":"string"}},` +
							`"additionalProperties":` +
							`false}`
						if union {
							parameters = `{"oneOf":[{"type":"object","properties":{"mode":{"type":` +
								`"string"}}}],"$defs":{"mode":{"type":` +
								`"string"}}}`
						}
						tool := `{"type":"function","name":"codex_app__automation_update","strict":true` +
							`,"description":"synthetic automation","parameters":` + parameters + `}`
						if kind != "qualified" {
							tool = `{"type":"function","name":"automation_update","strict":true,` +
								`"description":"synthetic automation","parameters":` +
								parameters +
								`}`
						}
						if kind == "namespace" || kind == "namespace-case" {
							namespace := "codex_app"
							if kind == "namespace-case" {
								namespace = "Codex_App"
							}
							tool = `{"type":"namespace","name":"` + namespace + `","tools":[` + tool + `]}`
						}
						if kind == "custom" {
							tool = `{"type":"custom","name":"codex_app__automation_update","strict":true,"` +
								`description":"synthetic automation","parameters":` + parameters + `}`
						}
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									calls++
									body, err := io.ReadAll(request.Body)
									if err != nil {
										t.Error(err)
									}
									tools := gjson.GetBytes(body, "tools").Array()
									if len(tools) != 1 {
										t.Errorf("tools=%s", body)
									} else {
										actual := tools[0]
										if actual.Get("description").String() != "synthetic automation" {
											t.Errorf("tool description=%s", actual.Raw)
										}
										if kind == "qualified" || kind == "namespace" {
											if actual.Get("parameters").Raw !=
												xaiSafeFunctionParameters ||
												actual.Get("strict").Type !=
													gjson.False {
												t.Errorf("automation schema=%s", actual.Raw)
											}
										} else {
											path := "parameters.properties.mode.type"
											if union {
												path = "parameters.oneOf.0.properties.mode.type"
											}
											if actual.Get(path).String() !=
												"string" ||
												actual.Get("strict").Type !=
													gjson.True {
												t.Errorf("unrelated schema=%s", actual.Raw)
											}
										}
									}
									wire := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" +
										"synthetic\",\"object\":\"response\",\"status\":\"completed\",\"" +
										"model\":\"grok-4.6\",\"output\":[]}}\n\n"
									return &http.Response{
										StatusCode: http.StatusOK,
										Header:     http.Header{"Content-Type": {"text/event-stream"}},
										Body:       io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						credential := &auth.Auth{
							ID: t.Name(), Provider: "xai", Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": "https://xai.synthetic.invalid",
							},
						}
						err := auditFinalUsageCall(
							ctx, newXAIExecutor(&config.Config{}), credential, core.Request{
								Model: "grok-4.6",
								Payload: []byte(
									`{"model":"grok-4.6","input":"synthetic","tools":[` + tool + `]}`,
								),
							}, core.Options{
								SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
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
