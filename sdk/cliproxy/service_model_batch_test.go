package cliproxy

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestModelBatchRechecksChangedAndNewAuths(t *testing.T) {
	auths := make([]*coreauth.Auth, 0, 4)
	for i := range 4 {
		auths = append(auths, &coreauth.Auth{ID: fmt.Sprintf("batch-reprojection-%d", i), Provider: "qwen", Status: coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": "oauth"}})
	}
	service := newModelRefreshTestService(t, &config.Config{}, auths...)
	service.pluginHost = pluginhost.New()
	unlocks := make(map[string]func(), len(auths))
	for _, auth := range auths {
		unlock, err := service.lockAuthApply(context.Background(), auth.ID)
		if err != nil {
			t.Fatal(err)
		}
		unlocks[auth.ID] = sync.OnceFunc(unlock)
	}
	done := make(chan struct{})
	go func() {
		service.syncPluginModelRuntime(context.Background())
		close(done)
	}()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
		<-done
	}()
	var firstID string
	waitForModelRefreshCondition(t, func() bool {
		service.authApplyMu.Lock()
		defer service.authApplyMu.Unlock()
		for id, lock := range service.authApplyLocks {
			if lock.refs >= 2 {
				firstID = id
				return true
			}
		}
		return false
	})
	unlocks[firstID]()
	waitForModelRefreshCondition(t, func() bool {
		service.authApplyMu.Lock()
		defer service.authApplyMu.Unlock()
		for id, lock := range service.authApplyLocks {
			if id != firstID && lock.refs >= 2 {
				return true
			}
		}
		return false
	})
	earlier, ok := service.coreManager.GetByID(firstID)
	models := GlobalModelRegistry().GetModelsForClient(firstID)
	if !ok || len(models) == 0 {
		t.Fatal("first account did not complete model reprojection")
	}
	modelID := models[0].ID
	earlier.Attributes["excluded_models"] = modelID
	if _, err := service.coreManager.Update(coreauth.WithSkipPersist(context.Background()), earlier); err != nil {
		t.Fatal(err)
	}
	added, err := service.coreManager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "batch-reprojection-new", Provider: "qwen", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, unlock := range unlocks {
		unlock()
	}
	waitForModelRefreshCondition(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if GlobalModelRegistry().ClientSupportsModel(earlier.ID, modelID) {
		t.Fatal("batch retained an excluded model for a changed auth generation")
	}
	if len(GlobalModelRegistry().GetModelsForClient(added.ID)) == 0 {
		t.Fatal("batch did not reproject an account added during processing")
	}
}
