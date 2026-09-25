package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type failingCooldownStateStore struct {
	err error
}

func (s *failingCooldownStateStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, s.err
}

func (s *failingCooldownStateStore) Save(context.Context, []CooldownStateRecord) error {
	return s.err
}

func registerCooldownTestAuth(t *testing.T, manager *Manager) *Auth {
	t.Helper()
	auth, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Status:   StatusActive,
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return auth
}

func markQuotaCooldown(manager *Manager, auth *Auth, model string) {
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  auth.Generation,
			Provider:        auth.Provider,
			Model:           model,
			ModelQuotaEpoch: modelQuotaEpoch(auth, model),
			Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
		},
	)
}

func TestManagerResetQuotaDeletesPersistedCooldown(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	auth := registerCooldownTestAuth(t, manager)
	markQuotaCooldown(manager, auth, "gpt-test")

	if len(store.records) == 0 {
		t.Fatal("persisted records after quota are empty")
	}
	if _, _, errReset := manager.ResetQuota(context.Background(), auth.ID); errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if len(store.records) != 0 {
		t.Fatalf("persisted records after reset = %#v, want none", store.records)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.ModelStates["gpt-test"] == nil || updated.ModelStates["gpt-test"].Quota.Exceeded {
		t.Fatalf("runtime quota after reset = %#v", updated)
	}
}

func TestManagerResetQuotaDeletesPersistedLegacy429Cooldown(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	next := time.Now().Add(time.Hour)
	auth, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:             "legacy-auth",
			Provider:       "codex",
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			ModelStates: map[string]*ModelState{
				"gpt-test": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.PersistCooldownStates(context.Background())
	if len(store.records) == 0 {
		t.Fatal("persisted legacy cooldown records are empty")
	}

	if _, _, errReset := manager.ResetQuota(context.Background(), auth.ID); errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if len(store.records) != 0 {
		t.Fatalf("persisted records after legacy reset = %#v, want none", store.records)
	}
}

func TestManagerRemoveDeletesPersistedCooldown(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	auth := registerCooldownTestAuth(t, manager)
	markQuotaCooldown(manager, auth, "gpt-test")

	if errRemove := manager.Remove(WithSkipPersist(context.Background()), auth.ID); errRemove != nil {
		t.Fatalf("Remove() error = %v", errRemove)
	}
	if len(store.records) != 0 {
		t.Fatalf("persisted records after remove = %#v, want none", store.records)
	}
}

func TestManagerRestoreCooldownCleansExpiredAndOrphanRecords(t *testing.T) {
	now := time.Now().UTC()
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{AuthID: "auth-1", Model: "expired", NextRetryAfter: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Hour)},
			{AuthID: "orphan", Model: "active", NextRetryAfter: now.Add(time.Hour), UpdatedAt: now},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	registerCooldownTestAuth(t, manager)

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	if len(store.records) != 0 {
		t.Fatalf("normalized records = %#v, want expired and orphan records removed", store.records)
	}
}

func TestManagerRestoreCooldownAdvancesQuotaEpoch(t *testing.T) {
	now := time.Now().UTC()
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{
				AuthID:         "auth-1",
				Model:          "gpt-test",
				NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: now.Add(time.Hour),
				},
				UpdatedAt: now,
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	_, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			ModelStates: map[string]*ModelState{
				"gpt-test": {QuotaEpoch: 7},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	auth, _ := manager.GetByID("auth-1")
	if got := auth.ModelStates["gpt-test"].QuotaEpoch; got != 8 {
		t.Fatalf("QuotaEpoch after restore = %d, want 8", got)
	}
}

func TestManagerRestoreCooldownPreservesPassiveQuotaObservations(t *testing.T) {
	now := time.Now().UTC()
	credentialObservedAt := now.Add(-2 * time.Minute)
	modelObservedAt := now.Add(-time.Minute)
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{
				AuthID:         "auth-1",
				NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "credential cooldown",
					NextRecoverAt: now.Add(time.Hour),
					ObservedAt:    now.Add(-time.Hour),
					Signals:       map[string]string{"X-Codex-Primary-Used-Percent": "10"},
				},
				UpdatedAt: now,
			},
			{
				AuthID:         "auth-1",
				Model:          "gpt-test",
				NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "model cooldown",
					NextRecoverAt: now.Add(time.Hour),
					ObservedAt:    now.Add(-time.Hour),
					Signals:       map[string]string{"X-Codex-Primary-Used-Percent": "20"},
				},
				UpdatedAt: now,
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	_, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Quota: QuotaState{
				ObservedAt: credentialObservedAt,
				Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "80"},
			},
			ModelStates: map[string]*ModelState{
				"gpt-test": {
					Quota: QuotaState{
						ObservedAt: modelObservedAt,
						Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "90"},
					},
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	auth, ok := manager.GetByID("auth-1")
	if !ok || auth == nil {
		t.Fatal("restored auth is missing")
	}
	if !auth.Quota.Exceeded || auth.Quota.Reason != "credential cooldown" {
		t.Fatalf("credential cooldown was not restored: %#v", auth.Quota)
	}
	if !auth.Quota.ObservedAt.Equal(credentialObservedAt) ||
		auth.Quota.Signals["X-Codex-Primary-Used-Percent"] != "80" {
		t.Fatalf("credential passive observation was replaced: %#v", auth.Quota)
	}
	state := auth.ModelStates["gpt-test"]
	if state == nil || !state.Quota.Exceeded || state.Quota.Reason != "model cooldown" {
		t.Fatalf("model cooldown was not restored: %#v", state)
	}
	if !state.Quota.ObservedAt.Equal(modelObservedAt) ||
		state.Quota.Signals["X-Codex-Primary-Used-Percent"] != "90" {
		t.Fatalf("model passive observation was replaced: %#v", state.Quota)
	}
}

func TestManagerConfigDisableClearsRuntimeAndPersistedCooldown(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	manager.SetCooldownStateStore(store)
	auth := registerCooldownTestAuth(t, manager)
	markQuotaCooldown(manager, auth, "gpt-test")

	if !manager.ApplyConfigWithCooldownStateStore(
		context.Background(),
		&internalconfig.Config{DisableCooling: true},
		store,
	) {
		t.Fatal("ApplyConfigWithCooldownStateStore() = false")
	}
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["gpt-test"]
	if state == nil || state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() {
		t.Fatalf("runtime cooldown after disable = %#v", state)
	}
	if len(store.records) != 0 {
		t.Fatalf("persisted cooldown after disable = %#v, want none", store.records)
	}
}

func TestManagerFailedStoreTransitionDoesNotApplyConfigOrClearCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	recordingStore := &recordingCooldownStateStore{}
	manager.SetCooldownStateStore(recordingStore)
	auth := registerCooldownTestAuth(t, manager)
	markQuotaCooldown(manager, auth, "gpt-test")

	transitionErr := errors.New("save failed")
	manager.SetCooldownStateStore(&failingCooldownStateStore{err: transitionErr})
	if manager.ApplyConfigWithCooldownStateStore(
		context.Background(),
		&internalconfig.Config{DisableCooling: true},
		nil,
	) {
		t.Fatal("ApplyConfigWithCooldownStateStore() = true, want false")
	}
	cfg, _ := manager.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || cfg.DisableCooling {
		t.Fatalf("runtime config after failed transition = %#v, want previous config", cfg)
	}
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["gpt-test"]
	if state == nil || !state.Unavailable || !state.Quota.Exceeded {
		t.Fatalf("runtime cooldown after failed transition = %#v", state)
	}
}

func TestCooldownPersistenceContextSurvivesCallerCancellation(t *testing.T) {
	ctx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	persistCtx, cancelPersist := newCooldownStatePersistenceContext(ctx)
	defer cancelPersist()
	if err := persistCtx.Err(); err != nil {
		t.Fatalf("persistence context error = %v, want active context", err)
	}
}

func TestManagerCooldownStoreOperationsAreSerialized(t *testing.T) {
	store := &serializedCooldownStateStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	registerCooldownTestAuth(t, manager)

	persistDone := make(chan struct{})
	go func() {
		manager.PersistCooldownStates(context.Background())
		close(persistDone)
	}()
	<-store.entered

	transitionDone := make(chan struct{})
	go func() {
		manager.SetCooldownStateStore(nil)
		close(transitionDone)
	}()
	select {
	case <-transitionDone:
		t.Fatal("store transition completed while persistence was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	<-persistDone
	<-transitionDone
}

type serializedCooldownStateStore struct {
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (*serializedCooldownStateStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, nil
}

func (s *serializedCooldownStateStore) Save(context.Context, []CooldownStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered <- struct{}{}
	<-s.release
	return nil
}
