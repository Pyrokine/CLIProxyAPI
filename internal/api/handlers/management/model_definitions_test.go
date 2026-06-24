package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
)

func TestGetRuntimeModelsReturnsUnfilteredRegistryModels(t *testing.T) {
	const clientID = "test-runtime-models-client"
	const modelID = "r600-runtime-visible"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelID, OwnedBy: "test"}})
	defer reg.UnregisterClient(clientID)

	h := NewHandler(&config.Config{}, "", nil)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/runtime-models", nil)

	h.GetRuntimeModels(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, model := range payload.Models {
		if model["id"] == modelID {
			return
		}
	}
	t.Fatalf("model %q not found in response: %s", modelID, w.Body.String())
}
