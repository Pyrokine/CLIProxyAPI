package cliproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type overrideTrackingExecutor struct {
	customOwnedExecutor
	executions int
	closed     []string
}

func (e *overrideTrackingExecutor) Execute(
	context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executions++
	return cliproxyexecutor.Response{Payload: []byte(`{"owner":"synthetic-sdk"}`)}, nil
}

func (e *overrideTrackingExecutor) CloseExecutionSession(id string) {
	e.closed = append(e.closed, id)
}

func TestServiceExecutorInjectionForceAndConfigReloadReachActualSDKConsumer(t *testing.T) {
	for _, namespaced := range []bool{false, true} {
		name, provider := "private-sdk", "private-sdk"
		if namespaced {
			name, provider = "kimi", "openai-compatible-kimi"
		}
		t.Run(
			provider, func(t *testing.T) {
				cfg := &config.Config{
					OpenAICompatibility: []config.OpenAICompatibility{
						{
							Name: name, BaseURL: "https://override-fixture.invalid/v1",
							Models: []config.OpenAICompatibilityModel{
								{
									Name: "private-override-upstream", Alias: "override-alias",
								},
							},
						},
					},
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.SetConfig(cfg)
				custom := &overrideTrackingExecutor{id: provider}
				manager.RegisterExecutor(custom)
				attributes := map[string]string{
					"api_key": "synthetic-override-key", "base_url": cfg.OpenAICompatibility[0].BaseURL,
				}
				if namespaced {
					attributes["compat_name"], attributes["provider_key"] = name, provider
				}
				auth, err := manager.Register(
					coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
						ID: "override-" + provider, Provider: provider, Status: coreauth.StatusActive,
						Attributes: attributes,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				service := &Service{cfg: cfg, coreManager: manager}
				service.registerModelsForAuthWithConfig(auth, cfg)
				manager.RefreshSchedulerEntry(auth.ID)
				t.Cleanup(
					func() {
						GlobalModelRegistry().UnregisterClient(auth.ID)
						_ = manager.StopDeferredAuthRecreates(context.Background())
					},
				)
				request := cliproxyexecutor.Request{
					Model: "override-alias",
					Payload: []byte(
						`{"model":"override-alias","messages":[{"role":"user","content":"hello"}],"temperature":2}`,
					),
				}
				options := cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI,
					Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID},
				}
				assertCustom := func() {
					t.Helper()
					bound, _ := manager.Executor(provider)
					if bound != custom || len(custom.closed) != 0 {
						t.Fatal("ordinary synchronization replaced or closed the public SDK executor")
					}
					response, err := manager.Execute(t.Context(), []string{provider}, request, options)
					if err != nil || string(response.Payload) != `{"owner":"synthetic-sdk"}` {
						t.Fatalf("injected SDK execution error=%v payload=%s", err, response.Payload)
					}
				}
				service.ensureExecutorsForAuth(auth)
				assertCustom()
				service.rebindExecutors()
				assertCustom()
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				if service.ensureExecutorsForAuthWithConfigContext(canceled, auth, true, cfg) {
					t.Fatal("canceled forceReplace published an executor")
				}
				if bound, _ := manager.Executor(provider); bound != custom || len(custom.closed) != 0 {
					t.Fatal("canceled forceReplace replaced or closed the SDK executor")
				}
				service.ensureExecutorsForAuthWithMode(auth, true)
				bound, _ := manager.Executor(provider)
				if _, ok := bound.(*runtimeexecutor.OpenAICompatExecutor); !ok || bound == custom {
					t.Fatalf("explicit forceReplace retained %T instead of the compatibility executor", bound)
				}
				if len(custom.closed) != 1 || custom.closed[0] != coreauth.CloseAllExecutionSessionsID {
					t.Fatalf("replaced SDK execution-session cleanup=%v", custom.closed)
				}
				calls := 0
				expectedModel := request.Model
				if namespaced {
					expectedModel = "private-override-upstream"
				}
				expectedMarker := ""
				transport := &configuredProviderTransport{
					respond: func(request *http.Request) (*http.Response, error) {
						calls++
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						if request.URL.String() != "https://override-fixture.invalid/v1/chat/completions" ||
							gjson.GetBytes(payload, "model").String() != expectedModel ||
							gjson.GetBytes(payload, "temperature").Raw != "2" ||
							gjson.GetBytes(payload, "metadata.override_fixture").String() != expectedMarker {
							t.Errorf("forced compatibility wire route=%s payload=%s", request.URL, payload)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
							Body: io.NopCloser(
								strings.NewReader(
									`{"id":"synthetic","choices":[{"message":{"role":"assistant","content":` +
										`"answer"},"finish_reason":` +
										`"stop"}]}`,
								),
							),
							Request: request,
						}, nil
					},
				}
				ctx := util.WithRoundTripper(t.Context(), transport)
				if auth.AuthKind() != coreauth.AuthKindAPIKey || runtimeexecutor.NewProxyAwareHTTPClient(
					ctx, cfg, auth, 0,
				).Transport != transport {
					t.Fatal(
						"actual compatibility client is not isolated to the synthetic API key and memory transport",
					)
				}
				manager.SetRoundTripperProvider(configuredProviderTransportSource{transport: transport})
				if _, err := manager.Execute(ctx, []string{provider}, request, options); err != nil ||
					calls != 1 {
					t.Fatalf("forced SDK consumer error=%v memory requests=%d", err, calls)
				}
				previous := bound
				nextConfig := *cfg
				nextConfig.Payload.Override = []config.PayloadRule{
					{
						Models: []config.PayloadModelRule{{Name: "*", Protocol: "openai"}},
						Params: map[string]any{"metadata.override_fixture": "reloaded"},
					},
				}
				service.cfgMu.Lock()
				service.cfg = &nextConfig
				service.cfgMu.Unlock()
				manager.SetConfig(&nextConfig)
				expectedMarker = "reloaded"
				service.rebindExecutors()
				bound, _ = manager.Executor(provider)
				if _, ok := bound.(*runtimeexecutor.OpenAICompatExecutor); !ok || bound == previous {
					t.Fatal("configuration rebind failed to replace the Service-owned compatibility executor")
				}
				if _, err := manager.Execute(
					ctx, []string{provider}, request, options,
				); err != nil || calls != 2 || custom.executions != 2 {
					t.Fatalf(
						"rebound SDK consumer error=%v memory requests=%d SDK executions=%d", err, calls,
						custom.executions,
					)
				}
			},
		)
	}
}
