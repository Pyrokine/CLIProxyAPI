package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
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
