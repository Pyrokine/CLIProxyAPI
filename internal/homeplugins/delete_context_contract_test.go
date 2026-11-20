package homeplugins

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
)

type contextPluginRuntime struct {
	fakePluginRuntime
	unloadContext context.Context
	onUnload      func(context.Context)
}

func (r *contextPluginRuntime) UnloadPluginContext(ctx context.Context, id string) bool {
	r.unloadContext = ctx
	if r.onUnload != nil {
		r.onUnload(ctx)
	}
	return r.fakePluginRuntime.UnloadPlugin(id)
}

type pluginDeleteCheckpointContext struct {
	context.Context
	check func() error
}

func (c *pluginDeleteCheckpointContext) Err() error { return c.check() }

// noinspection GoDirectComparisonOfErrors
func TestDeleteWithReportCancellationAtTransactionBoundary(t *testing.T) {
	for _, checkpoint := range []string{"prepared", "hidden", "after final cancellation check"} {
		t.Run(
			checkpoint, func(t *testing.T) {
				root := t.TempDir()
				paths := []string{
					writePluginFile(t, root, "sample", "0.2.0"), writePluginFile(t, root, "sample", "0.3.0"),
				}
				otherPath := writePluginFile(t, root, "other", "0.3.0")
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				triggered := false
				ctx := &pluginDeleteCheckpointContext{Context: parent}
				ctx.check = func() error {
					errSnapshot := parent.Err()
					if !triggered {
						pending, err := internalpluginstore.PendingDeletionPluginIDs(root)
						if err != nil {
							t.Fatal(err)
						}
						_, prepared := pending["sample"]
						_, err = os.Stat(paths[0])
						hidden := os.IsNotExist(err)
						if prepared && (checkpoint == "prepared" || hidden) {
							triggered = true
							cancel()
						}
					}
					if checkpoint == "after final cancellation check" {
						return errSnapshot
					}
					return parent.Err()
				}
				report := DeleteWithReport(
					ctx, &config.Config{Plugins: config.PluginsConfig{Dir: root}}, nil, 47, "sample",
				)
				if !triggered || parent.Err() != context.Canceled {
					t.Fatal("transaction checkpoint did not cancel the caller")
				}
				committed := checkpoint == "after final cancellation check"
				if report.OK != committed || len(report.Plugins) != 1 {
					t.Fatalf("checkpoint=%s report=%+v", checkpoint, report)
				}
				if committed {
					if report.Plugins[0].InstallStatus != pluginInstallStatusDeleted {
						t.Fatalf("committed deletion reported %+v", report.Plugins[0])
					}
				} else if !strings.Contains(report.Error, context.Canceled.Error()) {
					t.Fatalf("cancellation lost from report: %+v", report)
				}
				for _, path := range paths {
					_, err := os.Stat(path)
					if (committed && !os.IsNotExist(err)) || (!committed && err != nil) {
						t.Fatalf("committed=%t path=%s error=%v", committed, path, err)
					}
				}
				if _, err := os.Stat(otherPath); err != nil {
					t.Fatalf("unrelated plugin changed: %v", err)
				}
				if pending, err := internalpluginstore.PendingDeletionPluginIDs(root); err != nil || len(pending) != 0 {
					t.Fatalf("deletion transaction remains: %v %v", pending, err)
				}
			},
		)
	}
}

func TestDeleteWithReportRemovesAllCurrentPlatformPluginVersions(t *testing.T) {
	root := t.TempDir()
	older := writePluginFile(t, root, "sample", "0.2.0")
	newer := writePluginFile(t, root, "sample", "0.3.0")
	other := writePluginFile(t, root, "other", "0.3.0")
	runtimeHost := &fakePluginRuntime{busy: true, unloadResult: true}
	report := DeleteWithReport(
		t.Context(), &config.Config{Plugins: config.PluginsConfig{Dir: root}}, runtimeHost, 43, "sample",
	)
	if !report.OK || runtimeHost.unloadCalls != 1 || len(report.Plugins) != 1 ||
		report.Plugins[0].InstallStatus != pluginInstallStatusDeleted || report.Plugins[0].Path != newer {
		t.Fatalf("all-version delete report=%+v calls=%d", report, runtimeHost.unloadCalls)
	}
	for _, path := range []string{older, newer} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("version remains at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other plugin changed: %v", err)
	}
}

func TestDeleteWithReportStopsBeforeUnloadWhenContextCanceled(t *testing.T) {
	root := t.TempDir()
	path := writePluginFile(t, root, "sample", "1.0.0")
	runtimeHost := &contextPluginRuntime{busy: true, unloadResult: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report := DeleteWithReport(ctx, &config.Config{Plugins: config.PluginsConfig{Dir: root}}, runtimeHost, 44, "sample")
	if report.OK || !strings.Contains(report.Error, context.Canceled.Error()) {
		t.Fatalf("canceled delete report = %+v", report)
	}
	if runtimeHost.unloadContext != nil || runtimeHost.unloadCalls != 0 {
		t.Fatalf(
			"canceled delete unloaded plugin: context=%v calls=%d", runtimeHost.unloadContext, runtimeHost.unloadCalls,
		)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("canceled delete removed plugin artifact: %v", err)
	}
}

func TestDeleteWithReportUsesContextualUnload(t *testing.T) {
	root := t.TempDir()
	path := writePluginFile(t, root, "sample", "1.0.0")
	runtimeHost := &contextPluginRuntime{busy: true, unloadResult: true}
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "contextual")
	report := DeleteWithReport(ctx, &config.Config{Plugins: config.PluginsConfig{Dir: root}}, runtimeHost, 45, "sample")
	if !report.OK || runtimeHost.unloadContext != ctx || runtimeHost.unloadCalls != 1 {
		t.Fatalf("report=%+v, context=%v, calls=%d", report, runtimeHost.unloadContext, runtimeHost.unloadCalls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plugin artifact remains: %v", err)
	}
}

func TestDeleteWithReportCancellationDuringUnloadRetainsAllVersions(t *testing.T) {
	root := t.TempDir()
	paths := []string{writePluginFile(t, root, "sample", "0.2.0"), writePluginFile(t, root, "sample", "0.3.0")}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtimeHost := &contextPluginRuntime{
		busy: true, unloadResult: true,
		onUnload: func(context.Context) { cancel() },
	}
	report := DeleteWithReport(ctx, &config.Config{Plugins: config.PluginsConfig{Dir: root}}, runtimeHost, 46, "sample")
	if report.OK || !strings.Contains(report.Error, context.Canceled.Error()) || runtimeHost.unloadContext != ctx {
		t.Fatalf("canceled unload report=%+v, context=%v", report, runtimeHost.unloadContext)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("canceled deletion removed %s: %v", path, err)
		}
	}
}
