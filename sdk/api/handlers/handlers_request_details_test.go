package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestRequestHeadersAndQueryFromGinContext(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses?alt=sse&foo=bar", nil)
	ginCtx.Request.Header.Set("X-Amp-Thread-Id", "thread-1")
	ctx := util.WithGinContext(context.Background(), ginCtx)

	headers, query := requestHeadersAndQuery(ctx)
	if got := headers.Get("X-Amp-Thread-Id"); got != "thread-1" {
		t.Fatalf("X-Amp-Thread-Id = %q, want thread-1", got)
	}
	if got := query.Get("foo"); got != "bar" {
		t.Fatalf("query foo = %q, want bar", got)
	}

	ginCtx.Request.Header.Set("X-Amp-Thread-Id", "changed")
	if got := headers.Get("X-Amp-Thread-Id"); got != "thread-1" {
		t.Fatalf("headers must be cloned, got %q", got)
	}
}

func TestGetRequestDetails_PreservesSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient(
		"test-request-details-gemini", "gemini", []*registry.ModelInfo{
			{ID: "gemini-2.5-pro", Created: now + 30},
			{ID: "gemini-2.5-flash", Created: now + 25},
		},
	)
	modelRegistry.RegisterClient(
		"test-request-details-openai", "openai", []*registry.ModelInfo{
			{ID: "gpt-5.2", Created: now + 20},
			{ID: "gpt-5.4", Created: now + 19, ContextLength: 1_050_000},
			{ID: "gpt-5.5", Created: now + 18, ContextLength: 400_000},
		},
	)
	modelRegistry.RegisterClient(
		"test-request-details-claude", "claude", []*registry.ModelInfo{
			{ID: "claude-sonnet-4-5", Created: now + 5, ContextLength: 1_000_000},
		},
	)

	clientIDs := []string{
		"test-request-details-gemini",
		"test-request-details-openai",
		"test-request-details-claude",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(
			func() {
				modelRegistry.UnregisterClient(id)
			},
		)
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantProviders []string
		wantModel     string
		wantErr       bool
	}{
		{
			name:          "numeric suffix preserved",
			inputModel:    "gemini-2.5-pro(8192)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(8192)",
			wantErr:       false,
		},
		{
			name:          "level suffix preserved",
			inputModel:    "gpt-5.2(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.2(high)",
			wantErr:       false,
		},
		{
			name:          "no suffix unchanged",
			inputModel:    "claude-sonnet-4-5",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5",
			wantErr:       false,
		},
		{
			name:          "unknown model with suffix",
			inputModel:    "unknown-model(8192)",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "auto suffix resolved",
			inputModel:    "auto(high)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(high)",
			wantErr:       false,
		},
		{
			name:          "special suffix none preserved",
			inputModel:    "gemini-2.5-flash(none)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-flash(none)",
			wantErr:       false,
		},
		{
			name:          "special suffix auto preserved",
			inputModel:    "claude-sonnet-4-5(auto)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5(auto)",
			wantErr:       false,
		},
		{
			name:          "context 1m tag resolved to base model",
			inputModel:    "gpt-5.4[1m]",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.4[1m]",
			wantErr:       false,
		},
		{
			name:          "openai context 1m tag rejects unsupported model",
			inputModel:    "gpt-5.5[1m]",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "context 1m tag with thinking suffix preserved",
			inputModel:    "claude-sonnet-4-5[1m](high)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5[1m](high)",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				providers, model, errMsg := handler.getRequestDetails(tt.inputModel)
				if (errMsg != nil) != tt.wantErr {
					t.Fatalf("getRequestDetails() error = %v, wantErr %v", errMsg, tt.wantErr)
				}
				if errMsg != nil {
					return
				}
				if !reflect.DeepEqual(providers, tt.wantProviders) {
					t.Fatalf("getRequestDetails() providers = %v, want %v", providers, tt.wantProviders)
				}
				if model != tt.wantModel {
					t.Fatalf("getRequestDetails() model = %v, want %v", model, tt.wantModel)
				}
			},
		)
	}
}

func TestFilterModelsForCurrentAPIKeyUsesCatalogNameForAlias(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-filter-api-key-catalog-alias"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{ID: "py-gpt", CatalogName: "gpt-5.2", OwnedBy: "openai", Type: "openai"},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	handler := NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				"sk-test": {BlockedModels: []string{"gpt-5.2"}},
			},
		},
		nil,
	)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "sk-test")

	models := handler.FilterModelsForCurrentAPIKey(ginCtx, []map[string]any{{"id": "py-gpt"}})
	if len(models) != 0 {
		t.Fatalf("expected canonical API key block to hide alias model, got %+v", models)
	}
}

func TestFilterModelsForCurrentAPIKeyUsesCatalogNameForDisabledAlias(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-filter-disabled-catalog-alias"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{ID: "py-gpt-disabled", CatalogName: "gpt-5.3", OwnedBy: "openai", Type: "openai"},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })
	disabled := false
	handler := NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			ModelCatalog: sdkconfig.ModelCatalogConfig{
				Models: []sdkconfig.ModelCatalogModel{
					{Name: "gpt-5.3", Enabled: &disabled},
				},
			},
		},
		nil,
	)

	models := handler.FilterModelsForCurrentAPIKey(nil, []map[string]any{{"id": "py-gpt-disabled"}})
	if len(models) != 0 {
		t.Fatalf("expected disabled canonical model to hide alias model, got %+v", models)
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataUsesBodyValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"flex"}`))

	if got := meta[coreexecutor.ServiceTierMetadataKey]; got != "flex" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", got, "flex")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	if got := meta[coreexecutor.ServiceTierMetadataKey]; got != coreusage.DefaultServiceTier {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", got, coreusage.DefaultServiceTier)
	}
}
