package openai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestCodexClientCatalogConsumesXAIExecutorPatchCapability(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewXAIAutoExecutor(nil))
	models := registry.GetGlobalRegistry()
	models.RegisterClient(t.Name(), "xai", registry.GetEmbeddedXAIModels())
	t.Cleanup(func() { models.UnregisterClient(t.Name()) })
	for _, enabled := range []bool{false, true, false} {
		cfg := &sdkconfig.SDKConfig{}
		cfg.Client.Codex.EnableApplyPatch = enabled
		handler := openai.NewAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
		writer := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=cpa", nil)
		handler.OpenAIModels(ctx)
		entry := gjson.Get(writer.Body.String(), `models.#(slug=="grok-4.7")`)
		if writer.Code != http.StatusOK || !entry.Exists() {
			t.Fatalf("catalog status=%d body=%s", writer.Code, writer.Body.String())
		}
		capability := entry.Get("apply_patch_tool_type")
		if !capability.Exists() || (enabled && capability.String() != "freeform") || (!enabled && capability.Type != gjson.Null) {
			t.Fatalf("enabled=%t capability=%s", enabled, capability.Raw)
		}
	}
}

func TestCodexClientCatalogConsumesTranslatedPatchCapabilities(t *testing.T) {
	cfg := &config.Config{}
	for _, exec := range []coreauth.ProviderExecutor{
		executor.NewClaudeExecutor(cfg),
		executor.NewGeminiExecutor(cfg),
		executor.NewGeminiInteractionsExecutor(cfg),
		executor.NewGeminiCLIExecutor(cfg),
		executor.NewGeminiVertexExecutor(cfg),
		executor.NewAntigravityExecutor(cfg),
		executor.NewAIStudioExecutor(cfg, "aistudio", nil),
		executor.NewOpenAICompatExecutor("offline-chat", cfg),
		executor.NewQwenExecutor(cfg),
		executor.NewIFlowExecutor(cfg),
		executor.NewDevinExecutor(cfg),
		executor.NewKimiExecutor(cfg),
	} {
		t.Run(exec.Identifier(), func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(exec)
			models := registry.GetGlobalRegistry()
			modelID := "offline-patch-" + exec.Identifier()
			models.RegisterClient(t.Name(), exec.Identifier(), []*registry.ModelInfo{{ID: modelID, Object: "model", DisplayName: modelID, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}}})
			t.Cleanup(func() { models.UnregisterClient(t.Name()) })
			for _, enabled := range []bool{false, true, false} {
				clientCfg := &sdkconfig.SDKConfig{}
				clientCfg.Client.Codex.EnableApplyPatch = enabled
				handler := openai.NewAPIHandler(handlers.NewBaseAPIHandlers(clientCfg, manager))
				writer := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(writer)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=cpa", nil)
				handler.OpenAIModels(ctx)
				entry := gjson.Get(writer.Body.String(), `models.#(slug=="`+modelID+`")`)
				if writer.Code != http.StatusOK || !entry.Exists() {
					t.Fatalf("catalog status=%d body=%s", writer.Code, writer.Body.String())
				}
				capability := entry.Get("apply_patch_tool_type")
				wantFreeform := enabled && exec.Identifier() != "kimi"
				if !capability.Exists() || (wantFreeform && capability.String() != "freeform") || (!wantFreeform && capability.Type != gjson.Null) {
					t.Fatalf("enabled=%t capability=%s", enabled, capability.Raw)
				}
			}
		})
	}
}
