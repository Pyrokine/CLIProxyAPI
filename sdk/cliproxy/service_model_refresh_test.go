package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type blockingModelRefreshStore struct {
	mu      sync.Mutex
	block   bool
	blockID string
	entered chan struct{}
	release chan struct{}
	last    *coreauth.Auth
}

func (s *blockingModelRefreshStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *blockingModelRefreshStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	block := s.block && (s.blockID == "" || s.blockID == auth.ID)
	entered := s.entered
	release := s.release
	s.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	s.mu.Lock()
	s.last = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *blockingModelRefreshStore) Delete(context.Context, string) error {
	return nil
}

func newModelRefreshTestService(
	t *testing.T,
	cfg *config.Config,
	auths ...*coreauth.Auth,
) *Service {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
		GlobalModelRegistry().UnregisterClient(auth.ID)
	}
	service := &Service{cfg: cfg, coreManager: manager}
	t.Cleanup(
		func() {
			if err := service.stopModelRefreshWorkers(context.Background()); err != nil {
				t.Errorf("stopModelRefreshWorkers() error = %v", err)
			}
			for _, auth := range auths {
				GlobalModelRegistry().UnregisterClient(auth.ID)
			}
		},
	)
	return service
}

func TestAuthRemovalWaitsForInFlightAuthPublication(t *testing.T) {
	store := &blockingModelRefreshStore{
		block:   true,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := coreauth.NewManager(store, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "serialized-auth-update",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "new"},
	}
	GlobalModelRegistry().UnregisterClient(auth.ID)
	t.Cleanup(
		func() {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			_ = manager.StopDeferredAuthRecreates(context.Background())
		},
	)

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- service.applyCoreAuthAddOrUpdate(context.Background(), auth)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("auth persistence did not start")
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("auth was published before persistence completed")
	}
	if _, ok := manager.Executor("codex"); ok {
		t.Fatal("executor was replaced before auth publication")
	}

	removeDone := make(chan struct{})
	go func() {
		service.applyCoreAuthRemoval(context.Background(), auth.ID)
		close(removeDone)
	}()
	select {
	case <-removeDone:
		t.Fatal("auth removal bypassed the in-flight publication sequence")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if errApply := <-applyDone; errApply != nil {
		t.Fatalf("applyCoreAuthAddOrUpdate() error = %v", errApply)
	}
	select {
	case <-removeDone:
	case <-time.After(time.Second):
		t.Fatal("auth removal did not finish after publication")
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("serialized removal left the auth published")
	}
	if len(registry.GetGlobalRegistry().GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("serialized removal left stale models registered")
	}
}

func TestBlockedAuthPublicationDoesNotBlockDifferentAuthRemoval(t *testing.T) {
	store := &blockingModelRefreshStore{
		block:   true,
		blockID: "blocked-auth",
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := coreauth.NewManager(store, nil, nil)
	other := &coreauth.Auth{ID: "other-auth", Provider: "codex"}
	if _, errRegister := manager.Register(context.Background(), other); errRegister != nil {
		t.Fatalf("register other auth: %v", errRegister)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	blocked := &coreauth.Auth{
		ID:       store.blockID,
		Provider: "codex",
		Metadata: map[string]any{"access_token": "blocked"},
	}
	t.Cleanup(
		func() {
			GlobalModelRegistry().UnregisterClient(blocked.ID)
			GlobalModelRegistry().UnregisterClient(other.ID)
			_ = manager.StopDeferredAuthRecreates(context.Background())
		},
	)

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- service.applyCoreAuthAddOrUpdate(context.Background(), blocked)
	}()
	select {
	case <-store.entered:
	case errApply := <-applyDone:
		t.Fatalf("blocked auth publication finished before persistence block: %v", errApply)
	case <-time.After(time.Second):
		t.Fatal("blocked auth persistence did not start")
	}
	removeDone := make(chan struct{})
	go func() {
		service.applyCoreAuthRemoval(coreauth.WithSkipPersist(context.Background()), other.ID)
		close(removeDone)
	}()
	select {
	case <-removeDone:
	case <-time.After(time.Second):
		t.Fatal("different auth removal was blocked by unrelated publication")
	}
	if _, ok := manager.GetByID(other.ID); ok {
		t.Fatal("different auth remains published after removal")
	}
	close(store.release)
	if errApply := <-applyDone; errApply != nil {
		t.Fatalf("blocked applyCoreAuthAddOrUpdate() error = %v", errApply)
	}
}

func TestStopAuthUpdateQueueCancelsAndWaitsForConsumer(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	const authID = "queued-auth"
	unlockApply, errLock := service.lockAuthApply(context.Background(), authID)
	if errLock != nil {
		t.Fatalf("lockAuthApply() error = %v", errLock)
	}
	defer unlockApply()

	service.ensureAuthUpdateQueue(context.Background())
	service.emitAuthUpdate(
		context.Background(),
		AuthUpdate{
			Action: AuthUpdateActionAdd,
			Auth:   &coreauth.Auth{ID: authID, Provider: "codex"},
		},
	)
	deadline := time.Now().Add(time.Second)
	for len(service.authUpdates) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(service.authUpdates) != 0 {
		t.Fatal("auth update consumer did not receive the queued update")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if errStop := service.stopAuthUpdateQueue(stopCtx); errStop != nil {
		t.Fatalf("stopAuthUpdateQueue() error = %v", errStop)
	}
	if _, ok := manager.GetByID(authID); ok {
		t.Fatal("canceled queue consumer published the blocked auth update")
	}
	GlobalModelRegistry().UnregisterClient(authID)
	if errStop := manager.StopDeferredAuthRecreates(context.Background()); errStop != nil {
		t.Fatalf("StopDeferredAuthRecreates() error = %v", errStop)
	}
}

func waitForModelRefreshCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("model refresh condition was not satisfied")
	}
}

func TestRuntimeOnlyAuthChurnReleasesPerAuthState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	ctx := coreauth.WithSkipPersist(context.Background())
	for index := range 100 {
		id := fmt.Sprintf("runtime-only-%d", index)
		if errApply := service.applyCoreAuthAddOrUpdate(
			ctx,
			&coreauth.Auth{
				ID:         id,
				Provider:   "aistudio",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"runtime_only": "true"},
			},
		); errApply != nil {
			t.Fatalf("applyCoreAuthAddOrUpdate(%s) error = %v", id, errApply)
		}
		service.applyCoreAuthRemoval(ctx, id)
	}
	if auths := manager.List(); len(auths) != 0 {
		t.Fatalf("runtime auths remain after churn: %d", len(auths))
	}
	service.authApplyMu.Lock()
	applyLocks := len(service.authApplyLocks)
	service.authApplyMu.Unlock()
	if applyLocks != 0 {
		t.Fatalf("service auth apply lock entries = %d, want 0", applyLocks)
	}
}

func TestServiceAuthApplyLocksAreReleasedAfterUse(t *testing.T) {
	service := &Service{}
	unlock, errLock := service.lockAuthApply(context.Background(), "runtime-auth")
	if errLock != nil {
		t.Fatalf("lockAuthApply() error = %v", errLock)
	}
	unlock()
	service.authApplyMu.Lock()
	entries := len(service.authApplyLocks)
	service.authApplyMu.Unlock()
	if entries != 0 {
		t.Fatalf("auth apply lock entries = %d, want 0", entries)
	}
}

func TestModelRefreshWorkersBoundExternalDiscoveryConcurrency(t *testing.T) {
	previousFetch := fetchAntigravityModels
	var active atomic.Int32
	var maximum atomic.Int32
	var calls atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	fetchAntigravityModels = func(
		ctx context.Context,
		auth *coreauth.Auth,
		_ *config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryComplete,
				Models:  []*registry.ModelInfo{{ID: "model-" + auth.ID}},
			}
		case <-ctx.Done():
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
				Err:     ctx.Err(),
			}
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auths := make([]*coreauth.Auth, 0, 8)
	for index := range 8 {
		auths = append(
			auths,
			&coreauth.Auth{
				ID:         fmt.Sprintf("ag-bounded-%d", index),
				Provider:   "antigravity",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth"},
			},
		)
	}
	service := newModelRefreshTestService(t, &config.Config{}, auths...)
	for _, auth := range auths {
		service.registerModelsForAuth(auth)
	}

	for range modelRefreshWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("model refresh workers did not reach the concurrency limit")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := maximum.Load(); got != modelRefreshWorkerCount {
		t.Fatalf("maximum external discovery concurrency = %d, want %d", got, modelRefreshWorkerCount)
	}
	if got := calls.Load(); got != modelRefreshWorkerCount {
		t.Fatalf("external discovery calls before release = %d, want %d", got, modelRefreshWorkerCount)
	}

	close(release)
	waitForModelRefreshCondition(t, func() bool { return calls.Load() == int32(len(auths)) })
	for _, auth := range auths {
		modelID := "model-" + auth.ID
		waitForModelRefreshCondition(
			t,
			func() bool { return GlobalModelRegistry().ClientSupportsModel(auth.ID, modelID) },
		)
	}
}

func TestModelRefreshPendingJobsCoalesceByAuth(t *testing.T) {
	previousFetch := fetchAntigravityModels
	blockersStarted := make(chan struct{}, modelRefreshWorkerCount)
	releaseBlockers := make(chan struct{})
	var targetCalls atomic.Int32
	fetchAntigravityModels = func(
		ctx context.Context,
		auth *coreauth.Auth,
		_ *config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		if auth.ID != "ag-coalesced" {
			blockersStarted <- struct{}{}
			select {
			case <-releaseBlockers:
				return runtimeexecutor.ModelDiscoveryResult{
					Outcome: runtimeexecutor.ModelDiscoveryComplete,
					Models:  []*registry.ModelInfo{{ID: "blocker-model"}},
				}
			case <-ctx.Done():
				return runtimeexecutor.ModelDiscoveryResult{
					Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
					Err:     ctx.Err(),
				}
			}
		}
		targetCalls.Add(1)
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: auth.Metadata["generation"].(string)}},
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auths := make([]*coreauth.Auth, 0, modelRefreshWorkerCount+1)
	for index := range modelRefreshWorkerCount {
		auths = append(
			auths,
			&coreauth.Auth{
				ID:         fmt.Sprintf("ag-blocker-%d", index),
				Provider:   "antigravity",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth"},
			},
		)
	}
	target := &coreauth.Auth{
		ID:         "ag-coalesced",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"generation": "model-1"},
	}
	auths = append(auths, target)
	service := newModelRefreshTestService(t, &config.Config{}, auths...)
	for _, auth := range auths[:modelRefreshWorkerCount] {
		service.registerModelsForAuth(auth)
	}
	for range modelRefreshWorkerCount {
		select {
		case <-blockersStarted:
		case <-time.After(time.Second):
			t.Fatal("blocking model refresh job did not start")
		}
	}

	for generation := 1; generation <= 3; generation++ {
		updated := target.Clone()
		updated.Metadata["generation"] = fmt.Sprintf("model-%d", generation)
		stored, err := service.coreManager.Update(context.Background(), updated)
		if err != nil {
			t.Fatalf("update target auth: %v", err)
		}
		target = stored
		service.registerModelsForAuth(target)
	}
	close(releaseBlockers)

	waitForModelRefreshCondition(
		t,
		func() bool { return GlobalModelRegistry().ClientSupportsModel(target.ID, "model-3") },
	)
	if got := targetCalls.Load(); got != 1 {
		t.Fatalf("coalesced auth discovery calls = %d, want 1", got)
	}
	if GlobalModelRegistry().ClientSupportsModel(target.ID, "model-1") ||
		GlobalModelRegistry().ClientSupportsModel(target.ID, "model-2") {
		t.Fatal("superseded coalesced model result was registered")
	}
}

func TestAntigravityTimeoutKeepsCurrentConfigProjection(t *testing.T) {
	previousFetch := fetchAntigravityModels
	previousTimeout := modelRefreshTimeout
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	fetchAntigravityModels = func(
		ctx context.Context,
		_ *coreauth.Auth,
		_ *config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		if calls.Add(1) == 1 {
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryComplete,
				Models:  []*registry.ModelInfo{{ID: "cached-model"}},
			}
		}
		close(secondStarted)
		<-ctx.Done()
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
			Err:     ctx.Err(),
		}
	}
	modelRefreshTimeout = 30 * time.Millisecond
	t.Cleanup(
		func() {
			fetchAntigravityModels = previousFetch
			modelRefreshTimeout = previousTimeout
		},
	)

	auth := &coreauth.Auth{
		ID:         "ag-timeout-projection",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.registerModelsForAuth(auth)
	waitForModelRefreshCondition(
		t,
		func() bool { return GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") },
	)

	nextCfg := &config.Config{
		OAuthExcludedModels: map[string][]string{"antigravity": {"cached-model"}},
	}
	service.publishConfig(nextCfg)
	service.registerModelsForAuth(auth)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second Antigravity discovery did not start")
	}
	time.Sleep(2 * modelRefreshTimeout)
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") {
		t.Fatal("previous config generation model remained routable after discovery timeout")
	}
}

func TestConfigUpdateReprojectsExistingAuthModelsWithoutPluginHost(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "config-reproject-qwen",
		Provider:   "qwen",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.registerModelsForAuth(auth)
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	if len(models) == 0 || models[0] == nil {
		t.Fatal("initial qwen model projection is empty")
	}
	excludedModel := models[0].ID
	nextCfg := &config.Config{
		OAuthExcludedModels: map[string][]string{"qwen": {excludedModel}},
	}
	applied, errApply := service.applyConfigUpdateRuntimeGenerationOptions(
		context.Background(),
		nextCfg,
		1,
		false,
		false,
	)
	if errApply != nil || !applied {
		t.Fatalf("applyConfigUpdateRuntimeGenerationOptions() = (%t, %v)", applied, errApply)
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, excludedModel) {
		t.Fatalf("excluded model %q remained routable after config update", excludedModel)
	}
}

func TestRefreshModelClientsReprojectsExistingXAIAuths(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		ModelCatalog: config.ModelCatalogConfig{
			Models: []config.ModelCatalogModel{
				{
					Name: "grok-4.5", Enabled: &disabled,
				},
			},
		},
		XAIKey: []config.XAIKey{
			{
				APIKey:  "xai-key",
				BaseURL: "https://api.x.ai/v1",
				Models: []config.XAIModel{
					{
						Name: "grok-4.3", Alias: "grok-explicit",
					},
				},
			},
		},
	}
	oauth := &coreauth.Auth{
		ID:         "catalog-refresh-xai-oauth",
		Provider:   "xai",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	excluded := &coreauth.Auth{
		ID:       "catalog-refresh-xai-excluded",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "grok-4.6",
		},
	}
	apiKey := &coreauth.Auth{
		ID:       "catalog-refresh-xai-api-key",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":   "xai-key",
			"base_url":  "https://api.x.ai/v1",
			"auth_kind": "apikey",
		},
	}
	serverService := newConfigCommitSequenceServiceWithConfig(t, cfg)
	service := newModelRefreshTestService(t, cfg, oauth, excluded, apiKey)
	service.server = serverService.server
	service.managementConfigGeneration = 7
	for _, auth := range []*coreauth.Auth{oauth, excluded, apiKey} {
		GlobalModelRegistry().RegisterClient(
			auth.ID,
			"xai",
			[]*ModelInfo{{ID: "stale-xai-model"}},
		)
	}

	service.cfgMu.RLock()
	beforeConfig := service.cfg
	service.cfgMu.RUnlock()
	beforeManagement := service.server.ManagementHandler().ConfigState()
	beforeGeneration := service.managementConfigGeneration
	service.refreshModelClients()

	service.cfgMu.RLock()
	afterConfig := service.cfg
	service.cfgMu.RUnlock()
	afterManagement := service.server.ManagementHandler().ConfigState()
	if afterConfig != beforeConfig || afterManagement.Config != beforeManagement.Config {
		t.Fatal("catalog refresh replaced the published configuration")
	}
	if afterManagement.Generation != beforeManagement.Generation ||
		service.managementConfigGeneration != beforeGeneration {
		t.Fatalf(
			"catalog refresh changed config generations from (%d, %d) to (%d, %d)",
			beforeManagement.Generation,
			beforeGeneration,
			afterManagement.Generation,
			service.managementConfigGeneration,
		)
	}
	if !GlobalModelRegistry().ClientSupportsModel(oauth.ID, "grok-4.6") {
		t.Fatal("catalog refresh did not register Grok 4.6 for an existing xAI OAuth credential")
	}
	if GlobalModelRegistry().ClientSupportsModel(oauth.ID, "grok-4.5") {
		t.Fatal("catalog refresh ignored the disabled model catalog entry")
	}
	if GlobalModelRegistry().ClientSupportsModel(excluded.ID, "grok-4.6") ||
		!GlobalModelRegistry().ClientSupportsModel(excluded.ID, "grok-4.3") {
		t.Fatal("catalog refresh ignored the xAI OAuth exclusion")
	}
	if !GlobalModelRegistry().ClientSupportsModel(apiKey.ID, "grok-explicit") ||
		GlobalModelRegistry().ClientSupportsModel(apiKey.ID, "grok-4.6") {
		t.Fatal("catalog refresh replaced an explicit xAI API key model list")
	}
	for _, auth := range []*coreauth.Auth{oauth, excluded, apiKey} {
		if GlobalModelRegistry().ClientSupportsModel(auth.ID, "stale-xai-model") {
			t.Fatalf("catalog refresh left stale models registered for %s", auth.ID)
		}
	}
}

func TestModelRefreshCancelsAfterNativeCredentialMutation(t *testing.T) {
	for _, mutation := range []string{"update", "disable", "route", "prefix", "replace", "merge", "remove", "reload", "config-proxy"} {
		t.Run(mutation, func(t *testing.T) {
			previousFetch := fetchAntigravityModels
			started, canceled := make(chan struct{}), make(chan struct{})
			fetchAntigravityModels = func(
				ctx context.Context,
				_ *coreauth.Auth,
				_ *config.Config,
			) runtimeexecutor.ModelDiscoveryResult {
				close(started)
				<-ctx.Done()
				close(canceled)
				return runtimeexecutor.ModelDiscoveryResult{
					Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
					Err:     ctx.Err(),
				}
			}
			t.Cleanup(func() { fetchAntigravityModels = previousFetch })
			auth := &coreauth.Auth{
				ID: "native-model-refresh-" + mutation, Provider: "antigravity", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "synthetic-access-token"},
			}
			service := newModelRefreshTestService(t, &config.Config{}, auth)
			service.registerModelsForAuth(auth)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("model discovery did not start")
			}
			switch mutation {
			case "update", "disable", "route", "prefix", "replace", "merge":
				current, _ := service.coreManager.GetByID(auth.ID)
				updated := current.Clone()
				switch mutation {
				case "disable":
					updated.Disabled = true
				case "route":
					updated.ProxyURL = "http://synthetic-proxy.invalid"
				case "prefix":
					updated.Prefix = "synthetic-prefix"
				default:
					updated.Metadata["access_token"] = "synthetic-rotated-token"
				}
				var err error
				switch mutation {
				case "replace":
					_, err = service.coreManager.Register(t.Context(), updated)
				case "merge":
					_, err = service.coreManager.MergePersistentUpdate(t.Context(), current, updated)
				default:
					_, err = service.coreManager.Update(t.Context(), updated)
				}
				if err != nil {
					t.Fatalf("mutate auth: %v", err)
				}
			case "remove":
				if err := service.coreManager.Remove(coreauth.WithSkipPersist(t.Context()), auth.ID); err != nil {
					t.Fatalf("remove auth: %v", err)
				}
			case "reload":
				service.coreManager.SetStore(&blockingModelRefreshStore{})
				if err := service.coreManager.Load(t.Context()); err != nil {
					t.Fatalf("reload auth: %v", err)
				}
			case "config-proxy":
				next := &config.Config{}
				next.ProxyURL = "http://synthetic-config-proxy.invalid"
				service.publishConfig(next)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("native credential mutation did not cancel connected model discovery")
			}
			stopCtx, cancelStop := context.WithTimeout(t.Context(), time.Second)
			defer cancelStop()
			if err := service.stopModelRefreshWorkers(stopCtx); err != nil {
				t.Fatalf("stop canceled model discovery: %v", err)
			}
			service.modelRefreshMu.Lock()
			active, pending := len(service.modelRefreshActive), len(service.modelRefreshPending)
			service.modelRefreshMu.Unlock()
			if active != 0 || pending != 0 {
				t.Fatalf("canceled discovery retained work: active=%d pending=%d", active, pending)
			}
			if current, ok := service.coreManager.GetByID(auth.ID); ok &&
				(current.LastError != nil || current.Quota.Exceeded || len(current.ModelStates) != 0) {
				t.Fatal("credential invalidation recorded an upstream model failure")
			}
		})
	}
}

func TestModelRefreshRejectsStaleAuthIdentity(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{})
	release := make(chan struct{})
	fetchAntigravityModels = func(
		ctx context.Context,
		_ *coreauth.Auth,
		_ *config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		close(started)
		select {
		case <-release:
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryComplete,
				Models:  []*registry.ModelInfo{{ID: "stale-auth-model"}},
			}
		case <-ctx.Done():
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryInconclusive,
				Err:     ctx.Err(),
			}
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{
		ID:         "ag-stale-auth",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		ProxyURL:   "http://first.example",
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.registerModelsForAuth(auth)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Antigravity discovery did not start")
	}
	updated := auth.Clone()
	updated.ProxyURL = "http://second.example"
	if _, err := service.coreManager.Update(context.Background(), updated); err != nil {
		t.Fatalf("update auth identity: %v", err)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "stale-auth-model") {
		t.Fatal("stale auth discovery result was registered")
	}
}

func TestModelRefreshRejectsStalePluginSnapshot(t *testing.T) {
	auth := &coreauth.Auth{ID: "plugin-stale-snapshot", Provider: "plugin-provider", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.pluginHost = pluginhost.New()
	job := currentModelRefreshJob(t, service, auth.ID, true, 1)
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResultFromPlugin(
			pluginhost.AuthModelResult{
				Provider: "plugin-provider",
				Models:   []*registry.ModelInfo{{ID: "stale-plugin-model"}},
				Snapshot: &pluginhost.Snapshot{},
				Handled:  true,
			},
		),
	)
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "stale-plugin-model") {
		t.Fatal("stale plugin snapshot result was registered")
	}
}

func TestPluginModelRefreshPersistenceDoesNotHoldPublicationLock(t *testing.T) {
	store := &blockingModelRefreshStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:         "plugin-persistence-cancel",
		Provider:   "plugin-provider",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"token": "old"},
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg:                 &config.Config{},
		coreManager:         manager,
		modelRefreshVersion: map[string]uint64{auth.ID: 1},
		modelRefreshCancel:  map[string]context.CancelFunc{auth.ID: cancel},
		modelRefreshPending: make(map[string]modelRefreshJob),
		modelRefreshCache:   make(map[string]modelRefreshCacheEntry),
	}
	pluginAuth := registered.Clone()
	pluginAuth.Metadata["token"] = "new"
	job := modelRefreshJob{
		auth: registered.Clone(), cfg: service.cfg, version: 1,
		registryEpoch: GlobalModelRegistry().ClientRegistrationEpoch(auth.ID),
	}
	done := make(chan struct{})
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	result := modelDiscoveryResultFromPlugin(
		pluginhost.AuthModelResult{
			Provider: "plugin-provider",
			Models:   []*registry.ModelInfo{{ID: "plugin-model"}},
			Auth:     pluginAuth,
			Handled:  true,
		},
	)
	go func() {
		service.applyModelDiscoveryAuthUpdate(ctx, job, registered.Clone(), result)
		close(done)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin auth persistence did not start")
	}

	cancelDone := make(chan struct{})
	go func() {
		service.cancelModelRefresh(auth.ID)
		close(cancelDone)
	}()
	select {
	case <-cancelDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelModelRefresh blocked on plugin auth persistence")
	}
	close(store.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("plugin model refresh did not finish after persistence release")
	}

	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Metadata["token"] != "new" {
		t.Fatalf("reserved plugin auth update was not published: %#v", current)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "new" {
		t.Fatalf("reserved plugin auth update was not persisted: %#v", persisted)
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "plugin-model") {
		t.Fatal("canceled plugin model result was published")
	}
}

func TestPluginModelRefreshPreservesLatestRuntimeState(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "plugin-runtime-state",
		Provider:   "plugin-provider",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"token": "old"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.modelRefreshVersion = map[string]uint64{auth.ID: 1}
	jobAuth, ok := service.coreManager.GetByID(auth.ID)
	if !ok {
		t.Fatal("registered auth is missing")
	}

	retryAfter := time.Hour
	service.coreManager.MarkResult(
		context.Background(), coreauth.Result{
			AuthID: auth.ID,
			Model:  "plugin-model",
			Error: &coreauth.Error{
				HTTPStatus: 429,
				Message:    "quota exhausted",
			},
			RetryAfter: &retryAfter,
		},
	)
	pluginAuth := jobAuth.Clone()
	pluginAuth.Metadata["token"] = "new"
	pluginAuth.Status = coreauth.StatusActive
	pluginAuth.StatusMessage = ""
	pluginAuth.ModelStates = nil
	service.publishModelRefreshResult(
		context.Background(),
		modelRefreshJob{
			auth: jobAuth.Clone(), cfg: service.cfg, version: 1,
			registryEpoch: GlobalModelRegistry().ClientRegistrationEpoch(auth.ID),
		},
		modelDiscoveryResultFromPlugin(
			pluginhost.AuthModelResult{
				Provider: "plugin-provider",
				Models:   []*registry.ModelInfo{{ID: "plugin-model"}},
				Auth:     pluginAuth,
				Handled:  true,
			},
		),
	)

	updated, ok := service.coreManager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth is missing")
	}
	if updated.Metadata["token"] != "new" {
		t.Fatalf("plugin token = %v, want new", updated.Metadata["token"])
	}
	state := updated.ModelStates["plugin-model"]
	if state == nil || !state.Quota.Exceeded || state.Status != coreauth.StatusError {
		t.Fatalf("runtime model state was overwritten: %#v", state)
	}
	if updated.Status != coreauth.StatusError {
		t.Fatalf("auth status = %q, want %q", updated.Status, coreauth.StatusError)
	}
}

func TestModelRefreshFailureBackoffWindowsAndRetention(t *testing.T) {
	for _, sample := range []struct {
		name   string
		random func(int64) int64
	}{
		{"lower", func(int64) int64 { return 0 }},
		{"middle", func(n int64) int64 { return n / 2 }},
		{"upper", func(n int64) int64 { return n - 1 }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var state modelRefreshFailureState
			for i := range 300 {
				window := []time.Duration{2, 4, 8, 16, 30}[min(i, 4)] * time.Minute
				state = nextModelRefreshFailure(state, now, sample.random)
				delay := state.nextRetry.Sub(now)
				expected := window/2 + time.Duration(sample.random(int64(window/2)+1))
				if delay != expected || state.failures != uint8(min(i+1, 5)) || !state.lastFailure.Equal(now) {
					t.Fatalf("attempt %d: state=%+v delay=%s want=%s", i+1, state, delay, expected)
				}
				now = state.nextRetry
			}
			state = nextModelRefreshFailure(state, state.nextRetry.Add(time.Minute), sample.random)
			if state.failures != 5 {
				t.Fatal("expired retry deadline discarded failure history")
			}
			state = nextModelRefreshFailure(state, state.lastFailure.Add(2*modelRefreshCacheTTL+time.Nanosecond), sample.random)
			if state.failures != 1 {
				t.Fatal("inactive failure history did not reset")
			}
		})
	}
}

func TestAntigravityModelRefreshBackoffLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var success atomic.Bool
		previousFetch := fetchAntigravityModels
		fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
			calls.Add(1)
			if success.Load() {
				return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete, Models: []*registry.ModelInfo{{ID: "synthetic-backoff-model"}}}
			}
			return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryFailed, Err: fmt.Errorf("synthetic discovery failure")}
		}
		t.Cleanup(func() { fetchAntigravityModels = previousFetch })
		auth := &coreauth.Auth{ID: "synthetic-backoff-lifecycle", Provider: "antigravity", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "synthetic-initial"}}
		service := newModelRefreshTestService(t, &config.Config{}, auth)
		service.registerModelsForAuth(auth)
		synctest.Wait()
		for attempt := range 8 {
			service.modelRefreshMu.Lock()
			state, exists := service.modelRefreshFailures[auth.ID]
			service.modelRefreshMu.Unlock()
			window := []time.Duration{2, 4, 8, 16, 30}[min(attempt, 4)] * time.Minute
			delay := state.nextRetry.Sub(state.lastFailure)
			if !exists || calls.Load() != int32(attempt+1) || state.failures != uint8(min(attempt+1, 5)) || delay < window/2 || delay > window {
				t.Fatalf("attempt %d: calls=%d state=%+v", attempt+1, calls.Load(), state)
			}
			time.Sleep(time.Until(state.nextRetry) - time.Nanosecond)
			service.refreshExternalModelClients(t.Context())
			service.registerModelsForAuth(auth)
			synctest.Wait()
			if calls.Load() != int32(attempt+1) {
				t.Fatal("periodic or registration refresh bypassed the retry deadline")
			}
			if attempt == 7 {
				success.Store(true)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
		}
		service.modelRefreshMu.Lock()
		_, failed := service.modelRefreshFailures[auth.ID]
		cached := service.modelRefreshCache[auth.ID]
		service.modelRefreshMu.Unlock()
		if calls.Load() != 9 || failed || !GlobalModelRegistry().ClientSupportsModel(auth.ID, "synthetic-backoff-model") {
			t.Fatal("successful retry did not publish the catalog and reset failure history")
		}
		success.Store(false)
		time.Sleep(modelRefreshCacheTTL)
		synctest.Wait()
		service.modelRefreshMu.Lock()
		failure := service.modelRefreshFailures[auth.ID]
		retained := service.modelRefreshCache[auth.ID]
		service.modelRefreshMu.Unlock()
		if calls.Load() != 10 || failure.failures != 1 || !retained.observedAt.Equal(cached.observedAt) || !GlobalModelRegistry().ClientSupportsModel(auth.ID, "synthetic-backoff-model") {
			t.Fatal("failed refresh discarded the last catalog or inherited the pre-success failure count")
		}
		rotated, _ := service.coreManager.GetByID(auth.ID)
		rotated.Metadata["access_token"] = "synthetic-rotated"
		rotated, err := service.coreManager.Update(t.Context(), rotated)
		if err != nil {
			t.Fatal(err)
		}
		service.registerModelsForAuth(rotated)
		synctest.Wait()
		service.modelRefreshMu.Lock()
		rotatedFailure := service.modelRefreshFailures[auth.ID]
		service.modelRefreshMu.Unlock()
		if calls.Load() != 11 || rotatedFailure.failures != 1 || rotatedFailure.identityKey == failure.identityKey {
			t.Fatal("rotated credentials inherited the previous retry history")
		}
	})
}

func TestAntigravityModelRefreshPreservesRetryDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		type observation struct {
			id      string
			elapsed time.Duration
		}
		observed := make(chan observation, 2)
		previousFetch := fetchAntigravityModels
		fetchAntigravityModels = func(_ context.Context, auth *coreauth.Auth, _ *config.Config) runtimeexecutor.ModelDiscoveryResult {
			observed <- observation{auth.ID, time.Since(start)}
			return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete}
		}
		t.Cleanup(func() { fetchAntigravityModels = previousFetch })
		service := newModelRefreshTestService(t, &config.Config{},
			&coreauth.Auth{ID: "synthetic-early-retry", Provider: "antigravity", Status: coreauth.StatusActive},
			&coreauth.Auth{ID: "synthetic-late-retry", Provider: "antigravity", Status: coreauth.StatusActive})
		deadlines := map[string]time.Duration{"synthetic-early-retry": 70 * time.Second, "synthetic-late-retry": 110 * time.Second}
		service.modelRefreshFailures = make(map[string]modelRefreshFailureState)
		for id, deadline := range deadlines {
			job := currentModelRefreshJob(t, service, id, false, 1)
			service.modelRefreshFailures[id] = modelRefreshFailureState{identityKey: job.identity.key, jobVersion: job.version, failures: 1, lastFailure: start, nextRetry: start.Add(deadline)}
		}
		service.modelRefreshMu.Lock()
		service.ensureModelRefreshWorkersLocked()
		service.modelRefreshMu.Unlock()
		synctest.Wait()
		time.Sleep(65 * time.Second)
		service.modelRefreshMu.Lock()
		service.wakeModelRefreshWorkerLocked()
		service.modelRefreshMu.Unlock()
		synctest.Wait()
		if len(observed) != 0 {
			t.Fatal("a retry started before its deadline")
		}
		time.Sleep(65 * time.Second)
		synctest.Wait()
		if len(observed) != 2 {
			t.Fatalf("observed %d retries", len(observed))
		}
		for range 2 {
			got := <-observed
			if got.elapsed != deadlines[got.id] {
				t.Errorf("%s retried at %s, want %s", got.id, got.elapsed, deadlines[got.id])
			}
		}
	})
}

func TestAntigravityModelRefreshFailureCleanup(t *testing.T) {
	for _, action := range []string{"remove", "disable", "stop"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				previousFetch := fetchAntigravityModels
				fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
					calls.Add(1)
					return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryFailed, Err: fmt.Errorf("synthetic discovery failure")}
				}
				t.Cleanup(func() { fetchAntigravityModels = previousFetch })
				auth := &coreauth.Auth{ID: "synthetic-failure-cleanup-" + action, Provider: "antigravity", Status: coreauth.StatusActive}
				service := newModelRefreshTestService(t, &config.Config{}, auth)
				service.registerModelsForAuth(auth)
				synctest.Wait()
				service.modelRefreshMu.Lock()
				_, failed := service.modelRefreshFailures[auth.ID]
				service.modelRefreshMu.Unlock()
				if !failed || calls.Load() != 1 {
					t.Fatal("initial failure did not create retry state")
				}
				switch action {
				case "remove":
					service.applyCoreAuthRemoval(t.Context(), auth.ID)
				case "disable":
					disabled, _ := service.coreManager.GetByID(auth.ID)
					disabled.Disabled = true
					disabled, err := service.coreManager.Update(t.Context(), disabled)
					if err != nil {
						t.Fatal(err)
					}
					service.registerModelsForAuth(disabled)
				case "stop":
					if err := service.stopModelRefreshWorkers(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				service.modelRefreshMu.Lock()
				remaining := len(service.modelRefreshFailures) + len(service.modelRefreshCache) + len(service.modelRefreshPending) + len(service.modelRefreshActive)
				service.modelRefreshMu.Unlock()
				if remaining != 0 || calls.Load() != 1 {
					t.Fatalf("%s retained retry work: state=%d calls=%d", action, remaining, calls.Load())
				}
			})
		})
	}
}

func TestAntigravityModelRefreshTimeoutDoesNotChangeAvailability(t *testing.T) {
	for _, outcome := range []string{"timeout", "cancel", "credential-change"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				previousFetch := fetchAntigravityModels
				fetchAntigravityModels = func(ctx context.Context, _ *coreauth.Auth, _ *config.Config) runtimeexecutor.ModelDiscoveryResult {
					<-ctx.Done()
					return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryInconclusive, Err: ctx.Err()}
				}
				t.Cleanup(func() { fetchAntigravityModels = previousFetch })
				auth := &coreauth.Auth{ID: "synthetic-discovery-availability-" + outcome, Provider: "antigravity", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "synthetic-initial"}}
				service := newModelRefreshTestService(t, &config.Config{}, auth)
				retry := time.Hour
				service.coreManager.MarkResult(t.Context(), coreauth.Result{AuthID: auth.ID, Model: "synthetic-cooling-model", Error: &coreauth.Error{HTTPStatus: 429, Message: "synthetic existing cooldown"}, RetryAfter: &retry})
				before, _ := service.coreManager.GetByID(auth.ID)
				service.registerModelsForAuth(before)
				synctest.Wait()
				switch outcome {
				case "cancel":
					service.cancelModelRefresh(auth.ID)
				case "credential-change":
					updated := before.Clone()
					updated.Metadata["access_token"] = "synthetic-updated"
					if _, err := service.coreManager.Update(t.Context(), updated); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(modelRefreshTimeout)
				synctest.Wait()
				service.modelRefreshMu.Lock()
				failure, failed := service.modelRefreshFailures[auth.ID]
				service.modelRefreshMu.Unlock()
				if failed != (outcome == "timeout") || failed && failure.failures != 1 {
					t.Fatalf("%s retry state = %+v, present=%t", outcome, failure, failed)
				}
				after, _ := service.coreManager.GetByID(auth.ID)
				if after.Status != before.Status || !reflect.DeepEqual(after.LastError, before.LastError) || !reflect.DeepEqual(after.Quota, before.Quota) || !reflect.DeepEqual(after.ModelStates, before.ModelStates) {
					t.Fatal("model discovery changed credential availability or model cooldown")
				}
			})
		})
	}
}

func TestAntigravityModelRefreshHonorsCacheLifetime(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				previousFetch := fetchAntigravityModels
				fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
					calls.Add(1)
					result := runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete}
					if !empty {
						result.Models = []*registry.ModelInfo{{ID: "synthetic-cached-model"}}
					}
					return result
				}
				t.Cleanup(func() { fetchAntigravityModels = previousFetch })
				auth := &coreauth.Auth{ID: "synthetic-cache-lifetime", Provider: "antigravity", Status: coreauth.StatusActive}
				service := newModelRefreshTestService(t, &config.Config{}, auth)
				service.registerModelsForAuth(auth)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("initial discovery calls = %d", calls.Load())
				}
				for range 10 {
					service.registerModelsForAuth(auth)
					synctest.Wait()
				}
				if calls.Load() != 1 {
					t.Errorf("fresh authoritative catalog was fetched again: calls=%d", calls.Load())
				}
				time.Sleep(3*time.Hour - time.Nanosecond)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Errorf("catalog refreshed before its TTL: calls=%d", calls.Load())
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if calls.Load() != 2 {
					t.Errorf("expired catalog was not refreshed without a config change: calls=%d", calls.Load())
				}
			})
		})
	}
}

func TestAntigravityModelRefreshThrottlesFailedRegistrations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		previousFetch := fetchAntigravityModels
		fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
			calls.Add(1)
			return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryFailed, Err: fmt.Errorf("synthetic discovery failure")}
		}
		t.Cleanup(func() { fetchAntigravityModels = previousFetch })
		auth := &coreauth.Auth{ID: "synthetic-registration-backoff", Provider: "antigravity", Status: coreauth.StatusActive}
		service := newModelRefreshTestService(t, &config.Config{}, auth)
		service.registerModelsForAuth(auth)
		synctest.Wait()
		for range 10 {
			service.registerModelsForAuth(auth)
			synctest.Wait()
		}
		if calls.Load() != 1 {
			t.Fatalf("registration bypassed failure retry deadline: calls=%d", calls.Load())
		}
	})
}

func TestAntigravityModelRefreshRetriesWithoutCatalogChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		previousFetch := fetchAntigravityModels
		fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
			if calls.Add(1) == 1 {
				return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryFailed, Err: fmt.Errorf("synthetic first failure")}
			}
			return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete, Models: []*registry.ModelInfo{{ID: "synthetic-retry-model"}}}
		}
		t.Cleanup(func() { fetchAntigravityModels = previousFetch })
		auth := &coreauth.Auth{ID: "synthetic-autonomous-retry", Provider: "antigravity", Status: coreauth.StatusActive}
		service := newModelRefreshTestService(t, &config.Config{}, auth)
		service.registerModelsForAuth(auth)
		synctest.Wait()
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if calls.Load() != 2 || !GlobalModelRegistry().ClientSupportsModel(auth.ID, "synthetic-retry-model") {
			t.Fatalf("failed catalog was not retried independently: calls=%d", calls.Load())
		}
	})
}

func TestAntigravityModelRefreshDoesNotMigrateCatalogAcrossProject(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "synthetic-project-migration", Provider: "antigravity", Status: coreauth.StatusActive,
		Metadata: map[string]any{"project_id": "synthetic-old-project"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	service.publishModelRefreshResult(context.Background(), job, modelDiscoveryResult{
		source: modelDiscoverySourceAntigravity, outcome: modelDiscoveryAuthoritativePositive,
		models: []*ModelInfo{{ID: "synthetic-old-project-model"}},
	})
	job = currentModelRefreshJob(t, service, auth.ID, false, 2)
	updated := job.auth.Clone()
	updated.Metadata["project_id"] = "synthetic-new-project"
	service.publishModelRefreshResult(context.Background(), job, modelDiscoveryResult{
		source: modelDiscoverySourceAntigravity, outcome: modelDiscoveryInconclusive,
		auth: updated, err: fmt.Errorf("synthetic incomplete response"),
	})
	current, ok := service.coreManager.GetByID(auth.ID)
	if !ok || current.Metadata["project_id"] != "synthetic-new-project" {
		t.Fatal("discovery credential update was not applied")
	}
	if _, exists := service.modelRefreshCache[auth.ID]; exists || GlobalModelRegistry().ClientSupportsModel(auth.ID, "synthetic-old-project-model") {
		t.Fatal("catalog from the old project crossed into the refreshed project")
	}
}

func currentModelRefreshJob(
	t *testing.T,
	service *Service,
	authID string,
	tryPlugin bool,
	version uint64,
) modelRefreshJob {
	t.Helper()
	auth, ok := service.coreManager.GetByID(authID)
	if !ok {
		t.Fatalf("auth %q is missing", authID)
	}
	identity, errIdentity := service.buildModelRefreshIdentity(auth, service.cfg, tryPlugin)
	if errIdentity != nil {
		t.Fatalf("buildModelRefreshIdentity() error = %v", errIdentity)
	}
	service.modelRefreshMu.Lock()
	if service.modelRefreshVersion == nil {
		service.modelRefreshVersion = make(map[string]uint64)
	}
	service.modelRefreshVersion[authID] = version
	service.modelRefreshMu.Unlock()
	return modelRefreshJob{
		auth:          auth,
		cfg:           service.cfg,
		identity:      identity,
		version:       version,
		registryEpoch: GlobalModelRegistry().ClientRegistrationEpoch(authID),
		tryPlugin:     tryPlugin,
	}
}

func TestAntigravityDiscoveryWebSearchCapabilityReachesClientCatalog(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "antigravity-web-search-discovery", Provider: "antigravity", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	supported, unsupported := true, false
	service.publishModelRefreshResult(context.Background(), job, modelDiscoveryResult{
		source: modelDiscoverySourceAntigravity, outcome: modelDiscoveryAuthoritativePositive,
		models: []*ModelInfo{
			fromRegistryModelInfo(&registry.ModelInfo{ID: "ag-search-discovered", SupportsWebSearch: true,
				NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported}}),
			fromRegistryModelInfo(&registry.ModelInfo{ID: "ag-search-denied",
				NativeCapabilities: &registry.NativeCapabilities{WebSearch: &unsupported}}),
		},
	})
	catalog := registry.GetGlobalRegistry()
	for _, test := range []struct {
		id   string
		want bool
	}{
		{id: "ag-search-discovered", want: true},
		{id: "ag-search-denied", want: false},
	} {
		if !catalog.ClientSupportsModel(auth.ID, test.id) {
			t.Fatalf("discovered model %q not registered", test.id)
		}
		capability := catalog.GetResponsesWebSearchCapability(test.id)
		if capability == nil || *capability != test.want {
			t.Fatalf("%s capability = %v, want %t", test.id, capability, test.want)
		}
		response := codexmodels.BuildResponseForClient([]map[string]any{{"id": test.id}}, catalog.GetModelProviders, false, "cpa")
		models, ok := response["models"].([]map[string]any)
		if !ok || len(models) != 1 {
			t.Fatalf("%s client models = %#v", test.id, response["models"])
		}
		meta, ok := models[0]["cpa_capabilities"].(map[string]any)
		if !ok || meta["web_search"] != test.want {
			t.Fatalf("%s client search capability = %#v, want %t", test.id, meta, test.want)
		}
	}
}

func TestOpenAICompatibilityDiscoveryRegistersGPT6Astra(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "openai-compat-astra-discovery",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "gpt-6-astra") {
		t.Fatal("Astra was registered before provider discovery")
	}

	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResultFromPlugin(
			pluginhost.AuthModelResult{
				Provider: "openai-compatibility",
				Outcome:  "complete",
				Models:   []*registry.ModelInfo{{ID: "gpt-6-astra", Type: "openai-compatibility"}},
				Handled:  true,
			},
		),
	)
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "gpt-6-astra") {
		t.Fatal("authoritatively discovered Astra model was not registered")
	}
}

func TestPluginModelDiscoveryOutcomeMapping(t *testing.T) {
	tests := []struct {
		name        string
		plugin      pluginhost.AuthModelResult
		wantOutcome modelDiscoveryOutcome
	}{
		{
			name: "default complete positive",
			plugin: pluginhost.AuthModelResult{
				Models: []*registry.ModelInfo{{ID: "model-1"}}, Handled: true,
			},
			wantOutcome: modelDiscoveryAuthoritativePositive,
		},
		{
			name:        "complete empty",
			plugin:      pluginhost.AuthModelResult{Outcome: "complete", Handled: true},
			wantOutcome: modelDiscoveryAuthoritativeNegative,
		},
		{
			name: "partial",
			plugin: pluginhost.AuthModelResult{
				Models: []*registry.ModelInfo{{ID: "partial-model"}}, Outcome: "partial", Handled: true,
			},
			wantOutcome: modelDiscoveryPartial,
		},
		{
			name:        "inconclusive",
			plugin:      pluginhost.AuthModelResult{Outcome: "inconclusive", Handled: true},
			wantOutcome: modelDiscoveryInconclusive,
		},
		{
			name:        "failure",
			plugin:      pluginhost.AuthModelResult{Err: fmt.Errorf("discovery failed"), Handled: true},
			wantOutcome: modelDiscoveryFailure,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				result := modelDiscoveryResultFromPlugin(test.plugin)
				if result.outcome != test.wantOutcome {
					t.Fatalf("outcome = %q, want %q", result.outcome, test.wantOutcome)
				}
			},
		)
	}
}

func TestAntigravityModelDiscoveryPersistsRefreshedAuth(t *testing.T) {
	previousFetch := fetchAntigravityModels
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: "refreshed-model"}},
			Auth: &coreauth.Auth{
				ID:         "ignored-id",
				Provider:   "ignored-provider",
				ProxyURL:   "http://ignored.example",
				Attributes: map[string]string{"ignored": "value"},
				Metadata:   map[string]any{"access_token": "new-token"},
			},
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	store := &blockingModelRefreshStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:         "antigravity-refreshed-auth",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		ProxyURL:   "http://original.example",
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "old-token"},
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)

	result := service.discoverModelsForRefresh(context.Background(), job)
	if result.auth == nil || result.auth.Metadata["access_token"] != "new-token" {
		t.Fatalf("mapped Antigravity auth update = %#v", result.auth)
	}
	service.publishModelRefreshResult(context.Background(), job, result)

	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Metadata["access_token"] != "new-token" {
		t.Fatalf("refreshed auth was not published: %#v", current)
	}
	if current.Provider != registered.Provider || current.ProxyURL != registered.ProxyURL ||
		!reflect.DeepEqual(current.Attributes, registered.Attributes) {
		t.Fatalf("Antigravity refresh changed immutable auth fields: %#v", current)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["access_token"] != "new-token" {
		t.Fatalf("refreshed auth was not persisted: %#v", persisted)
	}
	GlobalModelRegistry().UnregisterClient(auth.ID)
}

func TestModelRefreshKeepsAuthoritativeResultUntilAuthoritativeEmpty(t *testing.T) {
	auth := &coreauth.Auth{ID: "ag-authoritative-cache", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)

	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive,
			models:  []*ModelInfo{{ID: "cached-model"}},
		},
	)
	cached := service.modelRefreshCache[auth.ID]
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") {
		t.Fatal("authoritative model was not registered")
	}

	job = currentModelRefreshJob(t, service, auth.ID, false, 2)
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryInconclusive,
			err:     fmt.Errorf("temporary discovery failure"),
		},
	)
	retained := service.modelRefreshCache[auth.ID]
	if retained.outcome != modelDiscoveryAuthoritativePositive || !retained.observedAt.Equal(cached.observedAt) {
		t.Fatalf("inconclusive refresh replaced authoritative cache: %#v", retained)
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") {
		t.Fatal("inconclusive refresh removed the authoritative model")
	}

	job = currentModelRefreshJob(t, service, auth.ID, false, 3)
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativeNegative,
		},
	)
	negative := service.modelRefreshCache[auth.ID]
	if negative.outcome != modelDiscoveryAuthoritativeNegative || len(negative.models) != 0 {
		t.Fatalf("authoritative empty cache = %#v", negative)
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") {
		t.Fatal("authoritative empty result left the old model registered")
	}
}

func TestModelRefreshCacheIsolatesCredentialPositiveAndNegativeResults(t *testing.T) {
	first := &coreauth.Auth{ID: "ag-isolation-first", Provider: "antigravity", Status: coreauth.StatusActive}
	second := &coreauth.Auth{ID: "ag-isolation-second", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, first, second)
	firstJob := currentModelRefreshJob(t, service, first.ID, false, 1)
	secondJob := currentModelRefreshJob(t, service, second.ID, false, 1)

	service.publishModelRefreshResult(
		context.Background(),
		firstJob,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive,
			models:  []*ModelInfo{{ID: "first-model"}},
		},
	)
	service.publishModelRefreshResult(
		context.Background(),
		secondJob,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativeNegative,
		},
	)
	if !GlobalModelRegistry().ClientSupportsModel(first.ID, "first-model") ||
		len(registry.GetGlobalRegistry().GetModelsForClient(second.ID)) != 0 {
		t.Fatal("first positive result crossed into the second credential")
	}
	if service.modelRefreshCache[first.ID].outcome != modelDiscoveryAuthoritativePositive ||
		service.modelRefreshCache[second.ID].outcome != modelDiscoveryAuthoritativeNegative {
		t.Fatalf("credential caches = %#v", service.modelRefreshCache)
	}

	firstJob = currentModelRefreshJob(t, service, first.ID, false, 2)
	secondJob = currentModelRefreshJob(t, service, second.ID, false, 2)
	service.publishModelRefreshResult(
		context.Background(),
		secondJob,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive,
			models:  []*ModelInfo{{ID: "second-model"}},
		},
	)
	service.publishModelRefreshResult(
		context.Background(),
		firstJob,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativeNegative,
		},
	)
	if !GlobalModelRegistry().ClientSupportsModel(second.ID, "second-model") ||
		len(registry.GetGlobalRegistry().GetModelsForClient(first.ID)) != 0 {
		t.Fatal("second positive result crossed into the first credential")
	}
	if service.modelRefreshCache[first.ID].outcome != modelDiscoveryAuthoritativeNegative ||
		service.modelRefreshCache[second.ID].outcome != modelDiscoveryAuthoritativePositive {
		t.Fatalf("reversed credential caches = %#v", service.modelRefreshCache)
	}
}

func TestPartialModelRefreshDoesNotReplaceAuthoritativeCache(t *testing.T) {
	auth := &coreauth.Auth{ID: "ag-partial-cache", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive,
			models:  []*ModelInfo{{ID: "complete-model"}},
		},
	)
	job = currentModelRefreshJob(t, service, auth.ID, false, 2)
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResult{
			source:  modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryPartial,
			models:  []*ModelInfo{{ID: "partial-model"}},
		},
	)

	cached := service.modelRefreshCache[auth.ID]
	if cached.outcome != modelDiscoveryAuthoritativePositive || len(cached.models) != 1 || cached.models[0].ID != "complete-model" {
		t.Fatalf("partial refresh replaced authoritative cache: %#v", cached)
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "complete-model") ||
		GlobalModelRegistry().ClientSupportsModel(auth.ID, "partial-model") {
		t.Fatal("partial refresh replaced the authoritative registry projection")
	}
}

func TestModelRefreshCacheDoesNotCrossAuthGeneration(t *testing.T) {
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
		ID: "ag-generation-isolation", Provider: "antigravity", Status: coreauth.StatusActive,
		Metadata: map[string]any{"credential": "first"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	service.modelRefreshCache = map[string]modelRefreshCacheEntry{
		auth.ID: {
			identity: job.identity, source: modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive, models: []*ModelInfo{{ID: "first-model"}},
			observedAt: time.Now().UTC(),
		},
	}
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID], job.registryEpoch)

	updated := job.auth.Clone()
	updated.Metadata["credential"] = "second"
	stored, errUpdate := service.coreManager.Update(context.Background(), updated)
	if errUpdate != nil {
		t.Fatalf("update auth generation: %v", errUpdate)
	}
	service.registerModelsForAuth(stored)
	if _, ok := service.modelRefreshCache[auth.ID]; ok {
		t.Fatal("authoritative cache survived a credential generation change")
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "first-model") {
		t.Fatal("model from the previous credential generation remained registered")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("new credential generation discovery did not start")
	}
}

func TestModelRefreshReusesCacheAcrossUnrelatedConfigUpdate(t *testing.T) {
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

	auth := &coreauth.Auth{ID: "ag-config-cache", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	service.modelRefreshCache = map[string]modelRefreshCacheEntry{
		auth.ID: {
			identity: job.identity, source: modelDiscoverySourceAntigravity,
			outcome: modelDiscoveryAuthoritativePositive, models: []*ModelInfo{{ID: "cached-model"}},
			observedAt: time.Now().UTC(),
		},
	}
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID], job.registryEpoch)

	nextCfg := &config.Config{Debug: true}
	service.publishConfig(nextCfg)
	service.registerModelsForAuth(job.auth)
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-model") {
		t.Fatal("unrelated config update discarded the authoritative model cache")
	}
	cached, ok := service.modelRefreshCache[auth.ID]
	if !ok || cached.identity.mappingRevision != job.identity.mappingRevision {
		t.Fatalf("cache was not retained across unrelated config update: %#v", cached)
	}
	select {
	case <-started:
		t.Fatal("unrelated config update fetched a fresh catalog again")
	default:
	}
	service.modelRefreshMu.Lock()
	cached.observedAt = time.Now().Add(-modelRefreshCacheTTL)
	service.modelRefreshCache[auth.ID] = cached
	service.modelRefreshMu.Unlock()
	service.registerModelsForAuth(job.auth)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expired catalog refresh did not start")
	}
}

func TestModelRefreshSingleflightUsesExactIdentity(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: "singleflight-model"}},
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{ID: "ag-singleflight", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			service.executeModelRefreshJob(context.Background(), job)
			done <- struct{}{}
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("singleflight discovery did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("exact identity discovery calls = %d, want 1", got)
	}
	close(release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("singleflight waiter did not finish")
		}
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "singleflight-model") {
		t.Fatal("singleflight result was not published")
	}
}

func TestCanceledModelRefreshRemainsTrackedUntilDiscoveryReturns(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{})
	release := make(chan struct{})
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		close(started)
		<-release
		return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryInconclusive}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{ID: "ag-canceled-flight", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.executeModelRefreshJob(ctx, job)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model discovery did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled discovery waiter did not return")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelStop()
	if errStop := service.stopModelRefreshWorkers(stopCtx); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("stopModelRefreshWorkers() error = %v, want tracked discovery timeout", errStop)
	}
	close(release)
	if errStop := service.stopModelRefreshWorkers(context.Background()); errStop != nil {
		t.Fatalf("stopModelRefreshWorkers() after release error = %v", errStop)
	}
}

func TestCanceledModelRefreshesKeepExternalConcurrencyBounded(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int32
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryInconclusive}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auths := make([]*coreauth.Auth, 0, 8)
	for index := range 8 {
		auths = append(
			auths,
			&coreauth.Auth{
				ID: fmt.Sprintf("ag-canceled-bounded-%d", index), Provider: "antigravity",
				Status: coreauth.StatusActive,
			},
		)
	}
	service := newModelRefreshTestService(t, &config.Config{}, auths...)
	cancels := make([]context.CancelFunc, 0, len(auths))
	done := make(chan struct{}, len(auths))
	for index, auth := range auths {
		job := currentModelRefreshJob(t, service, auth.ID, false, uint64(index+1))
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		go func() {
			service.executeModelRefreshJob(ctx, job)
			done <- struct{}{}
		}()
	}
	for range modelRefreshWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("model discovery did not reach the concurrency bound")
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	for range auths {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("canceled model refresh waiter did not return")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != modelRefreshWorkerCount {
		t.Fatalf("external calls after waiter cancellation = %d, want %d", got, modelRefreshWorkerCount)
	}
	close(release)
	if errStop := service.stopModelRefreshWorkers(context.Background()); errStop != nil {
		t.Fatalf("stopModelRefreshWorkers() error = %v", errStop)
	}
}

func TestScheduleExternalModelRefreshRejectsRemovedAuth(t *testing.T) {
	previousFetch := fetchAntigravityModels
	var calls atomic.Int32
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		calls.Add(1)
		return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{ID: "ag-removed-refresh", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	if errRemove := service.coreManager.Remove(
		coreauth.WithSkipPersist(context.Background()), auth.ID,
	); errRemove != nil {
		t.Fatalf("remove auth: %v", errRemove)
	}
	GlobalModelRegistry().RegisterClient(auth.ID, "antigravity", []*ModelInfo{{ID: "stale-model"}})
	service.scheduleExternalModelRefresh(auth, false)
	if calls.Load() != 0 {
		t.Fatalf("removed auth discovery calls = %d, want 0", calls.Load())
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("removed auth retained models: %#v", models)
	}
}

func TestAuthRemovalClearsModelRefreshStateAndRejectsLateResult(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fetchAntigravityModels = func(
		context.Context,
		*coreauth.Auth,
		*config.Config,
	) runtimeexecutor.ModelDiscoveryResult {
		close(started)
		<-release
		close(finished)
		return runtimeexecutor.ModelDiscoveryResult{
			Outcome: runtimeexecutor.ModelDiscoveryComplete,
			Models:  []*registry.ModelInfo{{ID: "late-model"}},
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auth := &coreauth.Auth{ID: "ag-remove-active", Provider: "antigravity", Status: coreauth.StatusActive}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	service.registerModelsForAuth(auth)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model discovery did not start")
	}
	service.modelRefreshMu.Lock()
	active := service.modelRefreshActive[auth.ID]
	service.modelRefreshCache[auth.ID] = modelRefreshCacheEntry{
		identity: active.identity,
		source:   modelDiscoverySourceAntigravity,
		outcome:  modelDiscoveryAuthoritativePositive,
		models:   []*ModelInfo{{ID: "cached-before-removal"}},
	}
	service.modelRefreshMu.Unlock()
	GlobalModelRegistry().RegisterClient(
		auth.ID,
		"antigravity",
		[]*ModelInfo{{ID: "cached-before-removal"}},
	)

	service.applyCoreAuthRemoval(context.Background(), auth.ID)
	service.modelRefreshMu.Lock()
	_, hasCancel := service.modelRefreshCancel[auth.ID]
	_, hasPending := service.modelRefreshPending[auth.ID]
	_, hasActive := service.modelRefreshActive[auth.ID]
	_, hasCache := service.modelRefreshCache[auth.ID]
	_, hasVersion := service.modelRefreshVersion[auth.ID]
	service.modelRefreshMu.Unlock()
	if hasCancel || hasPending || hasActive || hasCache || hasVersion {
		t.Fatalf(
			"removed auth retained refresh state: cancel=%t pending=%t active=%t cache=%t version=%t",
			hasCancel,
			hasPending,
			hasActive,
			hasCache,
			hasVersion,
		)
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("removed auth retained models: %#v", models)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceled model discovery did not return")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if errStop := service.stopModelRefreshWorkers(stopCtx); errStop != nil {
		t.Fatalf("stopModelRefreshWorkers() error = %v", errStop)
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "late-model") {
		t.Fatal("late model discovery result revived the removed auth")
	}
}

func TestPluginAuthUpdateMigratesCacheOnlyForSameSubject(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "plugin-cache-migration", Provider: "plugin-provider", Status: coreauth.StatusActive,
		Metadata: map[string]any{"token": "old"},
	}
	service := newModelRefreshTestService(t, &config.Config{}, auth)
	job := currentModelRefreshJob(t, service, auth.ID, false, 1)
	service.modelRefreshCache = map[string]modelRefreshCacheEntry{
		auth.ID: {
			identity: job.identity, source: modelDiscoverySourcePlugin,
			outcome: modelDiscoveryAuthoritativePositive, provider: "plugin-provider",
			models: []*ModelInfo{{ID: "cached-plugin-model"}}, observedAt: time.Now().UTC(),
		},
	}
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID], job.registryEpoch)
	job = currentModelRefreshJob(t, service, auth.ID, false, 1)

	pluginAuth := job.auth.Clone()
	pluginAuth.Metadata["token"] = "new"
	service.publishModelRefreshResult(
		context.Background(),
		job,
		modelDiscoveryResultFromPlugin(
			pluginhost.AuthModelResult{
				Provider: "plugin-provider", Outcome: "partial", Auth: pluginAuth, Handled: true,
				Models: []*registry.ModelInfo{{ID: "partial-plugin-model"}},
			},
		),
	)
	updated, ok := service.coreManager.GetByID(auth.ID)
	if !ok || updated.Metadata["token"] != "new" {
		t.Fatalf("plugin auth update was not published: %#v", updated)
	}
	cached, ok := service.modelRefreshCache[auth.ID]
	if !ok || cached.identity.authGeneration != updated.Generation || cached.models[0].ID != "cached-plugin-model" {
		t.Fatalf("same-subject plugin update did not migrate authoritative cache: %#v", cached)
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-plugin-model") ||
		GlobalModelRegistry().ClientSupportsModel(auth.ID, "partial-plugin-model") {
		t.Fatal("partial plugin result replaced the migrated authoritative cache")
	}

	endpointJob := currentModelRefreshJob(t, service, auth.ID, false, 2)
	endpointAuth := updated.Clone()
	endpointAuth.Provider = "other-provider"
	endpointAuth.FileName = "other.json"
	endpointAuth.Label = "other-label"
	endpointAuth.Prefix = "other-prefix"
	endpointAuth.ProxyURL = "http://other-proxy.example"
	endpointAuth.Disabled = true
	endpointAuth.Attributes = map[string]string{"endpoint": "other"}
	endpointAuth.Metadata["token"] = "newer"
	service.publishModelRefreshResult(
		context.Background(),
		endpointJob,
		modelDiscoveryResultFromPlugin(
			pluginhost.AuthModelResult{
				Provider: "plugin-provider", Outcome: "inconclusive", Auth: endpointAuth, Handled: true,
			},
		),
	)
	latest, ok := service.coreManager.GetByID(auth.ID)
	if !ok || latest.Metadata["token"] != "newer" {
		t.Fatalf("second plugin auth update was not published: %#v", latest)
	}
	if latest.Provider != updated.Provider || latest.FileName != updated.FileName || latest.Label != updated.Label ||
		latest.Prefix != updated.Prefix || latest.ProxyURL != updated.ProxyURL || latest.Disabled != updated.Disabled ||
		!reflect.DeepEqual(latest.Attributes, updated.Attributes) {
		t.Fatalf("plugin model update changed immutable auth fields: %#v", latest)
	}
	cached, ok = service.modelRefreshCache[auth.ID]
	if !ok || cached.identity.authGeneration != latest.Generation || cached.models[0].ID != "cached-plugin-model" {
		t.Fatalf("immutable plugin fields changed discovery subject: %#v", cached)
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-plugin-model") {
		t.Fatal("immutable plugin fields removed the same-subject cached model")
	}
}

func TestModelRefreshCoalescingRetainsPublicationOwnership(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			auth := &coreauth.Auth{ID: "synthetic-coalesced-epoch", Provider: "antigravity", Status: coreauth.StatusActive}
			service := newModelRefreshTestService(t, &config.Config{}, auth)
			service.modelRefreshStop = func() {}
			job := currentModelRefreshJob(t, service, auth.ID, false, 1)
			if active {
				service.modelRefreshActive = map[string]modelRefreshJob{auth.ID: job}
			} else {
				service.modelRefreshPending = map[string]modelRefreshJob{auth.ID: job}
			}
			modelRegistry := GlobalModelRegistry()
			modelRegistry.RegisterClient(auth.ID, auth.Provider, []*ModelInfo{{ID: "external-publisher-model"}})
			epoch := modelRegistry.ClientRegistrationEpoch(auth.ID)
			service.scheduleExternalModelRefresh(job.auth, false)
			if modelRegistry.ClientRegistrationEpoch(auth.ID) != epoch ||
				!modelRegistry.ClientSupportsModel(auth.ID, "external-publisher-model") {
				t.Fatal("coalesced admission replaced a newer model publication")
			}
		})
	}
}

func TestModelRefreshDoesNotAdoptLaterRegistryEpoch(t *testing.T) {
	for _, provider := range []string{"antigravity", "openai-compatibility"} {
		for _, negative := range []bool{false, true} {
			for _, removed := range []bool{false, true} {
				name := provider
				if negative {
					name += "/negative"
				} else {
					name += "/positive"
				}
				if removed {
					name += "/removed"
				} else {
					name += "/replaced"
				}
				t.Run(name, func(t *testing.T) {
					auth := &coreauth.Auth{ID: "synthetic-epoch-" + provider, Provider: provider, Status: coreauth.StatusActive}
					service := newModelRefreshTestService(t, &config.Config{}, auth)
					modelRegistry := GlobalModelRegistry()
					modelRegistry.RegisterClient(auth.ID, provider, []*ModelInfo{{ID: "original-model"}})
					job := currentModelRefreshJob(t, service, auth.ID, false, 1)
					modelRegistry.RegisterClient(auth.ID, provider, []*ModelInfo{{ID: "external-publisher-model"}})
					if removed {
						modelRegistry.UnregisterClient(auth.ID)
					}
					externalEpoch := modelRegistry.ClientRegistrationEpoch(auth.ID)
					result := modelDiscoveryResult{
						source: modelDiscoverySourceAntigravity, provider: provider,
						outcome: modelDiscoveryAuthoritativePositive, models: []*ModelInfo{{ID: "late-discovered-model"}},
					}
					if provider == "openai-compatibility" {
						result.source = modelDiscoverySourcePlugin
					}
					if negative {
						result.outcome, result.models = modelDiscoveryAuthoritativeNegative, nil
					}
					service.publishModelRefreshResult(t.Context(), job, result)
					if modelRegistry.ClientRegistrationEpoch(auth.ID) != externalEpoch ||
						modelRegistry.ClientSupportsModel(auth.ID, "late-discovered-model") ||
						modelRegistry.ClientSupportsModel(auth.ID, "external-publisher-model") == removed {
						t.Fatal("late discovery replaced a newer registry publication")
					}
					if models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID); removed && len(models) != 0 {
						t.Fatalf("late discovery restored removed registry models: %#v", models)
					}
					service.modelRefreshMu.Lock()
					_, cached := service.modelRefreshCache[auth.ID]
					service.modelRefreshMu.Unlock()
					if cached {
						t.Fatal("rejected discovery remained in the authoritative cache")
					}
				})
			}
		}
	}
}

func TestModelRefreshQueuedJobRetainsRegistrationEpoch(t *testing.T) {
	previousFetch := fetchAntigravityModels
	started := make(chan struct{}, modelRefreshWorkerCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fetchAntigravityModels = func(ctx context.Context, auth *coreauth.Auth, _ *config.Config) runtimeexecutor.ModelDiscoveryResult {
		if auth.ID == "synthetic-queued-epoch" {
			return runtimeexecutor.ModelDiscoveryResult{
				Outcome: runtimeexecutor.ModelDiscoveryComplete, Models: []*registry.ModelInfo{{ID: "late-queued-model"}},
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryInconclusive}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })
	auths := make([]*coreauth.Auth, 0, modelRefreshWorkerCount+1)
	for index := range modelRefreshWorkerCount {
		auths = append(auths, &coreauth.Auth{
			ID: "synthetic-epoch-blocker-" + string(rune('a'+index)), Provider: "antigravity", Status: coreauth.StatusActive,
		})
	}
	target := &coreauth.Auth{ID: "synthetic-queued-epoch", Provider: "antigravity", Status: coreauth.StatusActive}
	auths = append(auths, target)
	service := newModelRefreshTestService(t, &config.Config{}, auths...)
	for _, auth := range auths[:modelRefreshWorkerCount] {
		service.registerModelsForAuth(auth)
	}
	for range modelRefreshWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("blocking discovery did not start")
		}
	}
	service.registerModelsForAuth(target)
	modelRegistry := GlobalModelRegistry()
	modelRegistry.RegisterClient(target.ID, target.Provider, []*ModelInfo{{ID: "external-publisher-model"}})
	externalEpoch := modelRegistry.ClientRegistrationEpoch(target.ID)
	releaseOnce.Do(func() { close(release) })
	waitForModelRefreshCondition(t, func() bool {
		service.modelRefreshMu.Lock()
		defer service.modelRefreshMu.Unlock()
		return len(service.modelRefreshPending) == 0 && len(service.modelRefreshActive) == 0
	})
	if modelRegistry.ClientRegistrationEpoch(target.ID) != externalEpoch ||
		!modelRegistry.ClientSupportsModel(target.ID, "external-publisher-model") ||
		modelRegistry.ClientSupportsModel(target.ID, "late-queued-model") {
		t.Fatal("queued discovery adopted a later publisher's registration epoch")
	}
}

func TestHomeAuthRuntimeKeepsStandaloneDiscoveryDisabled(t *testing.T) {
	for _, homeEnabled := range []bool{false, true} {
		for _, entry := range []string{"post-persist-hook", "config-reprojection", "model-registration"} {
			name := "standalone/" + entry
			if homeEnabled {
				name = "home/" + entry
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var probes atomic.Int32
					previousFetch := fetchAntigravityModels
					fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
						probes.Add(1)
						return runtimeexecutor.ModelDiscoveryResult{
							Outcome: runtimeexecutor.ModelDiscoveryComplete,
							Models:  []*registry.ModelInfo{{ID: "standalone-probe-model"}},
						}
					}
					t.Cleanup(func() { fetchAntigravityModels = previousFetch })
					cfg := &config.Config{}
					cfg.Home.Enabled = homeEnabled
					auth := &coreauth.Auth{
						ID: "home-discovery-boundary", Provider: "antigravity", Status: coreauth.StatusActive,
						Attributes: map[string]string{coreauth.AttributePath: "/synthetic/antigravity.json"},
						Metadata:   map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project"},
					}
					service := newModelRefreshTestService(t, cfg, auth)
					current, ok := service.coreManager.GetByID(auth.ID)
					if !ok {
						t.Fatal("registered auth is missing")
					}
					switch entry {
					case "post-persist-hook":
						if err := service.runtimeAuthSyncHook()(t.Context(), current); err != nil {
							t.Fatal(err)
						}
					case "config-reprojection":
						service.reprojectModelsForCurrentAuths()
					case "model-registration":
						service.registerModelsForAuth(current)
					}
					synctest.Wait()
					want := int32(1)
					if homeEnabled {
						want = 0
					}
					if got := probes.Load(); got != want {
						t.Fatalf("standalone discovery calls = %d, want %d for Home=%v", got, want, homeEnabled)
					}
					if homeEnabled && GlobalModelRegistry().ClientSupportsModel(auth.ID, "standalone-probe-model") {
						t.Fatal("Home auth published a standalone discovery model")
					}
				})
			})
		}
	}
}

func TestHomeModelDiscoveryDoesNotFallbackToStandalone(t *testing.T) {
	for _, homeEnabled := range []bool{false, true} {
		name := "standalone"
		if homeEnabled {
			name = "home"
		}
		t.Run(name, func(t *testing.T) {
			var probes atomic.Int32
			previousFetch := fetchAntigravityModels
			fetchAntigravityModels = func(context.Context, *coreauth.Auth, *config.Config) runtimeexecutor.ModelDiscoveryResult {
				probes.Add(1)
				return runtimeexecutor.ModelDiscoveryResult{Outcome: runtimeexecutor.ModelDiscoveryComplete}
			}
			t.Cleanup(func() { fetchAntigravityModels = previousFetch })
			cfg := &config.Config{}
			cfg.Home.Enabled = homeEnabled
			service := &Service{cfg: cfg}
			result := service.discoverModelsForRefresh(t.Context(), modelRefreshJob{
				auth: &coreauth.Auth{ID: "home-direct-discovery", Provider: "antigravity"},
				cfg:  cfg,
			})
			want := int32(1)
			if homeEnabled {
				want = 0
				if result.outcome != modelDiscoveryInconclusive {
					t.Errorf("Home discovery outcome = %s, want inconclusive", result.outcome)
				}
			}
			if got := probes.Load(); got != want {
				t.Fatalf("standalone fallback calls = %d, want %d for Home=%v", got, want, homeEnabled)
			}
		})
	}
}
