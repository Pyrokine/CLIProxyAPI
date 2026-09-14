package cliproxy

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
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
	) runtimeexecutor.ModelDiscoveryResult {
		close(started)
		<-ctx.Done()
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
			Err:     ctx.Err(),
		}
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
	) runtimeexecutor.ModelDiscoveryResult {
		if cfg.ProxyURL == "first" {
			close(firstStarted)
			<-ctx.Done()
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryComplete,
				Models:  []*registry.ModelInfo{{ID: "stale-model"}},
			}
		}
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: "fresh-model"}},
		}
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
	firstCfg := &config.Config{ProxyURL: "first"}
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
	secondCfg := &config.Config{ProxyURL: "second"}
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

func TestAntigravityDiscoveryDoesNotBackfillOtherCredential(t *testing.T) {
	previousFetch := fetchAntigravityModels
	fetchAntigravityModels = func(
		_ context.Context,
		auth *coreauth.Auth,
		_ *config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: "model-" + auth.ID}},
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	source := &coreauth.Auth{ID: "ag-isolated-source", Provider: "antigravity", Status: coreauth.StatusActive}
	target := &coreauth.Auth{ID: "ag-isolated-target", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, source, target)
	service.registerModelsForAuth(source)
	waitForModelRefreshCondition(
		t,
		func() bool { return GlobalModelRegistry().ClientSupportsModel(source.ID, "model-"+source.ID) },
	)
	if models := registry.GetGlobalRegistry().GetModelsForClient(target.ID); len(models) != 0 {
		t.Fatalf("source discovery populated another credential: %#v", models)
	}
}
