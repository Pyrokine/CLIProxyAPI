package homeplugins

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"gopkg.in/yaml.v3"
)

const homePluginStoreHTTPTimeout = 30 * time.Second

type Platform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type PluginRuntime interface {
	PluginBusy(id string) bool
	UnloadPlugin(id string) bool
}

type PluginLoadInspector interface {
	PluginRegistered(id string) bool
}

type SyncReport struct {
	SchemaVersion int                   `json:"schema_version"`
	TaskID        uint                  `json:"task_id,omitempty"`
	Task          string                `json:"task"`
	NodeID        string                `json:"node_id,omitempty"`
	Status        string                `json:"status"`
	Phase         string                `json:"phase"`
	OK            bool                  `json:"ok"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Platform      Platform              `json:"platform"`
	Plugins       []PluginInstallStatus `json:"plugins"`
	Error         string                `json:"error,omitempty"`
}

type PluginInstallStatus struct {
	ID            string `json:"id"`
	Version       string `json:"version,omitempty"`
	ReleaseTag    string `json:"release_tag,omitempty"`
	Repository    string `json:"repository,omitempty"`
	InstallType   string `json:"install_type,omitempty"`
	InstallStatus string `json:"install_status"`
	LoadStatus    string `json:"load_status,omitempty"`
	Path          string `json:"path,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"`
	Overwritten   bool   `json:"overwritten,omitempty"`
	Error         string `json:"error,omitempty"`
}

type preparedPluginInstall struct {
	status            PluginInstallStatus
	artifact          sdkpluginstore.PreparedInstall
	configFingerprint string
	prepareError      error
}

// PreparedSync contains downloaded and verified Home plugin artifacts that have
// not changed the plugin directory.
type PreparedSync struct {
	report        SyncReport
	root          string
	platform      Platform
	installs      []preparedPluginInstall
	prepareErrors []error
	terminalError error
	enabled       bool
}

const (
	pluginTaskName         = "plugin-sync"
	pluginDeleteTaskName   = "plugin-delete"
	pluginTaskStatusOK     = "success"
	pluginTaskStatusError  = "failed"
	pluginTaskPhaseInstall = "install"
	pluginTaskPhaseLoad    = "load"
	pluginTaskPhaseDelete  = "delete"

	pluginInstallStatusInstalled = "installed"
	pluginInstallStatusSkipped   = "skipped"
	pluginInstallStatusFailed    = "failed"
	pluginInstallStatusDeleted   = "deleted"
	pluginInstallStatusMissing   = "missing"
	pluginLoadStatusLoaded       = "loaded"
	pluginLoadStatusFailed       = "failed"
)

func CurrentPlatform() Platform {
	return Platform{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
	}
}

func NormalizePlatform(platform Platform) Platform {
	goos := strings.ToLower(strings.TrimSpace(platform.GOOS))
	switch goos {
	case "mac", "macos", "osx":
		goos = "darwin"
	}
	goarch := strings.ToLower(strings.TrimSpace(platform.GOARCH))
	switch goarch {
	case "x64", "x86_64":
		goarch = "amd64"
	case "aarch64":
		goarch = "arm64"
	}
	return Platform{GOOS: goos, GOARCH: goarch}
}

func Sync(ctx context.Context, cfg *config.Config, pluginRuntime PluginRuntime) error {
	_, errSync := SyncPlatformWithReport(ctx, cfg, pluginRuntime, CurrentPlatform())
	return errSync
}

func SyncPlatform(ctx context.Context, cfg *config.Config, pluginRuntime PluginRuntime, platform Platform) error {
	_, errSync := SyncPlatformWithReport(ctx, cfg, pluginRuntime, platform)
	return errSync
}

func SyncWithReport(ctx context.Context, cfg *config.Config, pluginRuntime PluginRuntime) (SyncReport, error) {
	return SyncPlatformWithReport(ctx, cfg, pluginRuntime, CurrentPlatform())
}

func SyncPlatformWithReport(
	ctx context.Context,
	cfg *config.Config,
	pluginRuntime PluginRuntime,
	platform Platform,
) (SyncReport, error) {
	if errRecover := recoverPluginTransactions(cfg); errRecover != nil {
		report := newSyncReport(NormalizePlatform(platform))
		finishReport(&report, errRecover)
		return report, errRecover
	}
	prepared, _ := PrepareSyncPlatformWithReport(ctx, cfg, platform)
	return prepared.Apply(pluginRuntime)
}

func recoverPluginTransactions(cfg *config.Config) error {
	if cfg == nil || !cfg.Home.Enabled || !cfg.Plugins.Enabled {
		return nil
	}
	root := strings.TrimSpace(cfg.Plugins.Dir)
	if root == "" {
		root = "plugins"
	}
	fingerprints := make(map[string]string, len(cfg.Plugins.Configs))
	configured := make(map[string]struct{}, len(cfg.Plugins.Configs))
	for id, item := range cfg.Plugins.Configs {
		canonicalID := sdkpluginstore.CanonicalPluginID(id)
		if canonicalID == "" {
			return fmt.Errorf("home plugins: invalid plugin id %q", id)
		}
		if _, duplicate := configured[canonicalID]; duplicate {
			return fmt.Errorf("home plugins: duplicate plugin id %q", id)
		}
		fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
		if errFingerprint != nil {
			return fmt.Errorf("home plugins: fingerprint %s config: %w", canonicalID, errFingerprint)
		}
		configured[canonicalID] = struct{}{}
		fingerprints[canonicalID] = fingerprint
	}
	rootLock, errLock := sdkpluginstore.LockRoot(root)
	if errLock != nil {
		return fmt.Errorf("home plugins: lock plugin root for recovery: %w", errLock)
	}
	defer rootLock.Unlock()
	if errRecover := sdkpluginstore.RecoverInstallTransactionsLocked(root, fingerprints, rootLock); errRecover != nil {
		return fmt.Errorf("home plugins: recover install transactions: %w", errRecover)
	}
	if errRecover := sdkpluginstore.RecoverDeletionTransactionsLocked(root, configured, rootLock); errRecover != nil {
		return fmt.Errorf("home plugins: recover deletion transactions: %w", errRecover)
	}
	return nil
}

func PrepareSyncWithReport(ctx context.Context, cfg *config.Config) (PreparedSync, error) {
	return PrepareSyncPlatformWithReport(ctx, cfg, CurrentPlatform())
}

func PrepareSyncPlatformWithReport(
	ctx context.Context,
	cfg *config.Config,
	platform Platform,
) (PreparedSync, error) {
	prepared := PreparedSync{report: newSyncReport(platform)}
	if cfg == nil || !cfg.Home.Enabled || !cfg.Plugins.Enabled {
		return prepared, nil
	}
	platform = NormalizePlatform(platform)
	prepared.platform = platform
	prepared.report.Platform = platform
	if platform.GOOS == "" {
		prepared.terminalError = fmt.Errorf("home plugins: goos is required")
		finishReport(&prepared.report, prepared.terminalError)
		return prepared, prepared.terminalError
	}
	if platform.GOARCH == "" {
		prepared.terminalError = fmt.Errorf("home plugins: goarch is required")
		finishReport(&prepared.report, prepared.terminalError)
		return prepared, prepared.terminalError
	}
	prepared.enabled = true
	prepared.root = strings.TrimSpace(cfg.Plugins.Dir)
	if prepared.root == "" {
		prepared.root = "plugins"
	}
	client := newPluginStoreClient(cfg)
	ids := make([]string, 0, len(cfg.Plugins.Configs))
	for id := range cfg.Plugins.Configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := cfg.Plugins.Configs[id]
		if !pluginConfigEnabled(item) {
			continue
		}
		manifest, okManifest, errManifest := storeManifestFromPluginConfig(id, item)
		if errManifest != nil {
			prepared.installs = append(
				prepared.installs,
				preparedPluginInstall{
					status: PluginInstallStatus{
						ID:            sdkpluginstore.CanonicalPluginID(id),
						InstallStatus: pluginInstallStatusFailed,
						Error:         errManifest.Error(),
					},
					prepareError: errManifest,
				},
			)
			prepared.prepareErrors = append(prepared.prepareErrors, errManifest)
			continue
		}
		if !okManifest {
			continue
		}
		status := pluginStatusFromManifest(manifest)
		fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
		if errFingerprint != nil {
			errFingerprint = fmt.Errorf("home plugins: fingerprint %s config: %w", manifest.ID, errFingerprint)
			status.InstallStatus = pluginInstallStatusFailed
			status.Error = errFingerprint.Error()
			prepared.installs = append(
				prepared.installs,
				preparedPluginInstall{status: status, prepareError: errFingerprint},
			)
			prepared.prepareErrors = append(prepared.prepareErrors, errFingerprint)
			continue
		}
		artifact, errPrepare := client.PrepareManifest(
			ctx,
			manifest,
			sdkpluginstore.InstallOptions{
				PluginsDir: prepared.root,
				GOOS:       platform.GOOS,
				GOARCH:     platform.GOARCH,
			},
		)
		if errPrepare != nil {
			errPrepare = fmt.Errorf("home plugins: prepare %s: %w", strings.TrimSpace(manifest.ID), errPrepare)
			status.InstallStatus = pluginInstallStatusFailed
			status.Error = errPrepare.Error()
			prepared.installs = append(
				prepared.installs,
				preparedPluginInstall{status: status, prepareError: errPrepare},
			)
			prepared.prepareErrors = append(prepared.prepareErrors, errPrepare)
			continue
		}
		if errSpool := artifact.SpoolToTemp(); errSpool != nil {
			errSpool = fmt.Errorf("home plugins: spool %s: %w", strings.TrimSpace(manifest.ID), errSpool)
			status.InstallStatus = pluginInstallStatusFailed
			status.Error = errSpool.Error()
			prepared.installs = append(
				prepared.installs,
				preparedPluginInstall{status: status, prepareError: errSpool},
			)
			prepared.prepareErrors = append(prepared.prepareErrors, errSpool)
			continue
		}
		prepared.installs = append(
			prepared.installs,
			preparedPluginInstall{
				status:            status,
				artifact:          artifact,
				configFingerprint: fingerprint,
			},
		)
	}
	return prepared, errors.Join(prepared.prepareErrors...)
}

func (p *PreparedSync) Close() error {
	if p == nil {
		return nil
	}
	var cleanupErrors []error
	for index := range p.installs {
		if errClose := p.installs[index].artifact.Close(); errClose != nil {
			cleanupErrors = append(cleanupErrors, errClose)
		}
	}
	return errors.Join(cleanupErrors...)
}

func (p *PreparedSync) Apply(pluginRuntime PluginRuntime) (SyncReport, error) {
	if p == nil {
		return SyncReport{}, fmt.Errorf("home plugins: prepared sync is nil")
	}
	report := p.report
	if p.terminalError != nil {
		_ = p.Close()
		return report, p.terminalError
	}
	if !p.enabled {
		_ = p.Close()
		return report, nil
	}
	rootLock, errRootLock := sdkpluginstore.LockRoot(p.root)
	if errRootLock != nil {
		_ = p.Close()
		finishReport(&report, errRootLock)
		return report, errRootLock
	}
	defer rootLock.Unlock()

	syncErrors := append([]error(nil), p.prepareErrors...)
	for _, install := range p.installs {
		status := install.status
		if install.prepareError != nil {
			report.Plugins = append(report.Plugins, status)
			continue
		}
		result, errInstall := installPreparedArtifact(
			install.artifact,
			status.ID,
			install.configFingerprint,
			p.root,
			p.platform,
			pluginRuntime,
			rootLock,
		)
		if errInstall != nil {
			status.InstallStatus = pluginInstallStatusFailed
			status.Error = errInstall.Error()
			report.Plugins = append(report.Plugins, status)
			syncErrors = append(syncErrors, errInstall)
			continue
		}
		status.Path = strings.TrimSpace(result.Path)
		status.Skipped = result.Skipped
		status.Overwritten = result.Overwritten
		if result.Skipped {
			status.InstallStatus = pluginInstallStatusSkipped
		} else {
			status.InstallStatus = pluginInstallStatusInstalled
		}
		report.Plugins = append(report.Plugins, status)
	}
	if errCleanup := p.Close(); errCleanup != nil {
		syncErrors = append(syncErrors, fmt.Errorf("home plugins: clean prepared archives: %w", errCleanup))
	}
	errSync := errors.Join(syncErrors...)
	finishReport(&report, errSync)
	return report, errSync
}

func installPreparedArtifact(
	artifact sdkpluginstore.PreparedInstall,
	id string,
	configFingerprint string,
	root string,
	platform Platform,
	pluginRuntime PluginRuntime,
	rootLock *sdkpluginstore.RootLock,
) (sdkpluginstore.InstallResult, error) {
	id = sdkpluginstore.CanonicalPluginID(id)
	if id == "" {
		return sdkpluginstore.InstallResult{}, fmt.Errorf("home plugins: manifest plugin id is empty")
	}
	pluginIsBusy := func() bool {
		return pluginRuntime != nil && pluginRuntime.PluginBusy(id)
	}
	var installTransaction *sdkpluginstore.InstallTransaction
	result, errInstall := artifact.Install(
		sdkpluginstore.InstallOptions{
			PluginsDir:   root,
			GOOS:         platform.GOOS,
			GOARCH:       platform.GOARCH,
			PluginLoaded: pluginIsBusy,
			RootLock:     rootLock,
			BeforeWrite: func() error {
				return preparePluginOverwrite(id, pluginRuntime)
			},
			BeforePublish: func(path, version string, overwritten bool, digest string) error {
				transaction, errPrepare := sdkpluginstore.PrepareInstallTransactionLocked(
					root,
					id,
					path,
					version,
					digest,
					overwritten,
					rootLock,
				)
				if errPrepare != nil {
					return errPrepare
				}
				installTransaction = transaction
				return nil
			},
		},
	)
	if errInstall != nil {
		if installTransaction != nil {
			errInstall = errors.Join(errInstall, installTransaction.Rollback(result))
		}
		return result, fmt.Errorf("home plugins: install %s: %w", id, errInstall)
	}
	if installTransaction != nil {
		if errExpected := installTransaction.SetExpectedConfigFingerprint(configFingerprint); errExpected != nil {
			return result, fmt.Errorf(
				"home plugins: journal %s config: %w",
				id,
				errors.Join(errExpected, installTransaction.Rollback(result)),
			)
		}
	}
	return result, nil
}

func preparePluginOverwrite(id string, pluginRuntime PluginRuntime) error {
	if pluginRuntime == nil || !pluginRuntime.PluginBusy(id) {
		return nil
	}
	if pluginRuntime.UnloadPlugin(id) || !pluginRuntime.PluginBusy(id) {
		return nil
	}
	return sdkpluginstore.ErrLoadedPluginLocked
}

func DeleteWithReport(
	ctx context.Context,
	cfg *config.Config,
	pluginRuntime PluginRuntime,
	taskID uint,
	pluginID string,
) SyncReport {
	_ = ctx
	platform := CurrentPlatform()
	report := newSyncReport(platform)
	report.TaskID = taskID
	report.Task = pluginDeleteTaskName
	report.Phase = pluginTaskPhaseDelete
	pluginID = sdkpluginstore.CanonicalPluginID(pluginID)
	status := PluginInstallStatus{ID: pluginID}
	if cfg == nil {
		status.InstallStatus = pluginInstallStatusFailed
		status.Error = "home plugins: config is nil"
		report.Plugins = append(report.Plugins, status)
		finishReport(&report, errors.New(status.Error))
		return report
	}
	root := strings.TrimSpace(cfg.Plugins.Dir)
	if root == "" {
		root = "plugins"
	}
	path, deleted, errDelete := deletePluginArtifact(root, pluginID, pluginRuntime)
	status.Path = strings.TrimSpace(path)
	switch {
	case errDelete != nil:
		status.InstallStatus = pluginInstallStatusFailed
		status.Error = errDelete.Error()
	case deleted:
		status.InstallStatus = pluginInstallStatusDeleted
	default:
		status.InstallStatus = pluginInstallStatusMissing
	}
	report.Plugins = append(report.Plugins, status)
	finishReport(&report, errDelete)
	return report
}

func MarkLoadResults(report *SyncReport, inspector PluginLoadInspector) error {
	if report == nil {
		return nil
	}
	report.Phase = pluginTaskPhaseLoad
	var loadErrors []error
	for index := range report.Plugins {
		status := &report.Plugins[index]
		if status.InstallStatus == pluginInstallStatusFailed {
			if status.LoadStatus == "" {
				status.LoadStatus = pluginInstallStatusSkipped
			}
			if strings.TrimSpace(status.Error) != "" {
				loadErrors = append(loadErrors, errors.New(status.Error))
			} else {
				loadErrors = append(loadErrors, fmt.Errorf("home plugins: plugin %s install failed", status.ID))
			}
			continue
		}
		if inspector != nil && inspector.PluginRegistered(status.ID) {
			status.LoadStatus = pluginLoadStatusLoaded
			continue
		}
		status.LoadStatus = pluginLoadStatusFailed
		errLoad := fmt.Errorf("home plugins: plugin %s installed but not loaded", status.ID)
		if strings.TrimSpace(status.Error) == "" {
			status.Error = errLoad.Error()
		}
		loadErrors = append(loadErrors, errLoad)
	}
	errLoad := errors.Join(loadErrors...)
	finishReport(report, errLoad)
	return errLoad
}

func newSyncReport(platform Platform) SyncReport {
	now := time.Now().UTC()
	return SyncReport{
		SchemaVersion: 1,
		Task:          pluginTaskName,
		Status:        pluginTaskStatusOK,
		Phase:         pluginTaskPhaseInstall,
		OK:            true,
		StartedAt:     now,
		UpdatedAt:     now,
		Platform:      NormalizePlatform(platform),
		Plugins:       []PluginInstallStatus{},
	}
}

func finishReport(report *SyncReport, errTask error) {
	if report == nil {
		return
	}
	now := time.Now().UTC()
	report.FinishedAt = now
	report.UpdatedAt = now
	report.OK = errTask == nil
	if errTask != nil {
		report.Status = pluginTaskStatusError
		report.Error = errTask.Error()
		return
	}
	report.Status = pluginTaskStatusOK
	report.Error = ""
}

func pluginStatusFromManifest(manifest sdkpluginstore.Manifest) PluginInstallStatus {
	return PluginInstallStatus{
		ID:            sdkpluginstore.CanonicalPluginID(manifest.ID),
		Version:       strings.TrimSpace(manifest.Version),
		ReleaseTag:    strings.TrimSpace(manifest.ReleaseTag),
		Repository:    strings.TrimSpace(manifest.Repository),
		InstallType:   manifest.InstallType(),
		InstallStatus: pluginInstallStatusFailed,
	}
}

func storeManifestFromPluginConfig(id string, item config.PluginInstanceConfig) (sdkpluginstore.Manifest, bool, error) {
	if item.Raw.Kind == 0 {
		return sdkpluginstore.Manifest{}, false, nil
	}
	storeNode := yamlMappingValue(&item.Raw, "store")
	if storeNode == nil || storeNode.Kind == 0 {
		return sdkpluginstore.Manifest{}, false, nil
	}
	var manifest sdkpluginstore.Manifest
	if errDecode := storeNode.Decode(&manifest); errDecode != nil {
		return sdkpluginstore.Manifest{}, false, fmt.Errorf(
			"home plugins: decode store manifest for %s: %w", id, errDecode,
		)
	}
	canonicalID := sdkpluginstore.CanonicalPluginID(id)
	if strings.TrimSpace(manifest.ID) == "" {
		manifest.ID = canonicalID
	} else if sdkpluginstore.CanonicalPluginID(manifest.ID) != canonicalID {
		return sdkpluginstore.Manifest{}, false, fmt.Errorf(
			"home plugins: store manifest id %q does not match plugin config id %q", manifest.ID, id,
		)
	} else {
		manifest.ID = canonicalID
	}
	if errValidate := manifest.Validate(); errValidate != nil {
		return sdkpluginstore.Manifest{}, false, fmt.Errorf(
			"home plugins: invalid store manifest for %s: %w", id, errValidate,
		)
	}
	return manifest, true, nil
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode == nil || keyNode.Value != key {
			continue
		}
		return node.Content[i+1]
	}
	return nil
}

var newPluginStoreClient = func(cfg *config.Config) sdkpluginstore.Client {
	client := &http.Client{Timeout: homePluginStoreHTTPTimeout}
	var storeAuth []sdkpluginstore.AuthConfig
	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
		util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(cfg.ProxyURL)}, client)
	}
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}
	client.Transport = proxyutil.PublicHostTransport(client.Transport)
	if cfg != nil {
		storeAuth = cfg.Plugins.StoreAuth
	}
	return sdkpluginstore.NewClientWithAuth(client, "", storeAuth)
}

func pluginConfigEnabled(item config.PluginInstanceConfig) bool {
	return item.Enabled != nil && *item.Enabled
}
