package gemini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestGeminiModelsUsesConfiguredDisplayName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-gemini-display-name-catalog"
	modelID := "gemini-display-name-catalog"
	modelRegistry.RegisterClient(
		clientID, "gemini", []*registry.ModelInfo{
			{
				ID:          modelID,
				Name:        modelID,
				DisplayName: "Configured Gemini Name",
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)

	handler.GeminiModels(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	models := gjson.Get(recorder.Body.String(), "models").Array()
	for _, model := range models {
		if model.Get("name").String() != "models/"+modelID {
			continue
		}
		if got := model.Get("displayName").String(); got != "Configured Gemini Name" {
			t.Fatalf("displayName = %q, want Configured Gemini Name", got)
		}
		return
	}
	t.Fatalf("model %q not found in response", modelID)
}
