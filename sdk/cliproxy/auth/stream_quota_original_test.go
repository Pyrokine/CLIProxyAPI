package auth

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// noinspection GoDirectComparisonOfErrors
func TestOriginalStreamQuotaFailurePreservesCooldownAndScope(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, acknowledged := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "legacy", true: "acknowledged"}[acknowledged], func(t *testing.T) {
				for _, credential := range []bool{true, false} {
					for _, weighted := range []bool{false, true} {
						name := fmt.Sprintf("credential_scope=%t/weighted=%t", credential, weighted)
						t.Run(
							name, func(t *testing.T) {
								var selector Selector = &RoundRobinSelector{}
								if weighted {
									selector = &WeightedRoundRobinSelector{}
								}
								hook := newStreamDeliveryResultHook()
								manager := NewManager(nil, selector, hook)
								manager.SetHomeClientProvider(func() HomeClient { return nil })
								manager.SetRetryConfig(3, 30*time.Second, 0)
								const model, sibling = "stream-quota-model", "stream-quota-sibling"
								high, low := "stream-quota-high-"+name, "stream-quota-low-"+name
								for _, auth := range []*Auth{
									{
										ID: high, Provider: "codex", Status: StatusActive,
										Attributes: map[string]string{"priority": "4", AttributeWeight: "1"},
									},
									{
										ID: low, Provider: "codex", Status: StatusActive,
										Attributes: map[string]string{"priority": "3", AttributeWeight: "1"},
									},
								} {
									registerProjectionTestClient(
										t, auth.ID, auth.Provider, &registry.ModelInfo{ID: model},
										&registry.ModelInfo{ID: sibling},
									)
									if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
										t.Fatal(err)
									}
								}
								delay := time.Hour
								body := `{"error":{"type":"rate_limit_error","message":"Model rate limit exceeded"}}`
								scope, key := core.StateEffectScopeExactModel, model
								if credential {
									body = `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
									scope, key = core.StateEffectScopeCredential, high
								}
								failure := &requestScopedDispositionError{
									requestScopedTestError: &requestScopedTestError{
										status:  http.StatusTooManyRequests,
										message: body, body: []byte(body), retryAfter: &delay,
									},
									disposition: core.ExecutionDisposition{
										DispatchPhase:  core.DispatchPhaseResponseStarted,
										Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
										RetryDirective: core.RetryDirectiveNoRetry,
										EffectScope:    scope, EffectKey: key,
									},
								}
								var attempts []string
								provider := &requestScopedTestExecutor{
									provider: "codex", stream: func(auth *Auth) (*core.StreamResult, error) {
										attempts = append(attempts, auth.ID)
										chunks := make(chan core.StreamChunk, 2)
										chunks <- core.StreamChunk{
											Payload: []byte(("data: {\"type\":" +
												"\"response.created\"}\n\n")),
										}
										chunks <- core.StreamChunk{Err: failure}
										close(chunks)
										return &core.StreamResult{Chunks: chunks}, nil
									},
								}
								manager.RegisterExecutor(provider)
								before := time.Now()
								ctx := t.Context()
								if acknowledged {
									ctx = core.WithStreamDeliveryAcknowledgement(ctx)
								}
								stream, err := manager.ExecuteStream(
									ctx,
									[]string{"codex"},
									core.Request{Model: model},
									core.Options{Stream: true},
								)
								if err != nil {
									t.Fatal(err)
								}
								payloads, failures := 0, 0
								for chunk := range stream.Chunks {
									if len(chunk.Payload) > 0 {
										payloads++
										if string(
											chunk.Payload,
										) != "data: {\"type\":\"response.created\"}\n\n" {
											t.Fatalf("stream payload=%q", chunk.Payload)
										}
									}
									if chunk.Err != nil {
										failures++
										if chunk.Err != failure {
											t.Fatalf("stream error=%v, want original quota error", chunk.Err)
										}
									}
									if acknowledged {
										if chunk.Acknowledge == nil {
											t.Fatal("stream chunk did not carry delivery acknowledgement")
										}
										chunk.Acknowledge(nil)
									}
								}
								if payloads != 1 || failures != 1 || len(attempts) != 1 ||
									attempts[0] != high ||
									provider.streamCalls.Load() != 1 {
									t.Fatalf(
										"payloads=%d errors=%d attempts=%v calls=%d",
										payloads,
										failures,
										attempts,
										provider.streamCalls.Load(),
									)
								}
								current, ok := manager.GetByID(high)
								if !ok || current.Failed != 1 {
									t.Fatalf("quota credential=%+v found=%t", current, ok)
								}
								select {
								case result := <-hook.result:
									if result.Success || result.AuthGeneration != current.Generation ||
										result.StateGeneration == 0 ||
										result.Disposition.EffectScope != scope ||
										result.Disposition.EffectKey != key {
										t.Fatalf("quota result=%+v", result)
									}
								default:
									t.Fatal("stream quota failure did not publish its result")
								}
								next := current.NextRetryAfter
								if credential {
									if current.StateEffectScope !=
										scope ||
										current.StateEffectKey !=
											high ||
										current.ResetAuthority !=
											StateResetAuthoritySuccess ||
										!current.Quota.Exceeded ||
										current.Quota.NextRecoverAt.Before(before.Add(delay)) ||
										current.ModelStates[model] != nil {
										t.Fatalf(
											"credential quota scope=%+v models=%v",
											current.Quota,
											current.ModelStates,
										)
									}
								} else {
									state := current.ModelStates[model]
									if state == nil || state.EffectScope != scope || state.EffectKey != model ||
										state.ResetAuthority != StateResetAuthoritySuccess || !state.Quota.Exceeded {
										t.Fatalf("model quota=%+v", state)
									}
									next = state.NextRetryAfter
								}
								if next.Before(before.Add(delay)) || next.After(time.Now().Add(delay)) {
									t.Fatalf("quota retry=%v, want %s", next, delay)
								}
								for _, requested := range []string{model, sibling} {
									want := low
									if requested == sibling && !credential {
										want = high
									}
									selected, _, _, err := manager.pickNextMixed(
										t.Context(), []string{"codex"}, requested, core.Options{}, nil,
									)
									if err != nil || selected == nil || selected.ID != want {
										t.Fatalf(
											"selected %s=%+v error=%v, want %s",
											requested,
											selected,
											err,
											want,
										)
									}
								}
								if blocked, _, _ := isAuthBlockedForModel(
									current, model, before.Add(time.Minute),
								); !blocked {
									t.Fatal("quota credential became selectable before reset")
								}
							},
						)
					}
				}
			},
		)
	}
}
