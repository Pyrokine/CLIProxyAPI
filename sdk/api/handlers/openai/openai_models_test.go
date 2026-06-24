package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestOpenAIModels_PreservesExtendedMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := registry.GetGlobalRegistry()
	clientID := "test-openai-models-metadata"
	reg.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:                  "gpt-5.5",
				OwnedBy:             "openai",
				Type:                "openai",
				ContextLength:       400000,
				MaxCompletionTokens: 128000,
				SupportedParameters: []string{"temperature", "max_completion_tokens"},
				Thinking:            &registry.ThinkingSupport{Levels: []string{"none", "low", "high"}},
			},
		},
	)
	defer reg.UnregisterClient(clientID)

	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = req

	handler.OpenAIModels(ctx)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Object string                   `json:"object"`
		Data   []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Object != "list" {
		t.Fatalf("expected object=list, got %q", resp.Object)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected at least one model")
	}

	var model map[string]interface{}
	for _, item := range resp.Data {
		if item["id"] == "gpt-5.5" {
			model = item
			break
		}
	}
	if model == nil {
		t.Fatalf("expected gpt-5.5 in response, got %+v", resp.Data)
	}
	if got := model["context_length"]; got != float64(400000) {
		t.Fatalf("expected context_length 400000, got %#v", got)
	}
	if got := model["max_completion_tokens"]; got != float64(128000) {
		t.Fatalf("expected max_completion_tokens 128000, got %#v", got)
	}
	if _, ok := model["thinking"].(map[string]interface{}); !ok {
		t.Fatalf("expected thinking object, got %#v", model["thinking"])
	}
}

func TestOpenAIModels_FiltersDisabledCatalogModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := registry.GetGlobalRegistry()
	clientID := "test-openai-models-disabled-catalog"
	reg.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{ID: "catalog-disabled-model", OwnedBy: "openai", Type: "openai"},
			{ID: "catalog-enabled-model", OwnedBy: "openai", Type: "openai"},
		},
	)
	defer reg.UnregisterClient(clientID)

	disabled := false
	handler := NewAPIHandler(
		handlers.NewBaseAPIHandlers(
			&sdkconfig.SDKConfig{
				ModelCatalog: sdkconfig.ModelCatalogConfig{
					Models: []sdkconfig.ModelCatalogModel{
						{Name: "catalog-disabled-model", Enabled: &disabled},
					},
				},
			},
			nil,
		),
	)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = req

	handler.OpenAIModels(ctx)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	for _, model := range resp.Data {
		if model["id"] == "catalog-disabled-model" {
			t.Fatalf("disabled model returned in response: %+v", resp.Data)
		}
	}
}

func TestCodexClientModelsResponseSanitizesReasoningLevels(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-codex-client-reasoning-levels"
	reg.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:            "custom-codex-model",
				DisplayName:   "Custom Codex Model",
				Description:   "Custom model from registry",
				ContextLength: 123456,
				Thinking: &registry.ThinkingSupport{
					Levels: []string{
						"none", "minimal", "low", "medium", "unsupported", "high", "xhigh",
					},
				},
			},
		},
	)
	defer reg.UnregisterClient(clientID)

	resp := CodexClientModelsResponse([]map[string]any{{"id": "custom-codex-model"}})
	models, ok := resp["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", resp["models"])
	}

	model := models[0]
	if got := model["default_reasoning_level"]; got != "medium" {
		t.Fatalf("default_reasoning_level = %v, want medium", got)
	}
	assertCodexSupportedReasoningLevels(t, model, []string{"none", "low", "medium", "high", "xhigh"})
}

func assertCodexSupportedReasoningLevels(t *testing.T, model map[string]any, want []string) {
	t.Helper()

	rawLevels, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v, want array", model["supported_reasoning_levels"])
	}
	if len(rawLevels) != len(want) {
		t.Fatalf("supported_reasoning_levels length = %d, want %d: %#v", len(rawLevels), len(want), rawLevels)
	}
	for index, rawLevel := range rawLevels {
		levelEntry, ok := rawLevel.(map[string]any)
		if !ok {
			t.Fatalf("supported_reasoning_levels[%d] = %#v, want object", index, rawLevel)
		}
		if got, _ := levelEntry["effort"].(string); got != want[index] {
			t.Fatalf("supported_reasoning_levels[%d].effort = %q, want %q", index, got, want[index])
		}
	}
}
