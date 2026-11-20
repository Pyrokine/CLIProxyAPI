package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
)

func TestHomePendingSettleFailureReachesServiceRun(t *testing.T) {
	homeCfg, subscriptions := homeLifecycleRedis(
		t, func(args []string) string {
			if strings.EqualFold(args[0], "GET") && args[1] == "config" {
				return homeLifecycleBulk("credential-concurrency:\n  cpa-cancel-bound: 20ms\n")
			}
			return ""
		},
	)
	base := &config.Config{Home: homeCfg, UsageStatisticsFile: "disabled"}
	base.RemoteManagement.DisableControlPanel = true
	service := &Service{cfg: base}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	service.hooks.OnBeforeStart = func(*config.Config) {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			service.homeClientMu.RLock()
			registry := service.homeRegistry
			service.homeClientMu.RUnlock()
			if registry != nil {
				pending, err := registry.BeginDispatch()
				if err != nil {
					t.Error(err)
					cancel()
					return
				}
				// 该分支的所有路径均返回，pending 保留至订阅替换检查结束
				// noinspection GoDeferInLoop
				defer pending.End()
				select {
				case subscription := <-subscriptions:
					_ = subscription.Close() // Closing the synthetic peer triggers the disconnect assertions below.
				case <-ctx.Done():
					t.Error(ctx.Err())
					return
				}
				service.homeLifecycleMu.Lock()
				supervisor := service.homeSupervisor
				service.homeLifecycleMu.Unlock()
				select {
				case <-supervisor.done:
				case <-ctx.Done():
					t.Error(ctx.Err())
				}
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Error("Home lifetime did not publish")
				return
			}
		}
	}
	err := service.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(
		err.Error(), "settle pending Home dispatches before subscriber replacement",
	) {
		t.Fatalf("Run lost the subscriber failure: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("Run waited for its parent context instead of the Home failure: %v", ctx.Err())
	}
	if !service.shuttingDown.Load() || service.currentHomeClient() != nil {
		t.Fatal("fatal Home failure did not complete service shutdown")
	}
}

func TestHomePendingDeleteReportSurvivesLifetimeCancellationAndResumesOrder(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "plugin-b-v1.0.0.so")
	secondPath := filepath.Join(root, "plugin-c-v1.0.0.so")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("original-artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var reports []string
	homeCfg, _ := homeLifecycleRedis(
		t, func(args []string) string {
			if strings.EqualFold(args[0], "GET") && args[1] == "plugin-tasks" {
				return homeLifecycleBulk(
					`[{"id":8,"operation":"delete","plugin_id":"plugin-b","target_node_type":"cpa","` +
						`target_node_id":"synthetic-home-node"},{"id":9,"operation":"delete","plugin_id":"` +
						`plugin-c","target_node_type":"cpa","target_node_id":"synthetic-home-node"}]`,
				)
			}
			if strings.EqualFold(args[0], "RPUSH") && args[1] == "plugin-status" {
				mu.Lock()
				defer mu.Unlock()
				reports = append(reports, args[2])
				if len(reports) == 1 {
					if err := os.WriteFile(firstPath, []byte("replacement-must-survive"), 0o600); err != nil {
						t.Error(err)
					}
					return "-ERR synthetic status rejection\r\n"
				}
				return ":1\r\n"
			}
			return ""
		},
	)
	client := home.New(homeCfg)
	t.Cleanup(client.Close)
	cfg := &config.Config{Home: homeCfg, Plugins: config.PluginsConfig{Dir: root}}
	service := &Service{cfg: cfg}
	service.processHomePluginTasksWithClient(t.Context(), cfg, client)
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("advanced to the second task before the first report succeeded: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	stopped := make(chan struct{})
	go func() {
		service.runHomePluginTaskReports(canceledCtx, client)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("canceled report retry worker did not stop")
	}
	service.homePluginTasksMu.Lock()
	retained := service.homePluginTasks != nil && service.homePluginTasks.report != nil &&
		service.homePluginTasks.nextTask == 0
	service.homePluginTasksMu.Unlock()
	if !retained {
		t.Fatal("lifetime cancellation discarded the pending deletion report")
	}
	replacement := client.NewLifetime()
	client.Close()
	t.Cleanup(replacement.Close)
	retryCtx, stopRetry := context.WithTimeout(t.Context(), 4*time.Second)
	defer stopRetry()
	done := make(chan struct{})
	go func() {
		service.runHomePluginTaskReports(retryCtx, replacement)
		close(done)
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		service.homePluginTasksMu.Lock()
		complete := service.homePluginTasks == nil
		service.homePluginTasksMu.Unlock()
		if complete {
			break
		}
		select {
		case <-ticker.C:
		case <-retryCtx.Done():
			t.Fatal("replacement lifetime did not finish the saved task reports")
		}
	}
	stopRetry()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 3 || reports[0] != reports[1] || !strings.Contains(reports[2], `"task_id":9`) {
		t.Fatalf("retry lost the original report or task ordering: %v", reports)
	}
	data, err := os.ReadFile(firstPath)
	if err != nil || string(data) != "replacement-must-survive" {
		t.Fatalf("replacement lifetime repeated deletion: %q, %v", data, err)
	}
	if _, err := os.Stat(secondPath); !os.IsNotExist(err) {
		t.Fatalf("second deletion was not completed after report success: %v", err)
	}
}

func TestHomeSubscriberRetriesDeletionReportWithoutConfigUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plugin-b-v1.0.0.so")
	if err := os.WriteFile(path, []byte("original-artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reports []string
	taskFetches := 0
	homeCfg, subscriptions := homeLifecycleRedis(
		t, func(args []string) string {
			if strings.EqualFold(args[0], "GET") {
				switch args[1] {
				case "config":
					return homeLifecycleBulk(
						"plugins:\n  dir: " + root + "\ncredential-concurrency:\n  cpa-cancel-bound: 20ms\n",
					)
				case "plugin-tasks":
					mu.Lock()
					taskFetches++
					mu.Unlock()
					return homeLifecycleBulk(
						`[{"id":8,"operation":"delete","plugin_id":"plugin-b",` +
							`"target_node_type":"cpa","target_node_id":` +
							`"synthetic-home-node"}]`,
					)
				}
			}
			if strings.EqualFold(args[0], "RPUSH") && args[1] == "plugin-status" && strings.Contains(
				args[2], `"task":"plugin-delete"`,
			) {
				mu.Lock()
				defer mu.Unlock()
				reports = append(reports, args[2])
				if len(reports) == 1 {
					if err := os.WriteFile(path, []byte("replacement-must-survive"), 0o600); err != nil {
						t.Error(err)
					}
					return "-ERR synthetic status rejection\r\n"
				}
				return ":1\r\n"
			}
			return ""
		},
	)
	service := &Service{cfg: &config.Config{Home: homeCfg}}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	service.startHomeSubscriber(ctx)
	t.Cleanup(
		func() {
			cancel()
			select {
			case subscription := <-subscriptions:
				_ = subscription.Close() // Cancellation may have already closed this subscription.
			default:
			}
			stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := service.stopHomeSubscriber(stopCtx); err != nil {
				t.Error(err)
			}
			service.homeClientMu.Lock()
			forwarder := service.homeLogForwarder
			service.homeLogForwarder = nil
			service.homeClientMu.Unlock()
			if forwarder != nil {
				forwarder.Stop()
			}
		},
	)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		mu.Lock()
		count := len(reports)
		mu.Unlock()
		service.homePluginTasksMu.Lock()
		complete := service.homePluginTasks == nil
		service.homePluginTasksMu.Unlock()
		if count == 2 && complete {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("subscriber did not retry the saved report without another config event")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reports[0] != reports[1] || taskFetches != 1 {
		t.Fatalf(
			"subscriber repeated task acquisition or changed its report: fetches=%d reports=%v",
			taskFetches,
			reports,
		)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement-must-survive" {
		t.Fatalf("subscriber report retry repeated deletion: %q, %v", data, err)
	}
}

func TestHomeRuntimeEntryPointsClearTemporaryStoreAuth(t *testing.T) {
	t.Run(
		"non-home-keeps-store-auth", func(t *testing.T) {
			service := &Service{cfg: &config.Config{}}
			next := &config.Config{}
			next.Plugins.StoreAuth = []sdkpluginstore.AuthConfig{
				{
					Match: "https://downloads.example/", Type: sdkpluginstore.AuthTypeBearer,
					TokenEnv: "HOME_UNUSED_SYNTHETIC_TOKEN",
				},
			}
			if err := service.applyConfigUpdateRuntime(t.Context(), next); err != nil {
				t.Fatal(err)
			}
			if len(service.cfg.Plugins.StoreAuth) != 1 {
				t.Error("non-Home runtime lost its configured repository authentication")
			}
		},
	)
	t.Run(
		"direct-runtime-publication", func(t *testing.T) {
			homeCfg := config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"}
			service := &Service{cfg: &config.Config{Home: homeCfg}}
			next := &config.Config{Home: homeCfg}
			next.Plugins.StoreAuth = []sdkpluginstore.AuthConfig{
				{
					Match: "https://downloads.example/", Type: sdkpluginstore.AuthTypeBearer,
					TokenEnv: "HOME_UNUSED_SYNTHETIC_TOKEN",
				},
			}
			if err := service.applyConfigUpdateRuntime(t.Context(), next); err != nil {
				t.Fatal(err)
			}
			if service.cfg.Plugins.StoreAuth != nil {
				t.Error("shared Home runtime publication retained temporary store authentication")
			}
		},
	)
	t.Run(
		"run-startup", func(t *testing.T) {
			homeCfg, _ := homeLifecycleRedis(t, nil)
			base := &config.Config{Home: homeCfg, UsageStatisticsFile: "disabled"}
			base.Plugins.StoreAuth = []sdkpluginstore.AuthConfig{
				{
					Match: "https://downloads.example/", Type: sdkpluginstore.AuthTypeBearer,
					TokenEnv: "HOME_UNUSED_SYNTHETIC_TOKEN",
				},
			}
			service := &Service{cfg: base}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service.hooks.OnBeforeStart = func(cfg *config.Config) {
				if cfg.Plugins.StoreAuth != nil {
					t.Error("Run published temporary store authentication to its runtime hook")
				}
				cancel()
				service.homeLifecycleMu.Lock()
				supervisor := service.homeSupervisor
				service.homeLifecycleMu.Unlock()
				select {
				case <-supervisor.done:
				case <-time.After(4 * time.Second):
					t.Fatal("Home subscriber did not finish cancellation before HTTP startup")
				}
			}
			if err := service.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run changed its cancellation result: %v", err)
			}
		},
	)
}
