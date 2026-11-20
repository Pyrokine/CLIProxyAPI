package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type homeStopExecutor struct {
	customOwnedExecutor
	started chan context.Context
}

func (e *homeStopExecutor) Execute(
	ctx context.Context,
	_ *coreauth.Auth,
	_ executor.Request,
	_ executor.Options,
) (executor.Response, error) {
	e.started <- ctx
	<-ctx.Done()
	return executor.Response{}, ctx.Err()
}

func TestServiceOriginalHomeSubscriberRecoveryInputs(t *testing.T) {
	for _, mode := range []string{
		"heartbeat", "pre-ack-retry", "blocking-scope", "ambiguous", "shutdown-during-retry",
	} {
		t.Run(
			mode, func(t *testing.T) {
				var configRequests atomic.Int32
				var replacementOnce sync.Once
				var attemptMu sync.Mutex
				var failedAttempts []time.Time
				replacementStarted, allowReplacement := make(chan struct{}), make(chan struct{})
				releaseReplacement := sync.OnceFunc(func() { close(allowReplacement) })
				defer releaseReplacement()
				allowClose := make(chan struct{})
				releaseClose := sync.OnceFunc(func() { close(allowClose) })
				defer releaseClose()
				homeConfig, subscriptions := homeLifecycleRedis(
					t, func(args []string) string {
						if strings.EqualFold(args[0], "GET") && args[1] == "config" {
							attempt := configRequests.Add(1)
							if mode == "pre-ack-retry" && (attempt == 2 || attempt == 3) {
								attemptMu.Lock()
								failedAttempts = append(failedAttempts, time.Now())
								attemptMu.Unlock()
								return "-ERR unavailable\r\n"
							}
							return homeLifecycleBulk(
								"credential-concurrency:\n  lifecycle-config-revision: 7\n  " +
									"cpa-heartbeat-timeout: 5s\n  cpa-cancel-bound: " +
									"500ms\n",
							)
						}
						if strings.EqualFold(args[0], "SUBSCRIBE") && configRequests.Load() > 1 {
							replacementOnce.Do(
								func() {
									close(replacementStarted)
									select {
									case <-allowReplacement:
									case <-t.Context().Done():
									}
								},
							)
						}
						return ""
					},
				)
				service := &Service{cfg: &config.Config{Home: homeConfig}}
				previous := home.Current()
				t.Cleanup(func() { home.SetCurrent(previous) })
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(
					func() {
						cancel()
						releaseReplacement()
						stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
						defer stop()
						if err := service.stopHomeSubscriber(stopCtx); err != nil {
							t.Error(err)
						}
						if service.homeLogForwarder != nil {
							service.homeLogForwarder.Stop()
						}
					},
				)
				service.startHomeSubscriber(ctx)
				waitPublished := func(previous *home.Client) (*home.Client, *executionregistry.Registry) {
					deadline := time.NewTimer(2 * time.Second)
					defer deadline.Stop()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						service.homeClientMu.RLock()
						client, registry := service.homeClient, service.homeRegistry
						service.homeClientMu.RUnlock()
						if client != nil && client != previous && registry != nil {
							return client, registry
						}
						select {
						case <-ticker.C:
						case <-deadline.C:
							t.Fatal("Home lifetime did not publish after its subscription ACK")
						}
					}
				}
				client, registry := waitPublished(nil)
				pending, err := registry.BeginDispatch()
				if err != nil {
					t.Fatal(err)
				}
				scope, err := registry.Install(pending, executionregistry.ScopeSpec{})
				if err != nil {
					t.Fatal(err)
				}
				defer scope.End("test cleanup")
				resourceClosed := make(chan struct{})
				if err := scope.Bind(
					func() error {
						close(resourceClosed)
						if mode == "blocking-scope" {
							<-allowClose
						}
						go scope.End("recovery")
						return nil
					},
				); err != nil {
					t.Fatal(err)
				}
				if mode == "shutdown-during-retry" {
					unresolved, err := registry.BeginDispatch()
					if err != nil {
						t.Fatal(err)
					}
					defer unresolved.End()
					_ =
						// Closing the synthetic peer triggers the disconnect assertions below.
						(<-subscriptions).Close()
					deadline := time.NewTimer(time.Second)
					defer deadline.Stop()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						if err := client.Ping(t.Context()); err != nil &&
							err.Error() == "home auth dispatch is fenced" &&
							service.currentHomeClient() == nil {
							break
						}
						select {
						case <-ticker.C:
						case <-deadline.C:
							t.Fatal("retry did not detach the original Home lifetime")
						}
					}
					cancel()
					unresolved.End()
					stopCtx, stop := context.WithTimeout(t.Context(), 2*time.Second)
					defer stop()
					if err := service.Shutdown(stopCtx); err != nil {
						t.Fatal(err)
					}
					select {
					case <-resourceClosed:
					default:
						t.Fatal("shutdown retained the detached registry's active resource")
					}
					return
				}
				if mode == "ambiguous" {
					client.AbortAmbiguousDispatch()
				} else {
					_ =
						// Closing the synthetic peer triggers the disconnect assertions below.
						(<-subscriptions).Close()
				}
				select {
				case <-replacementStarted:
				case <-time.After(4 * time.Second):
					t.Fatal("Home subscriber did not request a replacement lifetime")
				}
				if current := service.currentHomeClient(); current != nil && current != client {
					t.Fatal("replacement lifetime became visible before its subscription ACK")
				}
				select {
				case <-resourceClosed:
					if mode != "ambiguous" {
						t.Fatal("heartbeat recovery drained the original active scope")
					}
				default:
					if mode == "ambiguous" {
						t.Fatal("ambiguous dispatch did not drain its old scope before replacement")
					}
				}
				if mode == "pre-ack-retry" {
					attemptMu.Lock()
					if len(failedAttempts) != 2 ||
						failedAttempts[1].Sub(failedAttempts[0]) < 75*time.Millisecond {
						t.Errorf("pre-ACK retries did not retain their backoff: %v", failedAttempts)
					}
					attemptMu.Unlock()
				}
				releaseReplacement()
				_, replacement := waitPublished(client)
				if (replacement == registry) != (mode != "ambiguous") || ctx.Err() != nil {
					t.Fatal("recovery changed registry ownership or canceled the active service")
				}
				releaseClose()
			},
		)
	}
}

func TestServiceHeartbeatLossCancelsBlockedConfigFinalizationWithoutDrainingRegistry(t *testing.T) {
	const initial = "credential-concurrency:\n  lifecycle-config-revision: 1\n  " +
		"cpa-heartbeat-timeout: 100ms\n  cpa-cancel-bound: " +
		"100ms\n"
	const update = "credential-concurrency:\n  lifecycle-config-revision: 2\n  cpa-heartbeat-timeout: 100ms\n" +
		"  cpa-cancel-bound: 100ms\nplugins:\n  enabled: true\n"
	var configRequests, statusWrites atomic.Int32
	statusStarted, statusRelease, secondConfig := make(
		chan struct{},
	), make(
		chan struct{},
	), make(
		chan struct{},
		1,
	)
	releaseStatus := sync.OnceFunc(func() { close(statusRelease) })
	defer releaseStatus()
	homeConfig, subscriptions := homeLifecycleRedis(
		t, func(args []string) string {
			switch strings.ToUpper(args[0]) {
			case "GET":
				switch args[1] {
				case "config":
					if configRequests.Add(1) > 1 {
						select {
						case secondConfig <- struct{}{}:
						default:
						}
						return "-ERR unavailable\r\n"
					}
					return homeLifecycleBulk(initial)
				case "plugin-sync":
					return homeLifecycleBulk(
						fmt.Sprintf(
							`{"schema_version":1,"expires_at":%q,"items":[]}`,
							time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
						),
					)
				}
			case "RPUSH":
				if args[1] == "plugin-status" && statusWrites.Add(1) > 1 {
					close(statusStarted)
					<-statusRelease
				}
				return ":1\r\n"
			}
			return ""
		},
	)
	homeConfig.NodeID = "node-1"
	service := &Service{cfg: &config.Config{Home: homeConfig}}
	previous := home.Current()
	t.Cleanup(func() { home.SetCurrent(previous) })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(
		func() {
			releaseStatus()
			cancel()
			stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := service.stopHomeSubscriber(stopCtx); err != nil {
				t.Error(err)
			}
			if service.homeLogForwarder != nil {
				service.homeLogForwarder.Stop()
			}
		},
	)
	service.startHomeSubscriber(ctx)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var registry *executionregistry.Registry
	for registry == nil {
		service.homeClientMu.RLock()
		registry = service.homeRegistry
		service.homeClientMu.RUnlock()
		if registry != nil {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("initial Home lifetime did not publish")
		}
	}
	pending, err := registry.BeginDispatch()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := registry.Install(pending, executionregistry.ScopeSpec{})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.End("completed")
	resourceClosed := make(chan struct{})
	if err := scope.Bind(
		func() error {
			close(resourceClosed)
			go scope.End("canceled")
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	conn := <-subscriptions
	if _, err := io.WriteString(
		conn, fmt.Sprintf(">3\r\n$7\r\nmessage\r\n$6\r\nconfig\r\n$%d\r\n%s\r\n", len(update), update),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-statusStarted:
	case <-time.After(time.Second):
		t.Fatal("updated config did not enter blocked finalization")
	}
	select {
	case <-resourceClosed:
		t.Fatal("heartbeat loss drained the active execution")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-secondConfig:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not retry after heartbeat loss during blocked finalization")
	}
	service.homeClientMu.RLock()
	currentRegistry, currentClient := service.homeRegistry, service.homeClient
	service.homeClientMu.RUnlock()
	if currentRegistry != nil || currentClient != nil || home.Current() != nil {
		t.Fatal("heartbeat-lost lifetime left a published Home client or registry")
	}
}

// noinspection GoDirectComparisonOfErrors
func TestHomeSubscriberStopDrainsActualManagerAndAcknowledgesRelease(t *testing.T) {
	var releases atomic.Int32
	dispatched := make(chan map[string]any, 1)
	homeConfig, _ := homeLifecycleRedis(
		t, func(args []string) string {
			switch strings.ToUpper(args[0]) {
			case "GET":
				if args[1] == "config" {
					return homeLifecycleBulk(
						"credential-concurrency:\n  lifecycle-config-revision: 7\n  " +
							"cpa-heartbeat-timeout: 5s\n  cpa-cancel-bound: " +
							"500ms\n",
					)
				}
			case "RPOP":
				var wire map[string]any
				if err := json.Unmarshal([]byte(args[1]), &wire); err != nil {
					t.Error(err)
				}
				dispatched <- wire
				return homeLifecycleBulk(
					`{"auth_index":"home-stop-auth","model":"home-stop-model","concurrency"` +
						`:{"accounted":true,"credential_id":"home-stop-auth","model":"home-stop` +
						`-model"},"auth":{"id":"home-stop-auth","provider":"home-stop-provider"` +
						`,"status":"active"}}`,
				)
			case "LPUSH":
				if args[1] == "concurrency-release" {
					var release struct {
						CredentialID string `json:"credential_id"`
						Model        string `json:"model"`
						Sequence     int64  `json:"release_seq"`
					}
					if err := json.Unmarshal([]byte(args[2]), &release); err != nil ||
						release.CredentialID !=
							"home-stop-auth" ||
						release.Model !=
							"home-stop-model" ||
						release.Sequence !=
							1 {
						t.Errorf("release=%s error=%v", args[2], err)
					}
					releases.Add(1)
				}
				return ":1\r\n"
			}
			return ""
		},
	)
	manager := coreauth.NewManager(nil, nil, nil)
	provider := &homeStopExecutor{
		id: "home-stop-provider", started: make(chan context.Context, 1),
	}
	manager.RegisterExecutor(provider)
	service := &Service{cfg: &config.Config{Home: homeConfig}, coreManager: manager}
	previous := home.Current()
	t.Cleanup(func() { home.SetCurrent(previous) })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(
		func() {
			cancel()
			stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := service.stopHomeSubscriber(stopCtx); err != nil {
				t.Error(err)
			}
			if service.homeLogForwarder != nil {
				service.homeLogForwarder.Stop()
			}
		},
	)
	service.startHomeSubscriber(ctx)
	startup, stopStartup := context.WithTimeout(t.Context(), time.Second)
	defer stopStartup()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for manager.HomeDispatchBundle() == nil {
		select {
		case <-ticker.C:
		case <-startup.Done():
			t.Fatal("subscriber did not publish its Home dispatch lifetime")
		}
	}
	service.homeClientMu.Lock()
	client, registry := service.homeClient, service.homeRegistry
	service.homeClientMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Execute(
			t.Context(),
			[]string{"home-stop-provider"},
			executor.Request{Model: "home-stop-model"},
			executor.Options{},
		)
		done <- err
	}()
	var attempt context.Context
	select {
	case attempt = <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("Manager did not execute the Home-dispatched credential")
	}
	wire := <-dispatched
	if wire["retry_round"] != float64(0) || wire["count"] != float64(1) {
		t.Fatalf("Manager initial Home wire=%v", wire)
	}
	stopCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := service.stopHomeSubscriber(stopCtx); err != nil {
		t.Fatalf("stop actual subscriber: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || attempt.Err() != context.Canceled {
			t.Fatalf("Manager error=%v attempt error=%v", err, attempt.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber stop retained the Manager execution")
	}
	if len(registry.FreezeInFlight(time.Now()).Executions) != 0 || releases.Load() == 0 {
		t.Fatal("subscriber stop retained an accounted execution or failed to acknowledge its release")
	}
	if service.currentHomeClient() != nil || manager.HomeDispatchBundle() != nil || home.Current() == client ||
		client.HeartbeatOK() {
		t.Fatal("stopped Home lifetime remained published or healthy")
	}
}
