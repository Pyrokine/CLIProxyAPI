package cliproxy

import (
	"context"
	"fmt"
	"testing"

	codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type geminiSearchServiceExecutor struct {
	*customOwnedExecutor
	cfg     *config.Config
	request cliproxyexecutor.Request
}

func (e *geminiSearchServiceExecutor) Execute(ctx context.Context, _ *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.request = req
	to := sdktranslator.FormatGemini
	if e.id == "gemini-cli" {
		to = sdktranslator.FormatGeminiCLI
	}
	_, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(ctx, opts.Headers, e.cfg,
		opts.SourceFormat, to, req.Model, opts.OriginalRequest, req.Payload, false, helps.APIKeyModelIsCompat(req), &req)
	return cliproxyexecutor.Response{Payload: body}, err
}

func TestServiceGeminiNativeSearchModelRegistration(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex", "gemini-cli", "aistudio"} {
		for _, prefix := range []string{"", "tenant"} {
			t.Run(provider+"/prefix="+prefix, func(t *testing.T) {
				const upstream = "gemini-2.5-flash"
				const alias = "gemini-native-service-fixture"
				cfg := &config.Config{
					ForceModelPrefix:   true,
					GeminiKey:          []config.GeminiKey{{APIKey: "fixture-key", Models: []config.GeminiModel{{Name: upstream, Alias: alias}}}},
					VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "fixture-key", Models: []config.VertexCompatModel{{Name: upstream, Alias: alias}}}},
					OAuthModelAlias:    map[string][]config.OAuthModelAlias{provider: {{Name: upstream, Alias: alias}}},
				}
				auth := &coreauth.Auth{ID: "native-search-service-" + provider, Provider: provider, Prefix: prefix, Status: coreauth.StatusActive}
				if provider == "gemini" || provider == "vertex" {
					auth.Attributes = map[string]string{"api_key": "fixture-key"}
				} else {
					auth.Attributes = map[string]string{"auth_kind": "oauth"}
				}
				service := newModelRefreshTestService(t, cfg, auth)
				service.coreManager.SetConfig(cfg)
				service.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				service.registerModelsForAuthWithConfig(auth, cfg)
				public := alias
				if prefix != "" {
					public = prefix + "/" + alias
				}
				catalog := registry.GetGlobalRegistry()
				info := catalog.GetModelInfoForClient(auth.ID, public)
				if info == nil || info.CatalogName != upstream || info.NativeCapabilities == nil || info.NativeCapabilities.WebSearch == nil || !*info.NativeCapabilities.WebSearch {
					t.Fatalf("service model registration lost native metadata: %+v", info)
				}
				capability := catalog.GetResponsesWebSearchCapability(public)
				if capability == nil || !*capability {
					t.Fatalf("service model capability = %v, want true", capability)
				}
				if prefix != "" && catalog.ClientSupportsModel(auth.ID, alias) {
					t.Fatal("forced prefix also registered the unprefixed alias")
				}
				client := codexmodels.BuildResponseForClient([]map[string]any{{"id": public}}, catalog.GetModelProviders, false, "cpa")
				models := client["models"].([]map[string]any)
				if meta := models[0]["cpa_capabilities"].(map[string]any); meta["web_search"] != true {
					t.Fatalf("client native metadata lost after alias/prefix registration: %+v", meta)
				}
			})
		}
	}
}

func TestServiceGeminiDiscoveredSearchMetadataReachesExecution(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex", "gemini-cli", "aistudio"} {
		for _, prefix := range []string{"", "tenant"} {
			for _, metadata := range []string{"true", "false", "nil-capabilities", "nil-web-search"} {
				for _, upstream := range []string{"gemini-2.5-flash", "gemini-private-native-fixture"} {
					t.Run(fmt.Sprintf("%s/prefix=%s/%s/%s", provider, prefix, metadata, upstream), func(t *testing.T) {
						const alias = "gemini-discovered-search-fixture"
						cfg := &config.Config{
							ForceModelPrefix:   true,
							GeminiKey:          []config.GeminiKey{{APIKey: "fixture-key", Models: []config.GeminiModel{{Name: upstream, Alias: alias}}}},
							VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "fixture-key", Models: []config.VertexCompatModel{{Name: upstream, Alias: alias}}}},
							OAuthModelAlias:    map[string][]config.OAuthModelAlias{provider: {{Name: upstream, Alias: alias}}},
						}
						auth := &coreauth.Auth{ID: "native-search-discovery-" + provider, Provider: provider, Prefix: prefix, Status: coreauth.StatusActive}
						if provider == "gemini" || provider == "vertex" {
							auth.Attributes = map[string]string{"api_key": "fixture-key"}
						} else {
							auth.Attributes = map[string]string{"auth_kind": "oauth"}
						}
						service := newModelRefreshTestService(t, cfg, auth)
						service.coreManager.SetConfig(cfg)
						service.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
						executor := &geminiSearchServiceExecutor{customOwnedExecutor: &customOwnedExecutor{id: provider}, cfg: cfg}
						service.coreManager.RegisterExecutor(executor)
						job := currentModelRefreshJob(t, service, auth.ID, false, 1)
						info := &registry.ModelInfo{ID: upstream, CatalogName: upstream, UserDefined: true}
						if provider == "gemini" || provider == "vertex" {
							info.ID = alias
						}
						value := metadata != "false"
						switch metadata {
						case "true", "false":
							info.NativeCapabilities = &registry.NativeCapabilities{WebSearch: &value}
						case "nil-web-search":
							info.NativeCapabilities = &registry.NativeCapabilities{}
						}
						ctx := context.Background()
						service.publishModelRefreshResult(ctx, job, modelDiscoveryResultFromPlugin(pluginhost.AuthModelResult{
							Provider: provider, Outcome: "complete", Models: []*registry.ModelInfo{info}, Handled: true,
						}))
						public := alias
						if prefix != "" {
							public = prefix + "/" + alias
						}
						catalog := registry.GetGlobalRegistry()
						capability := catalog.GetResponsesWebSearchCapability(public)
						if metadata == "true" || metadata == "false" {
							if capability == nil || *capability != value {
								t.Fatalf("discovered public capability = %v, want %t", capability, value)
							}
						} else if capability != nil {
							t.Fatalf("unknown discovered metadata became known: %v", capability)
						}
						payload, err := sjson.SetBytes([]byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`), "model", public)
						if err != nil {
							t.Fatal(err)
						}
						response, err := service.coreManager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: public, Payload: payload},
							cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload})
						if err != nil {
							t.Fatal(err)
						}
						if executor.request.Model != upstream {
							t.Fatalf("execution model = %q, want %q", executor.request.Model, upstream)
						}
						path := "tools.0.googleSearch"
						if provider == "gemini-cli" {
							path = "request." + path
						}
						wantSearch := metadata == "true" || metadata != "false" && upstream == "gemini-2.5-flash"
						if search := gjson.GetBytes(response.Payload, path); search.Exists() != wantSearch || wantSearch && search.Get("includedDomains").Raw != `["example.com"]` {
							t.Fatalf("discovered metadata not consumed by upstream request: %s", response.Payload)
						}
						if metadata == "true" || metadata == "false" {
							bound, ok := coreauth.ResolvedModelInfo(executor.request)
							if !ok || bound.NativeCapabilities == nil || bound.NativeCapabilities.WebSearch == nil || *bound.NativeCapabilities.WebSearch != value {
								t.Fatalf("execution lost selected model metadata: %+v", bound)
							}
							*info.NativeCapabilities.WebSearch = !value
							retained := catalog.GetResponsesWebSearchCapability(public)
							if retained == nil || *retained != (metadata == "true") {
								t.Fatalf("plugin source pointer mutated registered metadata: %v", retained)
							}
						}
					})
				}
			}
		}
	}
}
