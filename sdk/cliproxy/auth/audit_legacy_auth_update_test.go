package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditLegacyUpdateStore struct {
	saved  *Auth
	fail   error
	onSave func()
}

func (*auditLegacyUpdateStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (*auditLegacyUpdateStore) Delete(context.Context, string) error  { return nil }
func (s *auditLegacyUpdateStore) Save(_ context.Context, auth *Auth) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	s.saved = auth.Clone()
	if s.onSave != nil {
		s.onSave()
	}
	return auth.ID, nil
}

func TestAuditLegacyUpdateDisabledRefreshTimes(t *testing.T) {
	for _, before := range []string{"active", "disabled_flag", "disabled_status"} {
		for _, after := range []string{"active", "disabled_flag", "disabled_status"} {
			for _, zero := range []bool{false, true} {
				name := before + "/" + after + map[bool]string{false: "/explicit", true: "/zero"}[zero]
				t.Run(
					name, func(t *testing.T) {
						oldTime := time.Now().Add(-time.Hour).UTC()
						manager := NewManager(nil, nil, nil)
						auth := &Auth{
							ID: "legacy-times", Provider: "test", Status: StatusActive, LastRefreshedAt: oldTime,
							NextRefreshAfter: oldTime.Add(2 * time.Hour),
						}
						auth.Disabled = before == "disabled_flag"
						if before == "disabled_status" {
							auth.Status = StatusDisabled
						}
						if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
							t.Fatal(err)
						}
						incoming, _ := manager.GetByID(auth.ID)
						incoming.Disabled = after == "disabled_flag"
						incoming.Status = StatusActive
						if after == "disabled_status" {
							incoming.Status = StatusDisabled
						}
						incoming.LastRefreshedAt = oldTime.Add(time.Minute)
						incoming.NextRefreshAfter = oldTime.Add(3 * time.Hour)
						if zero {
							incoming.LastRefreshedAt = time.Time{}
							incoming.NextRefreshAfter = time.Time{}
						}
						updated, err := manager.Update(WithSkipPersist(t.Context()), incoming)
						if err != nil {
							t.Fatal(err)
						}
						wantLast := incoming.LastRefreshedAt
						if before == "active" && after == "active" && zero {
							wantLast = oldTime
						}
						if !updated.LastRefreshedAt.Equal(wantLast) ||
							!updated.NextRefreshAfter.Equal(incoming.NextRefreshAfter) {
							t.Fatalf(
								"last=%v next=%v want=%v/%v", updated.LastRefreshedAt, updated.NextRefreshAfter,
								wantLast, incoming.NextRefreshAfter,
							)
						}
					},
				)
			}
		}
	}
}

func TestAuditLegacyUpdateManualQuotaBlocksActualSelection(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	provider := &roundExecutor{provider: "codex"}
	manager.RegisterExecutor(provider)
	auth, err := manager.Register(
		WithSkipPersist(t.Context()), &Auth{ID: "legacy-manual-quota", Provider: "codex", Status: StatusActive},
	)
	if err != nil {
		t.Fatal(err)
	}
	registerReservationContextModels(t, "codex", "gpt-6-astra", auth.ID)
	oldSuccess := executionResult(auth, "codex", "gpt-6-astra", nil, executor.DispatchPhaseResponseStarted)
	quotaAuth := auth.Clone()
	quotaAuth.Quota = QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(10 * time.Minute),
	}
	updated, err := manager.Update(WithSkipPersist(t.Context()), quotaAuth)
	if err != nil || updated == nil || !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" ||
		!updated.Quota.NextRecoverAt.Equal(quotaAuth.Quota.NextRecoverAt) {
		t.Fatalf("quota=%+v error=%v", updated, err)
	}
	if updated.Quota.Generation == 0 || updated.StateGeneration == auth.StateGeneration {
		t.Fatal("manual quota update did not establish a new runtime generation")
	}
	repeated, err := manager.Update(WithSkipPersist(t.Context()), quotaAuth)
	if err != nil || repeated.Quota.Generation != updated.Quota.Generation {
		t.Fatalf("repeated legacy quota update was not idempotent: auth=%+v error=%v", repeated, err)
	}
	manager.MarkResult(t.Context(), oldSuccess)
	if _, err := manager.Execute(
		apiKeyReservationTestContext(), []string{"codex"}, executor.Request{Model: "gpt-6-astra"}, executor.Options{},
	); err == nil || len(provider.calls) != 0 {
		t.Fatalf("quota did not block actual selection: calls=%v error=%v", provider.calls, err)
	}
	if _, err := manager.Update(
		WithSkipPersist(t.Context()), &Auth{ID: auth.ID, Provider: auth.Provider, Status: StatusActive},
	); err != nil {
		t.Fatal(err)
	}
	current, _ := manager.GetByID(auth.ID)
	if !current.Quota.Exceeded || !current.Quota.NextRecoverAt.Equal(quotaAuth.Quota.NextRecoverAt) {
		t.Fatal("persistent-only update cleared the active credential quota")
	}
}

func TestAuditLegacyUpdateQuotaPersistsAndPreservesNewObservation(t *testing.T) {
	store := &auditLegacyUpdateStore{}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(
		t.Context(), &Auth{
			ID: "legacy-observation", Provider: "codex", Status: StatusActive,
			Metadata: map[string]any{"note": "synthetic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	store.onSave = func() {
		manager.mu.Lock()
		manager.auths[auth.ID].Quota.ObservedAt = observed
		manager.auths[auth.ID].Quota.Signals = map[string]string{"X-Codex-Primary-Used-Percent": "87"}
		manager.mu.Unlock()
	}
	auth.Quota = QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: observed.Add(10 * time.Minute),
		ResetAuthority: StateResetAuthorityReset,
	}
	updated, err := manager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if !store.saved.Quota.Exceeded || !updated.Quota.Exceeded || !updated.Quota.ObservedAt.Equal(observed) ||
		updated.Quota.Signals["X-Codex-Primary-Used-Percent"] != "87" {
		t.Fatalf("saved=%+v published=%+v", store.saved.Quota, updated.Quota)
	}
	if updated.Quota.ResetAuthority != StateResetAuthorityReset {
		t.Fatal("manual update lost its reset authority")
	}
}

func TestAuditLegacyUpdateRejectsStaleManualQuota(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(
		WithSkipPersist(t.Context()), &Auth{ID: "legacy-stale-quota", Provider: "test", Status: StatusActive},
	)
	if err != nil {
		t.Fatal(err)
	}
	stale := auth.Clone()
	auth.Quota = QuotaState{
		Exceeded: true, Reason: "current", NextRecoverAt: time.Now().Add(time.Hour),
		ResetAuthority: StateResetAuthorityOperator,
	}
	current, err := manager.Update(WithSkipPersist(t.Context()), auth)
	if err != nil {
		t.Fatal(err)
	}
	stale.Quota = QuotaState{Exceeded: true, Reason: "stale", NextRecoverAt: time.Now().Add(time.Minute)}
	if _, err := manager.Update(WithSkipPersist(t.Context()), stale); !errors.Is(err, ErrAuthGenerationConflict) {
		t.Fatalf("stale manual quota error=%v", err)
	}
	after, _ := manager.GetByID(auth.ID)
	if !reflect.DeepEqual(after.Quota, current.Quota) {
		t.Fatal("stale manual update replaced current authoritative quota")
	}
}

func TestAuditLegacyUpdateQuotaSaveFailureDoesNotPublish(t *testing.T) {
	store := &auditLegacyUpdateStore{}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(
		t.Context(), &Auth{
			ID: "legacy-quota-save", Provider: "test", Status: StatusActive,
			Metadata: map[string]any{"note": "synthetic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store.fail = errors.New("synthetic save failure")
	auth.Quota = QuotaState{Exceeded: true, Reason: "manual", NextRecoverAt: time.Now().Add(time.Hour)}
	if _, err := manager.Update(t.Context(), auth); !errors.Is(err, store.fail) {
		t.Fatalf("save error=%v", err)
	}
	current, _ := manager.GetByID(auth.ID)
	if current.Quota.Exceeded || current.Generation != auth.Generation {
		t.Fatal("failed persistent quota update was published")
	}
}

func TestAuditLegacyUpdateOldRegistrationCannotChangeQuotaOrTimes(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	old, err := manager.Register(
		WithSkipPersist(t.Context()), &Auth{ID: "legacy-reregister", Provider: "test", Status: StatusActive},
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := manager.Register(
		WithSkipPersist(t.Context()),
		&Auth{ID: old.ID, Provider: "test", Status: StatusActive, LastRefreshedAt: time.Now().UTC()},
	)
	if err != nil {
		t.Fatal(err)
	}
	old.Disabled = true
	old.LastRefreshedAt = time.Time{}
	old.Quota = QuotaState{Exceeded: true, Reason: "stale registration"}
	if _, err := manager.Update(WithSkipPersist(t.Context()), old); !errors.Is(err, ErrAuthGenerationConflict) {
		t.Fatalf("old registration error=%v", err)
	}
	after, _ := manager.GetByID(old.ID)
	if after.Disabled || after.Quota.Exceeded || !after.LastRefreshedAt.Equal(current.LastRefreshedAt) {
		t.Fatal("old registration changed the replacement auth")
	}
}
