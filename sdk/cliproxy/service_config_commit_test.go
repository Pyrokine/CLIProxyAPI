package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalapi "github.com/Pyrokine/CLIProxyAPI/v6/internal/api"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalpluginhost "github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestNewRoutingSelectorSupportsQuotaAware(t *testing.T) {
	for _, strategy := range []string{"quota-aware", "quotaaware", "qa"} {
		if _, ok := newRoutingSelector(strategy).(*coreauth.QuotaAwareSelector); !ok {
			t.Fatalf("newRoutingSelector(%q) did not return quota-aware selector", strategy)
		}
	}
	if _, ok := newRoutingSelector("").(*coreauth.RoundRobinSelector); !ok {
		t.Fatal("newRoutingSelector(\"\") did not preserve round-robin default")
	}
}

func newConfigCommitSequenceService(t *testing.T) *Service {
	return newConfigCommitSequenceServiceWithConfig(t, &config.Config{Debug: true})
}

func newConfigCommitSequenceServiceWithConfig(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	root := t.TempDir()
	t.Setenv("WRITABLE_PATH", root)
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	cfg.AuthDir = authDir
	cfg.LoggingToFile = false
	cfg.UsageStatisticsEnabled = false
	configPath := filepath.Join(root, "config.yaml")
	server := internalapi.NewServer(
		cfg,
		coreauth.NewManager(nil, nil, nil),
		sdkaccess.NewManager(),
		configPath,
	)
	t.Cleanup(
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if errStop := server.Stop(ctx); errStop != nil {
				t.Errorf("Stop() error = %v", errStop)
			}
		},
	)
	return &Service{cfg: cfg, configPath: configPath, server: server}
}

func TestApplyConfigUpdateWaitsForManagementCommitSequence(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	handler := svc.server.ManagementHandler()
	sequenceStarted := make(chan struct{})
	releaseSequence := make(chan struct{})
	sequenceDone := make(chan error, 1)
	go func() {
		sequenceDone <- handler.ApplyExternalConfigUpdate(
			func() error {
				close(sequenceStarted)
				<-releaseSequence
				return nil
			},
		)
	}()
	<-sequenceStarted

	next := &config.Config{Debug: false}
	applyDone := make(chan bool, 1)
	go func() {
		applyDone <- svc.applyConfigUpdate(next)
	}()
	select {
	case <-applyDone:
		t.Fatal("external config update bypassed the management commit sequence")
	case <-time.After(100 * time.Millisecond):
	}
	svc.cfgMu.RLock()
	current := svc.cfg
	svc.cfgMu.RUnlock()
	if current == next {
		t.Fatal("service config changed before the management commit sequence was released")
	}

	close(releaseSequence)
	if errSequence := <-sequenceDone; errSequence != nil {
		t.Fatalf("ApplyExternalConfigUpdate() error = %v", errSequence)
	}
	select {
	case applied := <-applyDone:
		if !applied {
			t.Fatal("external config update failed after the management commit sequence was released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("external config update did not resume")
	}
	svc.cfgMu.RLock()
	current = svc.cfg
	svc.cfgMu.RUnlock()
	if current != next {
		t.Fatalf("service config = %p, want %p", current, next)
	}
}

func TestApplyHomeOverlayWaitsForManagementCommitSequence(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	handler := svc.server.ManagementHandler()
	sequenceStarted := make(chan struct{})
	releaseSequence := make(chan struct{})
	sequenceDone := make(chan error, 1)
	go func() {
		sequenceDone <- handler.ApplyExternalConfigUpdate(
			func() error {
				close(sequenceStarted)
				<-releaseSequence
				return nil
			},
		)
	}()
	<-sequenceStarted

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- svc.applyHomeOverlayContext(
			context.Background(),
			&config.Config{Debug: false},
		)
	}()
	select {
	case <-applyDone:
		t.Fatal("Home config update bypassed the management commit sequence")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSequence)
	if errSequence := <-sequenceDone; errSequence != nil {
		t.Fatalf("ApplyExternalConfigUpdate() error = %v", errSequence)
	}
	select {
	case errApply := <-applyDone:
		if errApply != nil {
			t.Fatalf("applyHomeOverlayContext() error = %v", errApply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Home config update did not resume")
	}
}

func TestApplyHomeOverlayKeepsPersistedManagementSnapshot(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	handler := svc.server.ManagementHandler()
	persistedValue := *svc.cfg
	persisted := &persistedValue
	handler.SetConfigCommit(
		internalapi.ManagementConfigCommit{
			Config:     persisted,
			Generation: 1,
		},
	)
	if persisted == svc.cfg {
		t.Fatal("test requires distinct persisted and runtime config pointers")
	}

	if errApply := svc.applyHomeOverlayContext(
		context.Background(),
		&config.Config{Debug: false},
	); errApply != nil {
		t.Fatalf("applyHomeOverlayContext() error = %v", errApply)
	}
	state := handler.ConfigState()
	if state.Config != persisted || state.Generation != 1 {
		t.Fatalf("management state changed after Home runtime overlay: %#v", state)
	}
	svc.cfgMu.RLock()
	runtimeCfg := svc.cfg
	svc.cfgMu.RUnlock()
	if runtimeCfg == nil || runtimeCfg == persisted || runtimeCfg.Debug {
		t.Fatalf("runtime config = %#v, want separate Home overlay", runtimeCfg)
	}
}

func TestApplyManagementConfigCommitDoesNotMutatePublishedSnapshot(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	resolvedAuthDir := filepath.Join(filepath.Dir(svc.configPath), "relative-auth")
	svc.watcher = &WatcherWrapper{
		acknowledgeConfigFile: func(cfg *config.Config, expectedHash string) bool {
			if expectedHash != "persisted-hash" {
				return false
			}
			cfg.AuthDir = resolvedAuthDir
			return true
		},
	}

	published := &config.Config{AuthDir: "relative-auth"}
	if errApply := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{
			Config:         published,
			Generation:     1,
			ConfigFileHash: "persisted-hash",
		},
	); errApply != nil {
		t.Fatalf("applyManagementConfigCommit() error = %v", errApply)
	}
	if published.AuthDir != "relative-auth" {
		t.Fatalf("published AuthDir = %q, want immutable relative path", published.AuthDir)
	}
	svc.cfgMu.RLock()
	runtimeAuthDir := svc.cfg.AuthDir
	svc.cfgMu.RUnlock()
	if runtimeAuthDir != resolvedAuthDir {
		t.Fatalf("runtime AuthDir = %q, want %q", runtimeAuthDir, resolvedAuthDir)
	}
}

func TestApplyWatcherConfigUpdateUsesCommittedGeneration(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	data := []byte("debug: false\n")
	if errWrite := os.WriteFile(svc.configPath, data, 0o600); errWrite != nil {
		t.Fatalf("write watcher config: %v", errWrite)
	}
	digest := sha256.Sum256(data)
	if !svc.applyWatcherConfigUpdate(&config.Config{Debug: false}, hex.EncodeToString(digest[:])) {
		t.Fatal("watcher config update was rejected")
	}
	state := svc.server.ManagementHandler().ConfigState()
	if state.Generation != 1 || state.Config == nil || state.Config.Debug {
		t.Fatalf("management state = %#v", state)
	}
	if svc.managementConfigGeneration != 1 {
		t.Fatalf("service management generation = %d, want 1", svc.managementConfigGeneration)
	}
}

func TestApplyWatcherConfigUpdateDoesNotMutatePublishedSnapshot(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	data := []byte("debug: false\n")
	if errWrite := os.WriteFile(svc.configPath, data, 0o600); errWrite != nil {
		t.Fatalf("write watcher config: %v", errWrite)
	}
	digest := sha256.Sum256(data)
	next := &config.Config{Debug: false}
	if !svc.applyWatcherConfigUpdate(next, hex.EncodeToString(digest[:])) {
		t.Fatal("watcher config update was rejected")
	}
	svc.cfgMu.RLock()
	published := svc.cfg
	svc.cfgMu.RUnlock()
	if published == next {
		t.Fatal("watcher snapshot was published and mutated in place")
	}
	if len(next.ModelCatalog.Models) != 0 || len(next.OAuthModelAlias) != 0 {
		t.Fatalf("watcher snapshot was mutated: %#v", next)
	}
}

func TestApplyWatcherConfigUpdateAcceptsCommittedConfigWhenPluginSyncFails(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	blockedRoot := filepath.Join(t.TempDir(), "plugins")
	if errWrite := os.WriteFile(blockedRoot, []byte("not a directory"), 0o600); errWrite != nil {
		t.Fatalf("write blocked plugin root: %v", errWrite)
	}
	svc.pluginHost = internalpluginhost.New()
	previousDelay := pluginRuntimeRetryInitialDelay
	pluginRuntimeRetryInitialDelay = time.Millisecond
	t.Cleanup(
		func() {
			if errStop := svc.stopPluginRuntimeRetry(context.Background()); errStop != nil {
				t.Errorf("stopPluginRuntimeRetry() error = %v", errStop)
			}
			pluginRuntimeRetryInitialDelay = previousDelay
		},
	)
	data := []byte("debug: false\n")
	if errWrite := os.WriteFile(svc.configPath, data, 0o600); errWrite != nil {
		t.Fatalf("write watcher config: %v", errWrite)
	}
	digest := sha256.Sum256(data)
	next := &config.Config{
		Debug: false,
		Plugins: internalconfig.PluginsConfig{
			Enabled: true,
			Dir:     blockedRoot,
		},
	}
	if !svc.applyWatcherConfigUpdate(next, hex.EncodeToString(digest[:])) {
		t.Fatal("committed watcher config was rejected after plugin sync failure")
	}
	state := svc.server.ManagementHandler().ConfigState()
	if state.Generation != 1 || state.Config == nil || state.Config.Debug {
		t.Fatalf("management state = %#v", state)
	}
	svc.cfgMu.RLock()
	current := svc.cfg
	svc.cfgMu.RUnlock()
	if current == nil || current.Debug {
		t.Fatalf("service config = %#v", current)
	}
	svc.pluginRuntimeRetryMu.Lock()
	retryScheduled := svc.pluginRuntimeRetryCancel != nil
	svc.pluginRuntimeRetryMu.Unlock()
	if !retryScheduled {
		t.Fatal("plugin runtime retry was not scheduled")
	}
}

func TestStopPluginRuntimeRetryHonorsShutdownContext(t *testing.T) {
	svc := &Service{}
	release := make(chan struct{})
	svc.pluginRuntimeRetryWG.Go(func() {
		<-release
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if errStop := svc.stopPluginRuntimeRetry(ctx); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("stopPluginRuntimeRetry() error = %v, want deadline exceeded", errStop)
	}
	close(release)
}

func TestApplyWatcherConfigUpdateRejectsSupersededSnapshot(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	oldData := []byte("debug: true\n")
	oldDigest := sha256.Sum256(oldData)
	if errWrite := os.WriteFile(svc.configPath, []byte("debug: false\n"), 0o600); errWrite != nil {
		t.Fatalf("write current config: %v", errWrite)
	}
	if svc.applyWatcherConfigUpdate(&config.Config{Debug: true}, hex.EncodeToString(oldDigest[:])) {
		t.Fatal("superseded watcher snapshot was applied")
	}
	state := svc.server.ManagementHandler().ConfigState()
	if state.Generation != 0 || state.Config == nil || !state.Config.Debug {
		t.Fatalf("management state changed after stale watcher update: %#v", state)
	}
}

func TestApplyManagementConfigCommitAcknowledgesPersistedFileHash(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	acknowledged := false
	svc.watcher = &WatcherWrapper{
		acknowledgeConfigFile: func(cfg *config.Config, expectedHash string) bool {
			acknowledged = cfg != nil && !cfg.Debug && expectedHash == "persisted-hash"
			return acknowledged
		},
	}
	if errApply := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{
			Config:         &config.Config{Debug: false},
			Generation:     1,
			ConfigFileHash: "persisted-hash",
		},
	); errApply != nil {
		t.Fatalf("apply management config: %v", errApply)
	}
	if !acknowledged {
		t.Fatal("management config commit was not acknowledged by the watcher")
	}
}

func TestApplyManagementConfigCommitRejectsStaleGeneration(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	newer := &config.Config{Debug: false}
	older := &config.Config{Debug: true}
	if errApply := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{Config: newer, Generation: 2},
	); errApply != nil {
		t.Fatalf("apply newer management config: %v", errApply)
	}
	if errApply := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{Config: older, Generation: 1},
	); errApply != nil {
		t.Fatalf("apply stale management config: %v", errApply)
	}
	svc.cfgMu.RLock()
	current := svc.cfg
	svc.cfgMu.RUnlock()
	if current == nil || current.Debug {
		t.Fatalf("service config = %#v, want newer generation", current)
	}
}

func TestApplyManagementConfigCommitUsesHeldCommitSequence(t *testing.T) {
	svc := newConfigCommitSequenceService(t)
	next := &config.Config{Debug: false}
	done := make(chan error, 1)
	go func() {
		done <- svc.server.ManagementHandler().ApplyExternalConfigUpdate(
			func() error {
				return svc.applyManagementConfigCommit(
					context.Background(),
					internalapi.ManagementConfigCommit{Config: next, Generation: 1},
				)
			},
		)
	}()
	select {
	case errApply := <-done:
		if errApply != nil {
			t.Fatalf("applyManagementConfigCommit() error = %v", errApply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("management config callback reacquired the commit sequence")
	}
}
