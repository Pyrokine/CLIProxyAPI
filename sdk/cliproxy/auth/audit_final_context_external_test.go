package auth_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditContext1MUsesFinalConfiguredPayloadTarget(t *testing.T) {
	for _, context1M := range []bool{false, true} {
		for _, final := range []string{"gpt-5.4", "gpt-4.1-mini", "gpt-5.4-pro", "unknown-final-model"} {
			for _, operation := range []string{"execute", "count", "stream", "image"} {
				t.Run(
					fmt.Sprintf("1m=%t/final=%s/%s", context1M, final, operation), func(t *testing.T) {
						const provider = "audit-final-context"
						cfg := &config.Config{
							OpenAICompatibility: []config.OpenAICompatibility{
								{
									Name: provider, BaseURL: "https://context.synthetic.invalid/v1",
									APIKeyEntries: []config.OpenAICompatibilityAPIKey{
										{APIKey: "synthetic-key"},
									},
									Models: []config.OpenAICompatibilityModel{
										{
											Name: "gpt-5.4", Alias: "public-capable",
										}, {
											Name: "gpt-4.1-mini", Alias: "public-small",
										}, {
											Name: "gpt-5.4-pro", Alias: "public-other-capable",
										},
									},
								},
							},
						}
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{{Name: "gpt-5.4"}},
								Params: map[string]any{"model": final},
							},
						}
						manager := auth.NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
						manager.SetConfig(cfg)
						manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(provider, cfg))
						credential, err := manager.Register(
							t.Context(), &auth.Auth{
								ID: t.Name(), Provider: provider, Attributes: map[string]string{
									"api_key": "synthetic-key", "base_url": "https://context.synthetic.invalid/v1",
									"compat_name": provider, "provider_key": provider, "config_index": "0",
								},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						reg := registry.GetGlobalRegistry()
						reg.RegisterClient(
							credential.ID, provider, []*registry.ModelInfo{
								{ID: "public-capable", CatalogName: "gpt-5.4", ContextLength: 1_000_000},
								{ID: "public-small", CatalogName: "gpt-4.1-mini", ContextLength: 128_000},
								{
									ID: "public-other-capable", CatalogName: "gpt-5.4-pro", ContextLength: 128_000,
									MaxContextLength: 1_000_000,
								},
							},
						)
						t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
						manager.RefreshSchedulerEntry(credential.ID)
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), auditCompatRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									calls++
									body, readErr := io.ReadAll(request.Body)
									if readErr != nil || gjson.GetBytes(body, "model").String() != final {
										t.Errorf("body=%s error=%v", body, readErr)
									}
									wire := (`{"id":"synthetic","model":` +
										`"`) +
										final +
										(`","choices":[{"index":0,` +
											`"message":{"role":` +
											`"assistant","content":` +
											`"synthetic"},` +
											`"finish_reason":` +
											`"stop"}]}`)
									if operation == "image" {
										wire = `{"created":1,"data":[{"b64_json":"AA=="}]}`
									}
									if operation == "stream" {
										wire = ("data: {\"id\":\"synthetic\",\"model\":" +
											"\"") +
											final +
											("\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" +
												"synthetic\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n" +
												"\n")
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						model := "public-capable"
						if context1M {
							model += "[1m]"
						}
						request := core.Request{
							Model: model,
							Payload: []byte(
								`{"model":"` + model + `","messages":[{"role":"user","content":"synthetic"}]}`,
							),
						}
						opts := core.Options{
							SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI,
						}
						if operation == "image" {
							opts.SourceFormat = "openai-image"
							opts.Alt = "images/generations"
							request.Payload = []byte(`{"model":"` + model + `","prompt":"synthetic"}`)
						}
						switch operation {
						case "execute", "image":
							_, err = manager.Execute(ctx, []string{provider}, request, opts)
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{provider}, request, opts)
						case "stream":
							var result *core.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{provider}, request, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						}
						rejected := context1M && (final == "gpt-4.1-mini" || final == "unknown-final-model")
						if rejected {
							status, ok := err.(interface{ StatusCode() int })
							if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
								t.Fatalf("final target bypassed 1M requirement: error=%v calls=%d", err, calls)
							}
						} else {
							wantCalls := 1
							if operation == "count" {
								wantCalls = 0
							}
							if err != nil || calls != wantCalls {
								t.Fatalf("valid final target error=%v calls=%d", err, calls)
							}
						}
					},
				)
			}
		}
	}
}
