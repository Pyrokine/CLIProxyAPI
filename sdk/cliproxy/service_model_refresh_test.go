package cliproxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
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
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(auth.ID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

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
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(blocked.ID)
		GlobalModelRegistry().UnregisterClient(other.ID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

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
	for index := 0; index < 100; index++ {
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
	) []*registry.ModelInfo {
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
			return []*registry.ModelInfo{{ID: "model-" + auth.ID}}
		case <-ctx.Done():
			return nil
		}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auths := make([]*coreauth.Auth, 0, 8)
	for index := 0; index < 8; index++ {
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

	for index := 0; index < modelRefreshWorkerCount; index++ {
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
	) []*registry.ModelInfo {
		if auth.ID != "ag-coalesced" {
			blockersStarted <- struct{}{}
			select {
			case <-releaseBlockers:
				return []*registry.ModelInfo{{ID: "blocker-model"}}
			case <-ctx.Done():
				return nil
			}
		}
		targetCalls.Add(1)
		return []*registry.ModelInfo{{ID: auth.Metadata["generation"].(string)}}
	}
	t.Cleanup(func() { fetchAntigravityModels = previousFetch })

	auths := make([]*coreauth.Auth, 0, modelRefreshWorkerCount+1)
	for index := 0; index < modelRefreshWorkerCount; index++ {
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
	for index := 0; index < modelRefreshWorkerCount; index++ {
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
	) []*registry.ModelInfo {
		if calls.Add(1) == 1 {
			return []*registry.ModelInfo{{ID: "cached-model"}}
		}
		close(secondStarted)
		<-ctx.Done()
		return nil
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
	)
	if errApply != nil || !applied {
		t.Fatalf("applyConfigUpdateRuntimeGenerationOptions() = (%t, %v)", applied, errApply)
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, excludedModel) {
		t.Fatalf("excluded model %q remained routable after config update", excludedModel)
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
	) []*registry.ModelInfo {
		close(started)
		select {
		case <-release:
			return []*registry.ModelInfo{{ID: "stale-auth-model"}}
		case <-ctx.Done():
			return nil
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
	service.publishModelRefreshResult(
		context.Background(),
		modelRefreshJob{auth: auth.Clone(), cfg: service.cfg, version: 1, tryPlugin: true},
		pluginhost.AuthModelResult{
			Provider: "plugin-provider",
			Models:   []*registry.ModelInfo{{ID: "stale-plugin-model"}},
			Snapshot: &pluginhost.Snapshot{},
			Handled:  true,
		},
		nil,
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
		modelRefreshCache:   make(map[string][]*ModelInfo),
	}
	pluginAuth := registered.Clone()
	pluginAuth.Metadata["token"] = "new"
	job := modelRefreshJob{auth: registered.Clone(), cfg: service.cfg, version: 1}
	done := make(chan struct{})
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	go func() {
		service.applyPluginModelsForAuth(
			ctx,
			job,
			registered.Clone(),
			service.cfg,
			pluginhost.AuthModelResult{
				Provider: "plugin-provider",
				Models:   []*registry.ModelInfo{{ID: "plugin-model"}},
				Auth:     pluginAuth,
				Handled:  true,
			},
		)
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
		pluginhost.AuthModelResult{
			Provider: "plugin-provider",
			Models:   []*registry.ModelInfo{{ID: "plugin-model"}},
			Auth:     pluginAuth,
			Handled:  true,
		},
		nil,
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
