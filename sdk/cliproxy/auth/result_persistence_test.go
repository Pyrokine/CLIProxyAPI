package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type failingResultAuthStore struct {
	saves int
}

func (*failingResultAuthStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *failingResultAuthStore) Save(context.Context, *Auth) (string, error) {
	s.saves++
	return "", errors.New("auth persistence failed")
}

func (*failingResultAuthStore) Delete(context.Context, string) error {
	return nil
}

type recordingResultCooldownStore struct {
	mu      sync.Mutex
	saves   int
	records []CooldownStateRecord
}

func (*recordingResultCooldownStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, nil
}

func (s *recordingResultCooldownStore) Save(_ context.Context, records []CooldownStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.records = append([]CooldownStateRecord(nil), records...)
	return nil
}

func (s *recordingResultCooldownStore) snapshot() (int, []CooldownStateRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, append([]CooldownStateRecord(nil), s.records...)
}

func TestManagerMarkResultKeepsRuntimeSchedulerRegistryAndCooldownStoreConsistentWhenAuthPersistenceFails(t *testing.T) {
	const (
		authID   = "result-persistence-failure"
		provider = "result-persistence-provider"
		model    = "result-persistence-model"
	)
	authStore := &failingResultAuthStore{}
	cooldownStore := &recordingResultCooldownStore{}
	manager := NewManager(authStore, nil, nil)
	manager.SetCooldownStateStore(cooldownStore)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
			Metadata: map[string]any{"token": "test"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	if available, errAvailable := reg.GetFirstAvailableModel(provider); errAvailable != nil || available != model {
		t.Fatalf("registry model before failure = %q, %v", available, errAvailable)
	}

	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), executionResult(
			registered,
			provider,
			model,
			executionOutcomeStatusError{status: http.StatusTooManyRequests, retryAfter: &retryAfter},
			cliproxyexecutor.DispatchPhaseResponseStarted,
		),
	)

	failed := getExecutionOutcomeTestAuth(t, manager, authID)
	state := failed.ModelStates[model]
	if state == nil || !state.Unavailable || !state.Quota.Exceeded {
		t.Fatalf("runtime model state after failure = %+v", state)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler selected failed model: picked=%#v error=%v", picked, errPick)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(provider); errAvailable == nil && available == model {
		t.Fatalf("registry still exposed failed model %q", available)
	}
	saves, records := cooldownStore.snapshot()
	if saves != 1 {
		t.Fatalf("cooldown persistence attempts after failure = %d, want 1", saves)
	}
	foundModelRecord := false
	for _, record := range records {
		if record.Model == model && record.Quota.Exceeded {
			foundModelRecord = true
			break
		}
	}
	if !foundModelRecord {
		t.Fatalf("cooldown persistence missing failed model record: %+v", records)
	}

	manager.MarkResult(
		context.Background(), executionResult(
			failed,
			provider,
			model,
			nil,
			cliproxyexecutor.DispatchPhaseDownstreamCommitted,
		),
	)

	recovered := getExecutionOutcomeTestAuth(t, manager, authID)
	recoveredState := recovered.ModelStates[model]
	if recoveredState == nil || recoveredState.Unavailable || recoveredState.Quota.Exceeded {
		t.Fatalf("runtime model state after recovery = %+v", recoveredState)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("scheduler recovery = picked:%#v error:%v", picked, errPick)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(provider); errAvailable != nil || available != model {
		t.Fatalf("registry model after recovery = %q, %v", available, errAvailable)
	}
	if saves, records := cooldownStore.snapshot(); saves != 2 || len(records) != 0 {
		t.Fatalf("cooldown persistence after recovery = saves:%d records:%+v", saves, records)
	}
	if authStore.saves != 2 {
		t.Fatalf("auth persistence attempts = %d, want 2", authStore.saves)
	}
}
