package claude

import (
	"net/http"
	"net/http/httptest"
	"testing"

	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestClaudeModelsUsesClientCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-claude-client-catalog"
	modelRegistry.RegisterClient(
		clientID, "claude", []*registry.ModelInfo{
			{ID: "gpt-4o", Object: "model", DisplayName: "Alpha"},
			{ID: "claude-sonnet-test", Object: "model", DisplayName: "Beta"},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("User-Agent", "claude-cli/test")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request

	handler.ClaudeModels(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := gjson.Get(recorder.Body.String(), "data.0.id").String(); got != claudemodels.ClientModelID("gpt-4o") {
		t.Fatalf("data.0.id = %q, want %q", got, claudemodels.ClientModelID("gpt-4o"))
	}
	if got := gjson.Get(recorder.Body.String(), "data.1.id").String(); got != "claude-sonnet-test" {
		t.Fatalf("data.1.id = %q, want claude-sonnet-test", got)
	}
	if got := gjson.Get(recorder.Body.String(), "first_id").String(); got != claudemodels.ClientModelID("gpt-4o") {
		t.Fatalf("first_id = %q, want %q", got, claudemodels.ClientModelID("gpt-4o"))
	}
}

func TestClaudeModelsAppliesModelListCloakingConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-claude-model-list-cloaking"
	modelID := "gpt-model-list-cloaking"
	modelRegistry.RegisterClient(
		clientID, "claude", []*registry.ModelInfo{
			{
				ID: modelID, Object: "model", DisplayName: "Model List Cloaking",
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	baseHandler := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	handler := NewCodeAPIHandler(baseHandler)
	assertModelID := func(want string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		handler.ClaudeModels(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		for _, model := range gjson.Get(recorder.Body.String(), "data").Array() {
			if model.Get("id").String() == want {
				return
			}
		}
		t.Fatalf("model %q not found in response: %s", want, recorder.Body.String())
	}

	assertModelID(claudemodels.ClientModelID(modelID))
	baseHandler.UpdateClients(
		&config.SDKConfig{
			ClaudeCode: config.ClaudeCodeConfig{DisableCloakingModelList: true},
		},
	)
	assertModelID(modelID)
}

func TestResolveClaudeClientModelInBody(t *testing.T) {
	reservedProviderID := claudemodels.ClientModelID("gpt-4o")
	availableModels := []map[string]any{
		{"id": "gpt-4o", "display_name": "GPT-4o"},
		{"id": "team-coder", "display_name": "Team Coder"},
		{"id": reservedProviderID, "display_name": "Reserved Prefix"},
	}
	tests := []struct {
		name      string
		body      string
		wantModel string
	}{
		{
			name:      "external model",
			body:      `{"model":"` + claudemodels.ClientModelID("gpt-4o") + `","messages":[]}`,
			wantModel: "gpt-4o",
		},
		{
			name:      "alias with thinking suffix",
			body:      `{"model":"` + claudemodels.ClientModelID("team-coder") + `(high)","messages":[]}`,
			wantModel: "team-coder(high)",
		},
		{
			name:      "reserved prefix provider model",
			body:      `{"model":"` + claudemodels.ClientModelID(reservedProviderID) + `","messages":[]}`,
			wantModel: reservedProviderID,
		},
		{
			name:      "non-canonical namespace unchanged",
			body:      `{"model":"claude-fable-5-dd-c-Z3B0LTRv","messages":[]}`,
			wantModel: "claude-fable-5-dd-c-Z3B0LTRv",
		},
		{
			name:      "native Claude unchanged",
			body:      `{"model":"claude-sonnet-4-6","messages":[]}`,
			wantModel: "claude-sonnet-4-6",
		},
		{
			name:      "missing model unchanged",
			body:      `{"messages":[]}`,
			wantModel: "",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := resolveClaudeClientModelInBody([]byte(tt.body), availableModels, false)
				if model := gjson.GetBytes(got, "model").String(); model != tt.wantModel {
					t.Fatalf("model = %q, want %q", model, tt.wantModel)
				}
			},
		)
	}
}
