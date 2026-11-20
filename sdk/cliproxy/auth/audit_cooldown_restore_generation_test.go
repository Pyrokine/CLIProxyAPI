package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

type auditBlockedCooldownLoad struct {
	*recordingCooldownStateStore
	entered chan struct{}
	release chan struct{}
}

func (s *auditBlockedCooldownLoad) Load(ctx context.Context) ([]CooldownStateRecord, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.recordingCooldownStateStore.Load(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAuditCooldownRestorePreservesConcurrentSuccess(t *testing.T) {
	for _, successDuringLoad := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "unchanged", true: "success-during-load"}[successDuringLoad],
			func(t *testing.T) {
				store := &auditBlockedCooldownLoad{
					recordingCooldownStateStore: &recordingCooldownStateStore{}, entered: make(chan struct{}),
					release: make(chan struct{}),
				}
				manager := NewManager(nil, nil, nil)
				manager.SetCooldownStateStore(store)
				credential := registerCooldownTestAuth(t, manager)
				const model = "synthetic-cooldown-model"
				markQuotaCooldown(manager, credential, model)
				current, _ := manager.GetByID(credential.ID)
				generation := current.ModelStates[model].Generation
				for _, record := range store.records {
					if record.Model == model {
						store.load = append(store.load, record)
					}
				}
				if len(store.load) != 1 {
					t.Fatalf("model cooldown records=%+v", store.records)
				}
				var once sync.Once
				release := func() { once.Do(func() { close(store.release) }) }
				restoreDone := make(chan struct{})
				resultDone := make(chan struct{})
				var restoreErr error
				go func() { defer close(restoreDone); restoreErr = manager.RestoreCooldownStates(t.Context()) }()
				t.Cleanup(
					func() {
						release()
						select {
						case <-restoreDone:
						case <-time.After(3 * time.Second):
							t.Error("restore remained blocked")
						}
						select {
						case <-resultDone:
						case <-time.After(3 * time.Second):
							t.Error("success result remained blocked")
						}
					},
				)
				select {
				case <-store.entered:
				case <-time.After(time.Second):
					close(resultDone)
					t.Fatal("Load did not start")
				}
				if successDuringLoad {
					go func() {
						defer close(resultDone)
						manager.MarkResult(
							t.Context(), Result{
								AuthID: credential.ID, AuthGeneration: current.Generation,
								Provider: credential.Provider, Model: model,
								ModelQuotaEpoch: current.ModelStates[model].QuotaEpoch, Success: true,
							},
						)
					}()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					deadline := time.After(2 * time.Second)
				waitForSuccess:
					for {
						select {
						case <-ticker.C:
							next, _ := manager.GetByID(credential.ID)
							if state := next.ModelStates[model]; state !=
								nil &&
								!state.Unavailable &&
								state.Generation >
									generation {
								break waitForSuccess
							}
						case <-deadline:
							t.Fatal("success did not clear the model cooldown")
						}
					}
				} else {
					close(resultDone)
				}
				release()
				select {
				case <-restoreDone:
				case <-time.After(3 * time.Second):
					t.Fatal("restore did not finish")
				}
				select {
				case <-resultDone:
				case <-time.After(3 * time.Second):
					t.Fatal("result persistence did not finish")
				}
				if restoreErr != nil {
					t.Fatal(restoreErr)
				}
				next, _ := manager.GetByID(credential.ID)
				state := next.ModelStates[model]
				if state.Unavailable == successDuringLoad || state.Quota.Exceeded == successDuringLoad {
					t.Fatalf("concurrent-success=%t state=%+v", successDuringLoad, state)
				}
				if successDuringLoad && len(store.records) != 0 {
					t.Fatalf("obsolete cooldown persisted: %+v", store.records)
				}
			},
		)
	}
}
