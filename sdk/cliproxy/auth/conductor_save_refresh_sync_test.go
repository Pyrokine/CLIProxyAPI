package auth

import (
	"context"
	"testing"
	"time"
)

func TestBlockedSavePreservesConcurrentRefreshPending(t *testing.T) {
	for _, operation := range []string{"Update", "MergePersistentUpdate", "MutatePersistentAuth", "MarkResult"} {
		t.Run(operation, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			registered, err := manager.Register(context.Background(), &Auth{
				ID: "concurrent-refresh", Provider: "fixture", Status: StatusActive,
				Metadata: map[string]any{"access_token": "fixture-token"},
			})
			if err != nil {
				t.Fatal(err)
			}
			store := &blockingAuthStore{
				block: true, entered: make(chan struct{}, 1), release: make(chan struct{}),
			}
			manager.SetStore(store)
			defer close(store.release)
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "Update":
					_, errUpdate := manager.Update(context.Background(), registered.Clone())
					done <- errUpdate
				case "MergePersistentUpdate":
					_, errUpdate := manager.MergePersistentUpdate(context.Background(), registered, registered.Clone())
					done <- errUpdate
				case "MutatePersistentAuth":
					_, errUpdate := manager.MutatePersistentAuth(context.Background(), registered.ID, func(ctx context.Context) (*Auth, error) {
						record := registered.Clone()
						_, errSave := store.Save(ctx, record)
						return record, errSave
					})
					done <- errUpdate
				default:
					manager.MarkResult(context.Background(), Result{
						AuthID: registered.ID, AuthGeneration: registered.Generation,
						Provider: registered.Provider, Model: "fixture-model", Success: true,
					})
					done <- nil
				}
			}()
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("save did not start")
			}
			job := manager.markRefreshPending(registered.ID, registered.Generation, time.Now())
			if job == nil {
				t.Fatal("concurrent refresh was not scheduled")
			}
			store.release <- struct{}{}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			current, ok := manager.GetByID(registered.ID)
			if !ok || current == nil || !current.NextRefreshAfter.Equal(job.pendingUntil) {
				t.Fatalf("save lost concurrent refresh scheduling: %#v", current)
			}
		})
	}
}
