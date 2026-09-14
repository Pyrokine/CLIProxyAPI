package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFileCooldownStateStoreV2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileCooldownStateStore(dir)
	updatedAt := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	record := CooldownStateRecord{
		Provider: "provider-a", AuthID: "auth-v2", Model: "model-a", Status: "blocked",
		Reason: "quota", EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
		EffectKey: "shared-family", StateGeneration: 9, ResetAuthority: StateResetAuthorityOperator,
		Quota: QuotaState{
			Exceeded: true, Reason: "quota", Family: "shared-family", BackoffLevel: 3,
			Generation: 9, ResetAuthority: StateResetAuthorityOperator,
		},
		LastError: &Error{
			Code: "quota", Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests,
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
			EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
			EffectKey:      "shared-family", StateGeneration: 9,
		},
		UpdatedAt: updatedAt,
	}

	if errSave := store.Save(context.Background(), []CooldownStateRecord{record}); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	statePath := filepath.Join(dir, "auth-v2.cds")
	data, errRead := os.ReadFile(statePath)
	if errRead != nil {
		t.Fatalf("read cooldown state: %v", errRead)
	}
	var envelope cooldownStateFile
	if errUnmarshal := json.Unmarshal(data, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal cooldown state: %v", errUnmarshal)
	}
	if envelope.Version != cooldownStateFileVersion {
		t.Fatalf("version = %d, want %d", envelope.Version, cooldownStateFileVersion)
	}
	info, errStat := os.Stat(statePath)
	if errStat != nil {
		t.Fatalf("stat cooldown state: %v", errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cooldown state mode = %o, want 600", got)
	}

	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded records = %d, want 1", len(loaded))
	}
	got := loaded[0]
	if got.Provider != record.Provider || got.AuthID != record.AuthID || got.Model != record.Model ||
		got.Status != record.Status || got.Reason != record.Reason || got.EffectScope != record.EffectScope ||
		got.EffectKey != record.EffectKey || got.StateGeneration != record.StateGeneration ||
		got.ResetAuthority != record.ResetAuthority || !got.UpdatedAt.Equal(record.UpdatedAt) {
		t.Fatalf("loaded record = %+v", got)
	}
	if !cooldownQuotaEqual(got.Quota, record.Quota) || !cooldownErrorEqual(got.LastError, record.LastError) {
		t.Fatalf("loaded quota/error = %+v / %+v", got.Quota, got.LastError)
	}
}

func TestFileCooldownStateStoreLoadsV1AndRejectsUnsupportedVersion(t *testing.T) {
	t.Run(
		"version one", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "legacy.cds")
			data := []byte(`{
  "version": 1,
  "auth_id": "legacy-auth",
  "provider": "provider-a",
  "updated_at": "2026-09-07T12:00:00Z",
  "records": [
    {
      "provider": "provider-a",
      "auth_id": "legacy-auth",
      "model": "model-a",
      "status": "blocked",
      "reason": "model unavailable",
      "updated_at": "2026-09-07T12:00:00Z"
    }
  ]
}
`)
			if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
				t.Fatalf("write version one state: %v", errWrite)
			}
			loaded, errLoad := NewFileCooldownStateStore(dir).Load(context.Background())
			if errLoad != nil {
				t.Fatalf("Load() error = %v", errLoad)
			}
			if len(loaded) != 1 || loaded[0].AuthID != "legacy-auth" || loaded[0].Model != "model-a" {
				t.Fatalf("loaded records = %+v", loaded)
			}
			if loaded[0].EffectScope != "" || loaded[0].StateGeneration != 0 || loaded[0].ResetAuthority != "" {
				t.Fatalf("version one fields were not left for restore defaults: %+v", loaded[0])
			}
		},
	)

	t.Run(
		"unsupported version", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "future.cds")
			data := []byte(`{"version":3,"records":[]}
`)
			if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
				t.Fatalf("write unsupported state: %v", errWrite)
			}
			_, errLoad := NewFileCooldownStateStore(dir).Load(context.Background())
			if errLoad == nil || !strings.Contains(errLoad.Error(), "unsupported version 3") {
				t.Fatalf("Load() error = %v", errLoad)
			}
		},
	)
}

func TestManagerRestoreCooldownStateWithoutDeadlineRemainsBlocked(t *testing.T) {
	dir := t.TempDir()
	store := NewFileCooldownStateStore(dir)
	record := CooldownStateRecord{
		Provider: "provider-a", AuthID: "blocked-auth", Model: "model-a", Status: "blocked",
		Reason: "operator block", EffectScope: cliproxyexecutor.StateEffectScopeExactModel,
		EffectKey: "model-a", StateGeneration: 7, ResetAuthority: StateResetAuthorityOperator,
		LastError: &Error{
			Message: "operator block", HTTPStatus: http.StatusNotFound,
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
			EffectKey:      "model-a", StateGeneration: 7,
		},
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	if errSave := store.Save(context.Background(), []CooldownStateRecord{record}); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}

	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, errRegister := manager.Register(
		WithSkipPersist(context.Background()),
		&Auth{ID: record.AuthID, Provider: record.Provider, Status: StatusActive},
	); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}

	restored := getExecutionOutcomeTestAuth(t, manager, record.AuthID)
	state := restored.ModelStates[record.Model]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.IsZero() ||
		state.Generation != record.StateGeneration || state.ResetAuthority != StateResetAuthorityOperator {
		t.Fatalf("restored state = %+v", state)
	}
	blocked, reason, retryAt := isAuthBlockedForModel(restored, record.Model, time.Now())
	if !blocked || reason != blockReasonOther || !retryAt.IsZero() {
		t.Fatalf("isAuthBlockedForModel() = (%t, %v, %v)", blocked, reason, retryAt)
	}

	manager.MarkResult(
		context.Background(), executionResult(
			restored, restored.Provider, record.Model, nil,
			cliproxyexecutor.DispatchPhaseDownstreamCommitted,
		),
	)
	stillBlocked := getExecutionOutcomeTestAuth(t, manager, record.AuthID).ModelStates[record.Model]
	if stillBlocked == nil || !stillBlocked.Unavailable || stillBlocked.Generation != record.StateGeneration {
		t.Fatalf("success cleared operator block: %+v", stillBlocked)
	}

	manager.mu.Lock()
	cleared := clearCooldownStateForAuth(manager.auths[record.AuthID], time.Now())
	manager.mu.Unlock()
	if !cleared {
		t.Fatal("operator action did not clear restored state")
	}
	updated := getExecutionOutcomeTestAuth(t, manager, record.AuthID).ModelStates[record.Model]
	if updated == nil || updated.Unavailable || updated.LastError != nil {
		t.Fatalf("state after operator action = %+v", updated)
	}
}
