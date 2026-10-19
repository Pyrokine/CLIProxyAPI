package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type catalogPatchExecutor struct {
	coreauth.ProviderExecutor
	provider  string
	supported bool
}

func (e *catalogPatchExecutor) Identifier() string { return e.provider }

func (e *catalogPatchExecutor) SupportsApplyPatch() bool { return e.supported }

func TestCodexClientCatalogApplyPatchRouting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		providers []string
		support   map[string]bool
		want      string
	}{
		{name: "disabled", providers: []string{"codex"}, support: map[string]bool{"codex": true}},
		{name: "native", enabled: true, providers: []string{"codex"}, support: map[string]bool{"codex": true}, want: "freeform"},
		{name: "custom provider", enabled: true, providers: []string{"fixture-provider"}, support: map[string]bool{"fixture-provider": true}, want: "freeform"},
		{name: "unsupported", enabled: true, providers: []string{"codex"}, support: map[string]bool{"codex": false}},
		{name: "missing executor", enabled: true, providers: []string{"codex"}},
		{name: "mixed", enabled: true, providers: []string{"codex", "fixture-provider"}, support: map[string]bool{"codex": true, "fixture-provider": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			for provider, supported := range tc.support {
				manager.RegisterExecutor(&catalogPatchExecutor{provider: provider, supported: supported})
			}
			models := registry.GetGlobalRegistry()
			for _, provider := range tc.providers {
				id := t.Name() + "/" + provider
				models.RegisterClient(id, provider, []*registry.ModelInfo{{ID: "gpt-6-sol"}})
				t.Cleanup(func() { models.UnregisterClient(id) })
			}
			cfg := &sdkconfig.SDKConfig{}
			cfg.Client.Codex.EnableApplyPatch = tc.enabled
			handler := NewAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=cpa", nil)
			handler.OpenAIModels(ctx)
			entry := gjson.Get(writer.Body.String(), `models.#(slug=="gpt-6-sol")`)
			if writer.Code != http.StatusOK || !entry.Exists() {
				t.Fatalf("catalog status=%d body=%s", writer.Code, writer.Body.String())
			}
			capability := entry.Get("apply_patch_tool_type")
			if !capability.Exists() || capability.String() != tc.want || tc.want == "" && capability.Type != gjson.Null {
				t.Fatalf("apply_patch = %s, want %q", capability.Raw, tc.want)
			}
		})
	}
}

func TestCodexClientCatalogApplyPatchDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&catalogPatchExecutor{provider: "codex", supported: true})
	models := registry.GetGlobalRegistry()
	models.RegisterClient(t.Name(), "codex", []*registry.ModelInfo{{ID: "gpt-6-sol"}})
	t.Cleanup(func() { models.UnregisterClient(t.Name()) })
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.157.0", nil)
	handler.OpenAIModels(ctx)
	entry := gjson.Get(writer.Body.String(), `models.#(slug=="gpt-6-sol")`)
	if writer.Code != http.StatusOK || !entry.Exists() {
		t.Fatalf("catalog status=%d body=%s", writer.Code, writer.Body.String())
	}
	if capability := entry.Get("apply_patch_tool_type"); !capability.Exists() || capability.Type != gjson.Null {
		t.Fatalf("disabled apply_patch = %s, want explicit null", capability.Raw)
	}
}
