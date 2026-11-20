package homeplugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
)

func batchRollbackConfig(t *testing.T, root string, archives map[string][]byte) *config.Config {
	t.Helper()
	previous := newPluginStoreClient
	newPluginStoreClient = func(*config.Config) sdkpluginstore.Client {
		return sdkpluginstore.NewClient(
			networkScopeDoer(
				func(request *http.Request) (*http.Response, error) {
					id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/"), ".zip")
					return &http.Response{
						StatusCode: http.StatusOK, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(string(archives[id]))),
					}, nil
				},
			), "",
		)
	}
	t.Cleanup(func() { newPluginStoreClient = previous })
	cfg := &config.Config{
		Home:    config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{Enabled: true, Dir: root, Configs: map[string]config.PluginInstanceConfig{}},
	}
	for id, archive := range archives {
		digest := sha256.Sum256(archive)
		cfg.Plugins.Configs[id] = pluginConfigFromYAML(
			t, fmt.Sprintf(
				"enabled: true\nstore:\n  schema-version: 2\n  id: %s\n  version: 0.2.0"+
					"\n  install:\n    type: direct\n    artifacts:\n      - goos: %s\n    "+
					"    goarch: %s\n        url: https://plugins.example/%s.zip\n        s"+
					"ha256: %s\n",
				id, runtime.GOOS, runtime.GOARCH, id, hex.EncodeToString(digest[:]),
			),
		)
	}
	return cfg
}

func batchRollbackTarget(root, id string) string {
	return filepath.Join(root, runtime.GOOS, runtime.GOARCH, id+"-v0.2.0"+homePluginExtension(runtime.GOOS))
}

func TestPreparedSyncBatchFailureRollsBackEarlierInstallsAndRetries(t *testing.T) {
	for _, previous := range []string{"", "old-alpha", "new-alpha"} {
		t.Run(
			"previous="+previous, func(t *testing.T) {
				root := t.TempDir()
				oldVersion := writePluginFile(t, root, "alpha", "0.1.0")
				alpha := batchRollbackTarget(root, "alpha")
				if previous != "" {
					if err := os.WriteFile(alpha, []byte(previous), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				archives := map[string][]byte{
					"alpha": homePluginTestArchive(t, "alpha"+homePluginExtension(runtime.GOOS), "new-alpha"),
					"beta":  homePluginTestArchive(t, "unrelated.txt", "invalid-second-plugin"),
				}
				cfg := batchRollbackConfig(t, root, archives)
				prepared, err := PrepareSyncWithReport(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				report, err := prepared.Apply(nil)
				if err == nil || report.OK {
					t.Fatal("second-plugin install failure returned success")
				}
				pending, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
				if errPending != nil || len(pending) != 0 {
					t.Errorf("failed batch retained pending transactions: %v, %v", pending, errPending)
				}
				data, errRead := os.ReadFile(alpha)
				if previous == "" {
					if !os.IsNotExist(errRead) {
						t.Errorf("failed batch kept first install: %q, %v", data, errRead)
					}
				} else if errRead != nil || string(data) != previous {
					t.Errorf("failed batch did not restore previous artifact: %q, %v", data, errRead)
				}
				if previous != "new-alpha" && report.Plugins[0].InstallStatus == pluginInstallStatusInstalled {
					t.Errorf("rolled-back plugin was reported installed: %+v", report.Plugins[0])
				}
				if data, err := os.ReadFile(oldVersion); err != nil || string(data) != "plugin" {
					t.Fatalf("failed batch changed completed older version: %q, %v", data, err)
				}
				archives["beta"] = homePluginTestArchive(t, "beta"+homePluginExtension(runtime.GOOS), "new-beta")
				cfg = batchRollbackConfig(t, root, archives)
				prepared, err = PrepareSyncWithReport(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				report, err = prepared.Apply(nil)
				if err != nil || !report.OK {
					t.Fatalf("corrected batch could not retry without recovery: %+v, %v", report, err)
				}
				if data, err := os.ReadFile(
					batchRollbackTarget(
						root, "beta",
					),
				); err != nil || string(data) != "new-beta" {
					t.Fatalf("retry did not publish beta: %q, %v", data, err)
				}
				if err := RecoverPluginTransactions(cfg); err != nil {
					t.Fatal(err)
				}
				pending, errPending = sdkpluginstore.PendingInstallPluginIDs(root)
				if errPending != nil || len(pending) != 0 {
					t.Fatalf("persisted-config recovery failed: %v, %v", pending, errPending)
				}
			},
		)
	}
}

type batchRollbackRuntime struct {
	busy   func(string) bool
	unload func(string) bool
}

func (r batchRollbackRuntime) PluginBusy(id string) bool   { return r.busy(id) }
func (r batchRollbackRuntime) UnloadPlugin(id string) bool { return r.unload(id) }

func TestPreparedSyncBatchRollbackFailurePreservesRecoveryJournal(t *testing.T) {
	root := t.TempDir()
	alpha := writePluginFile(t, root, "alpha", "0.2.0")
	writePluginFile(t, root, "beta", "0.2.0")
	archives := map[string][]byte{
		"alpha": homePluginTestArchive(
			t, "alpha"+homePluginExtension(runtime.GOOS), "new-alpha",
		), "beta": homePluginTestArchive(t, "beta"+homePluginExtension(runtime.GOOS), "new-beta"),
	}
	cfg := batchRollbackConfig(t, root, archives)
	prepared, err := PrepareSyncWithReport(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pluginRuntime := batchRollbackRuntime{
		busy: func(id string) bool {
			if id == "beta" {
				if err := os.WriteFile(alpha, []byte("external-change"), 0o600); err != nil {
					t.Error(err)
				}
				return true
			}
			return false
		}, unload: func(string) bool { return false },
	}
	report, err := prepared.Apply(pluginRuntime)
	if err == nil || !strings.Contains(err.Error(), "rollback") || !strings.Contains(
		err.Error(), "changed before recovery",
	) {
		t.Fatalf("batch error lost the rollback failure: %+v, %v", report, err)
	}
	pending, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 1 {
		t.Fatalf("rollback failure discarded recovery state: %v, %v", pending, errPending)
	}
	if data, err := os.ReadFile(alpha); err != nil || string(data) != "external-change" {
		t.Fatalf("rollback overwrote an intervening artifact: %q, %v", data, err)
	}
}

func TestPreparedSyncCanceledBatchRestoresPreviousArtifacts(t *testing.T) {
	root := t.TempDir()
	alpha := writePluginFile(t, root, "alpha", "0.2.0")
	archives := map[string][]byte{
		"alpha": homePluginTestArchive(
			t, "alpha"+homePluginExtension(runtime.GOOS), "new-alpha",
		), "beta": homePluginTestArchive(t, "beta"+homePluginExtension(runtime.GOOS), "new-beta"),
	}
	cfg := batchRollbackConfig(t, root, archives)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prepared, err := PrepareSyncWithReport(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pluginRuntime := batchRollbackRuntime{
		busy: func(id string) bool { return id == "alpha" }, unload: func(string) bool { cancel(); return true },
	}
	report, err := prepared.Apply(pluginRuntime, ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("canceled batch returned success: %+v, %v", report, err)
	}
	if data, err := os.ReadFile(alpha); err != nil || string(data) != "plugin" {
		t.Fatalf("canceled batch did not restore alpha: %q, %v", data, err)
	}
	if _, err := os.Stat(batchRollbackTarget(root, "beta")); !os.IsNotExist(err) {
		t.Fatalf("canceled batch installed beta: %v", err)
	}
	pending, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("canceled batch retained transactions: %v, %v", pending, errPending)
	}
}

func TestPreparedSyncDownloadContextDoesNotCancelUnscopedApply(t *testing.T) {
	root := t.TempDir()
	archives := map[string][]byte{
		"alpha": homePluginTestArchive(
			t, "alpha"+homePluginExtension(runtime.GOOS), "new-alpha",
		),
	}
	cfg := batchRollbackConfig(t, root, archives)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prepared, err := PrepareSyncWithReport(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	report, err := prepared.Apply(nil)
	if err != nil || !report.OK {
		t.Fatalf("download-only context changed the existing unscoped Apply contract: %+v, %v", report, err)
	}
	if err := RecoverPluginTransactions(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedSyncAlreadyCanceledApplyDoesNotWaitForRootLock(t *testing.T) {
	root := t.TempDir()
	archives := map[string][]byte{
		"alpha": homePluginTestArchive(
			t, "alpha"+homePluginExtension(runtime.GOOS), "new-alpha",
		),
	}
	cfg := batchRollbackConfig(t, root, archives)
	prepared, err := PrepareSyncWithReport(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rootLock, err := sdkpluginstore.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error, 1)
	go func() { _, err := prepared.Apply(nil, ctx); result <- err }()
	select {
	case err := <-result:
		rootLock.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Apply error = %v", err)
		}
	case <-time.After(time.Second):
		rootLock.Unlock()
		<-result
		t.Fatal("already-canceled Apply waited for the mutation lock")
	}
}
