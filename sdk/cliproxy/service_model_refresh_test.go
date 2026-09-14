package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	service.modelRefreshVersion = map[string]uint64{auth.ID: 1}
	identity, errIdentity := service.buildModelRefreshIdentity(auth, service.cfg, true)
	if errIdentity != nil {
		t.Fatalf("buildModelRefreshIdentity() error = %v", errIdentity)
	}
	service.publishModelRefreshResult(
		context.Background(),
		modelRefreshJob{
			auth: auth.Clone(), cfg: service.cfg, identity: identity, version: 1, tryPlugin: true,
		},
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
	job := modelRefreshJob{auth: registered.Clone(), cfg: service.cfg, version: 1}
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
		modelRefreshJob{auth: jobAuth.Clone(), cfg: service.cfg, version: 1},
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
		auth:      auth,
		cfg:       service.cfg,
		identity:  identity,
		version:   version,
		tryPlugin: tryPlugin,
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
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID])

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
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID])

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
	case <-time.After(time.Second):
		t.Fatal("config refresh discovery did not start")
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
	service.projectModelDiscoveryLocked(job.auth, service.cfg, service.modelRefreshCache[auth.ID])

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
