package auth

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestOriginalManagerSetConfigSnapshotDefersCooldownPersistence(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	manager.MarkResult(
		context.Background(), Result{
			AuthID:   auth.ID,
			Provider: auth.Provider,
			Model:    "grok-4",
			Success:  false,
			Error:    &Error{Message: "rate limited", HTTPStatus: 429},
		},
	)
	store.saveCount.Store(0)

	if changed := manager.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true}); !changed {
		t.Fatal("SetConfigSnapshot() = false, want cleared cooldown state")
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("SetConfigSnapshot() persisted cooldown state %d times, want 0", got)
	}
	manager.PersistCooldownStates(context.Background())
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("PersistCooldownStates() saved cooldown state %d times, want 1", got)
	}
}

type originalBlockingCooldownStateStore struct {
	saveCount atomic.Int32
	err       error
	started   chan struct{}
	release   chan struct{}
	onSave    func()
	records   []CooldownStateRecord
}

func (s *originalBlockingCooldownStateStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, nil
}

func (s *originalBlockingCooldownStateStore) Save(ctx context.Context, records []CooldownStateRecord) error {
	s.saveCount.Add(1)
	select {
	case <-s.started:
	default:
		close(s.started)
	}
	select {
	case <-s.release:
		if s.err == nil {
			s.records = cloneCooldownStateRecords(records)
			if s.onSave != nil {
				s.onSave()
			}
		}
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestOriginalManagerSwapCooldownStateStorePersistsOldStoreBeforeSwap(t *testing.T) {
	oldStore := &recordingCooldownStateStore{}
	newStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(oldStore)
	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	manager.MarkResult(
		context.Background(), Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "grok-4", Success: false,
			Error: &Error{Message: "rate limited", HTTPStatus: 429},
		},
	)
	oldStore.saveCount.Store(0)
	if changed := manager.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true}); !changed {
		t.Fatal("SetConfigSnapshot() = false, want cleared cooldown state")
	}

	if swapped := manager.SwapCooldownStateStore(context.Background(), newStore, true); !swapped {
		t.Fatal("SwapCooldownStateStore() = false, want true")
	}
	if got := oldStore.saveCount.Load(); got != 1 {
		t.Fatalf("old store save count = %d, want 1", got)
	}
	if len(oldStore.records) != 0 {
		t.Fatalf("old store records = %+v, want cleared cooldown state", oldStore.records)
	}
	manager.mu.RLock()
	currentStore := manager.cooldownStore
	manager.mu.RUnlock()
	if currentStore != newStore {
		t.Fatal("cooldown store swapped before the old store was persisted")
	}
}

func TestOriginalManagerApplyConfigWithCooldownStoreSerializesTransitions(t *testing.T) {
	oldStore := &originalBlockingCooldownStateStore{started: make(chan struct{}), release: make(chan struct{})}
	firstStore := &recordingCooldownStateStore{}
	secondStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	manager.MarkResult(
		context.Background(), Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "grok-4", Success: false,
			Error: &Error{Message: "rate limited", HTTPStatus: 429},
		},
	)
	manager.SetCooldownStateStore(oldStore)

	firstDone := make(chan bool, 1)
	go func() {
		firstDone <- manager.ApplyConfigWithCooldownStateStore(
			context.Background(), &internalconfig.Config{DisableCooling: true}, firstStore,
		)
	}()
	select {
	case <-oldStore.started:
	case <-time.After(time.Second):
		t.Fatal("first old-store persistence did not start")
	}

	secondDone := make(chan bool, 1)
	go func() {
		secondDone <- manager.ApplyConfigWithCooldownStateStore(
			context.Background(), &internalconfig.Config{}, secondStore,
		)
	}()
	select {
	case <-secondDone:
		t.Fatal("concurrent config transition completed while old-store persistence was blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(oldStore.release)
	if applied := waitForOriginalCooldownTransition(t, firstDone, "first config transition"); !applied {
		t.Fatal("first config transition returned false")
	}
	if applied := waitForOriginalCooldownTransition(t, secondDone, "second config transition"); !applied {
		t.Fatal("second config transition returned false")
	}
	manager.mu.RLock()
	currentStore := manager.cooldownStore
	manager.mu.RUnlock()
	if currentStore != secondStore {
		t.Fatal("concurrent config transitions did not leave the final resolved store installed")
	}
}

func waitForOriginalCooldownTransition(t *testing.T, done <-chan bool, name string) bool {
	t.Helper()
	select {
	case applied := <-done:
		return applied
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return false
	}
}

func TestOriginalManagerSwapCooldownStateStoreKeepsOldStoreWhenCanceled(t *testing.T) {
	oldStore := &originalBlockingCooldownStateStore{started: make(chan struct{}), release: make(chan struct{})}
	newStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(oldStore)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- manager.SwapCooldownStateStore(ctx, newStore, true) }()
	select {
	case <-oldStore.started:
	case <-time.After(time.Second):
		t.Fatal("old cooldown store persistence did not start")
	}
	manager.mu.RLock()
	currentStore := manager.cooldownStore
	manager.mu.RUnlock()
	if currentStore != oldStore {
		t.Fatal("cooldown store swapped while old store persistence was blocked")
	}
	cancel()
	select {
	case swapped := <-done:
		if swapped {
			t.Fatal("SwapCooldownStateStore() = true after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("SwapCooldownStateStore() did not honor cancellation")
	}

	close(oldStore.release)
	if swapped := manager.SwapCooldownStateStore(context.Background(), newStore, false); !swapped {
		t.Fatal("SwapCooldownStateStore() = false, want retry to persist the old store before swapping")
	}
	if oldStore.saveCount.Load() != 2 {
		t.Fatal("retry skipped the old store's canceled persistence")
	}
	manager.mu.RLock()
	currentStore = manager.cooldownStore
	manager.mu.RUnlock()
	if currentStore != newStore {
		t.Fatal("cooldown store was not swapped after pending persistence completed")
	}
}

func TestOriginalManagerResultSaveWaitsForCooldownStoreTransition(t *testing.T) {
	oldStore := &originalBlockingCooldownStateStore{started: make(chan struct{}), release: make(chan struct{})}
	newStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	manager.SetCooldownStateStore(oldStore)

	transitionDone := make(chan bool, 1)
	go func() {
		transitionDone <- manager.SwapCooldownStateStore(context.Background(), newStore, true)
	}()
	select {
	case <-oldStore.started:
	case <-time.After(time.Second):
		t.Fatal("old-store transition save did not start")
	}

	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(
			context.Background(), Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "grok-4", Success: false,
				Error: &Error{Message: "rate limited", HTTPStatus: 429},
			},
		)
		close(resultDone)
	}()
	select {
	case <-resultDone:
		t.Fatal("result save completed while the store transition was blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(oldStore.release)
	if swapped := waitForOriginalCooldownTransition(t, transitionDone, "cooldown store transition"); !swapped {
		t.Fatal("SwapCooldownStateStore() = false")
	}
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("result save did not complete after store transition")
	}
	if got := newStore.saveCount.Load(); got != 1 {
		t.Fatalf("new store save count = %d, want 1", got)
	}
}

func TestCooldownCompatibilityQueriesRetainPublishedInputs(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		auth                       *Auth
		unauthorized, invalidGrant bool
	}{
		{"nil", nil, false, false},
		{"active", &Auth{Status: StatusActive}, false, false},
		{
			"terminal 401", &Auth{Status: StatusError, Unavailable: true, LastError: &Error{HTTPStatus: 401}}, true,
			false,
		},
		{
			"terminal code", &Auth{Status: StatusError, Unavailable: true, LastError: &Error{Code: "UNAUTHORIZED"}},
			true, false,
		},
		{
			"pending refresh", &Auth{
				Status: StatusError, Unavailable: true, NextRefreshAfter: time.Unix(1700000000, 0),
				LastError: &Error{HTTPStatus: 401},
			}, false, false,
		},
		{
			"pending retry", &Auth{
				Status: StatusError, Unavailable: true, NextRetryAfter: time.Unix(1700000000, 0),
				LastError: &Error{HTTPStatus: 401},
			}, false, false,
		},
		{"available 401", &Auth{Status: StatusError, LastError: &Error{HTTPStatus: 401}}, false, false},
		{
			"disabled status", &Auth{Status: StatusDisabled, Unavailable: true, LastError: &Error{HTTPStatus: 401}},
			false, false,
		},
		{
			"disabled invalid grant body",
			&Auth{Disabled: true, LastError: &Error{HTTPStatus: 400, Message: "invalid_grant"}}, false, true,
		},
		{
			"disabled invalid grant code", &Auth{Status: StatusDisabled, LastError: &Error{Code: "invalid_grant"}},
			false, true,
		},
		{"active invalid grant", &Auth{Status: StatusError, LastError: &Error{Message: "invalid_grant"}}, false, false},
		{
			"disabled other error",
			&Auth{Disabled: true, LastError: &Error{Message: "service unavailable", HTTPStatus: 503}}, false, false,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				before := tc.auth.Clone()
				if HasUnauthorizedAuthFailure(tc.auth) != tc.unauthorized ||
					HasDisabledInvalidGrantFailure(tc.auth) != tc.invalidGrant {
					t.Fatalf(
						"unauthorized/invalid grant = %t/%t, want %t/%t", HasUnauthorizedAuthFailure(tc.auth),
						HasDisabledInvalidGrantFailure(tc.auth), tc.unauthorized, tc.invalidGrant,
					)
				}
				if !reflect.DeepEqual(tc.auth, before) {
					t.Fatal("query changed its credential input")
				}
			},
		)
	}
	previous := quotaCooldownDisabled.Load()
	t.Cleanup(func() { SetQuotaCooldownDisabled(previous) })
	for _, global := range []bool{false, true} {
		SetQuotaCooldownDisabled(global)
		if QuotaCooldownDisabledForAuth(nil) != global {
			t.Fatal("nil credential lost the global cooling setting")
		}
		for _, explicit := range []bool{false, true} {
			auth := &Auth{Metadata: map[string]any{"disable_cooling": explicit}}
			if QuotaCooldownDisabledForAuth(auth) != explicit {
				t.Fatal("credential override lost precedence over the global setting")
			}
		}
	}
}

func TestCooldownCompatibilityConfigSettersOwnTheirConfiguration(t *testing.T) {
	for _, mode := range []string{"snapshot", "set", "apply"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				publish := func(cfg *internalconfig.Config) {
					t.Helper()
					switch mode {
					case "snapshot":
						if manager.SetConfigSnapshot(cfg) {
							t.Fatal("empty manager reported a cleared credential")
						}
					case "set":
						manager.SetConfig(cfg)
					case "apply":
						if !manager.ApplyConfigWithCooldownStateStore(t.Context(), cfg, nil) {
							t.Fatal("empty manager rejected the configuration")
						}
					}
				}
				cfg := &internalconfig.Config{DisableCooling: true, AuthAutoRefreshWorkers: 3}
				publish(cfg)
				cfg.DisableCooling = false
				cfg.AuthAutoRefreshWorkers = 7
				current, _ := manager.runtimeConfig.Load().(*internalconfig.Config)
				if current == nil || !current.DisableCooling || current.AuthAutoRefreshWorkers != 3 ||
					cap(manager.refreshSemaphore) != 3 {
					t.Fatal("caller mutation changed the published configuration")
				}
				publish(nil)
				current, _ = manager.runtimeConfig.Load().(*internalconfig.Config)
				if current == nil || current.DisableCooling {
					t.Fatal("nil snapshot did not publish the default configuration")
				}
			},
		)
	}
}

func TestCooldownCompatibilitySwapRetainsFailedPendingStore(t *testing.T) {
	release := make(chan struct{})
	close(release)
	oldStore := &originalBlockingCooldownStateStore{
		started: make(chan struct{}), release: release, err: errors.New("old cooldown save failed"),
	}
	newStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(oldStore)
	if manager.SwapCooldownStateStore(t.Context(), newStore, true) {
		t.Fatal("failed old-store save published a replacement")
	}
	if manager.cooldownStore != oldStore || manager.pendingCooldownStateStore != oldStore ||
		oldStore.saveCount.Load() != 1 {
		t.Fatal("failed transition lost its current or pending old store")
	}
	oldStore.err = nil
	if !manager.SwapCooldownStateStore(t.Context(), newStore, false) || oldStore.saveCount.Load() != 2 ||
		manager.cooldownStore != newStore || manager.pendingCooldownStateStore != nil {
		t.Fatal("retry did not save and release the original pending store")
	}
	thirdStore := &recordingCooldownStateStore{}
	//lint:ignore SA1012 验证已发布的 nil context 存储替换合同
	if !manager.SwapCooldownStateStore(nil, thirdStore, false) || newStore.saveCount.Load() != 0 {
		t.Fatal("explicit no-persist swap saved a clean old store")
	}
}

func TestCooldownCompatibilityConfigCompletesAfterSuccessfulSaveCancellation(t *testing.T) {
	release := make(chan struct{})
	close(release)
	oldStore := &originalBlockingCooldownStateStore{started: make(chan struct{}), release: release}
	newStore := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(oldStore)
	auth := registerCooldownTestAuth(t, manager)
	markQuotaCooldown(manager, auth, "gpt-test")
	if len(oldStore.records) == 0 {
		t.Fatal("initial credential cooldown was not saved")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldStore.onSave = cancel
	if !manager.ApplyConfigWithCooldownStateStore(ctx, &internalconfig.Config{DisableCooling: true}, newStore) {
		t.Fatal("successful old-store save was not followed by configuration publication")
	}
	if !errors.Is(ctx.Err(), context.Canceled) || len(oldStore.records) != 0 {
		t.Fatal("fixture did not save cleared cooldowns before canceling")
	}
	cfg, _ := manager.runtimeConfig.Load().(*internalconfig.Config)
	current, _ := manager.GetByID(auth.ID)
	if cfg == nil || !cfg.DisableCooling || manager.cooldownStore != newStore || current.Unavailable ||
		current.ModelStates["gpt-test"] == nil || current.ModelStates["gpt-test"].Unavailable {
		t.Fatal("saved cooldown snapshot differs from published config and credential state")
	}
}
