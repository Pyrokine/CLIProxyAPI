package cliproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type configuredProviderTransport struct {
	respond func(*http.Request) (*http.Response, error)
}

func (transport *configuredProviderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.respond(request)
}

type configuredProviderTransportSource struct{ transport http.RoundTripper }

func (source configuredProviderTransportSource) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return source.transport
}

func TestConfiguredKimiNamePreservesCompatWireInBothRegistrationOrders(t *testing.T) {
	for _, name := range []string{"kimi", "audit-kimi"} {
		for _, nativeFirst := range []bool{true, false} {
			order := "compat-first"
			if nativeFirst {
				order = "native-first"
			}
			t.Run(
				name+"/"+order, func(t *testing.T) {
					cfg := &config.Config{
						OpenAICompatibility: []config.OpenAICompatibility{
							{
								Name: name, BaseURL: "https://configured-provider.invalid/v1",
								APIKeyEntries: []config.OpenAICompatibilityAPIKey{
									{APIKey: "synthetic-configured-key"},
								},
								Models: []config.OpenAICompatibilityModel{
									{
										Name: "kimi-private-upstream", Alias: "identity-alias",
									},
								},
							},
						},
					}
					cfg.SanitizeOpenAICompatibility()
					auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
						&synthesizer.SynthesisContext{
							Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
						},
					)
					if err != nil || len(auths) != 1 {
						t.Fatalf("synthesis auths=%d error=%v", len(auths), err)
					}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
					manager.SetConfig(cfg)
					auth, err := manager.Register(coreauth.WithSkipPersist(t.Context()), auths[0])
					if err != nil {
						t.Fatal(err)
					}
					service := &Service{cfg: cfg, coreManager: manager}
					native := &coreauth.Auth{
						ID:       "native-" + t.Name(),
						Provider: "kimi",
						Status:   coreauth.StatusActive,
					}
					if nativeFirst {
						service.ensureExecutorsForAuth(native)
						service.ensureExecutorsForAuth(auth)
					} else {
						service.ensureExecutorsForAuth(auth)
						service.ensureExecutorsForAuth(native)
					}
					service.registerModelsForAuthWithConfig(auth, cfg)
					t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
					manager.RefreshSchedulerEntry(auth.ID)
					var captured []byte
					calls := 0
					transport := &configuredProviderTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.String() != "https://configured-provider.invalid/v1/chat/completions" {
								t.Errorf("unexpected memory request route %s", request.URL)
							}
							captured, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
								Body: io.NopCloser(
									strings.NewReader(
										`{"id":"synthetic","object":"chat.completion","model":"kimi-private-` +
											`upstream","choices":[{"index":0,"message":{"role":"assistant","` +
											`content":"answer"},"finish_reason":"stop"}]}`,
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
						t.Fatal("actual client transport or synthetic API Key classification is not isolated")
					}
					manager.SetRoundTripperProvider(configuredProviderTransportSource{transport: transport})
					_, err = manager.Execute(
						ctx, []string{auth.Provider}, cliproxyexecutor.Request{
							Model: "identity-alias",
							Payload: []byte(
								`{"model":"identity-alias","messages":[{"role":"user","content":"hi"}],` +
									`"temperature":` +
									`2}`,
							),
						}, cliproxyexecutor.Options{
							SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI,
							Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID},
						},
					)
					if err != nil || calls != 1 {
						t.Fatalf("execution error=%v requests=%d", err, calls)
					}
					if model := gjson.GetBytes(captured, "model").String(); model != "kimi-private-upstream" {
						t.Errorf("configured upstream model=%q", model)
					}
					if temperature := gjson.GetBytes(captured, "temperature").Raw; temperature != "2" {
						t.Errorf("configured temperature=%q", temperature)
					}
					if bound, _ := manager.Executor(auth.Attributes["provider_key"]); bound != nil {
						if _, compat := bound.(*runtimeexecutor.OpenAICompatExecutor); !compat {
							t.Errorf("configured executor=%T, want OpenAICompatExecutor", bound)
						}
					}
					if bound, _ := manager.Executor("kimi"); bound == nil {
						t.Error("native Kimi executor disappeared")
					} else if _, nativeKimi := bound.(*runtimeexecutor.KimiExecutor); !nativeKimi {
						t.Errorf("native Kimi executor=%T", bound)
					}
				},
			)
		}
	}
}

func TestConfiguredNativeProviderNamesKeepDistinctExecutorOwnership(t *testing.T) {
	for _, provider := range []string{
		"gemini", "gemini-interactions", "vertex", "gemini-cli", "antigravity", "devin", "claude", "codex", "qwen",
		"iflow", "kimi", "kimi-ai", "meta", "xai",
	} {
		for _, nativeFirst := range []bool{true, false} {
			order := "compat-first"
			if nativeFirst {
				order = "native-first"
			}
			t.Run(
				provider+"/"+order, func(t *testing.T) {
					cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: provider}}}
					auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
						&synthesizer.SynthesisContext{
							Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
						},
					)
					if err != nil || len(auths) != 1 {
						t.Fatalf("synthesis auths=%d error=%v", len(auths), err)
					}
					service := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
					native := &coreauth.Auth{ID: "native-" + provider, Provider: provider}
					first, second := native, auths[0]
					if !nativeFirst {
						first, second = second, first
					}
					service.ensureExecutorsForAuth(first)
					service.ensureExecutorsForAuth(second)
					key := "openai-compatible-" + provider
					if auths[0].Provider != key || auths[0].Attributes["provider_key"] != key ||
						auths[0].Label != provider {
						t.Fatal("synthesized internal identity or original channel label changed")
					}
					compat, found := service.coreManager.Executor(key)
					if !found {
						t.Fatal("configured compatibility executor was not registered")
					}
					if _, ok := compat.(*runtimeexecutor.OpenAICompatExecutor); !ok {
						t.Fatalf("configured executor=%T", compat)
					}
					nativeExecutor, found := service.coreManager.Executor(provider)
					if !found || nativeExecutor.Identifier() != provider {
						t.Fatal("native executor ownership was replaced")
					}
					if _, ok := nativeExecutor.(*runtimeexecutor.OpenAICompatExecutor); ok {
						t.Fatal("compatibility executor owns the native provider name")
					}
				},
			)
		}
	}
}

func TestLegacyFlatConfiguredAuthIsNormalizedBeforeServicePublication(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "kimi", BaseURL: "https://legacy-configured.invalid/v1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "kimi-private-upstream", Alias: "legacy-identity-alias",
					},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	service := &Service{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{
		ID: "legacy-flat-configured-kimi", Provider: "kimi", Status: coreauth.StatusActive,
		Attributes: map[string]string{
			"source": "config:kimi[synthetic]", "compat_name": "kimi", "provider_key": "kimi",
			"base_url": cfg.OpenAICompatibility[0].BaseURL,
		},
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	if err := service.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(t.Context()), auth); err != nil {
		t.Fatal(err)
	}
	stored, exists := manager.GetByID(auth.ID)
	if !exists || stored.Provider != "openai-compatible-kimi" ||
		stored.Attributes["provider_key"] != stored.Provider {
		t.Fatal("legacy configured auth was not normalized before publication")
	}
	if selected, execute, err := manager.PickNext(
		t.Context(), stored.Provider, "legacy-identity-alias", cliproxyexecutor.Options{}, nil,
	); err != nil || selected == nil || selected.ID != auth.ID {
		t.Fatalf("normalized selection executor=%T error=%v", execute, err)
	} else if _, compat := execute.(*runtimeexecutor.OpenAICompatExecutor); !compat {
		t.Fatalf("normalized executor=%T", execute)
	}
	if auth.Provider != "kimi" || auth.Attributes["provider_key"] != "kimi" {
		t.Fatal("service changed caller-owned legacy identity")
	}
}

func TestFlatSDKCompatibilityIdentityRemainsUsable(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:   "flat-sdk-channel",
				Models: []config.OpenAICompatibilityModel{{Name: "flat-upstream", Alias: "flat-sdk-alias"}},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
	manager.SetConfig(cfg)
	implementation := runtimeexecutor.NewOpenAICompatExecutor("flat-sdk-channel", cfg)
	manager.RegisterExecutor(implementation)
	auth, err := manager.Register(
		coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
			ID: "flat-sdk-identity", Provider: "flat-sdk-channel", Status: coreauth.StatusActive,
			Attributes: map[string]string{
				"api_key": "synthetic-sdk-key", "compat_name": "flat-sdk-channel", "provider_key": "flat-sdk-channel",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	GlobalModelRegistry().RegisterClient(auth.ID, "flat-sdk-channel", []*ModelInfo{{ID: "flat-sdk-alias"}})
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)
	selected, execute, err := manager.PickNext(
		t.Context(), "flat-sdk-channel", "flat-sdk-alias", cliproxyexecutor.Options{}, nil,
	)
	if err != nil || selected == nil || selected.ID != auth.ID || execute != implementation {
		t.Fatalf("flat SDK selection executor=%T error=%v", execute, err)
	}
	if auth.Provider != "flat-sdk-channel" || implementation.Identifier() != "flat-sdk-channel" {
		t.Fatal("the direct SDK identity was changed by internal configuration namespacing")
	}
}

func TestKimiLegacyDomainAliasesReachCanonicalRegistryAndSelection(t *testing.T) {
	for _, sample := range []struct{ original, canonical string }{
		{"kimi", "kimi"}, {"kimi-ai", "kimi-ai"}, {"kimi.com", "kimi"}, {"kimi.ai", "kimi-ai"},
	} {
		t.Run(
			sample.original, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				service := &Service{cfg: &config.Config{}, coreManager: manager}
				auth := &coreauth.Auth{
					ID: "legacy-alias-" + sample.original, Provider: sample.original,
					Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": coreauth.AuthKindOAuth},
				}
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
				if err := service.applyCoreAuthAddOrUpdate(
					coreauth.WithSkipPersist(context.Background()), auth,
				); err != nil {
					t.Fatal(err)
				}
				stored, exists := manager.GetByID(auth.ID)
				if !exists {
					t.Fatal("legacy alias auth was not stored")
				}
				if stored.Provider != sample.canonical {
					t.Fatalf("stored provider=%q expected=%q", stored.Provider, sample.canonical)
				}
				if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
					t.Fatal("legacy alias was stored without its native model catalog")
				}
				selected, execute, err := manager.PickNext(
					t.Context(), sample.canonical, "kimi-k2", cliproxyexecutor.Options{
						Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID},
					}, nil,
				)
				if err != nil || selected == nil || selected.ID != auth.ID || execute == nil ||
					execute.Identifier() != sample.canonical {
					t.Fatalf("selection auth=%v executor=%v error=%v", selected, execute, err)
				}
				if auth.Provider != sample.original {
					t.Fatal("service changed the caller-owned auth snapshot")
				}
			},
		)
	}
}
