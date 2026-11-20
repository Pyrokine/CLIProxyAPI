package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
)

type savedFieldsHook struct{ auth *Auth }

func (h *savedFieldsHook) OnAuthRegistered(_ context.Context, auth *Auth) {
	h.auth = authUpdateSnapshot(auth)
}
func (h *savedFieldsHook) OnAuthUpdated(_ context.Context, auth *Auth) {
	h.auth = authUpdateSnapshot(auth)
}
func (*savedFieldsHook) OnResult(context.Context, Result) {}

func TestManagerSavedFieldsPublished(t *testing.T) {
	for _, saveErr := range []error{nil, errors.New("save failed after enrichment")} {
		name := "success"
		if saveErr != nil {
			name = "failure"
		}
		for _, operation := range []string{"Register", "Update", "Prepare", "Refresh"} {
			t.Run(
				name+"/"+operation, func(t *testing.T) {
					ctx := t.Context()
					hook := &savedFieldsHook{}
					store := &enrichingAuthStore{err: saveErr}
					manager := NewManager(store, nil, hook)
					input := &Auth{
						ID: "saved-fields", Provider: "audit", Status: StatusActive,
						Metadata: map[string]any{"type": "audit"},
					}
					var base, returned *Auth
					var err error
					if operation == "Register" {
						returned, err = manager.Register(ctx, input)
					} else {
						base, err = manager.Register(WithSkipPersist(ctx), input)
						if err != nil {
							t.Fatal(err)
						}
						input = base.Clone()
						input.FileName = ""
						input.Attributes = nil
						delete(input.Metadata, "disabled")
						switch operation {
						case "Update":
							returned, err = manager.Update(ctx, input)
						case "Prepare", "Refresh":
							returned, err = manager.MergePersistentUpdate(ctx, base, input)
						}
					}
					published, ok := manager.GetByID(input.ID)
					if saveErr != nil {
						if !errors.Is(err, saveErr) || returned != nil {
							t.Fatalf("failed save returned auth=%+v error=%v", returned, err)
						}
						if operation == "Register" {
							if ok || hook.auth != nil {
								t.Fatal("failed registration published enriched fields")
							}
						} else if !ok || !reflect.DeepEqual(base, published) || !reflect.DeepEqual(base, hook.auth) {
							t.Fatal("failed save changed published or hook state")
						}
						return
					}
					if err != nil || !ok {
						t.Fatalf("published=%t error=%v", ok, err)
					}
					for name, snapshot := range map[string]*Auth{
						"manager": published, "return": returned, "hook": hook.auth,
					} {
						if snapshot == nil || snapshot.FileName != "saved-fields.json" ||
							snapshot.Attributes[AttributePath] != "/fixture/saved-fields.json" ||
							snapshot.Attributes[AttributeSource] != "/fixture/saved-fields.json" ||
							snapshot.Attributes[AttributeSourceBackend] !=
								AuthSourceFile ||
							snapshot.Metadata["disabled"] !=
								false {
							t.Errorf("%s snapshot missing Store.Save enrichment: %+v", name, snapshot)
						}
						if !reflect.DeepEqual(authUpdateSnapshot(snapshot), published) {
							t.Errorf("%s snapshot differs from manager", name)
						}
					}
					manager.scheduler.mu.Lock()
					scheduled := manager.scheduler.providers["audit"].auths[input.ID].auth.Clone()
					manager.scheduler.mu.Unlock()
					if !reflect.DeepEqual(authUpdateSnapshot(scheduled), published) {
						t.Error("scheduler snapshot differs from manager")
					}
				},
			)
		}
	}
}

func TestManagerSavedFieldsMetaMintPublication(t *testing.T) {
	for _, operation := range []string{"Prepare", "Refresh"} {
		for _, fail := range []bool{false, true} {
			name := operation + "/success"
			if fail {
				name = operation + "/failure"
			}
			t.Run(
				name, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							ctx := t.Context()
							store := &blockingEnrichingAuthStore{
								entered: make(chan struct{}), release: make(chan struct{}),
							}
							if fail {
								store.err = errors.New("mint save failed")
							}
							hook := &savedFieldsHook{}
							manager := NewManager(store, nil, hook)
							base, err := manager.Register(
								WithSkipPersist(ctx), &Auth{
									ID: "meta-mint", Provider: "meta", Status: StatusActive,
									Metadata: map[string]any{"access_token": "old"},
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							updated := base.Clone()
							updated.Metadata["access_token"] = "minted"
							done := make(chan struct{})
							var returned *Auth
							var errUpdate error
							go func() {
								defer close(done)
								returned, errUpdate = manager.MergePersistentUpdate(ctx, base, updated)
							}()
							<-store.entered
							pending, _ := manager.GetByID(base.ID)
							if !reflect.DeepEqual(base, pending) {
								t.Error("mint published before persistence completed")
							}
							close(store.release)
							<-done
							got, _ := manager.GetByID(base.ID)
							if fail {
								if !errors.Is(errUpdate, store.err) || returned != nil ||
									!reflect.DeepEqual(base, got) || !reflect.DeepEqual(base, hook.auth) {
									t.Fatal("failed Meta mint changed published state")
								}
							} else if errUpdate !=
								nil ||
								got.Metadata["access_token"] !=
									"minted" ||
								got.FileName ==
									"" ||
								!reflect.DeepEqual(got, authUpdateSnapshot(returned)) || !reflect.DeepEqual(
								got, hook.auth,
							) {
								t.Fatalf("successful mint snapshots differ: %v", errUpdate)
							}
						},
					)
				},
			)
		}
	}
}
