package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestPublishedAuthSnapshotRace(t *testing.T) {
	for _, operation := range []string{"Register", "Update", "MergePersistentUpdate", "MutatePersistentAuth", "RefreshPublication"} {
		t.Run(operation, func(t *testing.T) {
			ctx := WithSkipPersist(context.Background())
			manager := NewManager(nil, nil, nil)
			executor := schedulerTestExecutor{}
			manager.RegisterExecutor(executor)
			registered, err := manager.Register(ctx, &Auth{
				ID: "publication-race", Provider: executor.Identifier(), Status: StatusActive,
				Metadata: map[string]any{"access_token": "test"},
			})
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range 16 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					for i := range 100 {
						manager.MarkResult(ctx, Result{AuthID: registered.ID, Provider: registered.Provider,
							Model: fmt.Sprintf("model-%d", i%8), Success: true})
					}
				}()
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for i := range 100 {
					current, _ := manager.GetByID(registered.ID)
					updated := current.Clone()
					updated.Metadata["note"] = i
					var updateErr error
					switch operation {
					case "Register":
						_, updateErr = manager.Register(ctx, updated)
					case "Update":
						_, updateErr = manager.Update(ctx, updated)
					case "MergePersistentUpdate":
						_, updateErr = manager.MergePersistentUpdate(ctx, current, updated)
					case "MutatePersistentAuth":
						_, updateErr = manager.MutatePersistentAuth(ctx, registered.ID, func(context.Context) (*Auth, error) {
							return updated, nil
						})
					case "RefreshPublication":
						if published, _ := manager.publishRefreshedAuthStates(current, updated, current, updated, executor, nil); published == nil {
							updateErr = fmt.Errorf("refresh publication was rejected")
						}
					}
					if updateErr != nil {
						t.Errorf("%s: %v", operation, updateErr)
						return
					}
				}
			}()
			close(start)
			workers.Wait()
		})
	}
}
