package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type auditPublicationExecutor struct {
	schedulerTestExecutor
	sequence int
}

func (*auditPublicationExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }

func (e *auditPublicationExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.sequence++
	prepared := auth.Clone()
	prepared.Metadata["project_id"] = fmt.Sprintf("synthetic-project-%d", e.sequence)
	return prepared, nil
}

func (e *auditPublicationExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.sequence++
	refreshed := auth.Clone()
	refreshed.Metadata["access_token"] = fmt.Sprintf("synthetic-access-%d", e.sequence)
	return refreshed, nil
}

func TestAuditPublishedAuthSnapshotsKeepOriginalPressure(t *testing.T) {
	for _, operation := range []string{"Register", "Update", "Prepare", "Refresh"} {
		t.Run(
			operation, func(t *testing.T) {
				ctx := WithSkipPersist(t.Context())
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &auditPublicationExecutor{}
				manager.RegisterExecutor(executor)
				auth := &Auth{
					ID: "snapshot-pressure", Provider: "test", Status: StatusActive, Metadata: map[string]any{
						"type": "audit", "access_token": "synthetic-initial-access", "refresh_token": "synthetic-owner",
					},
				}
				for i := range 200 {
					auth.Metadata[fmt.Sprintf("field-%d", i)] = i
				}
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				var workers sync.WaitGroup
				for range 32 {
					workers.Go(
						func() {
							<-start
							for i := range 100 {
								manager.MarkResult(
									ctx, Result{
										AuthID: auth.ID, Provider: auth.Provider, Model: fmt.Sprintf("model-%d", i%8),
										Success: true,
									},
								)
							}
						},
					)
				}
				workers.Go(
					func() {
						<-start
						for i := range 100 {
							base, ok := manager.GetByID(auth.ID)
							if !ok {
								t.Error("missing auth")
								return
							}
							updated := base.Clone()
							updated.Metadata["note"] = fmt.Sprint(i)
							var err error
							switch operation {
							case "Register":
								_, err = manager.Register(ctx, updated)
							case "Update":
								_, err = manager.Update(ctx, updated)
							case "Prepare":
								_, err = manager.PrepareRequestAuth(ctx, executor, base)
							case "Refresh":
								_, err = manager.ForceRefreshAuth(ctx, base.ID)
							}
							if err != nil {
								t.Error(err)
								return
							}
						}
					},
				)
				close(start)
				workers.Wait()
				current, _ := manager.GetByID(auth.ID)
				if current.Metadata["field-199"] != 199 ||
					((operation == "Prepare" || operation == "Refresh") && executor.sequence != 100) {
					t.Fatalf(
						"actual publication path was not exercised: sequence=%d metadata=%#v", executor.sequence,
						current.Metadata,
					)
				}
			},
		)
	}
}
