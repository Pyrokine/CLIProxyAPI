package cliproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestDevinServiceCatalogAdvertisesActualGeminiMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const clientID = "fixture-devin-gemini-http-catalog"
	registry := GlobalModelRegistry()
	t.Cleanup(func() { registry.UnregisterClient(clientID) })
	service := &Service{cfg: &config.Config{}}
	service.registerModelsForAuth(&coreauth.Auth{ID: clientID, Provider: "devin"})
	models := registry.GetModelsForClient(clientID)
	if len(models) == 0 {
		t.Fatal("Devin service did not publish any catalog models")
	}
	router := gin.New()
	router.GET(
		"/v1beta/models",
		gemini.NewAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)).GeminiModels,
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1beta/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("Gemini models status=%d body=%s", response.Code, response.Body)
	}
	var payload struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	methods := make(map[string][]string, len(payload.Models))
	for _, model := range payload.Models {
		methods[model.Name] = model.Methods
	}
	for _, model := range models {
		name := model.Name
		if name == "" {
			name = model.ID
		}
		if !strings.HasPrefix(name, "models/") {
			name = "models/" + name
		}
		if got := methods[name]; !slices.Contains(got, "generateContent") {
			t.Errorf("Devin model %s does not advertise its Gemini generation method: %v", name, got)
		}
	}
}

func TestPublicModelRegistryReturnsIndependentMutableMetadata(t *testing.T) {
	const clientID, provider, modelID = "fixture-public-detached-client",
		"fixture-public-detached-provider", "fixture-" +
			"public-detached-model"
	search := true
	registry := GlobalModelRegistry()
	registry.RegisterClient(
		clientID, provider, []*ModelInfo{
			{
				ID: modelID, SupportedParameters: []string{"temperature", "top_p"},
				Thinking:           &ThinkingSupport{Levels: []string{"low", "high"}},
				NativeCapabilities: &NativeCapabilities{WebSearch: &search},
			},
		},
	)
	t.Cleanup(func() { registry.UnregisterClient(clientID) })
	find := func() map[string]any {
		for _, model := range registry.GetAvailableModels("openai") {
			if model["id"] == modelID {
				return model
			}
		}
		t.Fatal("public model registry omitted the registered model")
		return nil
	}
	first := find()
	parameters, ok := first["supported_parameters"].([]string)
	if !ok || len(parameters) != 2 {
		t.Fatalf("public supported parameters=%v", first["supported_parameters"])
	}
	parameters[0] = "mutated"
	if got := find()["supported_parameters"].([]string)[0]; got != "temperature" {
		t.Fatalf("public map mutation reached registry: %q", got)
	}
	byProvider := registry.GetAvailableModelsByProvider(provider)
	if len(byProvider) != 1 || byProvider[0].Thinking == nil || byProvider[0].NativeCapabilities == nil ||
		byProvider[0].NativeCapabilities.WebSearch == nil {
		t.Fatalf("public provider metadata is incomplete: %+v", byProvider)
	}
	byProvider[0].Thinking.Levels[0] = "mutated"
	*byProvider[0].NativeCapabilities.WebSearch = false
	current := registry.GetModelsForClient(clientID)
	if len(current) != 1 || current[0].Thinking.Levels[0] != "low" ||
		!*current[0].NativeCapabilities.WebSearch {
		t.Fatalf("public provider mutation reached registry: %+v", current)
	}
}
