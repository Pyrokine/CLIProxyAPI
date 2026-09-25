package pluginhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func TestSelectPluginFilesSkipsPluginWithPendingDeletionJournal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	extension := pluginExtension(runtime.GOOS)
	originalPath := filepath.Join(dir, "sample-v1.0.0"+extension)
	if errWrite := os.WriteFile(originalPath, []byte("original"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(original) error = %v", errWrite)
	}
	transaction, errPrepare := pluginstore.PrepareDeletionTransaction(root, "sample", []string{originalPath})
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	defer transaction.Deactivate()
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	concurrentPath := filepath.Join(dir, "sample-v2.0.0"+extension)
	if errWrite := os.WriteFile(concurrentPath, []byte("concurrent"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(concurrent) error = %v", errWrite)
	}

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	if len(files) != 0 {
		t.Fatalf("selectPluginFiles() = %#v, want pending plugin omitted", files)
	}
	if errRemove := os.Remove(concurrentPath); errRemove != nil {
		t.Fatalf("Remove(concurrent) error = %v", errRemove)
	}
	if errRollback := transaction.Rollback(); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
}

func TestSelectPluginFilesSkipsPluginWithPendingInstallJournal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(dir, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	data := []byte("new")
	digest := sha256.Sum256(data)
	rootLock, errLock := pluginstore.LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	transaction, errPrepare := pluginstore.PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", hex.EncodeToString(digest[:]), false, rootLock,
	)
	if errPrepare != nil {
		rootLock.Unlock()
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		rootLock.Unlock()
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	rootLock.Unlock()
	defer func() {
		if errRollback := transaction.Rollback(pluginstore.InstallResult{Path: target}); errRollback != nil {
			t.Fatalf("Rollback() error = %v", errRollback)
		}
	}()

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	if len(files) != 0 {
		t.Fatalf("selectPluginFiles() = %#v, want pending install omitted", files)
	}
}

func TestApplyConfigFinalizesInstallTransactionWithPersistedConfigFingerprint(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(dir, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	data := []byte("new")
	digest := sha256.Sum256(data)
	var item config.PluginInstanceConfig
	if errYAML := yaml.Unmarshal([]byte("enabled: false\nsetting: value\n"), &item); errYAML != nil {
		t.Fatalf("decode plugin config: %v", errYAML)
	}
	fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
	if errFingerprint != nil {
		t.Fatalf("PluginInstanceConfigFingerprint() error = %v", errFingerprint)
	}
	rootLock, errLock := pluginstore.LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	transaction, errPrepare := pluginstore.PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", hex.EncodeToString(digest[:]), false, rootLock,
	)
	if errPrepare != nil {
		rootLock.Unlock()
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		rootLock.Unlock()
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	if errExpected := transaction.SetExpectedConfigFingerprint(fingerprint); errExpected != nil {
		rootLock.Unlock()
		t.Fatalf("SetExpectedConfigFingerprint() error = %v", errExpected)
	}
	rootLock.Unlock()

	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: false,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{"sample": item},
		},
	}
	if errApply := New().ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("ApplyConfig() error = %v", errApply)
	}
	pending, errPending := pluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending install transactions = %#v, error = %v", pending, errPending)
	}
	written, errRead := os.ReadFile(target)
	if errRead != nil || string(written) != string(data) {
		t.Fatalf("committed artifact = %q, error = %v", written, errRead)
	}
}

func TestApplyConfigQuiescesBeforePublishingReplacement(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		quiesceResult func(context.Context, []byte) ([]byte, error)
	}{
		{
			name: "supported",
			quiesceResult: func(context.Context, []byte) ([]byte, error) {
				return marshalRPCResult(rpcEmptyResponse{})
			},
		},
		{
			name: "explicitly unsupported",
			quiesceResult: func(context.Context, []byte) ([]byte, error) {
				return marshalRPCError("method_not_found", "quiesce unsupported"), nil
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := makePluginDir(t, "alpha")
			loader := newTestSymbolLoader()
			oldPlugin := reloadTestPlugin("old")
			newPlugin := reloadTestPlugin("new")
			oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldPlugin, reconfigureResult: oldPlugin})
			loader.lookups["alpha"] = oldLookup
			host := newForTest(loader)
			t.Cleanup(func() {
				_ = host.ShutdownAllContext(context.Background())
			})
			initialConfig := pluginRuntimeConfigForTest(root, 1)
			if errApply := host.ApplyConfig(context.Background(), initialConfig); errApply != nil {
				t.Fatalf("initial ApplyConfig() error = %v", errApply)
			}

			events := make([]string, 0, 3)
			oldLookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
				pluginabi.MethodPluginQuiesce: func(ctx context.Context, raw []byte) ([]byte, error) {
					events = append(events, "old-quiesce")
					return testCase.quiesceResult(ctx, raw)
				},
			}
			oldLookup.shutdownHook = func() {
				events = append(events, "old-shutdown")
			}
			newLookup := newTestSymbolLookup(&testPlugin{registerResult: newPlugin, reconfigureResult: newPlugin})
			newLookup.registerOverride = func([]byte) pluginapi.Plugin {
				events = append(events, "candidate-register")
				return newPlugin
			}
			loader.lookups["alpha"] = newLookup

			if errApply := host.ApplyConfig(
				context.Background(), pluginRuntimeConfigForTest(root, 2),
			); errApply != nil {
				t.Fatalf("replacement ApplyConfig() error = %v", errApply)
			}
			wantEvents := "old-quiesce,candidate-register,old-shutdown"
			if strings.Join(events, ",") != wantEvents {
				t.Fatalf("replacement events = %q, want %q", strings.Join(events, ","), wantEvents)
			}
			records := host.activeRecords()
			if len(records) != 1 || records[0].plugin.Metadata.Name != "new" {
				t.Fatalf("published records = %#v", records)
			}
			if newLookup.shutdownCalls != 0 {
				t.Fatalf("new plugin shutdown calls = %d before host cleanup", newLookup.shutdownCalls)
			}
		})
	}
}

func TestApplyConfigClosesFailedCandidateBeforeRestoringQuiescedPlugin(t *testing.T) {
	root := makePluginDir(t, "alpha")
	loader := newTestSymbolLoader()
	oldPlugin := reloadTestPlugin("old")
	oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldPlugin, reconfigureResult: oldPlugin})
	loader.lookups["alpha"] = oldLookup
	host := newForTest(loader)
	t.Cleanup(func() {
		_ = host.ShutdownAllContext(context.Background())
	})
	initialConfig := pluginRuntimeConfigForTest(root, 1)
	if errApply := host.ApplyConfig(context.Background(), initialConfig); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	originalConfigYAML := string(oldLookup.lastLifecycle.ConfigYAML)

	events := make([]string, 0, 4)
	oldLookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodPluginQuiesce: func(context.Context, []byte) ([]byte, error) {
			events = append(events, "old-quiesce")
			return marshalRPCResult(rpcEmptyResponse{})
		},
	}
	oldLookup.reconfigureOverride = func(configYAML []byte) pluginapi.Plugin {
		events = append(events, "old-reconfigure")
		if string(configYAML) != originalConfigYAML {
			t.Fatalf("restored config = %q, want %q", configYAML, originalConfigYAML)
		}
		if host.currentRuntimeConfig() != initialConfig {
			t.Fatal("old plugin reconfigured before previous runtime config was restored")
		}
		return oldPlugin
	}
	candidatePlugin := reloadTestPlugin("candidate")
	candidateLookup := newTestSymbolLookup(&testPlugin{registerResult: candidatePlugin})
	candidateLookup.registerOverride = func([]byte) pluginapi.Plugin {
		events = append(events, "candidate-register")
		panic("candidate register panic")
	}
	candidateLookup.shutdownHook = func() {
		events = append(events, "candidate-shutdown")
	}
	loader.lookups["alpha"] = candidateLookup

	errApply := host.ApplyConfig(context.Background(), pluginRuntimeConfigForTest(root, 2))
	if errApply == nil {
		t.Fatal("replacement ApplyConfig() error = nil, want registration failure")
	}
	wantEvents := "old-quiesce,candidate-register,candidate-shutdown,old-reconfigure"
	if strings.Join(events, ",") != wantEvents {
		t.Fatalf("rollback events = %q, want %q", strings.Join(events, ","), wantEvents)
	}
	if candidateLookup.shutdownCalls != 1 || oldLookup.shutdownCalls != 0 {
		t.Fatalf(
			"lifecycle calls: candidate shutdown=%d old shutdown=%d",
			candidateLookup.shutdownCalls,
			oldLookup.shutdownCalls,
		)
	}
	if host.currentRuntimeConfig() != initialConfig || host.isPluginFused("alpha") {
		t.Fatalf("old runtime was not restored: config=%p want=%p fused=%t", host.currentRuntimeConfig(), initialConfig, host.isPluginFused("alpha"))
	}
	records := host.activeRecords()
	if len(records) != 1 || records[0].plugin.Metadata.Name != "old" {
		t.Fatalf("active records after rollback = %#v", records)
	}
	host.mu.Lock()
	_, pending := host.pendingLoaded["alpha"]
	_, loading := host.loading["alpha"]
	host.mu.Unlock()
	if pending || loading {
		t.Fatalf("candidate state remained after rollback: pending=%t loading=%t", pending, loading)
	}
}

func TestApplyConfigCancellationAfterQuiesceRestoresOldPluginWithoutLoadingCandidate(t *testing.T) {
	root := makePluginDir(t, "alpha")
	loader := newTestSymbolLoader()
	oldPlugin := reloadTestPlugin("old")
	oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldPlugin, reconfigureResult: oldPlugin})
	loader.lookups["alpha"] = oldLookup
	host := newForTest(loader)
	t.Cleanup(func() {
		_ = host.ShutdownAllContext(context.Background())
	})
	initialConfig := pluginRuntimeConfigForTest(root, 1)
	if errApply := host.ApplyConfig(context.Background(), initialConfig); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make([]string, 0, 2)
	oldLookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodPluginQuiesce: func(context.Context, []byte) ([]byte, error) {
			events = append(events, "old-quiesce")
			cancel()
			return marshalRPCResult(rpcEmptyResponse{})
		},
	}
	oldLookup.reconfigureOverride = func([]byte) pluginapi.Plugin {
		events = append(events, "old-reconfigure")
		return oldPlugin
	}
	candidateLookup := newTestSymbolLookup(&testPlugin{registerResult: reloadTestPlugin("candidate")})
	loader.lookups["alpha"] = candidateLookup
	openCallsBefore := loader.openCalls

	errApply := host.ApplyConfig(ctx, pluginRuntimeConfigForTest(root, 2))
	if !errors.Is(errApply, context.Canceled) {
		t.Fatalf("replacement ApplyConfig() error = %v, want canceled", errApply)
	}
	if strings.Join(events, ",") != "old-quiesce,old-reconfigure" {
		t.Fatalf("cancellation events = %q", strings.Join(events, ","))
	}
	if loader.openCalls != openCallsBefore {
		t.Fatalf("candidate load calls = %d, want unchanged %d", loader.openCalls, openCallsBefore)
	}
	if host.currentRuntimeConfig() != initialConfig || host.isPluginFused("alpha") {
		t.Fatal("old plugin state was not restored after cancellation")
	}
}

func TestApplyConfigCancellationDuringQuiesceRestoresOldPlugin(t *testing.T) {
	root := makePluginDir(t, "alpha")
	loader := newTestSymbolLoader()
	oldPlugin := reloadTestPlugin("old")
	oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldPlugin, reconfigureResult: oldPlugin})
	loader.lookups["alpha"] = oldLookup
	host := newForTest(loader)
	t.Cleanup(func() {
		_ = host.ShutdownAllContext(context.Background())
	})
	initialConfig := pluginRuntimeConfigForTest(root, 1)
	if errApply := host.ApplyConfig(context.Background(), initialConfig); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := make([]string, 0, 2)
	oldLookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodPluginQuiesce: func(callCtx context.Context, _ []byte) ([]byte, error) {
			events = append(events, "old-quiesce")
			cancel()
			<-callCtx.Done()
			return nil, callCtx.Err()
		},
	}
	oldLookup.reconfigureOverride = func([]byte) pluginapi.Plugin {
		events = append(events, "old-reconfigure")
		return oldPlugin
	}
	candidateLookup := newTestSymbolLookup(&testPlugin{registerResult: reloadTestPlugin("candidate")})
	loader.lookups["alpha"] = candidateLookup
	openCallsBefore := loader.openCalls

	errApply := host.ApplyConfig(ctx, pluginRuntimeConfigForTest(root, 2))
	if !errors.Is(errApply, context.Canceled) {
		t.Fatalf("replacement ApplyConfig() error = %v, want canceled", errApply)
	}
	if strings.Join(events, ",") != "old-quiesce,old-reconfigure" {
		t.Fatalf("quiesce cancellation events = %q", strings.Join(events, ","))
	}
	if loader.openCalls != openCallsBefore {
		t.Fatalf("candidate load calls = %d, want unchanged %d", loader.openCalls, openCallsBefore)
	}
	if host.currentRuntimeConfig() != initialConfig || host.isPluginFused("alpha") {
		t.Fatal("old plugin state was not restored after quiesce cancellation")
	}
}

func TestApplyConfigCancellationDuringCandidateRegistrationClosesCandidateBeforeRestore(t *testing.T) {
	root := makePluginDir(t, "alpha")
	loader := newTestSymbolLoader()
	oldPlugin := reloadTestPlugin("old")
	oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldPlugin, reconfigureResult: oldPlugin})
	loader.lookups["alpha"] = oldLookup
	host := newForTest(loader)
	t.Cleanup(func() {
		_ = host.ShutdownAllContext(context.Background())
	})
	initialConfig := pluginRuntimeConfigForTest(root, 1)
	if errApply := host.ApplyConfig(context.Background(), initialConfig); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := make([]string, 0, 4)
	oldLookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodPluginQuiesce: func(context.Context, []byte) ([]byte, error) {
			events = append(events, "old-quiesce")
			return marshalRPCResult(rpcEmptyResponse{})
		},
	}
	oldLookup.reconfigureOverride = func([]byte) pluginapi.Plugin {
		events = append(events, "old-reconfigure")
		return oldPlugin
	}
	candidatePlugin := reloadTestPlugin("candidate")
	candidateLookup := newTestSymbolLookup(&testPlugin{registerResult: candidatePlugin})
	candidateLookup.registerOverride = func([]byte) pluginapi.Plugin {
		events = append(events, "candidate-register")
		cancel()
		return candidatePlugin
	}
	candidateLookup.shutdownHook = func() {
		events = append(events, "candidate-shutdown")
	}
	loader.lookups["alpha"] = candidateLookup

	errApply := host.ApplyConfig(ctx, pluginRuntimeConfigForTest(root, 2))
	if !errors.Is(errApply, context.Canceled) {
		t.Fatalf("replacement ApplyConfig() error = %v, want canceled", errApply)
	}
	wantEvents := "old-quiesce,candidate-register,candidate-shutdown,old-reconfigure"
	if strings.Join(events, ",") != wantEvents {
		t.Fatalf("cancellation events = %q, want %q", strings.Join(events, ","), wantEvents)
	}
	if candidateLookup.shutdownCalls != 1 || oldLookup.shutdownCalls != 0 {
		t.Fatalf(
			"lifecycle calls: candidate shutdown=%d old shutdown=%d",
			candidateLookup.shutdownCalls,
			oldLookup.shutdownCalls,
		)
	}
	if host.currentRuntimeConfig() != initialConfig || host.isPluginFused("alpha") {
		t.Fatal("old plugin state was not restored after candidate registration cancellation")
	}
	records := host.activeRecords()
	if len(records) != 1 || records[0].plugin.Metadata.Name != "old" {
		t.Fatalf("active records after cancellation = %#v", records)
	}
}

func pluginRuntimeConfigForTest(root string, priority int) *config.Config {
	enabled := true
	return &config.Config{Plugins: config.PluginsConfig{
		Enabled: true,
		Dir:     root,
		Configs: map[string]config.PluginInstanceConfig{
			"alpha": {Enabled: &enabled, Priority: priority},
		},
	}}
}

func reloadTestPlugin(name string) pluginapi.Plugin {
	plugin := validTestPlugin(name)
	plugin.Capabilities = pluginapi.Capabilities{
		RequestInterceptor: requestInterceptorFunc(func(
			_ context.Context,
			req pluginapi.RequestInterceptRequest,
		) (pluginapi.RequestInterceptResponse, error) {
			return pluginapi.RequestInterceptResponse{Body: req.Body}, nil
		}),
	}
	return plugin
}

func TestCleanupUnselectedPluginFilesPreservesConcurrentReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	extension := pluginExtension(runtime.GOOS)
	oldPath := filepath.Join(dir, "sample-v1.0.0"+extension)
	selectedPath := filepath.Join(dir, "sample-v2.0.0"+extension)
	for path, data := range map[string]string{oldPath: "old", selectedPath: "selected"} {
		if errWrite := os.WriteFile(path, []byte(data), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	_, candidates, errSelect := selectPluginFilesWithCandidates(root)
	if errSelect != nil {
		t.Fatal(errSelect)
	}
	candidates, errSnapshots := preparePluginCleanupSnapshots(candidates)
	if errSnapshots != nil {
		t.Fatal(errSnapshots)
	}
	if errRemove := os.Remove(oldPath); errRemove != nil {
		t.Fatal(errRemove)
	}
	if errWrite := os.WriteFile(oldPath, []byte("concurrent"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errCleanup := cleanupUnselectedPluginFiles(
		candidates,
		[]pluginFile{{ID: "sample", Path: selectedPath, Version: "2.0.0"}},
	); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	written, errRead := os.ReadFile(oldPath)
	if errRead != nil || string(written) != "concurrent" {
		t.Fatalf("concurrent plugin = %q, error = %v", written, errRead)
	}
}

func TestCandidateDirs(t *testing.T) {
	got := candidateDirs("plugins", "darwin", "arm64", "v3")
	want := []string{
		filepath.Join("plugins", "darwin", "arm64-v3"),
		filepath.Join("plugins", "darwin", "arm64"),
		"plugins",
	}
	if len(got) != len(want) {
		t.Fatalf("len(candidateDirs) = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidateDirs[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestCandidateDirsOmitsEmptyVariant(t *testing.T) {
	got := candidateDirs("plugins", "linux", "arm64", "")
	want := []string{
		filepath.Join("plugins", "linux", "arm64"),
		"plugins",
	}
	if len(got) != len(want) {
		t.Fatalf("len(candidateDirs) = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidateDirs[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestPluginExtensionForPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{goos: "linux", want: ".so"},
		{goos: "freebsd", want: ".so"},
		{goos: "darwin", want: ".dylib"},
		{goos: "windows", want: ".dll"},
	}

	for _, tc := range cases {
		if got := pluginExtension(tc.goos); got != tc.want {
			t.Fatalf("pluginExtension(%q) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

func TestPluginIDFromDynamicLibraryPath(t *testing.T) {
	cases := map[string]string{
		"plugins/example.so":        "example",
		"plugins/example.dylib":     "example",
		"plugins/example.dll":       "example",
		"plugins/example-v1.2.3.so": "example",
		"plugins/example.custom":    "example.custom",
	}

	for path, want := range cases {
		if got := pluginIDFromPath(path); got != want {
			t.Fatalf("pluginIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSelectPluginFilesFiltersInvalidIDAndDeduplicatesByID(t *testing.T) {
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdirAll := os.MkdirAll(archDir, 0o755); errMkdirAll != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdirAll)
	}

	extension := pluginExtension(runtime.GOOS)
	paths := []string{
		filepath.Join(root, "sample"+extension),
		filepath.Join(archDir, "sample"+extension),
		filepath.Join(archDir, "bad name"+extension),
		filepath.Join(archDir, "-bad"+extension),
		filepath.Join(archDir, "another"+strings.ToUpper(extension)),
		filepath.Join(archDir, "ignored.txt"),
	}
	for _, path := range paths {
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}
	if errMkdir := os.Mkdir(filepath.Join(archDir, "dir"+extension), 0o755); errMkdir != nil {
		t.Fatalf("Mkdir() error = %v", errMkdir)
	}

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}

	want := []pluginFile{
		{ID: "another", Path: filepath.Join(archDir, "another"+strings.ToUpper(extension))},
		{ID: "sample", Path: filepath.Join(archDir, "sample"+extension)},
	}
	if len(files) != len(want) {
		t.Fatalf("selectPluginFiles() = %v, want %v", files, want)
	}
	for index := range want {
		if files[index] != want[index] {
			t.Fatalf("selectPluginFiles()[%d] = %v, want %v", index, files[index], want[index])
		}
	}
}

func TestSelectPluginFilesPrefersPlatformDirOverRootFallback(t *testing.T) {
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdirAll := os.MkdirAll(archDir, 0o755); errMkdirAll != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdirAll)
	}

	extension := pluginExtension(runtime.GOOS)
	platformPath := filepath.Join(archDir, "alpha"+extension)
	rootPath := filepath.Join(root, "alpha"+extension)
	for _, path := range []string{rootPath, platformPath} {
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	if len(files) != 1 {
		t.Fatalf("selectPluginFiles() = %v, want exactly one alpha plugin", files)
	}
	if files[0] != (pluginFile{ID: "alpha", Path: platformPath}) {
		t.Fatalf("selectPluginFiles()[0] = %v, want platform plugin %s", files[0], platformPath)
	}
}

func TestSelectPluginFilesUsesVersionedPluginIDAndPrefersNewestVersion(t *testing.T) {
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdirAll := os.MkdirAll(archDir, 0o755); errMkdirAll != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdirAll)
	}

	extension := pluginExtension(runtime.GOOS)
	paths := []string{
		filepath.Join(archDir, "sample-v1.2.0"+extension),
		filepath.Join(archDir, "sample-v1.10.0"+extension),
		filepath.Join(archDir, "sample-v1.9.9"+extension),
	}
	for _, path := range paths {
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	wantPath := filepath.Join(archDir, "sample-v1.10.0"+extension)
	if len(files) != 1 {
		t.Fatalf("selectPluginFiles() = %v, want exactly one sample plugin", files)
	}
	if files[0] != (pluginFile{ID: "sample", Path: wantPath, Version: "1.10.0"}) {
		t.Fatalf("selectPluginFiles()[0] = %v, want newest version %s", files[0], wantPath)
	}
}

func TestSelectPluginFilesPrefersDesiredVersion(t *testing.T) {
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdirAll := os.MkdirAll(archDir, 0o755); errMkdirAll != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdirAll)
	}

	extension := pluginExtension(runtime.GOOS)
	for _, name := range []string{"sample-v1.2.0", "sample-v1.10.0"} {
		path := filepath.Join(archDir, name+extension)
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}

	files, errSelect := selectPluginFiles(root, map[string]string{"sample": "1.2.0"})
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	wantPath := filepath.Join(archDir, "sample-v1.2.0"+extension)
	if len(files) != 1 || files[0] != (pluginFile{ID: "sample", Path: wantPath, Version: "1.2.0"}) {
		t.Fatalf("selectPluginFiles() = %v, want desired version %s", files, wantPath)
	}
}

func TestDiscoverPluginFilesReturnsSelectedPluginFiles(t *testing.T) {
	root := makePluginDir(t, "alpha")

	files, errDiscover := DiscoverPluginFiles(root)
	if errDiscover != nil {
		t.Fatalf("DiscoverPluginFiles() error = %v", errDiscover)
	}

	if len(files) != 1 || files[0].ID != "alpha" || files[0].Path == "" {
		t.Fatalf("DiscoverPluginFiles() = %#v, want alpha file", files)
	}
}

func TestSelectPluginFilesRejectsPlatformDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	tests := []struct {
		name       string
		linkTarget func(root, outside string) (string, string)
	}{
		{
			name: "goos directory",
			linkTarget: func(root, outside string) (string, string) {
				target := filepath.Join(outside, runtime.GOOS)
				return filepath.Join(root, runtime.GOOS), target
			},
		},
		{
			name: "architecture directory",
			linkTarget: func(root, outside string) (string, string) {
				if err := os.MkdirAll(filepath.Join(root, runtime.GOOS), 0o755); err != nil {
					t.Fatalf("MkdirAll(goos) error = %v", err)
				}
				target := filepath.Join(outside, runtime.GOARCH)
				return filepath.Join(root, runtime.GOOS, runtime.GOARCH), target
			},
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name,
			func(t *testing.T) {
				root := t.TempDir()
				outside := t.TempDir()
				linkPath, targetDir := tt.linkTarget(root, outside)
				if err := os.MkdirAll(targetDir, 0o755); err != nil {
					t.Fatalf("MkdirAll(target) error = %v", err)
				}
				artifact := filepath.Join(targetDir, "sample"+pluginExtension(runtime.GOOS))
				if err := os.WriteFile(artifact, []byte("plugin"), 0o600); err != nil {
					t.Fatalf("WriteFile(artifact) error = %v", err)
				}
				if err := os.Symlink(targetDir, linkPath); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				if _, err := selectPluginFiles(root); err == nil {
					t.Fatal("selectPluginFiles() accepted a symlink platform directory")
				}
			},
		)
	}
}

func TestSelectPluginFilesPrefersCPUVariantOverGenericArchDir(t *testing.T) {
	variant := cpuVariant()
	if variant == "" {
		t.Skip("current GOARCH has no plugin CPU variant")
	}
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	variantDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH+"-"+variant)
	for _, dir := range []string{archDir, variantDir} {
		if errMkdirAll := os.MkdirAll(dir, 0o755); errMkdirAll != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, errMkdirAll)
		}
	}

	extension := pluginExtension(runtime.GOOS)
	genericPath := filepath.Join(archDir, "alpha"+extension)
	variantPath := filepath.Join(variantDir, "alpha"+extension)
	for _, path := range []string{genericPath, variantPath} {
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}

	files, errSelect := selectPluginFiles(root)
	if errSelect != nil {
		t.Fatalf("selectPluginFiles() error = %v", errSelect)
	}
	if len(files) != 1 {
		t.Fatalf("selectPluginFiles() = %v, want exactly one alpha plugin", files)
	}
	if files[0] != (pluginFile{ID: "alpha", Path: variantPath}) {
		t.Fatalf("selectPluginFiles()[0] = %v, want CPU variant plugin %s", files[0], variantPath)
	}
}
