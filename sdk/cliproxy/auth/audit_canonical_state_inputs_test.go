package auth

import (
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuditCanonicalStateEntryAndRestorePreserveAuthority(t *testing.T) {
	for _, entry := range []string{"register", "update", "load"} {
		for _, newerQuota := range []bool{false, true} {
			name := entry + "/quota-newer"
			if !newerQuota {
				name = entry + "/error-newer"
			}
			t.Run(
				name, func(t *testing.T) {
					now := time.Now()
					quotaAt := now.Add(2 * time.Hour)
					quotaGeneration, errorGeneration := uint64(8), uint64(4)
					if !newerQuota {
						quotaGeneration, errorGeneration = 4, 8
					}
					credential := &Auth{
						ID: t.Name(), Provider: "test", Status: StatusActive,
						Metadata: map[string]any{"access_token": "synthetic-access"},
						ModelStates: map[string]*ModelState{
							"gpt-5(high)": {
								Status: StatusError, Unavailable: true, NextRetryAfter: quotaAt,
								Generation: quotaGeneration, ResetAuthority: StateResetAuthorityOperator,
								EffectScope: core.StateEffectScopeExactModel, EffectKey: "gpt-5(high)",
								Quota: QuotaState{
									Exceeded: true, Family: "independent", Generation: 9,
									NextRecoverAt: quotaAt, ResetAuthority: StateResetAuthorityOperator,
								},
							},
							"gpt-5": {
								Status: StatusError, Unavailable: true,
								NextRetryAfter: now.Add(time.Minute), Generation: errorGeneration,
								ResetAuthority: StateResetAuthoritySuccess,
								EffectScope:    core.StateEffectScopeExactModel, EffectKey: "gpt-5",
								LastError: &Error{HTTPStatus: 503, Message: "independent model failure"},
							},
						},
					}
					manager := NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					if entry == "load" {
						manager = NewManager(&countingStore{list: []*Auth{credential}}, nil, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						if err := manager.Load(t.Context()); err != nil {
							t.Fatal(err)
						}
					} else {
						registered, err := manager.Register(t.Context(), credential)
						if err != nil {
							t.Fatal(err)
						}
						if entry == "update" {
							registered.Label = "updated-label"
							if _, err := manager.Update(t.Context(), registered); err != nil {
								t.Fatal(err)
							}
						}
					}
					current, _ := manager.GetByID(credential.ID)
					state := current.ModelStates["gpt-5"]
					if len(current.ModelStates) != 1 || state == nil || state.Generation != 8 ||
						state.EffectKey != "gpt-5" ||
						!state.Quota.Exceeded ||
						state.Quota.Generation != 9 ||
						state.Quota.Family != "independent" ||
						state.Quota.ResetAuthority != StateResetAuthorityOperator ||
						!state.Quota.NextRecoverAt.Equal(quotaAt) {
						t.Fatalf("canonical state lost authority: states=%#v canonical=%#v", current.ModelStates, state)
					}
					if !newerQuota && (state.LastError == nil || state.LastError.HTTPStatus != 503) {
						t.Fatalf("newer independent model failure was lost: %#v", state)
					}
					if blocked, _, _ := isAuthBlockedForModel(current, "gpt-5", now.Add(2*time.Minute)); !blocked {
						t.Fatal("independent operator quota became selectable after the shorter model failure expired")
					}
					manager.mu.Lock()
					restored := manager.restoreCooldownRecordLocked(
						CooldownStateRecord{
							AuthID: credential.ID, Model: "gpt-5(high)", StateGeneration: 7,
							NextRetryAfter: now.Add(3 * time.Hour), LastError: &Error{HTTPStatus: 404},
						}, now,
					)
					manager.mu.Unlock()
					if restored {
						t.Fatal("older suffix cooldown replaced the newer canonical authority")
					}
				},
			)
		}
	}
}
