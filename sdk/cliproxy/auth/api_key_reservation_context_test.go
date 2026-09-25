package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type reservationContextRetryError struct {
	directive coreexecutor.RetryDirective
}

func (e reservationContextRetryError) Error() string {
	return "reservation context retry"
}

func (e reservationContextRetryError) ExecutionDisposition() coreexecutor.ExecutionDisposition {
	return coreexecutor.ExecutionDisposition{
		DispatchPhase:  coreexecutor.DispatchPhaseResponseStarted,
		Outcome:        coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: e.directive,
		EffectScope:    coreexecutor.StateEffectScopeRequest,
	}
}

func registerReservationContextModels(t *testing.T, provider, model string, authIDs ...string) {
	t.Helper()
	for _, authID := range authIDs {
		registry.GetGlobalRegistry().RegisterClient(
			authID,
			provider,
			[]*registry.ModelInfo{{ID: model}},
		)
	}
	t.Cleanup(
		func() {
			for _, authID := range authIDs {
				registry.GetGlobalRegistry().UnregisterClient(authID)
			}
		},
	)
}

func apiKeyReservationTestContext() context.Context {
	ctx := coreusage.WithAPIKeyIdentity(context.Background(), "sha256:test-identity")
	return coreusage.WithAPIKeyReservation(ctx, "rsv_test-reservation")
}

func assertAPIKeyReservationContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if got := coreusage.APIKeyIdentityFromContext(ctx); got != "sha256:test-identity" {
		t.Fatalf("API key identity = %q", got)
	}
	if got := coreusage.APIKeyReservationFromContext(ctx); got != "rsv_test-reservation" {
		t.Fatalf("API key reservation = %q", got)
	}
}

func TestMixedOncePreservesAPIKeyReservationAcrossCredentialRetry(t *testing.T) {
	const (
		provider = "test"
		model    = "reservation-context-mixed-model"
	)
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	authIDs := []string{"reservation-context-auth-a", "reservation-context-auth-b"}
	for _, authID := range authIDs {
		if _, err := manager.Register(
			context.Background(), &Auth{
				ID:       authID,
				Provider: provider,
				Status:   StatusActive,
			},
		); err != nil {
			t.Fatalf("register auth %s: %v", authID, err)
		}
	}
	registerReservationContextModels(t, provider, model, authIDs...)

	attempts := 0
	_, err := mixedOnce(
		manager,
		apiKeyReservationTestContext(),
		[]string{provider},
		coreexecutor.Request{Model: model},
		coreexecutor.Options{},
		len(authIDs),
		func(
			execCtx context.Context,
			_ *Auth,
			_ ProviderExecutor,
			_ string,
			_ coreexecutor.Request,
			_ coreexecutor.Options,
			_ OAuthModelAliasResult,
		) (struct{}, error) {
			assertAPIKeyReservationContext(t, execCtx)
			attempts++
			if attempts == 1 {
				return struct{}{}, reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}
			}
			return struct{}{}, nil
		},
		manager.MarkResult,
	)
	if err != nil {
		t.Fatalf("mixedOnce() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("mixedOnce() attempts = %d, want 2", attempts)
	}
}

func TestRetryWithCooldownPreservesAPIKeyReservationAcrossAttempts(t *testing.T) {
	const (
		provider = "test"
		model    = "reservation-context-cooldown-model"
		authID   = "reservation-context-cooldown-auth"
	)
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(1, 100*time.Millisecond, 0)
	manager.RegisterExecutor(schedulerTestExecutor{})
	registerReservationContextModels(t, provider, model, authID)
	if _, err := manager.Register(
		context.Background(), &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(10 * time.Millisecond),
					Quota:          QuotaState{Exceeded: true},
					EffectScope:    coreexecutor.StateEffectScopeQuotaFamily,
					EffectKey:      model,
				},
			},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	attempts := 0
	_, err := retryWithCooldown(
		manager,
		apiKeyReservationTestContext(),
		[]string{provider},
		coreexecutor.Request{Model: model},
		coreexecutor.Options{},
		func(
			attemptCtx context.Context,
			_ []string,
			_ coreexecutor.Request,
			_ coreexecutor.Options,
			_ int,
		) (struct{}, error) {
			assertAPIKeyReservationContext(t, attemptCtx)
			attempts++
			if attempts == 1 {
				return struct{}{}, reservationContextRetryError{directive: coreexecutor.RetryDirectiveRetryAfter}
			}
			return struct{}{}, nil
		},
	)
	if err != nil {
		t.Fatalf("retryWithCooldown() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("retryWithCooldown() attempts = %d, want 2", attempts)
	}
}
