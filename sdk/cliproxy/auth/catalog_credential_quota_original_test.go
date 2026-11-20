package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalCredentialQuotaKeepsHealthyCatalogAcrossRestart(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	store := NewFileCooldownStateStore(t.TempDir())
	manager.SetCooldownStateStore(store)
	reg := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: "audit-luna"}, {ID: "audit-sol"}, {ID: "audit-terra"}}
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		if _, err := manager.Register(ctx, &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "codex", models)
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	for _, model := range models {
		credential, _ := manager.GetByID("audit-quota-oauth")
		manager.MarkResult(
			ctx, executionResult(credential, "codex", model.ID, nil, core.DispatchPhaseDownstreamCommitted),
		)
	}
	credential, _ := manager.GetByID("audit-quota-oauth")
	disposition := core.ExecutionDisposition{
		DispatchPhase: core.DispatchPhaseResponseStarted, Outcome: core.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: core.RetryDirectiveRetryAfter, EffectScope: core.StateEffectScopeCredential,
		EffectKey: credential.ID,
	}
	retry := 3 * time.Hour
	before := time.Now()
	manager.MarkResult(
		ctx, Result{
			AuthID: credential.ID, AuthGeneration: credential.Generation, Provider: "codex", Model: "audit-sol",
			Error: &Error{
				HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "quota exceeded",
			},
			RetryAfter: &retry, Disposition: disposition,
			StateGeneration: stateGenerationForDisposition(credential, "audit-sol", disposition),
		},
	)
	after := time.Now()
	assertCatalog := func(t *testing.T, currentManager *Manager) *Auth {
		t.Helper()
		cooling, found := currentManager.GetByID("audit-quota-oauth")
		if !found || !cooling.Quota.Exceeded || cooling.StateEffectScope != core.StateEffectScopeCredential ||
			cooling.StateEffectKey != cooling.ID ||
			cooling.StateGeneration == 0 ||
			cooling.ResetAuthority != StateResetAuthoritySuccess {
			t.Fatalf("credential quota authority=%+v found=%t", cooling, found)
		}
		if cooling.Quota.Reason != "quota" || cooling.Quota.Generation == 0 ||
			cooling.Quota.ResetAuthority != StateResetAuthoritySuccess ||
			cooling.Quota.NextRecoverAt.Before(before.Add(retry)) ||
			cooling.Quota.NextRecoverAt.After(after.Add(retry)) {
			t.Fatalf("credential quota deadline or reset authority changed: %+v", cooling.Quota)
		}
		if cooling.LastError == nil || cooling.LastError.HTTPStatus != http.StatusTooManyRequests ||
			cooling.LastError.Code != "usage_limit_reached" ||
			cooling.LastError.Message != "quota exceeded" {
			t.Fatalf("original credential quota error changed: %+v", cooling.LastError)
		}
		healthy, found := currentManager.GetByID("audit-healthy-api")
		if !found {
			t.Fatal("healthy credential disappeared")
		}
		listed := map[string]bool{}
		for _, model := range reg.GetAvailableModels("openai") {
			id, _ := model["id"].(string)
			listed[id] = true
		}
		for _, model := range models {
			if blocked, _, _ := isAuthBlockedForModel(
				cooling, model.ID, time.Now(),
			); !blocked || !reg.IsModelSuspendedForClient(cooling.ID, model.ID) {
				t.Fatalf("credential quota did not block OAuth model %s", model.ID)
			}
			if reg.IsModelQuotaExceededForClient(cooling.ID, model.ID) {
				t.Fatalf("credential quota was mirrored as model quota for %s", model.ID)
			}
			if blocked, _, _ := isAuthBlockedForModel(
				healthy, model.ID, time.Now(),
			); blocked || reg.IsModelSuspendedForClient(healthy.ID, model.ID) || !listed[model.ID] {
				t.Fatalf("healthy provider or catalog blocked for %s: listed=%t", model.ID, listed[model.ID])
			}
		}
		return cooling
	}
	var saved *Auth
	t.Run("before restart", func(t *testing.T) { saved = assertCatalog(t, manager) })
	if saved == nil {
		t.Fatal("credential quota was not available before restart")
	}
	records, err := store.Load(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("real CDS records=%+v error=%v", records, err)
	}
	if records[0].AuthID != saved.ID || records[0].Model != "" ||
		records[0].EffectScope != core.StateEffectScopeCredential ||
		records[0].StateGeneration != saved.StateGeneration ||
		records[0].ResetAuthority != saved.ResetAuthority {
		t.Fatalf("real CDS changed credential quota identity: %+v", records[0])
	}
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		reg.UnregisterClient(id)
	}
	restarted := NewManager(nil, nil, nil)
	restarted.SetConfig(&internalconfig.Config{})
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		if _, err := restarted.Register(ctx, &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "codex", models)
	}
	restarted.SetCooldownStateStore(store)
	if err := restarted.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run(
		"after restart", func(t *testing.T) {
			restored := assertCatalog(t, restarted)
			if restored.StateGeneration != saved.StateGeneration ||
				!restored.Quota.NextRecoverAt.Equal(saved.Quota.NextRecoverAt) {
				t.Fatalf("restart changed quota generation or deadline: before=%+v after=%+v", saved, restored)
			}
		},
	)
}
