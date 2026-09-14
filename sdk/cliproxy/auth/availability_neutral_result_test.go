package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type contextRecordingAuthStore struct {
	mu       sync.Mutex
	saves    int
	contexts []error
}

func (*contextRecordingAuthStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *contextRecordingAuthStore) Save(ctx context.Context, _ *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.contexts = append(s.contexts, ctx.Err())
	return "saved", nil
}

func (*contextRecordingAuthStore) Delete(context.Context, string) error {
	return nil
}

func (s *contextRecordingAuthStore) snapshot() (int, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, append([]error(nil), s.contexts...)
}

type blockingUpdateAuthStore struct {
	mu        sync.Mutex
	blockNext bool
	entered   chan struct{}
	release   chan struct{}
	saved     []*Auth
}

func (*blockingUpdateAuthStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *blockingUpdateAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	block := s.blockNext
	if block {
		s.blockNext = false
	}
	s.mu.Unlock()
	if block {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	s.saved = append(s.saved, auth.Clone())
	s.mu.Unlock()
	return "saved", nil
}

func (*blockingUpdateAuthStore) Delete(context.Context, string) error {
	return nil
}

func (s *blockingUpdateAuthStore) snapshot() []*Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Auth, 0, len(s.saved))
	for _, auth := range s.saved {
		out = append(out, auth.Clone())
	}
	return out
}

func TestManagerAvailabilityNeutralResultPersistsStatisticsAfterRequestCancellation(t *testing.T) {
	store := &contextRecordingAuthStore{}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       "neutral-canceled-result",
			Provider: "provider-a",
			Status:   StatusActive,
			Metadata: map[string]any{"access_token": "token-a"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.MarkResult(
		ctx, Result{
			AuthID:         registered.ID,
			AuthGeneration: registered.Generation,
			Provider:       registered.Provider,
			Model:          "model-a",
			Error:          &Error{Message: "client canceled"},
			Disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
			},
		},
	)

	current := getExecutionOutcomeTestAuth(t, manager, registered.ID)
	if current.Failed != 1 || current.Unavailable || current.LastError != nil {
		t.Fatalf("availability-neutral canceled result state = %+v", current)
	}
	saves, contexts := store.snapshot()
	if saves != 1 || len(contexts) != 1 || contexts[0] != nil {
		t.Fatalf("persistence calls = %d contexts=%v, want one non-canceled context", saves, contexts)
	}
}

func TestManagerAvailabilityNeutralResultCannotOverwriteNewCredentialGeneration(t *testing.T) {
	const authID = "neutral-generation-order"
	store := &blockingUpdateAuthStore{
		blockNext: true,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID:       authID,
			Provider: "provider-a",
			Status:   StatusActive,
			Metadata: map[string]any{"access_token": "old-token"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated := registered.Clone()
	updated.Metadata["access_token"] = "new-token"
	updateDone := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), updated)
		updateDone <- errUpdate
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("Update() did not enter persistence")
	}

	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(
			context.Background(), Result{
				AuthID:         authID,
				AuthGeneration: registered.Generation,
				Provider:       registered.Provider,
				Model:          "model-a",
				Error:          &Error{Message: "client canceled"},
				Disposition: cliproxyexecutor.ExecutionDisposition{
					DispatchPhase:  cliproxyexecutor.DispatchPhaseMaybeSent,
					Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
					RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
					EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
				},
			},
		)
		close(resultDone)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		current, _ := manager.GetByID(authID)
		manager.authMutationLocksMu.Lock()
		lock := manager.authMutationLocks[authID]
		waitingOnMutation := lock != nil && lock.refs >= 2
		manager.authMutationLocksMu.Unlock()
		if current != nil && current.Failed > 0 || waitingOnMutation {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MarkResult() did not reach credential ordering barrier")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.release)

	select {
	case errUpdate := <-updateDone:
		if errUpdate != nil {
			t.Fatalf("Update() error = %v", errUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("Update() did not finish")
	}
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult() did not finish")
	}

	current := getExecutionOutcomeTestAuth(t, manager, authID)
	if got := current.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("runtime access token = %#v, want new-token", got)
	}
	saved := store.snapshot()
	if len(saved) != 1 {
		t.Fatalf("persisted auth count = %d, want only the credential update", len(saved))
	}
	if got := saved[0].Metadata["access_token"]; got != "new-token" {
		t.Fatalf("persisted access token = %#v, want new-token", got)
	}
}
