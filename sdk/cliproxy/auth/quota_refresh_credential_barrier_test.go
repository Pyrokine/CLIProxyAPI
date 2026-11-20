package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestQuotaRefreshCredentialBarrierWithFamilyRecovery(t *testing.T) {
	for _, newer := range []bool{false, true} {
		for _, family := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("newer=%t/family=%t", newer, family), func(t *testing.T) {
					ctx := context.Background()
					manager := NewManager(nil, nil, nil)
					const model = "gpt-5.6-sol"
					id := "quota-refresh-barrier-" + t.Name()
					old := time.Now().Add(-time.Minute)
					next := time.Now().Add(time.Hour)
					auth := &Auth{
						ID: id, Provider: "codex", Status: StatusError, Unavailable: true,
						StatusMessage: "old credential quota", NextRetryAfter: next, UpdatedAt: old,
						LastError: &Error{
							HTTPStatus: http.StatusTooManyRequests, Message: "old credential quota",
						},
						Quota:            QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
						StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
					}
					if family {
						auth.ModelStates = map[string]*ModelState{
							model: {
								Status: StatusError, Unavailable: true, NextRetryAfter: next, UpdatedAt: old,
								LastError: &Error{
									HTTPStatus: http.StatusTooManyRequests, Message: "old family quota",
								},
								Quota:       QuotaState{Exceeded: true, Family: "sol-family", Generation: 5},
								EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
								EffectKey:   "sol-family", Generation: 5, QuotaEpoch: 4,
							},
						}
					}
					registered, err := manager.Register(ctx, auth)
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { reg.UnregisterClient(id) })
					if family {
						reg.SetModelQuotaExceeded(id, model)
						reg.SuspendClientModel(id, model, "quota")
					}
					started := time.Now().UTC()
					if newer {
						manager.MarkResult(
							ctx, Result{
								AuthID: id, AuthGeneration: registered.Generation, Model: model,
								Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "new credential quota"},
								Disposition: cliproxyexecutor.ExecutionDisposition{
									DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
									Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
									RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
									EffectScope:    cliproxyexecutor.StateEffectScopeCredential, EffectKey: id,
								},
							},
						)
					}
					before, _ := manager.GetByID(id)
					if newer && (!before.UpdatedAt.After(started) || !before.Quota.Exceeded) {
						t.Fatalf("new 429 was not applied: %+v", before)
					}
					recovered, err := manager.RecoverQuotaAfterRefresh(
						ctx, id, started, QuotaFamilyRecoveryCandidates(before),
					)
					if err != nil {
						t.Fatal(err)
					}
					if recovered.Quota.Exceeded != newer || recovered.Unavailable != newer {
						t.Fatalf(
							"credential cooldown after refresh: quota=%+v unavailable=%t; want newer=%t",
							recovered.Quota, recovered.Unavailable, newer,
						)
					}
					if newer && (recovered.LastError == nil || recovered.LastError.Message != "new credential quota") {
						t.Fatalf("new credential error was overwritten: %+v", recovered)
					}
					if family {
						state := recovered.ModelStates[model]
						if state.Quota.Exceeded || state.Unavailable || state.QuotaEpoch != 5 ||
							reg.IsModelSuspendedForClient(id, model) != newer || reg.IsModelQuotaExceededForClient(
							id, model,
						) {
							t.Fatalf("old matching family did not recover independently: %+v", state)
						}
					}
					picked, err := manager.scheduler.pickSingle(ctx, "codex", model, cliproxyexecutor.Options{}, nil)
					if newer {
						if err == nil || picked != nil {
							t.Fatalf("new credential cooldown remained selectable: picked=%+v err=%v", picked, err)
						}
					} else if err != nil || picked == nil || picked.ID != id {
						t.Fatalf("old cooldown did not restore scheduling: picked=%+v err=%v", picked, err)
					}
				},
			)
		}
	}
}
