package auth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type auditCompatRoundTripper func(*http.Request) (*http.Response, error)

func (f auditCompatRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type auditActualCompatExecutor struct {
	*runtimeexecutor.OpenAICompatExecutor
	expect bool
	calls  int
}

func (e *auditActualCompatExecutor) check(req core.Request) error {
	e.calls++
	info, ok := auth.ResolvedModelInfo(req)
	if !ok || info.IsCompat != e.expect || req.Model != "deepseek-flash" {
		return fmt.Errorf(
			"resolved model=%+v present=%t requested=%s expected compat=%t", info, ok, req.Model, e.expect,
		)
	}
	return nil
}
func (e *auditActualCompatExecutor) Execute(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	if err := e.check(req); err != nil {
		return core.Response{}, err
	}
	return e.OpenAICompatExecutor.Execute(ctx, a, req, opts)
}
func (e *auditActualCompatExecutor) ExecuteStream(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	if err := e.check(req); err != nil {
		return nil, err
	}
	return e.OpenAICompatExecutor.ExecuteStream(ctx, a, req, opts)
}
func (e *auditActualCompatExecutor) CountTokens(
	ctx context.Context,
	a *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	if err := e.check(req); err != nil {
		return core.Response{}, err
	}
	return e.OpenAICompatExecutor.CountTokens(ctx, a, req, opts)
}

func TestAuditConfiguredCompatibilityReachesActualExecutor(t *testing.T) {
	for _, model := range registry.GetDeepSeekModels() {
		if model.IsCompat {
			t.Fatalf("static DeepSeek model %s enabled explicit compatibility mode", model.ID)
		}
	}
	for _, compat := range []bool{false, true} {
		for _, operation := range []string{"execute", "count", "stream"} {
			t.Run(
				fmt.Sprintf("compat=%t/%s", compat, operation), func(t *testing.T) {
					provider := fmt.Sprintf("audit-deepseek-%t-%s", compat, operation)
					cfg := &config.Config{
						OpenAICompatibility: []config.OpenAICompatibility{
							{
								Name: provider, BaseURL: "https://deepseek.synthetic.invalid/v1",
								APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-key"}},
								Models: []config.OpenAICompatibilityModel{
									{
										Name: "deepseek-flash", Alias: "public-deepseek", IsCompat: compat,
									},
								},
							},
						},
					}
					credentials, err := synthesizer.NewConfigSynthesizer().Synthesize(
						&synthesizer.SynthesisContext{
							Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
						},
					)
					if err != nil || len(credentials) != 1 ||
						cfg.OpenAICompatibility[0].Models[0].GetIsCompat() != compat {
						t.Fatalf("synthesis=%v error=%v", credentials, err)
					}
					provider = credentials[0].Provider
					manager := auth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					manager.SetConfig(cfg)
					execute := &auditActualCompatExecutor{
						OpenAICompatExecutor: runtimeexecutor.NewOpenAICompatExecutor(
							provider, cfg,
						), expect: compat,
					}
					manager.RegisterExecutor(execute)
					credential, err := manager.Register(t.Context(), credentials[0])
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(
						credential.ID, provider, []*registry.ModelInfo{
							{
								ID: "public-deepseek", CatalogName: "deepseek-flash",
								IsCompat: cfg.OpenAICompatibility[0].Models[0].GetIsCompat(),
							},
						},
					)
					t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
					manager.RefreshSchedulerEntry(credential.ID)
					transportCalls := 0
					ctx := util.WithRoundTripper(
						t.Context(), auditCompatRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								transportCalls++
								body, err := io.ReadAll(request.Body)
								if err != nil || gjson.GetBytes(body, "model").String() != "deepseek-flash" {
									t.Errorf("body=%s error=%v", body, err)
								}
								foundReasoning := false
								for _, message := range gjson.GetBytes(body, "messages").Array() {
									if message.Get("role").String() == "assistant" {
										foundReasoning = message.Get("reasoning_content").
											String() ==
											"synthetic reasoning"
									}
								}
								if foundReasoning != compat {
									t.Errorf("compat=%t reasoning=%t body=%s", compat, foundReasoning, body)
								}
								wire := `{"id":"synthetic","model":"deepseek-flash","choices":[{"index":0,"mess` +
									`age":{"role":"assistant","content":"synthetic"},"finish_reason":"stop"` +
									`}]}`
								if operation == "stream" {
									wire = "data: {\"id\":\"synthetic\",\"model\":\"deepseek-flash\",\"choices\":[" +
										"{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\"finish_reason\":" +
										"\"stop\"}]}\n\ndata: [DONE]\n\n"
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						),
					)
					request := core.Request{
						Model: "public-deepseek",
						Payload: []byte((`{"model":"public-deepseek","max_tokens":32,"messages":[{"role":"` +
							`user","content":"synthetic"},{"role":"assistant","content":[{"type":"thinking","` +
							`thinking":"synthetic reasoning","signature":"synthetic-anthropic-signature"},{"` +
							`type":"text","text":"answer"}]},{"role":"user","content":"continue"}]}`)),
					}
					opts := core.Options{SourceFormat: translator.FormatClaude, ResponseFormat: translator.FormatOpenAI}
					switch operation {
					case "execute":
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
					wantTransport := 1
					if operation == "count" {
						wantTransport = 0
					}
					if err != nil || execute.calls != 1 || transportCalls != wantTransport {
						t.Fatalf("error=%v executor calls=%d transport calls=%d", err, execute.calls, transportCalls)
					}
				},
			)
		}
	}
}
