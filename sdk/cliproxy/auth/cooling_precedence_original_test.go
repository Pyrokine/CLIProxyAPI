package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalManagerMarkResultUsesCredentialCoolingPrecedence(t *testing.T) {
	previousGlobal := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousGlobal) })
	disabled, enabled := true, false
	for _, tc := range []struct {
		name             string
		homeEnabled      bool
		globalDisable    bool
		credential       *bool
		providerOverride *bool
		wantCooldown     bool
	}{
		{name: "credential true overrides global false", credential: &disabled},
		{name: "credential false overrides global true", globalDisable: true, credential: &enabled, wantCooldown: true},
		{name: "unset inherits global true", globalDisable: true},
		{name: "unset inherits global false", wantCooldown: true},
		{
			name: "provider false overrides global true", globalDisable: true, providerOverride: &enabled,
			wantCooldown: true,
		},
		{name: "provider true overrides global false", providerOverride: &disabled},
		{
			name: "credential false overrides provider true", credential: &enabled, providerOverride: &disabled,
			wantCooldown: true,
		},
		{name: "home mode disables local cooling despite credential false", homeEnabled: true, credential: &enabled},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				hook := &resultPolicyRecordingHook{}
				manager := NewManager(nil, nil, hook)
				cfg := &internalconfig.Config{
					DisableCooling: tc.globalDisable,
					Home:           internalconfig.HomeConfig{Enabled: tc.homeEnabled},
				}
				auth := &Auth{ID: tc.name, Provider: "claude", Status: StatusActive}
				if tc.credential != nil {
					auth.Metadata = map[string]any{"disable_cooling": *tc.credential}
				}
				if tc.providerOverride != nil {
					auth.Provider = "openai-compatibility"
					auth.Attributes = map[string]string{"provider_key": "compat", "compat_name": "compat"}
					cfg.OpenAICompatibility = []internalconfig.OpenAICompatibility{
						{
							Name: "compat", BaseURL: "https://compat.example.com", DisableCooling: tc.providerOverride,
						},
					}
				}
				manager.SetConfig(cfg)
				registered, err := manager.Register(context.Background(), auth)
				if err != nil {
					t.Fatalf("Register() error = %v", err)
				}
				const model = "test-model"
				reg := registerProjectionTestClient(t, auth.ID, auth.Provider, &registry.ModelInfo{ID: model})
				manager.MarkResult(
					context.Background(), Result{
						AuthID: auth.ID, AuthGeneration: registered.Generation, Provider: auth.Provider, Model: model,
						Error: &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"},
					},
				)
				updated := getExecutionOutcomeTestAuth(t, manager, auth.ID)
				result := hook.last.Load()
				if updated.Unavailable ||
					!updated.NextRetryAfter.IsZero() ||
					len(updated.ModelStates) !=
						0 ||
					updated.Failed !=
						1 ||
					result == nil || result.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
					result.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
					result.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeProvider ||
					result.Disposition.EffectKey != auth.Provider || result.Error == nil ||
					result.Error.Message != "upstream failed" {
					t.Fatalf(
						"original unknown 500 changed local state or lost its result: auth=%+v result=%+v",
						updated,
						result,
					)
				}
				wantCooldown := tc.wantCooldown || tc.homeEnabled
				if got := QuotaCooldownDisabledForAuthWithConfig(registered, cfg); got == wantCooldown {
					t.Fatalf("cooling disabled=%t, want %t", got, !wantCooldown)
				}
				markOriginalCooldownResult(
					t, manager, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: model,
						Error: &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"},
						Disposition: cliproxyexecutor.ExecutionDisposition{
							DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
							Outcome:        cliproxyexecutor.ExecutionOutcomeFailed,
							RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
							EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
							EffectKey:      model,
						},
					},
				)
				updated = getExecutionOutcomeTestAuth(t, manager, auth.ID)
				state := updated.ModelStates[model]
				if state == nil || state.NextRetryAfter.IsZero() == wantCooldown || state.Unavailable != wantCooldown ||
					state.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
					state.ResetAuthority != StateResetAuthoritySuccess ||
					updated.Failed != 2 {
					t.Fatalf("typed model cooldown=%+v, want enabled=%t", state, wantCooldown)
				}
				assertOriginalCooldownRouting(t, updated, model, time.Now(), wantCooldown)
				if reg.IsModelSuspendedForClient(auth.ID, model) != wantCooldown {
					t.Fatal("registry projection disagreed with the configured cooling policy")
				}
			},
		)
	}
}

func TestCoolingPrecedenceKeepsCreditsOptInAndHomeAuthority(t *testing.T) {
	previousGlobal := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousGlobal) })
	previousHome := homekv.Current()
	homekv.SetCurrent(homekv.New(internalconfig.HomeConfig{Enabled: false}))
	t.Cleanup(func() { homekv.SetCurrent(previousHome) })
	for _, coolingDisabled := range []bool{false, true} {
		for _, credits := range []bool{false, true} {
			for _, homeEnabled := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("disable_cooling=%t/credits=%t/home=%t", coolingDisabled, credits, homeEnabled),
					func(t *testing.T) {
						const model = "claude-opus-4-6-thinking"
						executor := &originalAntigravityCreditsFallbackExecutor{}
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(
							&internalconfig.Config{
								DisableCooling: coolingDisabled,
								Home:           internalconfig.HomeConfig{Enabled: homeEnabled},
								QuotaExceeded:  internalconfig.QuotaExceeded{AntigravityCredits: credits},
							},
						)
						manager.RegisterExecutor(executor)
						registerProjectionTestClient(
							t,
							"ag-credits",
							"antigravity",
							&registry.ModelInfo{ID: model},
						)
						if _, err := manager.Register(
							context.Background(), &Auth{ID: "ag-credits", Provider: "antigravity"},
						); err != nil {
							t.Fatal(err)
						}
						stream, err := manager.ExecuteStream(
							context.Background(), []string{"antigravity"},
							cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
						)
						if homeEnabled {
							if statusCodeFromError(err) != http.StatusServiceUnavailable ||
								len(executor.streamCreditsRequested) != 0 {
								t.Fatalf(
									"Home gate error=%v local attempts=%v",
									err,
									executor.streamCreditsRequested,
								)
							}
							return
						}
						if !credits {
							if err != nil || stream == nil {
								t.Fatalf("free stream setup error=%v", err)
							}
							var rejection error
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									if rejection != nil {
										t.Fatal("free stream published more than one rejection")
									}
									rejection = chunk.Err
								}
								if len(chunk.Payload) != 0 {
									t.Fatalf("rejected free stream published payload=%q", chunk.Payload)
								}
							}
							if rejection == nil || statusCodeFromError(rejection) != http.StatusTooManyRequests ||
								!strings.Contains(
									rejection.Error(), "quota exhausted",
								) ||
								len(executor.streamCreditsRequested) != 1 ||
								executor.streamCreditsRequested[0] {
								t.Fatalf(
									"credits were not opted in: error=%v attempts=%v", rejection,
									executor.streamCreditsRequested,
								)
							}
							return
						}
						if err != nil || stream == nil {
							t.Fatalf("explicit credits execution error=%v", err)
						}
						if payload := readOpenAICompatStreamPayload(t, stream); payload != "credits fallback" {
							t.Fatalf("credits payload=%q", payload)
						}
						if stream.Headers.Get("X-Credits") != model || len(executor.streamCreditsRequested) != 2 ||
							executor.streamCreditsRequested[0] ||
							!executor.streamCreditsRequested[1] {
							t.Fatalf(
								"explicit credits result headers=%v attempts=%v", stream.Headers,
								executor.streamCreditsRequested,
							)
						}
					},
				)
			}
		}
	}
}
