package homeplugins

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

type fakePluginRuntime struct {
	busy         bool
	unloadResult bool
	unloadCalls  int
}

func (r *fakePluginRuntime) PluginBusy(string) bool {
	return r.busy
}

func (r *fakePluginRuntime) UnloadPlugin(string) bool {
	r.unloadCalls++
	if r.unloadResult {
		r.busy = false
	}
	return r.unloadResult
}

type fakePluginLoadInspector struct {
	registered map[string]bool
}

func (i fakePluginLoadInspector) PluginRegistered(id string) bool {
	return i.registered[id]
}

func TestStoreManifestFromPluginConfigUsesConfigID(t *testing.T) {
	t.Parallel()

	item := pluginConfigFromYAML(
		t, `
enabled: true
store:
  schema-version: 2
  version: 0.2.0
  source-url: https://example.com/plugins/sample.so
  install:
    type: direct
`,
	)

	manifest, ok, err := storeManifestFromPluginConfig("sample-provider", item)
	if err != nil {
		t.Fatalf("storeManifestFromPluginConfig() error = %v", err)
	}
	if !ok {
		t.Fatalf("storeManifestFromPluginConfig() ok = false, want true")
	}
	if manifest.ID != "sample-provider" {
		t.Fatalf("manifest.ID = %q, want sample-provider", manifest.ID)
	}
	if manifest.Version != "0.2.0" {
		t.Fatalf("manifest.Version = %q, want 0.2.0", manifest.Version)
	}
}

func TestStoreManifestFromPluginConfigCanonicalizesPluginID(t *testing.T) {
	item := pluginConfigFromYAML(
		t, "enabled: true\nstore:\n  schema-version: 2\n  id: Alpha\n  version: 0.2.0\n  source-url: https://example.com/plugins/alpha.so\n  install:\n    type: direct\n",
	)

	manifest, ok, errManifest := storeManifestFromPluginConfig("alpha", item)
	if errManifest != nil || !ok {
		t.Fatalf("storeManifestFromPluginConfig() = (%#v, %v, %v)", manifest, ok, errManifest)
	}
	if manifest.ID != "alpha" {
		t.Fatalf("manifest.ID = %q, want alpha", manifest.ID)
	}
}

func TestStoreManifestFromPluginConfigRejectsDifferentPluginID(t *testing.T) {
	item := pluginConfigFromYAML(
		t, "enabled: true\nstore:\n  schema-version: 2\n  id: beta\n  version: 0.2.0\n  source-url: https://example.com/plugins/beta.so\n  install:\n    type: direct\n",
	)

	_, _, errManifest := storeManifestFromPluginConfig("alpha", item)
	if errManifest == nil || !strings.Contains(errManifest.Error(), "does not match plugin config id") {
		t.Fatalf("storeManifestFromPluginConfig() error = %v, want id mismatch", errManifest)
	}
}

func TestHomePluginFileIdentityIgnoresCase(t *testing.T) {
	file, ok := pluginFileFromPath(filepath.Join("plugins", "Alpha-v1.0.0"+pluginExtension(runtime.GOOS)), "")
	if !ok || file.ID != "alpha" {
		t.Fatalf("pluginFileFromPath() = (%#v, %v), want canonical alpha", file, ok)
	}
}

func TestDeleteWithReportRejectsPendingInstall(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := sdkpluginstore.LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "alpha-v1.0.0"+pluginExtension(runtime.GOOS))
	digest := sha256.Sum256([]byte("new"))
	transaction, errPrepare := sdkpluginstore.PrepareInstallTransactionLocked(
		root, "alpha", target, "1.0.0", hex.EncodeToString(digest[:]), false, rootLock,
	)
	rootLock.Unlock()
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	defer func() {
		if errRollback := transaction.Rollback(sdkpluginstore.InstallResult{}); errRollback != nil {
			t.Errorf("Rollback() error = %v", errRollback)
		}
	}()

	report := DeleteWithReport(
		context.Background(),
		&config.Config{Plugins: config.PluginsConfig{Dir: root}},
		nil,
		1,
		"Alpha",
	)
	if report.OK || len(report.Plugins) != 1 || !strings.Contains(report.Plugins[0].Error, "pending install") {
		t.Fatalf("DeleteWithReport() = %#v, want pending install failure", report)
	}
}

func TestDeleteWithReportRejectsLinkedPlatformDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	root := t.TempDir()
	outside := t.TempDir()
	platformDir := filepath.Join(root, runtime.GOOS)
	if errSymlink := os.Symlink(outside, platformDir); errSymlink != nil {
		t.Fatalf("Symlink() error = %v", errSymlink)
	}
	outsideArchDir := filepath.Join(outside, runtime.GOARCH)
	if errMkdir := os.MkdirAll(outsideArchDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	outsidePlugin := filepath.Join(outsideArchDir, "alpha-v1.0.0"+pluginExtension(runtime.GOOS))
	if errWrite := os.WriteFile(outsidePlugin, []byte("outside"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	report := DeleteWithReport(
		context.Background(),
		&config.Config{Plugins: config.PluginsConfig{Dir: root}},
		nil,
		1,
		"alpha",
	)
	if report.OK || len(report.Plugins) != 1 || report.Plugins[0].Error == "" {
		t.Fatalf("DeleteWithReport() = %#v, want linked directory failure", report)
	}
	data, errRead := os.ReadFile(outsidePlugin)
	if errRead != nil || string(data) != "outside" {
		t.Fatalf("outside plugin = %q, error = %v", data, errRead)
	}
}

func TestPrepareSyncDoesNotChangePluginDirectory(t *testing.T) {
	archiveData := homePluginTestArchive(t, "sample"+homePluginExtension(runtime.GOOS), "plugin-data")
	checksum := sha256.Sum256(archiveData)
	artifactServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(archiveData)
			},
		),
	)
	defer artifactServer.Close()
	previousClientFactory := newPluginStoreClient
	newPluginStoreClient = func(cfg *config.Config) sdkpluginstore.Client {
		return sdkpluginstore.NewClientWithAuth(
			artifactServer.Client(),
			"",
			cfg.Plugins.StoreAuth,
		)
	}
	defer func() { newPluginStoreClient = previousClientFactory }()
	root := t.TempDir()
	archiveTempRoot := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, archiveTempRoot)
	}
	item := pluginConfigFromYAML(
		t,
		"enabled: true\n"+
			"store:\n"+
			"  schema-version: 2\n"+
			"  id: sample\n"+
			"  version: 0.2.0\n"+
			"  install:\n"+
			"    type: direct\n"+
			"    artifacts:\n"+
			"      - goos: "+runtime.GOOS+"\n"+
			"        goarch: "+runtime.GOARCH+"\n"+
			"        url: "+artifactServer.URL+"\n"+
			"        sha256: "+hex.EncodeToString(checksum[:])+"\n",
	)
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			StoreAuth: []sdkpluginstore.AuthConfig{
				{
					Match:         artifactServer.URL + "/",
					ApplyTo:       []string{sdkpluginstore.RequestKindArtifact},
					Type:          sdkpluginstore.AuthTypeNone,
					AllowInsecure: true,
				},
			},
			Configs: map[string]config.PluginInstanceConfig{"sample": item},
		},
	}
	targetPath := filepath.Join(
		root,
		runtime.GOOS,
		runtime.GOARCH,
		"sample-v0.2.0"+homePluginExtension(runtime.GOOS),
	)

	prepared, errPrepare := PrepareSyncWithReport(context.Background(), cfg)
	if errPrepare != nil {
		t.Fatalf("PrepareSyncWithReport() error = %v", errPrepare)
	}
	if _, errStat := os.Stat(targetPath); !os.IsNotExist(errStat) {
		t.Fatalf("target changed during preparation: %v", errStat)
	}
	preparedArchives, errReadDir := os.ReadDir(archiveTempRoot)
	if errReadDir != nil {
		t.Fatalf("ReadDir(prepared archives) error = %v", errReadDir)
	}
	if len(preparedArchives) != 1 {
		t.Fatalf("prepared archive directories = %d, want 1", len(preparedArchives))
	}
	report, errApply := prepared.Apply(nil)
	if errApply != nil {
		t.Fatalf("Apply() error = %v", errApply)
	}
	if len(report.Plugins) != 1 || report.Plugins[0].InstallStatus != pluginInstallStatusInstalled {
		t.Fatalf("report = %#v", report)
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "plugin-data" {
		t.Fatalf("installed data = %q, want plugin-data", data)
	}
	pending, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 1 {
		t.Fatalf("pending install transactions = %#v, error = %v", pending, errPending)
	}
	fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
	if errFingerprint != nil {
		t.Fatal(errFingerprint)
	}
	if errRecover := internalpluginstore.RecoverInstallTransactions(
		root,
		map[string]string{"sample": fingerprint},
	); errRecover != nil {
		t.Fatal(errRecover)
	}
	pending, errPending = sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending transactions after config commit = %#v, error = %v", pending, errPending)
	}
	if data, errRead = os.ReadFile(targetPath); errRead != nil || string(data) != "plugin-data" {
		t.Fatalf("committed plugin data = %q, error = %v", data, errRead)
	}
	preparedArchives, errReadDir = os.ReadDir(archiveTempRoot)
	if errReadDir != nil {
		t.Fatalf("ReadDir(after apply) error = %v", errReadDir)
	}
	if len(preparedArchives) != 0 {
		t.Fatalf("prepared archive directories after apply = %d, want 0", len(preparedArchives))
	}
}

func TestSyncRecoversPendingInstallInCustomPluginDirectory(t *testing.T) {
	root := t.TempDir()
	item := pluginConfigFromYAML(t, "enabled: true\n")
	fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
	if errFingerprint != nil {
		t.Fatal(errFingerprint)
	}
	targetDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	target := filepath.Join(targetDir, "sample-v1.0.0"+homePluginExtension(runtime.GOOS))
	data := []byte("plugin-data")
	digest := sha256.Sum256(data)
	rootLock, errLock := sdkpluginstore.LockRoot(root)
	if errLock != nil {
		t.Fatal(errLock)
	}
	transaction, errPrepare := sdkpluginstore.PrepareInstallTransactionLocked(
		root,
		"sample",
		target,
		"1.0.0",
		hex.EncodeToString(digest[:]),
		false,
		rootLock,
	)
	if errPrepare != nil {
		rootLock.Unlock()
		t.Fatal(errPrepare)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		rootLock.Unlock()
		t.Fatal(errWrite)
	}
	if errExpected := transaction.SetExpectedConfigFingerprint(fingerprint); errExpected != nil {
		rootLock.Unlock()
		t.Fatal(errExpected)
	}
	rootLock.Unlock()

	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{"sample": item},
		},
	}
	report, errSync := SyncWithReport(context.Background(), cfg, nil)
	if errSync != nil || !report.OK {
		t.Fatalf("SyncWithReport() = (%#v, %v)", report, errSync)
	}
	pending, errPending := sdkpluginstore.PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending install transactions = %#v, error = %v", pending, errPending)
	}
	written, errRead := os.ReadFile(target)
	if errRead != nil || string(written) != string(data) {
		t.Fatalf("recovered plugin = %q, error = %v", written, errRead)
	}
}

func TestMarkLoadResultsReportsUnloadedPlugin(t *testing.T) {
	t.Parallel()

	report := SyncReport{
		Plugins: []PluginInstallStatus{
			{ID: "loaded-plugin", InstallStatus: pluginInstallStatusInstalled},
			{ID: "missing-plugin", InstallStatus: pluginInstallStatusInstalled},
		},
	}

	err := MarkLoadResults(&report, fakePluginLoadInspector{registered: map[string]bool{"loaded-plugin": true}})
	if err == nil {
		t.Fatalf("MarkLoadResults() error = nil, want load error")
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if report.Phase != pluginTaskPhaseLoad {
		t.Fatalf("report.Phase = %q, want %q", report.Phase, pluginTaskPhaseLoad)
	}
	if report.Plugins[0].LoadStatus != pluginLoadStatusLoaded {
		t.Fatalf("loaded plugin LoadStatus = %q, want %q", report.Plugins[0].LoadStatus, pluginLoadStatusLoaded)
	}
	if report.Plugins[1].LoadStatus != pluginLoadStatusFailed {
		t.Fatalf("missing plugin LoadStatus = %q, want %q", report.Plugins[1].LoadStatus, pluginLoadStatusFailed)
	}
	if !strings.Contains(report.Plugins[1].Error, "installed but not loaded") {
		t.Fatalf("missing plugin Error = %q, want installed but not loaded", report.Plugins[1].Error)
	}
}

func TestDeleteWithReportUnloadsBusyPluginBeforeDeletingArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pluginPath := writePluginFile(t, root, "sample", "0.2.0")
	runtime := &fakePluginRuntime{busy: true, unloadResult: true}
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: root}}

	report := DeleteWithReport(context.Background(), cfg, runtime, 42, "sample")

	if !report.OK {
		t.Fatalf("report.OK = false, want true: %s", report.Error)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) error = %v, want not exist", pluginPath, err)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("len(report.Plugins) = %d, want 1", len(report.Plugins))
	}
	status := report.Plugins[0]
	if status.InstallStatus != pluginInstallStatusDeleted {
		t.Fatalf("InstallStatus = %q, want %q", status.InstallStatus, pluginInstallStatusDeleted)
	}
	if status.Path != pluginPath {
		t.Fatalf("Path = %q, want %q", status.Path, pluginPath)
	}
}

func TestDeleteWithReportDeletesAllVersionsTransactionally(t *testing.T) {
	root := t.TempDir()
	firstPath := writePluginFile(t, root, "sample", "0.1.0")
	secondPath := writePluginFile(t, root, "sample", "0.2.0")
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: root}}

	report := DeleteWithReport(context.Background(), cfg, nil, 43, "sample")
	if !report.OK {
		t.Fatalf("DeleteWithReport() failed: %s", report.Error)
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plugin version still exists at %s: %v", path, err)
		}
	}
	pending, err := internalpluginstore.PendingDeletionPluginIDs(root)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending deletion transactions = %#v, error = %v", pending, err)
	}
}

func TestDeleteWithReportKeepsBusyPluginWhenUnloadFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pluginPath := writePluginFile(t, root, "sample", "0.2.0")
	runtime := &fakePluginRuntime{busy: true, unloadResult: false}
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: root}}

	report := DeleteWithReport(context.Background(), cfg, runtime, 7, "sample")

	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("Stat(%s) error = %v, want kept", pluginPath, err)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("len(report.Plugins) = %d, want 1", len(report.Plugins))
	}
	status := report.Plugins[0]
	if status.InstallStatus != pluginInstallStatusFailed {
		t.Fatalf("InstallStatus = %q, want %q", status.InstallStatus, pluginInstallStatusFailed)
	}
	if !strings.Contains(status.Error, "loaded plugin") {
		t.Fatalf("Error = %q, want loaded plugin", status.Error)
	}
}

func TestPreparePluginOverwriteUnloadsBusyPlugin(t *testing.T) {
	t.Parallel()

	runtime := &fakePluginRuntime{busy: true, unloadResult: true}

	if err := preparePluginOverwrite("sample", runtime); err != nil {
		t.Fatalf("preparePluginOverwrite() error = %v", err)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if runtime.busy {
		t.Fatal("runtime.busy = true, want false")
	}
}

func TestPreparePluginOverwriteRejectsStillBusyPlugin(t *testing.T) {
	t.Parallel()

	runtime := &fakePluginRuntime{busy: true, unloadResult: false}

	err := preparePluginOverwrite("sample", runtime)
	if err == nil {
		t.Fatal("preparePluginOverwrite() error = nil, want error")
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if !strings.Contains(err.Error(), "loaded plugin") {
		t.Fatalf("preparePluginOverwrite() error = %v, want loaded plugin", err)
	}
}

func homePluginTestArchive(t *testing.T, name string, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, errCreate := writer.Create(name)
	if errCreate != nil {
		t.Fatalf("zip.Create() error = %v", errCreate)
	}
	if _, errWrite := entry.Write([]byte(content)); errWrite != nil {
		t.Fatalf("zip.Write() error = %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("zip.Close() error = %v", errClose)
	}
	return buffer.Bytes()
}

func homePluginExtension(goos string) string {
	switch goos {
	case "windows":
		return ".dll"
	case "darwin":
		return ".dylib"
	default:
		return ".so"
	}
}

func pluginConfigFromYAML(t *testing.T, raw string) config.PluginInstanceConfig {
	t.Helper()
	var item config.PluginInstanceConfig
	if err := yaml.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	return item
}

func writePluginFile(t *testing.T, root string, id string, version string) string {
	t.Helper()
	dir := filepath.Join(root, CurrentPlatform().GOOS, CurrentPlatform().GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
	path := filepath.Join(dir, id+"-v"+version+pluginExtension(CurrentPlatform().GOOS))
	if err := os.WriteFile(path, []byte("plugin"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}
