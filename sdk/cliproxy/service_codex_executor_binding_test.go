package cliproxy

import (
	"context"
	"strings"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestRegisterModelsForAuth_CodexPlanControlsGPT56Sol(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	freeAuth := &coreauth.Auth{
		ID:         "codex-free-plan",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "free"},
	}
	service.registerModelsForAuth(freeAuth)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(freeAuth.ID) })
	if GlobalModelRegistry().ClientSupportsModel(freeAuth.ID, "gpt-5.6-sol") {
		t.Fatal("free Codex auth unexpectedly supports gpt-5.6-sol")
	}
	if !GlobalModelRegistry().ClientSupportsModel(freeAuth.ID, "gpt-5.6-luna") {
		t.Fatal("free Codex auth should still support gpt-5.6-luna")
	}

	plusAuth := &coreauth.Auth{
		ID:         "codex-plus-plan",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "plus"},
	}
	service.registerModelsForAuth(plusAuth)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(plusAuth.ID) })
	if !GlobalModelRegistry().ClientSupportsModel(plusAuth.ID, "gpt-5.6-sol") {
		t.Fatal("plus Codex auth should support gpt-5.6-sol")
	}
}

func TestEnsureExecutorsForAuth_CodexDoesNotReplaceInNormalMode(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-auth-1",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("codex")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected codex executor after first bind")
	}

	service.ensureExecutorsForAuth(auth)
	secondExecutor, okSecond := service.coreManager.Executor("codex")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected codex executor after second bind")
	}

	if firstExecutor != secondExecutor {
		t.Fatal("expected codex executor to stay unchanged in normal mode")
	}
}

func TestEnsureExecutorsForAuthWithMode_CodexForceReplace(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "codex-auth-2",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("codex")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected codex executor after first bind")
	}

	service.ensureExecutorsForAuthWithMode(auth, true)
	secondExecutor, okSecond := service.coreManager.Executor("codex")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected codex executor after forced rebind")
	}

	if firstExecutor == secondExecutor {
		t.Fatal("expected codex executor replacement in force mode")
	}
}

func TestEnsureExecutorsForAuth_InteractionsRegistersNativeExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "interactions-auth-1",
		Provider: "gemini-interactions",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	resolved, ok := service.coreManager.Executor("gemini-interactions")
	if !ok || resolved == nil {
		t.Fatal("expected gemini-interactions executor after bind")
	}
	geminiExecutor, ok := resolved.(*executor.GeminiExecutor)
	if !ok {
		t.Fatalf("executor = %T, want *executor.GeminiExecutor", resolved)
	}
	if got := geminiExecutor.Identifier(); got != "gemini-interactions" {
		t.Fatalf("identifier = %q, want gemini-interactions", got)
	}
}

func TestEnsureExecutorsForAuth_XAIDoesNotReplaceInNormalMode(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "xai-auth-1",
		Provider: "xai",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("xai")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected xai executor after first bind")
	}
	if _, ok := firstExecutor.(*executor.XAIAutoExecutor); !ok {
		t.Fatalf("xai executor = %T, want *executor.XAIAutoExecutor", firstExecutor)
	}

	service.ensureExecutorsForAuth(auth)
	secondExecutor, okSecond := service.coreManager.Executor("xai")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected xai executor after second bind")
	}

	if firstExecutor != secondExecutor {
		t.Fatal("expected xai executor to stay unchanged in normal mode")
	}
}

func TestEnsureExecutorsForAuthWithMode_XAIForceReplace(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "xai-auth-2",
		Provider: "xai",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)
	firstExecutor, okFirst := service.coreManager.Executor("xai")
	if !okFirst || firstExecutor == nil {
		t.Fatal("expected xai executor after first bind")
	}

	service.ensureExecutorsForAuthWithMode(auth, true)
	secondExecutor, okSecond := service.coreManager.Executor("xai")
	if !okSecond || secondExecutor == nil {
		t.Fatal("expected xai executor after forced rebind")
	}

	if firstExecutor == secondExecutor {
		t.Fatal("expected xai executor replacement in force mode")
	}
	if _, ok := secondExecutor.(*executor.XAIAutoExecutor); !ok {
		t.Fatalf("xai executor = %T, want *executor.XAIAutoExecutor", secondExecutor)
	}
}

func TestRebindExecutorsReplacesAutoExecutorsWithoutCurrentAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewCodexAutoExecutor(&config.Config{}))
	manager.RegisterExecutor(executor.NewXAIAutoExecutor(&config.Config{}))
	beforeCodex, _ := manager.Executor("codex")
	beforeXAI, _ := manager.Executor("xai")
	service := &Service{
		cfg:         &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://new-proxy.example"}},
		coreManager: manager,
	}
	service.rebindExecutors()
	afterCodex, okCodex := manager.Executor("codex")
	afterXAI, okXAI := manager.Executor("xai")
	if !okCodex || afterCodex == nil || afterCodex == beforeCodex {
		t.Fatalf("codex executor was not replaced: before=%p after=%p", beforeCodex, afterCodex)
	}
	if !okXAI || afterXAI == nil || afterXAI == beforeXAI {
		t.Fatalf("xai executor was not replaced: before=%p after=%p", beforeXAI, afterXAI)
	}
}

func TestRegisterHomeExecutorsRegistersBaselineExecutors(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.ensureWebsocketGateway()
	if service.wsGateway != nil {
		t.Cleanup(func() { _ = service.wsGateway.Stop(context.Background()) })
	}

	service.registerHomeExecutors()

	providers := []string{
		"codex",
		"claude",
		"gemini",
		"gemini-interactions",
		"vertex",
		"gemini-cli",
		"aistudio",
		"antigravity",
		"kimi",
		"qwen",
		"iflow",
		"xai",
		"openai-compatibility",
	}
	for _, provider := range providers {
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil {
			t.Fatalf("expected executor for provider %s after Home registration", provider)
		}
	}
	resolved, ok := service.coreManager.Executor("xai")
	if !ok || resolved == nil {
		t.Fatal("expected xai executor after Home registration")
	}
	if _, ok := resolved.(*executor.XAIAutoExecutor); !ok {
		t.Fatalf("xai executor = %T, want *executor.XAIAutoExecutor", resolved)
	}
}

func TestRegisterModelsForAuthInteractionsUsesConfiguredModels(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "interactions-model-auth",
		Provider: "gemini-interactions",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":   "interactions-key",
			"auth_kind": "apikey",
		},
	}
	service := &Service{
		cfg: &config.Config{
			InteractionsKey: []config.GeminiKey{
				{
					APIKey:         "interactions-key",
					ExcludedModels: []string{"gemini-2.5-flash"},
					Models: []internalconfig.GeminiModel{
						{Name: "gemini-2.5-pro", Alias: "g25p"},
						{Name: "gemini-2.5-flash", Alias: "g25f"},
					},
				},
			},
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)

	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 1 {
		t.Fatalf("registered models length = %d, want 1", len(models))
	}
	if strings.TrimSpace(models[0].ID) != "g25p" {
		t.Fatalf("model id = %q, want g25p", models[0].ID)
	}
	if strings.TrimSpace(models[0].CatalogName) != "gemini-2.5-pro" {
		t.Fatalf("catalog name = %q, want gemini-2.5-pro", models[0].CatalogName)
	}
	if strings.TrimSpace(models[0].DisplayName) != "gemini-2.5-pro" {
		t.Fatalf("display name = %q, want gemini-2.5-pro", models[0].DisplayName)
	}
}

func TestHandleAuthUpdateAllowsFileWatcherRecreateRemovedAuth(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient("auth-1") })

	service.handleAuthUpdate(
		context.Background(), AuthUpdate{
			Action:            AuthUpdateActionAdd,
			ID:                "auth-1",
			Auth:              &coreauth.Auth{ID: "auth-1", Provider: "test", Metadata: map[string]any{"token": "old"}},
			AllowAuthRecreate: true,
		},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); !ok {
		t.Fatal("expected initial file auth to be registered")
	}

	service.handleAuthUpdate(
		context.Background(), AuthUpdate{Action: AuthUpdateActionDelete, ID: "auth-1"},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); ok {
		t.Fatal("expected auth to be removed")
	}

	service.handleAuthUpdate(
		context.Background(), AuthUpdate{
			Action: AuthUpdateActionAdd,
			ID:     "auth-1",
			Auth:   &coreauth.Auth{ID: "auth-1", Provider: "test", Metadata: map[string]any{"token": "stale"}},
		},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); ok {
		t.Fatal("expected unmarked update not to recreate tombstoned auth")
	}

	service.handleAuthUpdate(
		context.Background(), AuthUpdate{
			Action:            AuthUpdateActionAdd,
			ID:                "auth-1",
			Auth:              &coreauth.Auth{ID: "auth-1", Provider: "test", Metadata: map[string]any{"token": "new"}},
			AllowAuthRecreate: true,
		},
	)
	updated, ok := service.coreManager.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatal("expected file watcher recreate to register auth")
	}
	if updated.Metadata["token"] != "new" {
		t.Fatalf("token = %#v, want new", updated.Metadata["token"])
	}
}
