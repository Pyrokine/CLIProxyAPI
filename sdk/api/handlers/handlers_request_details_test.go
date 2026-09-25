package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestGetContextWithCancelIncludesUsageManager(t *testing.T) {
	manager := coreusage.NewManager(4)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetUsageManager(manager)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.RemoteAddr = "203.0.113.42:4321"
	ginCtx.Request.Header.Add("X-Forwarded-For", "198.51.100.1")
	ginCtx.Request.Header.Add("X-Forwarded-For", "203.0.113.42")
	ginCtx.Request.Header.Set("User-Agent", "handler-test-client/1.0")

	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()
	if got := coreusage.ManagerFromContext(ctx); got != manager {
		t.Fatalf("usage manager = %p, want %p", got, manager)
	}
	metadata := internallogging.GetClientRequestMetadata(ctx)
	if metadata.ClientIP != "203.0.113.42" || metadata.XForwardedFor != "198.51.100.1, 203.0.113.42" || metadata.UserAgent != "handler-test-client/1.0" {
		t.Fatalf("client request metadata = %+v", metadata)
	}
	ginCtx.Request.RemoteAddr = "127.0.0.1:9"
	ginCtx.Request.Header.Set("User-Agent", "mutated")
	if got := internallogging.GetClientRequestMetadata(ctx); got != metadata {
		t.Fatalf("client request metadata changed after request mutation: %+v", got)
	}
}

func TestGetContextWithCancelCapturesSessionHierarchy(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set("X-Claude-Code-Session-Id", "root-session")
	ginCtx.Request.Header.Set("X-Claude-Code-Agent-Id", "worker")

	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()
	metadata := internallogging.GetClientRequestMetadata(ctx)
	if metadata.SessionID != "claude:root-session:agent:worker" || metadata.ParentSessionID != "claude:root-session" {
		t.Fatalf("session hierarchy = (%q, %q)", metadata.SessionID, metadata.ParentSessionID)
	}
	if got := util.SessionIDFromContext(ctx); got != metadata.SessionID {
		t.Fatalf("context session ID = %q, want %q", got, metadata.SessionID)
	}
}

func TestEnrichContextWithSessionHierarchyReplacesAndClearsMetadata(t *testing.T) {
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		ClientIP:        "192.0.2.1",
		SessionID:       "header:old-child",
		ParentSessionID: "header:old-parent",
	})
	ctx = util.WithSessionID(ctx, "header:old-child")
	ctx = EnrichContextWithSessionHierarchy(
		ctx,
		nil,
		[]byte(`{"session_id":"new-child","parent_session_id":"new-parent"}`),
		nil,
	)
	metadata := internallogging.GetClientRequestMetadata(ctx)
	if metadata.ClientIP != "192.0.2.1" || metadata.SessionID != "session:new-child" || metadata.ParentSessionID != "session:new-parent" {
		t.Fatalf("replaced session metadata = %+v", metadata)
	}

	ctx = EnrichContextWithSessionHierarchy(ctx, nil, nil, nil)
	metadata = internallogging.GetClientRequestMetadata(ctx)
	if metadata.SessionID != "" || metadata.ParentSessionID != "" || util.SessionIDFromContext(ctx) != "" {
		t.Fatalf("cleared session metadata = %+v, context session = %q", metadata, util.SessionIDFromContext(ctx))
	}
}

func TestGetContextWithCancelUsesResolvedClientAddress(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.RemoteAddr = "127.0.0.1:4321"
	request.Header.Set("X-Forwarded-For", "198.51.100.20")
	client := access.ResolveClientAddress(request, []string{"127.0.0.1/32"})
	request = request.WithContext(access.WithClientAddress(request.Context(), client))
	ginCtx.Request = request

	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()
	metadata := internallogging.GetClientRequestMetadata(ctx)
	if metadata.ClientIP != "198.51.100.20" || metadata.XForwardedFor != "198.51.100.20" {
		t.Fatalf("client request metadata = %+v, want trusted forwarded client", metadata)
	}
}

func TestGetContextWithCancelPreservesRequestOutcomeState(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	outcomeState := internallogging.NewRequestOutcomeState()
	request = request.WithContext(
		internallogging.WithRequestOutcomeState(request.Context(), outcomeState),
	)
	ginCtx.Request = request

	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()
	if got := internallogging.RequestOutcomeStateFromContext(ctx); got != outcomeState {
		t.Fatalf("request outcome state = %p, want %p", got, outcomeState)
	}
	_ = coreusage.PublishRecord(
		ctx, coreusage.Record{
			Failed: true,
			Fail:   coreusage.Failure{Body: "upstream failed"},
		},
	)
	outcome := outcomeState.Snapshot()
	if !outcome.Failed || outcome.Kind != internallogging.RequestOutcomeUpstreamError {
		t.Fatalf("request outcome = %+v, want upstream failure", outcome)
	}
}

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

func TestGetRequestDetailsUnknownModelErrorResistsJSONInjection(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	for _, model := range []string{
		"unroutable-model",
		`foo"bar`,
		`x","code":"insufficient_quota","x":"`,
		`x"}}`,
		`foo\bar`,
		"foo\nbar",
	} {
		t.Run(
			model, func(t *testing.T) {
				_, _, errMsg := handler.getRequestDetails(model)
				if errMsg == nil || errMsg.Error == nil {
					t.Fatal("expected an error for an unroutable model")
				}
				if errMsg.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
				}
				body := errMsg.Error.Error()
				if !json.Valid([]byte(body)) {
					t.Fatalf("error body is not valid JSON: %s", body)
				}
				if got := gjson.Get(body, "error.code").String(); got != "model_not_found" {
					t.Fatalf("error code = %q, want model_not_found; body=%s", got, body)
				}
				wantMessage := "unknown provider for model " + model
				if got := gjson.Get(body, "error.message").String(); got != wantMessage {
					t.Fatalf("error message = %q, want %q", got, wantMessage)
				}
			},
		)
	}
}

func TestGetRequestDetailsImageOnlyModelsReturn503(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	models := []string{
		"gpt-image-1.5",
		"gpt-image-2",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"codex/gpt-image-2.5-flare-2026-09-08",
		"openai/gpt-image-2.5-sunburst",
		"gpt-image-2.5-sunburst-2026-09-08",
		"grok-imagine-image",
		"xai/grok-imagine-image",
		"grok-imagine-image-quality",
		"XAI/Grok-Imagine-Image-Quality(HIGH)",
		"xai/grok-imagine-image-2.0",
	}

	for _, model := range models {
		t.Run(
			model, func(t *testing.T) {
				_, _, errMsg := handler.getRequestDetails(model)
				if errMsg == nil {
					t.Fatalf("getRequestDetails(%q) error = nil, want image-only error", model)
				}
				if errMsg.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusServiceUnavailable)
				}
				message := errMsg.Error.Error()
				if !strings.Contains(message, "/v1/images/generations") || !strings.Contains(
					message, "/v1/images/edits",
				) {
					t.Fatalf("unexpected error message: %q", message)
				}
			},
		)
	}
}

func TestValidateImageOnlyModelAllowsImageEndpoints(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	models := []string{
		"gpt-image-1.5",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"openai/gpt-image-2.5-sunburst-2026-09-08",
		"grok-imagine-image",
		"xai/grok-imagine-image-quality(high)",
		"grok/grok-imagine-image-2.0",
	}

	for _, model := range models {
		t.Run(
			model, func(t *testing.T) {
				if errMsg := handler.validateImageOnlyModel(model, true); errMsg != nil {
					t.Fatalf("validateImageOnlyModel(%q, true) = %+v, want nil", model, errMsg)
				}
			},
		)
	}
}

func TestGetRequestDetailsWithOptionsCanonicalizesBuiltinImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		"test-image-details-codex",
		"codex",
		[]*registry.ModelInfo{
			{ID: "gpt-image-2"},
			{ID: "gpt-image-2.5-flare-2026-09-08"},
		},
	)
	modelRegistry.RegisterClient(
		"test-image-details-xai",
		"xai",
		[]*registry.ModelInfo{
			{ID: "grok-imagine-image-quality"},
			{ID: "grok-imagine-image-2.0"},
		},
	)
	modelRegistry.RegisterClient(
		"test-image-details-prefixed",
		"codex",
		[]*registry.ModelInfo{{ID: "TeamA/gpt-image-2"}},
	)
	t.Cleanup(
		func() {
			modelRegistry.UnregisterClient("test-image-details-codex")
			modelRegistry.UnregisterClient("test-image-details-xai")
			modelRegistry.UnregisterClient("test-image-details-prefixed")
		},
	)

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	tests := []struct {
		model        string
		wantProvider string
		wantModel    string
	}{
		{model: "Codex/GPT-IMAGE-2(high)", wantProvider: "codex", wantModel: "gpt-image-2"},
		{
			model:        "OPENAI/GPT-IMAGE-2.5-FLARE-2026-09-08(HIGH)",
			wantProvider: "codex",
			wantModel:    "gpt-image-2.5-flare-2026-09-08",
		},
		{
			model:        "XAI/Grok-Imagine-Image-Quality(HIGH)",
			wantProvider: "xai",
			wantModel:    "grok-imagine-image-quality",
		},
		{
			model:        "XAI/Grok-Imagine-Image-2.0",
			wantProvider: "xai",
			wantModel:    "grok-imagine-image-2.0",
		},
		{
			model:        "teama/GPT-IMAGE-2(HIGH)",
			wantProvider: "codex",
			wantModel:    "TeamA/gpt-image-2",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.model, func(t *testing.T) {
				providers, model, errMsg := handler.getRequestDetailsWithOptions(tt.model, true)
				if errMsg != nil {
					t.Fatalf("getRequestDetailsWithOptions(%q, true) error = %v", tt.model, errMsg.Error)
				}
				if len(providers) != 1 || providers[0] != tt.wantProvider {
					t.Fatalf("providers = %v, want [%s]", providers, tt.wantProvider)
				}
				if model != tt.wantModel {
					t.Fatalf("model = %q, want %q", model, tt.wantModel)
				}
			},
		)
	}

	if _, _, errMsg := handler.getRequestDetailsWithOptions("TeamB/GPT-IMAGE-2(HIGH)", true); errMsg == nil {
		t.Fatal("unknown image model prefix unexpectedly fell back to the bare model")
	}
}

func TestExecuteImageWithAuthManagerChecksRequestedModelAccess(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		"test-image-access-prefixed",
		"codex",
		[]*registry.ModelInfo{{ID: "TeamA/gpt-image-2"}},
	)
	t.Cleanup(
		func() {
			modelRegistry.UnregisterClient("test-image-access-prefixed")
		},
	)

	handler := NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				"sk-test": {BlockedModels: []string{"TeamA/gpt-image-2"}},
			},
		},
		coreauth.NewManager(nil, nil, nil),
	)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "sk-test")
	ctx := util.WithGinContext(context.Background(), ginCtx)

	_, _, errMsg := handler.ExecuteImageWithAuthManager(
		ctx,
		"openai-image",
		"teama/GPT-IMAGE-2(HIGH)",
		[]byte(`{"model":"teama/GPT-IMAGE-2(HIGH)"}`),
		"",
	)
	if errMsg == nil {
		t.Fatal("prefixed model access rule was bypassed")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusForbidden)
	}
}

func TestIsOpenAIImageOnlyModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "gpt-image-1.5", want: true},
		{model: "codex/gpt-image-2", want: true},
		{model: "GPT-IMAGE-2.5-FLARE", want: true},
		{model: "openai/gpt-image-2.5-flare-2026-09-08", want: true},
		{model: "gpt-image-2.5-sunburst", want: true},
		{model: "codex/GPT-IMAGE-2.5-SUNBURST-2026-09-08", want: true},
		{model: "gpt-image-2.5", want: false},
		{model: "grok-imagine-image", want: true},
		{model: "xai/grok-imagine-image", want: true},
		{model: "XAI/Grok-Imagine-Image-Quality", want: true},
		{model: "grok/GROK-IMAGINE-IMAGE-2.0", want: true},
		{model: "grok-3", want: false},
		{model: "grok-imagine-video", want: false},
	}

	for _, tt := range tests {
		t.Run(
			tt.model, func(t *testing.T) {
				if got := isOpenAIImageOnlyModel(tt.model); got != tt.want {
					t.Fatalf("isOpenAIImageOnlyModel(%q) = %v, want %v", tt.model, got, tt.want)
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

func TestSetServiceTierMetadataPreservesContextValue(t *testing.T) {
	ctx := coreusage.WithServiceTier(context.Background(), "flex")
	meta := requestExecutionMetadata(ctx)

	setServiceTierMetadata(meta, []byte(`{"service_tier":123}`))

	if got := meta[coreexecutor.ServiceTierMetadataKey]; got != "flex" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", got, "flex")
	}
}

func TestSetServiceTierMetadataBodyOverridesContextValue(t *testing.T) {
	ctx := coreusage.WithServiceTier(context.Background(), "flex")
	meta := requestExecutionMetadata(ctx)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	if got := meta[coreexecutor.ServiceTierMetadataKey]; got != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", got, "priority")
	}
}
