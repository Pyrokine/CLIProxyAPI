package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestSuccessfulCredentialRefreshPublishesAllPreservedModelStates(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const (
		authID            = "credential-refresh-projection-auth"
		provider          = "credential-refresh-projection-provider"
		unauthorizedModel = "credential-refresh-unauthorized-model"
		quotaModel        = "credential-refresh-quota-model"
		quotaFamily       = "credential-refresh-quota-family"
	)
	executor := &executorKeyRefreshExecutor{id: provider}
	manager.RegisterExecutor(executor)
	now := time.Now()
	future := now.Add(time.Hour)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:             authID,
		Provider:       provider,
		Status:         StatusError,
		StatusMessage:  "unauthorized",
		Unavailable:    true,
		NextRetryAfter: future,
		LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized"},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-token",
		},
		ModelStates: map[string]*ModelState{
			unauthorizedModel: {
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: future,
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized"},
				EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
				Generation:     2,
				UpdatedAt:      now.Add(-time.Minute),
			},
			quotaModel: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: future,
				LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Code: "rate_limit"},
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					Family:        quotaFamily,
					NextRecoverAt: future,
					Generation:    3,
				},
				EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
				EffectKey:   quotaFamily,
				Generation:  3,
				UpdatedAt:   now,
			},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: unauthorizedModel},
		&registry.ModelInfo{ID: quotaModel},
	)
	reg.SuspendClientModel(authID, unauthorizedModel, "unauthorized")
	reg.SuspendClientModel(authID, quotaModel, "quota")
	reg.SetModelQuotaExceeded(authID, quotaModel)

	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), authID, "old-token", registered.Generation,
	)
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	if refreshed == nil || authAccessToken(refreshed) != "refreshed-token" {
		t.Fatalf("refreshed auth = %#v", refreshed)
	}
	unauthorizedState := refreshed.ModelStates[unauthorizedModel]
	if unauthorizedState == nil || unauthorizedState.Unavailable || unauthorizedState.LastError != nil {
		t.Fatalf("unauthorized model state = %#v, want recovered", unauthorizedState)
	}
	quotaState := refreshed.ModelStates[quotaModel]
	if quotaState == nil || !quotaState.Unavailable || !quotaState.Quota.Exceeded || quotaState.LastError == nil {
		t.Fatalf("quota model state = %#v, want preserved", quotaState)
	}
	if reg.IsModelSuspendedForClient(authID, unauthorizedModel) ||
		reg.IsModelQuotaExceededForClient(authID, unauthorizedModel) {
		t.Fatal("credential refresh retained the recovered unauthorized model projection")
	}
	if !reg.IsModelSuspendedForClient(authID, quotaModel) ||
		!reg.IsModelQuotaExceededForClient(authID, quotaModel) {
		t.Fatal("credential refresh cleared an unrelated quota model projection")
	}
}

func TestManagerRestoreCooldownStatesPublishesCurrentRegistryProjection(t *testing.T) {
	const (
		authID     = "restore-cooldown-projection-auth"
		provider   = "restore-cooldown-projection-provider"
		quotaModel = "restore-cooldown-quota-model"
		cleanModel = "restore-cooldown-clean-model"
		family     = "restore-cooldown-family"
	)
	now := time.Now()
	future := now.Add(time.Hour)
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{
				Provider:       provider,
				AuthID:         authID,
				Model:          quotaModel,
				Status:         "cooling",
				NextRetryAfter: future,
				Reason:         "quota",
				Quota: QuotaState{
					Exceeded:       true,
					Reason:         "quota",
					Family:         family,
					NextRecoverAt:  future,
					Generation:     4,
					ResetAuthority: StateResetAuthoritySuccess,
				},
				LastError:       &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
				EffectScope:     cliproxyexecutor.StateEffectScopeQuotaFamily,
				EffectKey:       family,
				StateGeneration: 4,
				ResetAuthority:  StateResetAuthoritySuccess,
				UpdatedAt:       now,
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()),
		&Auth{ID: authID, Provider: provider, Status: StatusActive},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: quotaModel},
		&registry.ModelInfo{ID: cleanModel},
	)
	manager.RefreshSchedulerEntry(authID)
	if reg.IsModelSuspendedForClient(authID, quotaModel) ||
		reg.IsModelQuotaExceededForClient(authID, quotaModel) {
		t.Fatal("clean registration received stale cooldown state")
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	if !reg.IsModelSuspendedForClient(authID, quotaModel) ||
		!reg.IsModelQuotaExceededForClient(authID, quotaModel) {
		t.Fatal("restored cooldown was not published to registry")
	}
	if reg.IsModelSuspendedForClient(authID, cleanModel) ||
		reg.IsModelQuotaExceededForClient(authID, cleanModel) {
		t.Fatal("restored cooldown affected an unrelated registered model")
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Generation != registered.Generation {
		t.Fatalf("cooldown restore changed credential identity: %#v", current)
	}
}

func TestManagerRefreshSchedulerEntryReappliesCurrentRegistrationProjection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const (
		authID   = "refresh-entry-projection-auth"
		provider = "refresh-entry-projection-provider"
		model    = "refresh-entry-route-alias"
	)
	future := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusError,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: future,
				Quota: QuotaState{
					Exceeded: true, Reason: "quota", NextRecoverAt: future,
				},
			},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: model, CatalogName: "upstream-model"},
	)

	manager.RefreshSchedulerEntry(authID)
	if !reg.IsModelSuspendedForClient(authID, model) ||
		!reg.IsModelQuotaExceededForClient(authID, model) {
		t.Fatal("initial current-epoch projection was not applied")
	}
	_, oldEpoch := reg.GetModelsAndEpochForClient(authID)

	reg.UnregisterClient(authID)
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model, CatalogName: "upstream-model"}})
	_, currentEpoch := reg.GetModelsAndEpochForClient(authID)
	if currentEpoch <= oldEpoch {
		t.Fatalf("re-registration epoch = %d, want above %d", currentEpoch, oldEpoch)
	}
	if reg.IsModelSuspendedForClient(authID, model) || reg.IsModelQuotaExceededForClient(authID, model) {
		t.Fatal("re-registration retained the old registry projection")
	}
	if reg.ApplyClientModelProjections(authID, oldEpoch, ^uint64(0), []registry.ClientModelProjection{
		{ModelID: model},
	}) {
		t.Fatal("current registration accepted an old-epoch projection")
	}

	manager.RefreshSchedulerEntry(authID)
	if !reg.IsModelSuspendedForClient(authID, model) ||
		!reg.IsModelQuotaExceededForClient(authID, model) {
		t.Fatal("RefreshSchedulerEntry() did not project auth state onto the current registration")
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Generation != registered.Generation {
		t.Fatalf("RefreshSchedulerEntry() changed live auth identity: %#v", current)
	}
}
