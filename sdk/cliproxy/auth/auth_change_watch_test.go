package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAuthChangeWatchNativePublications(t *testing.T) {
	for _, mutation := range []string{"register", "update", "merge", "mutate", "prepare", "refresh", "remove", "conditional-remove", "reload"} {
		t.Run(mutation, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			executor := &requestRefreshEvaluatorExecutor{}
			manager.RegisterExecutor(executor)
			auth := &Auth{
				ID: "watch-" + mutation, Provider: executor.Identifier(), Status: StatusActive,
				Metadata: map[string]any{"access_token": "old", "refresh_token": "synthetic-refresh"},
			}
			if mutation != "register" {
				registered, err := manager.Register(t.Context(), auth)
				if err != nil {
					t.Fatal(err)
				}
				auth = registered
			}
			changes, unsubscribe := manager.WatchAuthChanges(auth.ID)
			defer unsubscribe()
			other, unsubscribeOther := manager.WatchAuthChanges("other-credential")
			defer unsubscribeOther()

			var err error
			switch mutation {
			case "register":
				_, err = manager.Register(t.Context(), auth)
			case "update":
				_, err = manager.Update(t.Context(), auth)
			case "merge":
				_, err = manager.MergePersistentUpdate(t.Context(), auth, auth.Clone())
			case "mutate":
				_, err = manager.MutatePersistentAuth(t.Context(), auth.ID, func(context.Context) (*Auth, error) {
					return auth.Clone(), nil
				})
			case "prepare":
				_, err = manager.prepareRequestAuth(t.Context(), executor, auth)
			case "refresh":
				_, err = manager.ForceRefreshAuth(t.Context(), auth.ID)
			case "remove":
				err = manager.Remove(WithSkipPersist(t.Context()), auth.ID)
			case "conditional-remove":
				var removed bool
				removed, err = manager.RemoveIfGeneration(WithRuntimeOnlyRemove(t.Context()), auth.ID, auth.Generation)
				if !removed && err == nil {
					t.Fatal("conditional removal was not published")
				}
			case "reload":
				manager.SetStore(&countingStore{})
				err = manager.Load(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-changes:
			default:
				t.Fatal("native publication did not notify its subscriber")
			}
			if mutation != "reload" {
				select {
				case <-other:
					t.Fatal("publication notified another credential")
				default:
				}
			}
		})
	}
}

func TestAuthChangeWatchCoalescesAndUnsubscribes(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	changes, unsubscribe := manager.WatchAuthChanges("coalesced")
	for range 3 {
		if _, err := manager.Update(t.Context(), &Auth{ID: "coalesced", Provider: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(changes) != 1 {
		t.Fatalf("buffered notifications = %d, want 1", len(changes))
	}
	unsubscribe()
	unsubscribe()
	for range changes {
	}
	manager.mu.RLock()
	remaining := len(manager.authChangeWatchers)
	manager.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("subscriptions remaining = %d", remaining)
	}
}

func TestAuthChangeWatchIgnoresRequestAccountingAndFailedPersistence(t *testing.T) {
	store := &countingStore{saveErr: errors.New("synthetic persistence failure")}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(WithSkipPersist(t.Context()), &Auth{
		ID: "accounting", Provider: "fixture", Status: StatusActive,
		Metadata: map[string]any{"access_token": "synthetic-access-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, unsubscribe := manager.WatchAuthChanges(auth.ID)
	defer unsubscribe()
	manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: "fixture-model", Success: true})
	if _, err = manager.Update(t.Context(), auth); !errors.Is(err, store.saveErr) {
		t.Fatalf("Update error = %v, want persistence failure", err)
	}
	select {
	case <-changes:
		t.Fatal("request accounting or failed persistence notified a structural change")
	default:
	}
}

func TestAuthChangeWatchRefreshNotifiesOnlyAfterAcceptedCommit(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &requestRefreshEvaluatorExecutor{}
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(t.Context(), &Auth{
		ID: "guarded-watch", Provider: executor.Identifier(), Metadata: map[string]any{"access_token": "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, unsubscribe := manager.WatchAuthChanges(auth.ID)
	defer unsubscribe()
	next := auth.Clone()
	next.Metadata["access_token"] = "new"
	if saved, _ := manager.publishRefreshedAuthStates(auth, next, nil, nil, executor, rejectingAfterCommitGuard{}); saved != nil {
		t.Fatal("rejected refresh was published")
	}
	select {
	case <-changes:
		t.Fatal("rolled back refresh notified a structural change")
	default:
	}
	if saved, _ := manager.publishRefreshedAuthStates(auth, next, nil, nil, executor, nil); saved == nil {
		t.Fatal("accepted refresh was not published")
	}
	select {
	case <-changes:
	default:
		t.Fatal("accepted refresh did not notify its subscriber")
	}
}

func TestAuthChangeWatchConcurrentUnsubscribe(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			changes, unsubscribe := manager.WatchAuthChanges("shared")
			manager.mu.Lock()
			manager.notifyAuthChangeLocked("shared")
			manager.mu.Unlock()
			unsubscribe()
			unsubscribe()
			for range changes {
			}
		})
	}
	workers.Wait()
	if len(manager.authChangeWatchers) != 0 {
		t.Fatal("concurrent unsubscribe left subscriptions behind")
	}
}

func TestAuthChangeWatchInvalidSubscriptionIsClosed(t *testing.T) {
	for _, manager := range []*Manager{nil, NewManager(nil, nil, nil)} {
		changes, unsubscribe := manager.WatchAuthChanges("")
		unsubscribe()
		unsubscribe()
		select {
		case _, open := <-changes:
			if open {
				t.Fatal("invalid subscription was not closed")
			}
		default:
			t.Fatal("invalid subscription remained active")
		}
	}
}
