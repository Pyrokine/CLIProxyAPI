package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestCodexClientModelsHTTPReasoningUpdatesFollowRegisteredRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		modelID   string
		providers []string
		support   []bool
		want      bool
	}{
		{name: "OAuth Astra", modelID: "gpt-6-astra", providers: []string{"codex"}, support: []bool{true}, want: true},
		{name: "OAuth Sol", modelID: "gpt-6-sol", providers: []string{"codex"}, support: []bool{true}, want: true},
		{name: "OAuth Luna", modelID: "gpt-6-luna", providers: []string{"codex"}, support: []bool{true}, want: true},
		{name: "Astra alias", modelID: "fixture-astra-alias", providers: []string{"codex"}, support: []bool{true}, want: true},
		{name: "API key default", modelID: "gpt-6-astra", providers: []string{"codex"}, support: []bool{false}},
		{name: "API key explicit support", modelID: "gpt-6-astra", providers: []string{"codex"}, support: []bool{true}, want: true},
		{name: "mixed providers", modelID: "gpt-6-astra", providers: []string{"codex", "xai"}, support: []bool{true, true}},
		{name: "mixed Codex credentials", modelID: "gpt-6-astra", providers: []string{"codex", "codex"}, support: []bool{false, true}},
		{name: "reverse mixed Codex credentials", modelID: "gpt-6-astra", providers: []string{"codex", "codex"}, support: []bool{true, false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := registry.GetGlobalRegistry()
			for i, provider := range test.providers {
				clientID := fmt.Sprintf("reasoning-update-fixture-%s-%d", test.name, i)
				catalogName := test.modelID
				if test.modelID == "fixture-astra-alias" {
					catalogName = "gpt-6-astra"
				}
				reg.RegisterClient(clientID, provider, []*registry.ModelInfo{{
					ID: test.modelID, Object: "model", Type: provider, OwnedBy: provider,
					CatalogName: catalogName, SupportConfigurationUpdate: test.support[i],
				}})
				t.Cleanup(func() { reg.UnregisterClient(clientID) })
			}
			handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.162.0", nil)
			handler.OpenAIModels(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("model catalog status = %d", recorder.Code)
			}
			var payload struct {
				Models []map[string]any `json:"models"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			for _, model := range payload.Models {
				if model["slug"] == test.modelID {
					if got, ok := model["supports_reasoning_effort_updates"].(bool); !ok || got != test.want {
						t.Fatalf("reasoning updates = %v, want %t", model["supports_reasoning_effort_updates"], test.want)
					}
					return
				}
			}
			t.Fatalf("model %q missing from client catalog", test.modelID)
		})
	}
}
