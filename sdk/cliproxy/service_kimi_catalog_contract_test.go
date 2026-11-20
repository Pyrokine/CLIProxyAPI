package cliproxy

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiFixedCatalogReachesClientAndExecutor(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai", "kimi.com", "kimi.ai"} {
		t.Run(
			provider, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				cfg := &config.Config{}
				service := &Service{cfg: cfg, coreManager: manager}
				auth := &coreauth.Auth{
					ID: "fixed-catalog-" + provider, Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{
						"auth_kind": coreauth.AuthKindOAuth, "access_token": "synthetic-kimi-token",
					},
				}
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
				if err := service.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(t.Context()), auth); err != nil {
					t.Fatal(err)
				}
				stored, exists := manager.GetByID(auth.ID)
				if !exists {
					t.Fatal("registered auth is missing")
				}
				calls := 0
				var captured []byte
				transport := &configuredProviderTransport{
					respond: func(request *http.Request) (*http.Response, error) {
						calls++
						var err error
						captured, err = io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
							Body: io.NopCloser(
								strings.NewReader(
									`{"id":"synthetic","object":"chat.completion","model":"synthetic","` +
										`choices":[{"index":0,"message":{"role":"assistant","content":"` +
										`answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"` +
										`completion_tokens":1,"total_tokens":2}}`,
								),
							),
							Request: request,
						}, nil
					},
				}
				manager.SetRoundTripperProvider(configuredProviderTransportSource{transport: transport})
				ctx := util.WithRoundTripper(t.Context(), transport)
				for _, test := range []struct{ model, upstream string }{
					{"kimi-k2.8", "kimi-for-coding"}, {"kimi-k2.8-code", "kimi-for-coding"},
					{"kimi-k3-256k", "k3-256k"},
				} {
					want := registry.LookupStaticModelInfoByChannel(test.model, "kimi")
					got := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, test.model)
					if want == nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("registered %s metadata = %+v, want %+v", test.model, got, want)
					}
					visible := false
					for _, model := range GlobalModelRegistry().GetAvailableModels("openai") {
						if model["id"] == test.model {
							visible = true
						}
					}
					if !visible {
						t.Fatalf("client catalog omits %s", test.model)
					}
					before := calls
					_, err := manager.Execute(
						ctx, []string{stored.Provider}, cliproxyexecutor.Request{
							Model: test.model,
							Payload: []byte(
								`{"model":"` + test.model + `","messages":[{"role":"user","content":"hi"}]}`,
							),
						}, cliproxyexecutor.Options{
							SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI,
							Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID},
						},
					)
					if err != nil || calls != before+1 {
						t.Fatalf("%s execution error=%v requests=%d", test.model, err, calls-before)
					}
					if got := gjson.GetBytes(captured, "model").String(); got != test.upstream {
						t.Fatalf("%s wire model = %q, want %q", test.model, got, test.upstream)
					}
				}
				service.registerModelsForAuthWithConfig(
					&coreauth.Auth{
						ID: auth.ID, Provider: stored.Provider, Disabled: true,
					}, cfg,
				)
				if models := GlobalModelRegistry().GetModelsForClient(auth.ID); len(models) != 0 {
					t.Fatal("disabled auth still publishes models")
				}
			},
		)
	}
}

func TestKimiFixedCatalogHonorsExclusionAndAlias(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai"} {
		t.Run(
			provider, func(t *testing.T) {
				cfg := &config.Config{
					OAuthExcludedModels: map[string][]string{provider: {"kimi-k2.8-code"}},
					OAuthModelAlias: map[string][]config.OAuthModelAlias{
						provider: {
							{
								Name: "kimi-k3-256k", Alias: "fixed-256k-alias",
							},
						},
					},
				}
				auth := &coreauth.Auth{
					ID: "fixed-catalog-policy-" + provider, Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"auth_kind": coreauth.AuthKindOAuth},
				}
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
				(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auth, cfg)
				models := GlobalModelRegistry().GetModelsForClient(auth.ID)
				foundAlias := false
				for _, model := range models {
					if model.ID == "kimi-k2.8-code" || model.ID == "kimi-k3-256k" {
						t.Fatalf("policy did not filter %q", model.ID)
					}
					if model.ID == "fixed-256k-alias" {
						foundAlias = model.CatalogName == "kimi-k3-256k" && model.ContextLength == 262144
					}
				}
				if !foundAlias {
					t.Fatal("alias lost its canonical catalog identity or capabilities")
				}
			},
		)
	}
}
