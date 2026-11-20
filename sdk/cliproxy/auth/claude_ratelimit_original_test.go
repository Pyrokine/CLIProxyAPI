package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/google/uuid"
)

func TestOriginalAuthManager_ConcurrentSuccessDoesNotClearActiveCredentialCooldown(t *testing.T) {
	now := time.Now()
	sevenDayReset := now.Add(7 * 24 * time.Hour)

	manager := NewManager(nil, nil, nil)

	baseID := uuid.NewString()
	auth := &Auth{
		ID:       baseID + "-claude-concurrent",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "test-key",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, "claude", []*registry.ModelInfo{
			{ID: "claude-3-5-sonnet-20241022"},
			{ID: "claude-3-opus-20240229"},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// 1. Request A fails with 7d credential-scoped cooldown
	sevenDayDuration := 7 * 24 * time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Provider:   "claude",
			Model:      "claude-3-5-sonnet-20241022",
			Success:    false,
			RetryAfter: &sevenDayDuration,
			Disposition: core.ExecutionDisposition{
				DispatchPhase:  core.DispatchPhaseResponseStarted,
				Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: core.RetryDirectiveRetryAfter,
				EffectScope:    core.StateEffectScopeCredential, EffectKey: auth.ID,
			},
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "7d limit rejected"},
		},
	)

	// 2. An earlier in-flight request on opus returns 200 OK after the 429
	manager.MarkResult(
		context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "claude",
			Model:    "claude-3-opus-20240229",
			Success:  true,
		},
	)

	// 3. The credential MUST still be blocked for all models
	updatedAuth, ok := manager.GetByID(auth.ID)
	if !ok || updatedAuth == nil {
		t.Fatal("auth not found")
	}
	if !updatedAuth.Quota.Exceeded || !updatedAuth.Quota.NextRecoverAt.After(now.Add(6*24*time.Hour)) {
		t.Fatalf("auth quota was cleared or shortened by concurrent success: quota=%+v", updatedAuth.Quota)
	}

	// Selecting any model on this credential must be blocked locally
	for _, m := range []string{"claude-3-5-sonnet-20241022", "claude-3-opus-20240229", "claude-3-7-sonnet-20250219"} {
		blocked, reason, next := isAuthBlockedForModel(updatedAuth, m, time.Now())
		if !blocked {
			t.Fatalf("model %q was unblocked despite active 7d credential cooldown", m)
		}
		if reason != blockReasonCooldown || next.Before(sevenDayReset.Add(-time.Minute)) {
			t.Fatalf("model %q block reason=%v next=%v, want cooldown ~7d", m, reason, next)
		}
	}
}

func TestClaudeOriginalMaterialUpdateKeepsOnlyMatchingCooldown(t *testing.T) {
	for _, key := range []string{"test-key", "test-key-updated"} {
		t.Run(
			key, func(t *testing.T) {
				now := time.Now()
				manager := NewManager(nil, nil, nil)
				auth := &Auth{
					ID: uuid.NewString() + "-claude-update", Provider: "claude",
					Attributes: map[string]string{"api_key": "test-key"},
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-3-5-sonnet-20241022"}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				registered, err := manager.Register(t.Context(), auth)
				if err != nil {
					t.Fatal(err)
				}
				disposition := core.ExecutionDisposition{
					DispatchPhase:  core.DispatchPhaseResponseStarted,
					Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: core.RetryDirectiveRetryAfter, EffectScope: core.StateEffectScopeCredential,
					EffectKey: auth.ID,
				}
				sevenDayDuration := 7 * 24 * time.Hour
				manager.MarkResult(
					t.Context(), Result{
						AuthID: auth.ID, AuthGeneration: registered.Generation, Provider: "claude",
						Model:      "claude-3-5-sonnet-20241022",
						RetryAfter: &sevenDayDuration, Disposition: disposition,
						StateGeneration: stateGenerationForDisposition(
							registered, "claude-3-5-sonnet-20241022", disposition,
						),
						Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "7d limit rejected"},
					},
				)
				updated, err := manager.Update(
					t.Context(),
					&Auth{ID: auth.ID, Provider: "claude", Attributes: map[string]string{"api_key": key}},
				)
				if err != nil || updated == nil {
					t.Fatalf("update auth=%+v error=%v", updated, err)
				}
				blocked, reason, _ := isAuthBlockedForModel(updated, "claude-3-5-sonnet-20241022", time.Now())
				if key == "test-key" {
					if !updated.Quota.Exceeded || updated.Quota.Reason != "quota" ||
						updated.StateEffectScope != core.StateEffectScopeCredential ||
						!updated.Quota.NextRecoverAt.After(now.Add(6*24*time.Hour)) ||
						!blocked ||
						reason != blockReasonCooldown {
						t.Fatalf(
							"matching material lost credential cooldown: auth=%+v blocked=%t reason=%v",
							updated,
							blocked,
							reason,
						)
					}
				} else if updated.Quota.Exceeded ||
					updated.Unavailable ||
					!updated.NextRetryAfter.IsZero() ||
					blocked ||
					updated.Generation <=
						registered.Generation {
					t.Fatalf("replaced credential retained old cooldown: auth=%+v blocked=%t", updated, blocked)
				}
			},
		)
	}
}

func TestOriginalAuthManager_DisableCoolingDoesNotPermanentlyBlock(t *testing.T) {
	SetQuotaCooldownDisabled(true)
	t.Cleanup(func() { SetQuotaCooldownDisabled(false) })

	manager := NewManager(nil, nil, nil)

	baseID := uuid.NewString()
	auth := &Auth{
		ID:       baseID + "-claude-disable-cooling",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "test-key",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, "claude", []*registry.ModelInfo{
			{ID: "claude-3-5-sonnet-20241022"},
			{ID: "claude-3-opus-20240229"},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// 429 arrives while cooling is disabled
	sevenDayDuration := 7 * 24 * time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Provider:   "claude",
			Model:      "claude-3-5-sonnet-20241022",
			Success:    false,
			RetryAfter: &sevenDayDuration,
			Disposition: core.ExecutionDisposition{
				DispatchPhase:  core.DispatchPhaseResponseStarted,
				Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: core.RetryDirectiveRetryAfter,
				EffectScope:    core.StateEffectScopeCredential, EffectKey: auth.ID,
			},
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "7d limit rejected"},
		},
	)

	// Must NOT be blocked when cooling is disabled
	for _, m := range []string{"claude-3-5-sonnet-20241022", "claude-3-opus-20240229"} {
		updatedAuth, _ := manager.GetByID(auth.ID)
		blocked, _, _ := isAuthBlockedForModel(updatedAuth, m, time.Now())
		if blocked {
			t.Fatalf("model %q was blocked even though cooling is disabled", m)
		}
	}
}

func TestOriginalAuthManager_NonClaudeProvider_Model429DoesNotBlockSiblingModels(t *testing.T) {
	manager := NewManager(nil, nil, nil)

	baseID := uuid.NewString()
	auth := &Auth{
		ID:       baseID + "-openai-auth",
		Provider: "openai",
		Attributes: map[string]string{
			"api_key": "test-key",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, "openai", []*registry.ModelInfo{
			{ID: "gpt-4o"},
			{ID: "gpt-4o-mini"},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Regular model 429 on gpt-4o (CredentialScope is false)
	manager.MarkResult(
		context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "openai",
			Model:    "gpt-4o",
			Success:  false,

			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"},
		},
	)

	//goland:noinspection GrazieInspection
	// `gpt-4o` should be blocked
	updatedAuth, _ := manager.GetByID(auth.ID)
	blocked4o, _, _ := isAuthBlockedForModel(updatedAuth, "gpt-4o", time.Now())
	if !blocked4o {
		t.Fatal("gpt-4o should be blocked after 429")
	}

	// gpt-4o-mini MUST remain selectable (unaffected by sibling model 429)
	blockedMini, _, _ := isAuthBlockedForModel(updatedAuth, "gpt-4o-mini", time.Now())
	if blockedMini {
		t.Fatal("gpt-4o-mini was incorrectly blocked by sibling model 429")
	}
}

func TestClaudeOriginalCooldownRestoreChecksCredentialMaterial(t *testing.T) {
	manager := NewManager(nil, nil, nil)

	baseID := uuid.NewString()
	auth := &Auth{
		ID:       baseID + "-persistence-test",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "k",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-3-5-sonnet-20241022"}})
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	futureCooldown := 7 * 24 * time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Provider:   "claude",
			Model:      "claude-3-5-sonnet-20241022",
			Success:    false,
			RetryAfter: &futureCooldown,
			Disposition: core.ExecutionDisposition{
				DispatchPhase:  core.DispatchPhaseResponseStarted,
				Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: core.RetryDirectiveRetryAfter,
				EffectScope:    core.StateEffectScopeCredential, EffectKey: auth.ID,
			},
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "7d rejected"},
		},
	)

	records := manager.cooldownStateRecordsSnapshot()
	if len(records) == 0 {
		t.Fatal("expected cooldown state records to be captured")
	}

	for _, key := range []string{"", "k"} {
		t.Run(
			"restored-key="+key, func(t *testing.T) {
				newManager := NewManager(nil, nil, nil)
				newAuth := &Auth{ID: auth.ID, Provider: "claude"}
				if key != "" {
					newAuth.Attributes = map[string]string{"api_key": key}
				}
				if _, err := newManager.Register(t.Context(), newAuth); err != nil {
					t.Fatal(err)
				}
				newManager.SetCooldownStateStore(&originalClaudeCooldownStateStore{records: records})
				if err := newManager.RestoreCooldownStates(t.Context()); err != nil {
					t.Fatal(err)
				}
				restored, ok := newManager.GetByID(auth.ID)
				if !ok || restored == nil {
					t.Fatal("restored auth not found")
				}
				if key == "k" {
					if !restored.Quota.Exceeded ||
						restored.Quota.NextRecoverAt.Before(time.Now().Add(6*24*time.Hour)) ||
						restored.StateEffectScope != core.StateEffectScopeCredential ||
						restored.StateGeneration != records[0].StateGeneration ||
						restored.ResetAuthority != records[0].ResetAuthority {
						t.Fatalf("matching material lost restored cooldown: %+v", restored)
					}
				} else if restored.Quota.Exceeded || restored.Unavailable || !restored.NextRetryAfter.IsZero() {
					t.Fatalf("old snapshot reached a credential without matching material: %+v", restored)
				}
			},
		)
	}
}

type originalClaudeCooldownStateStore struct {
	records []CooldownStateRecord
}

func (s *originalClaudeCooldownStateStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return s.records, nil
}

func (s *originalClaudeCooldownStateStore) Save(context.Context, []CooldownStateRecord) error {
	return nil
}
