package cliproxy

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

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
}

func TestHandleAuthUpdateAllowsFileWatcherRecreateRemovedAuth(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient("auth-1") })

	service.handleAuthUpdate(
		context.Background(), watcher.AuthUpdate{
			Action:            watcher.AuthUpdateActionAdd,
			ID:                "auth-1",
			Auth:              &coreauth.Auth{ID: "auth-1", Provider: "test", Metadata: map[string]any{"token": "old"}},
			AllowAuthRecreate: true,
		},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); !ok {
		t.Fatal("expected initial file auth to be registered")
	}

	service.handleAuthUpdate(
		context.Background(), watcher.AuthUpdate{Action: watcher.AuthUpdateActionDelete, ID: "auth-1"},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); ok {
		t.Fatal("expected auth to be removed")
	}

	service.handleAuthUpdate(
		context.Background(), watcher.AuthUpdate{
			Action: watcher.AuthUpdateActionAdd,
			ID:     "auth-1",
			Auth:   &coreauth.Auth{ID: "auth-1", Provider: "test", Metadata: map[string]any{"token": "stale"}},
		},
	)
	if _, ok := service.coreManager.GetByID("auth-1"); ok {
		t.Fatal("expected unmarked update not to recreate tombstoned auth")
	}

	service.handleAuthUpdate(
		context.Background(), watcher.AuthUpdate{
			Action:            watcher.AuthUpdateActionAdd,
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
