package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditPersistentMutationRejectsReplacedGenerationBeforeFileCallback(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		manager := NewManager(nil, nil, nil)
		original, err := manager.Register(
			t.Context(), &Auth{ID: "synthetic", Provider: "codex", Metadata: map[string]any{"access_token": "old"}},
		)
		if err != nil {
			t.Fatal(err)
		}
		next := original.Clone()
		next.Metadata["access_token"] = "new"
		if recreate {
			if err := manager.Remove(WithRuntimeOnlyRemove(WithSkipPersist(t.Context())), original.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Register(WithAllowAuthRecreate(t.Context()), next); err != nil {
				t.Fatal(err)
			}
		} else if _, err := manager.Update(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "synthetic.json")
		if err := os.WriteFile(path, []byte(`{"access_token":"new"}`), 0600); err != nil {
			t.Fatal(err)
		}
		called := false
		updated, err := manager.MutatePersistentAuth(
			WithExpectedAuthGeneration(t.Context(), original.Generation), original.ID,
			func(context.Context) (*Auth, error) {
				called = true
				if err := os.WriteFile(path, []byte(`{"access_token":"old"}`), 0600); err != nil {
					return nil, err
				}
				return original, nil
			},
		)
		if updated != nil || !errors.Is(err, ErrAuthGenerationConflict) || called {
			t.Fatalf("updated=%v error=%v called=%t", updated, err, called)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != `{"access_token":"new"}` {
			t.Fatalf("file=%s error=%v", raw, err)
		}
		current, _ := manager.GetByID(original.ID)
		if current.Metadata["access_token"] != "new" {
			t.Fatalf("new owner overwritten: %+v", current)
		}
		removed, err := manager.RemoveIfGeneration(
			WithRuntimeOnlyRemove(WithSkipPersist(t.Context())), original.ID, original.Generation,
		)
		if removed || err != nil {
			t.Fatalf("stale cleanup removed=%t error=%v", removed, err)
		}
	}
}

func TestAuditPersistentMutationCurrentGenerationAndUnboundCalls(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	original, err := manager.MutatePersistentAuth(
		t.Context(), "synthetic", func(context.Context) (*Auth, error) {
			return &Auth{ID: "synthetic", Provider: "codex", Metadata: map[string]any{"access_token": "current"}}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	updated, err := manager.MutatePersistentAuth(
		WithExpectedAuthGeneration(t.Context(), original.Generation), original.ID,
		func(context.Context) (*Auth, error) {
			calls++
			next := original.Clone()
			next.Metadata["access_token"] = "rollback"
			return next, nil
		},
	)
	if err != nil || calls != 1 || updated == nil || updated.Generation == original.Generation ||
		updated.Metadata["access_token"] != "rollback" {
		t.Fatalf("updated=%+v calls=%d error=%v", updated, calls, err)
	}
	called := false
	_, err = manager.MutatePersistentAuth(
		WithExpectedAuthGeneration(t.Context(), 0), original.ID,
		func(context.Context) (*Auth, error) { called = true; return original, nil },
	)
	if !errors.Is(err, ErrAuthGenerationConflict) || called {
		t.Fatalf("expected-absent mutation error=%v called=%t", err, called)
	}
}

func TestAuditRemoveIfGenerationsProtectsAllIdentitiesBeforeDeletion(t *testing.T) {
	for _, mode := range []string{"success", "generation_conflict", "delete_error", "delete_mismatch"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				expected := make(map[string]uint64)
				for _, id := range []string{"synthetic-b", "synthetic-a"} {
					auth, err := manager.Register(WithSkipPersist(t.Context()), &Auth{ID: id, Provider: "claude"})
					if err != nil {
						t.Fatal(err)
					}
					expected[id] = auth.Generation
				}
				if mode == "generation_conflict" {
					current, _ := manager.GetByID("synthetic-b")
					if _, err := manager.Update(WithSkipPersist(t.Context()), current); err != nil {
						t.Fatal(err)
					}
				}
				before := make(map[string]uint64)
				for id := range expected {
					current, _ := manager.GetByID(id)
					before[id] = current.Generation
				}
				calls := 0
				syntheticError := errors.New("synthetic conditional delete failure")
				removed, err := manager.RemoveIfGenerations(
					t.Context(), expected, func(ctx context.Context) (bool, error) {
						calls++
						for id := range expected {
							current, _ := manager.GetByID(id)
							blockedCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
							_, err := manager.Update(WithSkipPersist(blockedCtx), current)
							cancel()
							if !errors.Is(err, context.DeadlineExceeded) {
								t.Fatalf("identity %s was not protected during deletion: %v", id, err)
							}
						}
						if mode == "delete_error" {
							return false, syntheticError
						}
						return mode != "delete_mismatch", nil
					},
				)
				if (mode == "delete_error") != errors.Is(err, syntheticError) {
					t.Fatalf("unexpected delete error: %v", err)
				}
				wantCalls := 1
				if mode == "generation_conflict" {
					wantCalls = 0
				}
				if calls != wantCalls || removed != (mode == "success") {
					t.Fatalf("removed=%t calls=%d error=%v", removed, calls, err)
				}
				for id := range expected {
					current, exists := manager.GetByID(id)
					if exists != (mode != "success") || (exists && current.Generation != before[id]) {
						t.Fatalf("identity %s changed on failed cleanup: %+v", id, current)
					}
					if mode == "success" && (manager.authRemovalEpoch(id) == 0 || manager.removedAuths[id] == 0) {
						t.Fatalf("identity %s has no removal epoch or tombstone", id)
					}
				}
			},
		)
	}
}

func TestAuditRemoveIfGenerationsDistinctIDsShareMutationLock(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	expected := make(map[string]uint64)
	for _, id := range []string{"synthetic-alias", " synthetic-alias"} {
		auth, err := manager.Register(WithSkipPersist(t.Context()), &Auth{ID: id, Provider: "claude"})
		if err != nil || auth == nil {
			t.Fatalf("registration rejected %q: %v", id, err)
		}
		expected[auth.ID] = auth.Generation
	}
	if len(manager.List()) != 2 {
		t.Fatal("Register did not preserve distinct raw IDs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	removed, err := manager.RemoveIfGenerations(
		ctx, expected, func(context.Context) (bool, error) {
			calls++
			return true, nil
		},
	)
	if err != nil || !removed || calls != 1 || len(manager.List()) != 0 {
		t.Fatalf("removed=%t error=%v callbacks=%d remaining=%d", removed, err, calls, len(manager.List()))
	}
}

func TestAuditRemoveIfGenerationsExcludesAliasPublication(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "existing_runtime", true: "empty_runtime"}[empty], func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				expected := make(map[string]uint64)
				if !empty {
					auth, err := manager.Register(
						WithSkipPersist(t.Context()), &Auth{ID: "synthetic-owner", Provider: "claude"},
					)
					if err != nil {
						t.Fatal(err)
					}
					expected[auth.ID] = auth.Generation
				}
				calls := 0
				removed, err := manager.RemoveIfGenerations(
					t.Context(), expected, func(ctx context.Context) (bool, error) {
						calls++
						blockedCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
						defer cancel()
						alias, err := manager.Register(
							WithSkipPersist(blockedCtx), &Auth{ID: "synthetic-late-alias", Provider: "claude"},
						)
						if !errors.Is(err, context.DeadlineExceeded) || alias != nil {
							t.Fatalf("alias publication was not excluded: alias=%+v error=%v", alias, err)
						}
						return true, nil
					},
				)
				if err != nil || !removed || calls != 1 || len(manager.List()) != 0 {
					t.Fatalf("removed=%t error=%v callbacks=%d remaining=%d", removed, err, calls, len(manager.List()))
				}
			},
		)
	}
}
