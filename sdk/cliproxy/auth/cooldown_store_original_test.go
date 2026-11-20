package auth

import (
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalCooldownStoreUnknownFailureRemainsNeutral(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	credential, err := manager.Register(t.Context(), &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []*Error{nil, {Message: "upstream unavailable", HTTPStatus: 500}, nil, nil} {
		current, _ := manager.GetByID(credential.ID)
		manager.MarkResult(
			t.Context(),
			executionResult(current, "xai", "grok-4", failure, core.DispatchPhaseDownstreamCommitted),
		)
		current, _ = manager.GetByID(credential.ID)
		if current.Unavailable || !current.NextRetryAfter.IsZero() || len(
			CooldownSnapshotForAuth(
				current, time.Now(),
			),
		) != 0 ||
			store.saveCount.Load() != 0 || len(store.records) != 0 {
			t.Fatalf(
				"unknown failure created persistent cooling: credential=%+v saves=%d records=%+v", current,
				store.saveCount.Load(), store.records,
			)
		}
	}
}

func TestOriginalCooldownStoreCredentialChangeInputs(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	manager.SetCooldownStateStore(store)
	manager.RegisterExecutor(&roundExecutor{provider: "codex"})
	credential := &Auth{
		ID: "auth-codex-1", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"access_token": "token-1"},
	}
	const model = "gpt-6-astra"
	registerRequestScopedTestAuth(t, manager, credential, model)
	current, _ := manager.GetByID(credential.ID)
	manager.MarkResult(
		t.Context(), executionResult(
			current, "codex", model,
			&Error{Message: "invalidated token", HTTPStatus: 401}, core.DispatchPhaseDownstreamCommitted,
		),
	)
	if len(store.records) == 0 {
		t.Fatal("original unauthorized result did not persist its restriction")
	}
	for _, changed := range []bool{false, true} {
		metadata := map[string]any{"access_token": "token-1", "note": "updated note"}
		if changed {
			metadata = map[string]any{"access_token": "token-2"}
		}
		if _, err := manager.Update(
			WithSkipPersist(t.Context()),
			&Auth{ID: credential.ID, Provider: "codex", Status: StatusActive, Metadata: metadata},
		); err != nil {
			t.Fatal(err)
		}
		current, _ = manager.GetByID(credential.ID)
		selected, err := manager.SelectAuth(t.Context(), "codex", model, core.Options{})
		if changed {
			if current.Unavailable || len(store.records) != 0 || err != nil || selected == nil ||
				selected.ID != credential.ID {
				t.Fatalf(
					"new material retained old rejection: credential=%+v records=%+v selected=%+v error=%v",
					current,
					store.records,
					selected,
					err,
				)
			}
		} else if !current.Unavailable || len(store.records) == 0 || selected != nil || err == nil {
			t.Fatalf(
				"metadata-only update lost rejection: credential=%+v records=%+v selected=%+v error=%v", current,
				store.records, selected, err,
			)
		}
	}
}

func TestOriginalCooldownStoreThinkingRestoreInputs(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "high-first", true: "low-first"}[reversed], func(t *testing.T) {
				now := time.Now().UTC().Truncate(time.Second)
				const model = "gemini-3.1-pro-preview"
				later := now.Add(2 * time.Hour)
				records := []CooldownStateRecord{
					{
						Provider: "gemini", AuthID: "auth-thinking", Model: model + "(high)",
						NextRetryAfter: now.Add(time.Hour), UpdatedAt: now,
						Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
					},
					{
						Provider: "gemini", AuthID: "auth-thinking", Model: model + "(low)", NextRetryAfter: later,
						UpdatedAt: now.Add(time.Minute),
						Quota:     QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: later},
					},
				}
				if reversed {
					records[0], records[1] = records[1], records[0]
				}
				store := &recordingCooldownStateStore{load: records}
				manager := NewManager(nil, nil, nil)
				manager.SetCooldownStateStore(store)
				if _, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{ID: "auth-thinking", Provider: "gemini"},
				); err != nil {
					t.Fatal(err)
				}
				if err := manager.RestoreCooldownStates(t.Context()); err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID("auth-thinking")
				state := current.ModelStates[model]
				if len(current.ModelStates) !=
					1 ||
					state ==
						nil ||
					!state.Unavailable ||
					!state.NextRetryAfter.Equal(later) ||
					state.EffectScope != core.StateEffectScopeExactModel ||
					state.EffectKey != model {
					t.Fatalf("original thinking records were not normalized: %+v", current)
				}
				var persisted []CooldownStateRecord
				for _, record := range store.records {
					if record.Model != "" {
						persisted = append(persisted, record)
					}
				}
				if len(persisted) != 1 || persisted[0].Model != model ||
					!persisted[0].NextRetryAfter.Equal(later) {
					t.Fatalf("original normalized model record=%+v", persisted)
				}
			},
		)
	}
}
