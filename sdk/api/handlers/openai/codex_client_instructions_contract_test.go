package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestOpenAIModelsKeepsDedicatedCodexInstructionTemplates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(registry.GetCodexClientModelsJSON(), &catalog); err != nil {
		t.Fatal(err)
	}
	templates := make(map[string]map[string]any)
	for _, model := range catalog.Models {
		templates[model["slug"].(string)] = model
	}
	ids := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "codex-auto-review"}
	registered := make([]*registry.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if templates[id] == nil {
			t.Fatalf("dedicated template %q is missing", id)
		}
		registered = append(registered, &registry.ModelInfo{ID: id, Object: "model", Type: "codex"})
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(t.Name(), "codex", registered)
	t.Cleanup(func() { modelRegistry.UnregisterClient(t.Name()) })
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.159.0", nil)
	handler.OpenAIModels(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog status = %d", recorder.Code)
	}
	var response struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]map[string]any)
	for _, model := range response.Models {
		bySlug[model["slug"].(string)] = model
	}
	for _, id := range ids {
		actual := bySlug[id]
		if actual == nil {
			t.Fatalf("client response is missing %q", id)
		}
		for _, key := range []string{"base_instructions", "model_messages"} {
			if got, exists := actual[key]; !exists || !reflect.DeepEqual(got, templates[id][key]) {
				t.Errorf("%s.%s differs from the complete dedicated template", id, key)
			}
		}
	}
}
