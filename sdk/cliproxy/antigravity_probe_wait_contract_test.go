package cliproxy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func antigravityProbeWaiter(t *testing.T, service *Service) func() {
	t.Helper()
	waiter, ok := any(service).(interface{ WaitAntigravityProbes() })
	if !ok {
		t.Fatal("Service does not preserve the public WaitAntigravityProbes method")
	}
	return waiter.WaitAntigravityProbes
}

func TestWaitAntigravityProbesNilAndIdle(t *testing.T) {
	antigravityProbeWaiter(t, nil)()
	synctest.Test(
		t, func(t *testing.T) {
			service := newModelRefreshTestService(t, &config.Config{})
			service.modelRefreshMu.Lock()
			service.ensureModelRefreshWorkersLocked()
			service.modelRefreshMu.Unlock()
			returned := make(chan struct{})
			go func() {
				antigravityProbeWaiter(t, service)()
				close(returned)
			}()
			synctest.Wait()
			select {
			case <-returned:
			default:
				t.Fatal("idle probe wait is blocked by persistent model refresh workers")
			}
		},
	)
}

func TestWaitAntigravityProbesQueuedPublicationAndCancellation(t *testing.T) {
	_ = antigravityProbeWaiter(t, nil)
	for _, cancelPending := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("cancel_pending_%t", cancelPending), func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						started := make(chan string, 6)
						release := make(chan struct{})
						unblock := sync.OnceFunc(func() { close(release) })
						defer unblock()
						previous := fetchAntigravityModels
						fetchAntigravityModels = func(
							ctx context.Context,
							auth *coreauth.Auth,
							_ *config.Config,
						) runtimeexecutor.ModelDiscoveryResult {
							started <- auth.ID
							select {
							case <-release:
							case <-ctx.Done():
								return runtimeexecutor.ModelDiscoveryResult{
									Outcome: runtimeexecutor.ModelDiscoveryInconclusive, Err: ctx.Err(),
								}
							}
							return runtimeexecutor.ModelDiscoveryResult{
								Outcome: runtimeexecutor.ModelDiscoveryComplete,
								Models:  []*registry.ModelInfo{{ID: "model-" + auth.ID}},
							}
						}
						t.Cleanup(func() { fetchAntigravityModels = previous })
						auths := make([]*coreauth.Auth, 6)
						for index := range auths {
							auths[index] = &coreauth.Auth{
								ID: fmt.Sprintf("legacy-wait-%t-%d", cancelPending, index), Provider: "antigravity",
								Status: coreauth.StatusActive,
							}
						}
						service := newModelRefreshTestService(t, &config.Config{}, auths...)
						for _, auth := range auths {
							service.scheduleExternalModelRefresh(auth, false)
						}
						synctest.Wait()
						if len(started) != modelRefreshWorkerCount {
							t.Fatalf("started discoveries=%d, want %d", len(started), modelRefreshWorkerCount)
						}
						cancelled := ""
						if cancelPending {
							service.modelRefreshMu.Lock()
							for id := range service.modelRefreshPending {
								cancelled = id
								break
							}
							service.modelRefreshMu.Unlock()
							if cancelled == "" {
								t.Fatal("bounded discovery left no queued job to cancel")
							}
							service.cancelModelRefresh(cancelled)
						}
						returned := make(chan struct{})
						go func() {
							antigravityProbeWaiter(t, service)()
							close(returned)
						}()
						synctest.Wait()
						select {
						case <-returned:
							t.Fatal("probe wait returned before active and queued publication completed")
						default:
						}
						unblock()
						<-returned
						for _, auth := range auths {
							if published := GlobalModelRegistry().ClientSupportsModel(
								auth.ID, "model-"+auth.ID,
							); published != (auth.ID != cancelled) {
								t.Fatalf("auth=%s published=%t cancelled=%s", auth.ID, published, cancelled)
							}
						}
						antigravityProbeWaiter(t, service)()
					},
				)
			},
		)
	}
}

func TestWaitAntigravityProbesPendingReplacement(t *testing.T) {
	_ = antigravityProbeWaiter(t, nil)
	synctest.Test(
		t, func(t *testing.T) {
			service := newModelRefreshTestService(t, &config.Config{})
			service.coreManager = nil
			service.modelRefreshStop = func() {}
			service.modelRefreshWake = make(chan struct{}, 1)
			service.modelRefreshScheduleWake = make(chan struct{}, 1)
			auth := &coreauth.Auth{ID: "legacy-wait-pending-replacement", Provider: "antigravity", Generation: 1}
			service.scheduleExternalModelRefresh(auth, false)
			service.scheduleExternalModelRefresh(auth, false)
			returned := make(chan struct{})
			go func() {
				antigravityProbeWaiter(t, service)()
				close(returned)
			}()
			synctest.Wait()
			replacement := auth.Clone()
			replacement.Generation++
			service.scheduleExternalModelRefresh(replacement, false)
			synctest.Wait()
			select {
			case <-returned:
				t.Fatal("probe wait returned during an atomic queued-generation replacement")
			default:
			}
			service.cancelModelRefresh(auth.ID)
			<-returned
			antigravityProbeWaiter(t, service)()
		},
	)
}

func TestWaitAntigravityProbesRetainsCanceledAndTimedOutDiscovery(t *testing.T) {
	_ = antigravityProbeWaiter(t, nil)
	for _, action := range []string{"cancel", "timeout", "config_replace", "shutdown"} {
		t.Run(
			action, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						release := make(chan struct{})
						unblock := sync.OnceFunc(func() { close(release) })
						defer unblock()
						previous := fetchAntigravityModels
						fetchAntigravityModels = func(
							_ context.Context,
							auth *coreauth.Auth,
							cfg *config.Config,
						) runtimeexecutor.ModelDiscoveryResult {
							if cfg.ProxyURL != "replacement" {
								<-release
							}
							return runtimeexecutor.ModelDiscoveryResult{
								Outcome: runtimeexecutor.ModelDiscoveryComplete,
								Models:  []*registry.ModelInfo{{ID: cfg.ProxyURL + "-" + auth.ID}},
							}
						}
						t.Cleanup(func() { fetchAntigravityModels = previous })
						auth := &coreauth.Auth{
							ID: "legacy-wait-" + action, Provider: "antigravity", Status: coreauth.StatusActive,
						}
						service := newModelRefreshTestService(t, &config.Config{ProxyURL: "original"}, auth)
						service.scheduleExternalModelRefresh(auth, false)
						synctest.Wait()
						returned := make(chan struct{})
						go func() {
							antigravityProbeWaiter(t, service)()
							close(returned)
						}()
						synctest.Wait()
						switch action {
						case "cancel":
							service.cancelModelRefresh(auth.ID)
						case "timeout":
							time.Sleep(modelRefreshTimeout + time.Millisecond)
						case "config_replace":
							service.publishConfig(&config.Config{ProxyURL: "replacement"})
							service.scheduleExternalModelRefresh(auth, false)
						case "shutdown":
							ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
							defer cancel()
							if err := service.stopModelRefreshWorkers(ctx); err == nil {
								t.Fatal("shutdown returned before the blocked discovery released")
							}
						}
						synctest.Wait()
						select {
						case <-returned:
							t.Fatal("probe wait discarded discovery that is still returning")
						default:
						}
						unblock()
						<-returned
						if GlobalModelRegistry().ClientSupportsModel(auth.ID, "original-"+auth.ID) {
							t.Fatal("a canceled generation published its late discovery")
						}
						if action == "config_replace" && !GlobalModelRegistry().ClientSupportsModel(
							auth.ID, "replacement-"+auth.ID,
						) {
							t.Fatal("replacement discovery was not published before probe wait returned")
						}
						antigravityProbeWaiter(t, service)()
					},
				)
			},
		)
	}
}

func TestWaitAntigravityProbesDoesNotWaitForOtherProviderDiscovery(t *testing.T) {
	_ = antigravityProbeWaiter(t, nil)
	synctest.Test(
		t, func(t *testing.T) {
			service := newModelRefreshTestService(t, &config.Config{})
			service.modelRefreshSlots = make(chan struct{}, 1)
			service.modelRefreshSlots <- struct{}{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			discoveryDone := make(chan struct{})
			go func() {
				service.runModelDiscovery(ctx, modelRefreshJob{auth: &coreauth.Auth{Provider: "codex"}})
				close(discoveryDone)
			}()
			synctest.Wait()
			returned := make(chan struct{})
			go func() {
				antigravityProbeWaiter(t, service)()
				close(returned)
			}()
			synctest.Wait()
			select {
			case <-returned:
			default:
				t.Fatal("Antigravity probe wait includes another provider discovery")
			}
			cancel()
			<-discoveryDone
		},
	)
}
