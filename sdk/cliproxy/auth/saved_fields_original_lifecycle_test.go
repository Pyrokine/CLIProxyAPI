package auth

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestManagerSavedFieldsConcurrentLifecycle(t *testing.T) {
	for _, saveErr := range []error{nil, errors.New("save failed after enrichment")} {
		name := "success"
		if saveErr != nil {
			name = "failure"
		}
		for _, operation := range []string{"Register", "Update"} {
			for _, concurrent := range []string{"MarkResult", "Update", "Remove", "ReRegister"} {
				t.Run(
					name+"/"+operation+"/"+concurrent, func(t *testing.T) {
						synctest.Test(
							t, func(t *testing.T) {
								ctx := t.Context()
								store := &blockingEnrichingAuthStore{
									err:     saveErr,
									entered: make(chan struct{}), release: make(chan struct{}),
								}
								var releaseOnce sync.Once
								release := func() { releaseOnce.Do(func() { close(store.release) }) }
								defer release()
								manager := NewManager(store, nil, nil)
								input := &Auth{
									ID: "saved-fields", Provider: "audit", Status: StatusActive,
									Label: "old", Metadata: map[string]any{"type": "audit"},
								}
								if operation == "Update" {
									var errRegister error
									input, errRegister = manager.Register(WithSkipPersist(ctx), input)
									if errRegister != nil {
										t.Fatal(errRegister)
									}
								}
								next := input.Clone()
								next.Label = "new"
								done := make(chan error, 1)
								go func() {
									var err error
									if operation == "Register" {
										_, err = manager.Register(ctx, input)
									} else {
										_, err = manager.Update(ctx, input)
									}
									done <- err
								}()
								<-store.entered
								started, concurrentDone := make(chan struct{}), make(chan error, 1)
								go func() {
									close(started)
									var err error
									switch concurrent {
									case "MarkResult":
										manager.MarkResult(
											ctx, Result{
												AuthID: next.ID, Provider: next.Provider, Model: "model",
												Error: &Error{HTTPStatus: 429, Message: "quota exceeded"},
											},
										)
									case "Update":
										_, err = manager.Update(ctx, next)
									case "Remove":
										err = manager.Remove(ctx, next.ID)
									case "ReRegister":
										if err = manager.Remove(ctx, next.ID); err == nil {
											_, err = manager.Register(WithAllowAuthRecreate(ctx), next)
										}
									}
									concurrentDone <- err
								}()
								<-started
								synctest.Wait()
								select {
								case <-concurrentDone:
									t.Fatal("same-credential mutation completed before the active save")
								default:
								}
								release()
								firstErr, concurrentErr := <-done, <-concurrentDone
								removalCancelledSave := (concurrent == "Remove" || concurrent == "ReRegister") &&
									errors.Is(firstErr, ErrAuthDurableStateUncertain)
								if !errors.Is(firstErr, saveErr) && !removalCancelledSave {
									t.Fatalf("first save error = %v, want %v", firstErr, saveErr)
								}
								if concurrentErr != nil && (saveErr == nil || !errors.Is(concurrentErr, saveErr)) {
									t.Fatalf("concurrent mutation error: %v", concurrentErr)
								}
								published, ok := manager.GetByID(input.ID)
								manager.scheduler.mu.Lock()
								var scheduled *Auth
								if provider := manager.scheduler.providers["audit"]; provider != nil {
									if meta := provider.auths[input.ID]; meta != nil {
										scheduled = meta.auth.Clone()
									}
								}
								manager.scheduler.mu.Unlock()
								removed := concurrent == "Remove" || saveErr != nil &&
									(operation == "Register" || concurrent == "ReRegister")
								if removed {
									if ok || scheduled != nil {
										t.Fatal("save published a failed or removed credential")
									}
									return
								}
								if !ok {
									t.Fatal("credential disappeared after serialized save")
								}
								if saveErr == nil {
									if published.FileName != "saved-fields.json" ||
										published.Attributes[AttributePath] != "/fixture/saved-fields.json" {
										t.Fatalf("missing saved fields: %+v", published)
									}
								} else if published.FileName != input.FileName || !reflect.DeepEqual(
									published.Attributes, input.Attributes,
								) {
									t.Fatal("failed save published enrichment")
								}
								if concurrent == "MarkResult" {
									state := published.ModelStates["model"]
									if published.Failed != 1 || published.LastError == nil || state == nil ||
										!state.Quota.Exceeded || !state.Unavailable {
										t.Fatalf("save overwrote concurrent result: %+v", published)
									}
								} else if saveErr == nil && published.Label != "new" {
									t.Fatalf("save overwrote newer lifecycle state: %+v", published)
								}
								if !reflect.DeepEqual(authUpdateSnapshot(scheduled), published) {
									t.Fatal("scheduler retained an obsolete save snapshot")
								}
								if saveErr == nil {
									store.mu.Lock()
									saved := store.saved[len(store.saved)-1].Clone()
									store.mu.Unlock()
									if !persistentAuthStateMatches(
										saved, published,
									) || saved.Failed != published.Failed ||
										!reflect.DeepEqual(saved.ModelStates, published.ModelStates) {
										t.Fatal("store retained an obsolete credential or result state")
									}
								}
								if concurrent == "ReRegister" {
									if published.registrationGeneration <= input.registrationGeneration {
										t.Fatal("re-registration did not advance generation")
									}
									called := false
									stale, errUpdate := manager.MutatePersistentAuth(
										WithExpectedAuthGeneration(ctx, input.Generation), input.ID,
										func(_ context.Context) (*Auth, error) {
											called = true
											return input.Clone(), nil
										},
									)
									if stale != nil || !errors.Is(errUpdate, ErrAuthGenerationConflict) || called {
										t.Fatalf("stale registration reached mutation=%t error=%v", called, errUpdate)
									}
								}
							},
						)
					},
				)
			}
		}
	}
}

func TestManagerSavePreservesConcurrentRefreshState(t *testing.T) {
	for _, operation := range []string{"Update", "MarkResult"} {
		t.Run(
			operation, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						ctx := t.Context()
						store := &blockingEnrichingAuthStore{entered: make(chan struct{}), release: make(chan struct{})}
						manager := NewManager(store, nil, nil)
						base, err := manager.Register(
							WithSkipPersist(ctx), &Auth{
								ID: "runtime", Provider: "audit",
								Status: StatusActive, Metadata: map[string]any{"type": "audit"},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						done := make(chan error, 1)
						go func() {
							if operation == "Update" {
								_, errUpdate := manager.Update(ctx, base.Clone())
								done <- errUpdate
							} else {
								manager.MarkResult(ctx, Result{AuthID: base.ID, Provider: base.Provider, Success: true})
								done <- nil
							}
						}()
						<-store.entered
						job := manager.markRefreshPending(base.ID, base.Generation, time.Now())
						close(store.release)
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						got, _ := manager.GetByID(base.ID)
						if job == nil || !got.NextRefreshAfter.Equal(job.pendingUntil) {
							t.Fatal("save lost concurrent refresh scheduling")
						}
						if got.Attributes[AttributePath] == "" {
							t.Fatal("save enrichment lost")
						}
						if operation == "MarkResult" && got.Success != 1 {
							t.Fatal("save lost result counter")
						}
					},
				)
			},
		)
	}
}
