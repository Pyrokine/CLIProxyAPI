package cliproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestRegisterAntigravityModelsDoesNotWaitForDiscovery(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{})
	fetchAntigravityModels = func(
		ctx context.Context,
		_ *coreauth.Auth,
		_ *config.Config,
	) []*registry.ModelInfo {
		close(started)
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{
		ID:       "ag-async-discovery",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(
		func() {
			if err := service.stopModelRefreshWorkers(context.Background()); err != nil {
				t.Errorf("stopModelRefreshWorkers() error = %v", err)
			}
		},
	)

	returned := make(chan struct{})
	go func() {
		service.registerModelsForAuth(auth)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("registerModelsForAuth blocked on Antigravity discovery")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Antigravity discovery did not start")
	}
}

func TestAntigravityDiscoveryRejectsStaleConfigResult(t *testing.T) {
	previousFetch := fetchAntigravityModels
	firstStarted := make(chan struct{})
	fetchAntigravityModels = func(
		ctx context.Context,
		_ *coreauth.Auth,
		cfg *config.Config,
	) []*registry.ModelInfo {
		if cfg.ProxyURL == "first" {
			close(firstStarted)
			<-ctx.Done()
			return []*registry.ModelInfo{{ID: "stale-model"}}
		}
		return []*registry.ModelInfo{{ID: "fresh-model"}}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{
		ID:       "ag-stale-config",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	firstCfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "first"}}
	service := &Service{cfg: firstCfg, coreManager: manager}
	t.Cleanup(
		func() {
			if err := service.stopModelRefreshWorkers(context.Background()); err != nil {
				t.Errorf("stopModelRefreshWorkers() error = %v", err)
			}
		},
	)
	modelRegistry := GlobalModelRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first Antigravity discovery did not start")
	}
	secondCfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "second"}}
	service.publishConfig(secondCfg)
	service.registerModelsForAuth(auth)

	deadline := time.Now().Add(time.Second)
	for !modelRegistry.ClientSupportsModel(auth.ID, "fresh-model") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !modelRegistry.ClientSupportsModel(auth.ID, "fresh-model") {
		t.Fatal("fresh Antigravity model was not registered")
	}
	if modelRegistry.ClientSupportsModel(auth.ID, "stale-model") {
		t.Fatal("stale Antigravity discovery result was registered")
	}
}

func TestBackfillAntigravityModels_RegistersMissingAuth(t *testing.T) {
	source := &coreauth.Auth{
		ID:       "ag-backfill-source",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	target := &coreauth.Auth{
		ID:       "ag-backfill-target",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), source); err != nil {
		t.Fatalf("register source auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), target); err != nil {
		t.Fatalf("register target auth: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(source.ID)
	reg.UnregisterClient(target.ID)
	t.Cleanup(
		func() {
			reg.UnregisterClient(source.ID)
			reg.UnregisterClient(target.ID)
		},
	)

	primary := []*ModelInfo{
		{ID: "claude-sonnet-4-5"},
		{ID: "gemini-2.5-pro"},
	}
	reg.RegisterClient(source.ID, "antigravity", toRegistryModelInfos(primary))

	service.backfillAntigravityModels(source, primary, service.cfg)

	got := reg.GetModelsForClient(target.ID)
	if len(got) != 2 {
		t.Fatalf("expected target auth to be backfilled with 2 models, got %d", len(got))
	}

	ids := make(map[string]struct{}, len(got))
	for _, model := range got {
		if model == nil {
			continue
		}
		ids[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
	}
	if _, ok := ids["claude-sonnet-4-5"]; !ok {
		t.Fatal("expected backfilled model claude-sonnet-4-5")
	}
	if _, ok := ids["gemini-2.5-pro"]; !ok {
		t.Fatal("expected backfilled model gemini-2.5-pro")
	}
}

func TestBackfillAntigravityModels_RespectsExcludedModels(t *testing.T) {
	source := &coreauth.Auth{
		ID:       "ag-backfill-source-excluded",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	target := &coreauth.Auth{
		ID:       "ag-backfill-target-excluded",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-pro",
		},
	}

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), source); err != nil {
		t.Fatalf("register source auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), target); err != nil {
		t.Fatalf("register target auth: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(source.ID)
	reg.UnregisterClient(target.ID)
	t.Cleanup(
		func() {
			reg.UnregisterClient(source.ID)
			reg.UnregisterClient(target.ID)
		},
	)

	primary := []*ModelInfo{
		{ID: "claude-sonnet-4-5"},
		{ID: "gemini-2.5-pro"},
	}
	reg.RegisterClient(source.ID, "antigravity", toRegistryModelInfos(primary))

	service.backfillAntigravityModels(source, primary, service.cfg)

	got := reg.GetModelsForClient(target.ID)
	if len(got) != 1 {
		t.Fatalf("expected 1 model after exclusion, got %d", len(got))
	}
	if got[0] == nil || !strings.EqualFold(strings.TrimSpace(got[0].ID), "claude-sonnet-4-5") {
		t.Fatalf("expected remaining model %q, got %+v", "claude-sonnet-4-5", got[0])
	}
}
