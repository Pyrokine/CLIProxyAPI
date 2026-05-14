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
