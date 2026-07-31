package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	// pluginReleaseCacheTTL bounds how long a resolved latest release version is
	// reused before the GitHub API is queried again.
	pluginReleaseCacheTTL = 10 * time.Minute
	// pluginReleaseFailureCacheTTL throttles retries after a failed lookup so a
	// rate-limited or unreachable API is not hammered on every listing.
	pluginReleaseFailureCacheTTL         = 30 * time.Second
	pluginStoreHTTPTimeout               = 30 * time.Second
	pluginStoreInstallRequestMaxBytes    = 16 * 1024
	pluginStoreReleasePageSize           = 50
	pluginLatestVersionLookupConcurrency = 8
)

type pluginReleaseCacheEntry struct {
	version   string
	expiresAt time.Time
}

type pluginStoreListResponse struct {
	PluginsEnabled bool                   `json:"plugins_enabled"`
	PluginsDir     string                 `json:"plugins_dir"`
	Sources        []pluginStoreSource    `json:"sources"`
	SourceErrors   []pluginStoreSourceErr `json:"source_errors,omitempty"`
	Plugins        []pluginStoreListEntry `json:"plugins"`
}

type pluginStoreSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type pluginStoreSourceErr struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	SourceURL  string `json:"source_url"`
	Message    string `json:"message"`
}

type pluginStoreListEntry struct {
	StoreID             string                `json:"store_id"`
	SourceID            string                `json:"source_id"`
	SourceName          string                `json:"source_name"`
	SourceURL           string                `json:"source_url"`
	ID                  string                `json:"id"`
	Name                string                `json:"name"`
	Description         string                `json:"description"`
	Author              string                `json:"author"`
	Version             string                `json:"version"`
	Repository          string                `json:"repository"`
	InstallType         string                `json:"install_type"`
	AuthRequired        bool                  `json:"auth_required"`
	AuthConfigured      bool                  `json:"auth_configured"`
	Platforms           []pluginStorePlatform `json:"platforms,omitempty"`
	Logo                string                `json:"logo,omitempty"`
	Homepage            string                `json:"homepage,omitempty"`
	License             string                `json:"license,omitempty"`
	Tags                []string              `json:"tags,omitempty"`
	Installed           bool                  `json:"installed"`
	InstalledVersion    string                `json:"installed_version"`
	InstalledSourceID   string                `json:"installed_source_id,omitempty"`
	InstallSourceStatus string                `json:"install_source_status,omitempty"`
	Path                string                `json:"path"`
	Configured          bool                  `json:"configured"`
	Registered          bool                  `json:"registered"`
	Enabled             bool                  `json:"enabled"`
	EffectiveEnabled    bool                  `json:"effective_enabled"`
	UpdateAvailable     bool                  `json:"update_available"`
}

type pluginStorePlatform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type pluginInstallResponse struct {
	Status          string `json:"status"`
	SourceID        string `json:"source_id"`
	SourceName      string `json:"source_name"`
	SourceURL       string `json:"source_url"`
	ID              string `json:"id"`
	Version         string `json:"version"`
	InstallType     string `json:"install_type"`
	Path            string `json:"path"`
	PluginsEnabled  bool   `json:"plugins_enabled"`
	RestartRequired bool   `json:"restart_required"`
}

type pluginStoreReleaseEntry struct {
	TagName     string   `json:"tag_name"`
	Name        string   `json:"name"`
	PublishedAt string   `json:"published_at"`
	Prerelease  bool     `json:"prerelease"`
	HTMLURL     string   `json:"html_url"`
	AssetNames  []string `json:"asset_names"`
}

type pluginInstallRequest struct {
	Version string `json:"version"`
}

type pluginLocalStatus struct {
	Installed          bool
	InstalledVersion   string
	StoreManaged       bool
	InstalledSourceID  string
	InstalledSourceURL string
	Path               string
	Configured         bool
	Registered         bool
	Enabled            bool
	EffectiveEnabled   bool
}

type sourcedPlugin struct {
	source pluginstore.Source
	plugin pluginstore.Plugin
}

func (h *Handler) ListPluginStore(c *gin.Context) {
	pluginsEnabled, pluginsDir, proxyURL, sourceConfigs, storeAuth, configs, host, _ := h.pluginStoreSnapshot()
	sources, errSources := h.pluginStoreSources(sourceConfigs)
	if errSources != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_store_source_invalid", "message": errSources.Error()},
		)
		return
	}
	plugins, sourceErrors := h.fetchSourcedPlugins(c.Request.Context(), proxyURL, storeAuth, sources)
	if len(plugins) == 0 && len(sourceErrors) > 0 {
		c.JSON(
			http.StatusBadGateway, gin.H{"error": "plugin_store_registry_failed", "message": sourceErrors[0].Message},
		)
		return
	}
	statuses, errStatus := pluginLocalStatuses(pluginsEnabled, pluginsDir, configs, host)
	if errStatus != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_discovery_failed", "message": errStatus.Error()})
		return
	}

	latestInput := make([]pluginstore.Plugin, 0, len(plugins))
	for _, item := range plugins {
		latestInput = append(latestInput, item.plugin)
	}
	client := h.newPluginStoreClient(proxyURL, "", storeAuth)
	latestVersions := h.latestPluginVersions(c.Request.Context(), client, latestInput)
	pluginSourceCounts := make(map[string]int, len(plugins))
	for _, item := range plugins {
		pluginSourceCounts[item.plugin.ID]++
	}

	entries := make([]pluginStoreListEntry, 0, len(plugins))
	for index, item := range plugins {
		plugin := item.plugin
		status := statuses[plugin.ID]
		installedSourceID, installSourceStatus, sourceAllowsUpdate := pluginStoreInstallSourceStatus(
			status,
			sources,
			item.source.ID,
			pluginSourceCounts[plugin.ID],
		)
		installedVersion := status.InstalledVersion
		// Fall back to the registry version when the latest release is unknown.
		storeVersion := plugin.Version
		if latestVersions[index] != "" {
			storeVersion = latestVersions[index]
		}
		entries = append(
			entries, pluginStoreListEntry{
				StoreID:             pluginStoreText(item.source.ID + "/" + plugin.ID),
				SourceID:            pluginStoreText(item.source.ID),
				SourceName:          pluginStoreText(item.source.Name),
				SourceURL:           pluginStoreText(item.source.URL),
				ID:                  pluginStoreText(plugin.ID),
				Name:                pluginStoreText(plugin.Name),
				Description:         pluginStoreText(plugin.Description),
				Author:              pluginStoreText(plugin.Author),
				Version:             pluginStoreText(storeVersion),
				Repository:          pluginStoreText(plugin.Repository),
				InstallType:         pluginStoreText(pluginstore.PluginInstallType(plugin)),
				AuthRequired:        plugin.AuthRequired,
				AuthConfigured:      pluginAuthConfigured(item.source, plugin, storeAuth),
				Platforms:           sanitizePluginStorePlatforms(pluginstore.PluginPlatforms(plugin)),
				Logo:                pluginStoreText(plugin.Logo),
				Homepage:            pluginStoreText(plugin.Homepage),
				License:             pluginStoreText(plugin.License),
				Tags:                pluginStoreTextList(plugin.Tags),
				Installed:           status.Installed,
				InstalledVersion:    pluginStoreText(installedVersion),
				InstalledSourceID:   pluginStoreText(installedSourceID),
				InstallSourceStatus: pluginStoreText(installSourceStatus),
				Path:                pluginStoreText(status.Path),
				Configured:          status.Configured,
				Registered:          status.Registered,
				Enabled:             status.Enabled,
				EffectiveEnabled:    status.EffectiveEnabled,
				UpdateAvailable:     sourceAllowsUpdate && pluginstore.UpdateAvailable(installedVersion, storeVersion),
			},
		)
	}

	c.JSON(
		http.StatusOK, pluginStoreListResponse{
			PluginsEnabled: pluginsEnabled,
			PluginsDir:     pluginStoreText(pluginsDir),
			Sources:        sanitizePluginStoreSources(sources),
			SourceErrors:   sanitizePluginStoreSourceErrors(sourceErrors),
			Plugins:        entries,
		},
	)
}

func (h *Handler) ListPluginStoreReleases(c *gin.Context) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	_, _, proxyURL, sourceConfigs, storeAuth, _, _, _ := h.pluginStoreSnapshot()
	sources, errSources := h.pluginStoreSources(sourceConfigs)
	if errSources != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_store_source_invalid", "message": errSources.Error()},
		)
		return
	}
	_, plugin, client, okPlugin := h.findPluginStoreInstallTarget(
		c.Request.Context(), proxyURL, storeAuth, sources, id, c.Query("source"), c,
	)
	if !okPlugin {
		return
	}
	if pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "plugin_release_versions_unsupported", "message": "plugin does not use GitHub releases"},
		)
		return
	}

	releases, errReleases := client.FetchReleases(c.Request.Context(), plugin, pluginStoreReleasePageSize)
	if errReleases != nil {
		c.JSON(
			http.StatusBadGateway,
			gin.H{"error": "plugin_release_versions_failed", "message": errReleases.Error()},
		)
		return
	}
	entries := make([]pluginStoreReleaseEntry, 0, len(releases))
	for _, release := range releases {
		if release.Draft {
			continue
		}
		assetNames := make([]string, 0, len(release.Assets))
		for _, asset := range release.Assets {
			if name := strings.TrimSpace(asset.Name); name != "" {
				assetNames = append(assetNames, pluginStoreText(name))
			}
		}
		entries = append(
			entries,
			pluginStoreReleaseEntry{
				TagName:     pluginStoreText(strings.TrimSpace(release.TagName)),
				Name:        pluginStoreText(strings.TrimSpace(release.Name)),
				PublishedAt: pluginStoreText(strings.TrimSpace(release.PublishedAt)),
				Prerelease:  release.Prerelease,
				HTMLURL:     pluginStoreText(strings.TrimSpace(release.HTMLURL)),
				AssetNames:  assetNames,
			},
		)
	}
	c.JSON(http.StatusOK, entries)
}

func (h *Handler) InstallPluginFromStore(c *gin.Context) {
	h.installPluginFromStore(c, runtime.GOOS, runtime.GOARCH)
}

func (h *Handler) installPluginFromStore(c *gin.Context, goos, goarch string) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	requestedVersion, errVersionRequest := pluginInstallRequestedVersion(c)
	if errVersionRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": errVersionRequest.Error()})
		return
	}
	installCtx := c.Request.Context()
	pluginsEnabled, pluginsDir, proxyURL, sourceConfigs, storeAuth, configs, _, configGeneration :=
		h.pluginStoreSnapshot()
	sources, errSources := h.pluginStoreSources(sourceConfigs)
	if errSources != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_store_source_invalid", "message": errSources.Error()},
		)
		return
	}
	source, plugin, client, okPlugin := h.findPluginStoreInstallTarget(
		installCtx, proxyURL, storeAuth, sources, id, c.Query("source"), c,
	)
	if !okPlugin {
		return
	}
	if !validatePluginStoreInstallSource(c, configs, sources, id, source.ID) {
		return
	}

	prepareOptions := pluginstore.InstallOptions{GOOS: goos, GOARCH: goarch}
	var manifest pluginstore.Manifest
	var prepared pluginstore.PreparedInstall
	var errPrepare error
	switch pluginstore.PluginInstallType(plugin) {
	case pluginstore.InstallTypeDirect:
		manifest, errPrepare = pluginStoreDirectManifest(source, plugin, requestedVersion)
		if errPrepare != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_manifest_invalid", "message": errPrepare.Error()})
			return
		}
		prepared, errPrepare = client.PrepareManifest(installCtx, manifest, prepareOptions)
	case pluginstore.InstallTypeGitHubRelease:
		prepared, errPrepare = preparePluginStoreGitHubRelease(
			installCtx, client, plugin, requestedVersion, prepareOptions,
		)
	default:
		c.JSON(
			http.StatusBadGateway, gin.H{
				"error":   "plugin_manifest_invalid",
				"message": fmt.Sprintf("unsupported install type %q", plugin.Install.Type),
			},
		)
		return
	}
	if errPrepare != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_install_failed", "message": errPrepare.Error()})
		return
	}

	h.commitMu.Lock()
	commitMuHeld := true
	releaseCommitMu := func() {
		if commitMuHeld {
			h.commitMu.Unlock()
			commitMuHeld = false
		}
	}
	defer releaseCommitMu()
	rootLock, errRootLock := pluginstore.LockRoot(pluginsDir)
	if errRootLock != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_install_failed", "message": errRootLock.Error()})
		return
	}
	rootLockHeld := true
	releaseRootLock := func() {
		if rootLockHeld {
			rootLock.Unlock()
			rootLockHeld = false
		}
	}
	defer releaseRootLock()
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config_unavailable"})
		return
	}
	currentSourceConfigs := append([]string(nil), h.cfg.Plugins.StoreSources...)
	currentStoreAuth := pluginStoreAuthConfigs(h.cfg.Plugins.StoreAuth)
	currentProxyURL := strings.TrimSpace(h.cfg.ProxyURL)
	currentPluginsDir := normalizedPluginsDir(h.cfg.Plugins.Dir)
	currentSources, errCurrentSources := h.pluginStoreSources(currentSourceConfigs)
	if errCurrentSources != nil || h.configGeneration != configGeneration ||
		currentProxyURL != proxyURL || currentPluginsDir != pluginsDir ||
		!reflect.DeepEqual(currentSourceConfigs, sourceConfigs) ||
		!reflect.DeepEqual(currentStoreAuth, storeAuth) {
		h.mu.Unlock()
		c.JSON(
			http.StatusConflict,
			gin.H{
				"error":   "plugin_store_config_changed",
				"message": "plugin store configuration changed while the artifact was prepared; retry install",
			},
		)
		return
	}
	currentSource, sourceUnchanged := pluginStoreSourceByID(currentSources, source.ID)
	if !sourceUnchanged || currentSource.URL != source.URL {
		h.mu.Unlock()
		c.JSON(
			http.StatusConflict,
			gin.H{
				"error":   "plugin_store_config_changed",
				"message": "plugin store source changed while the artifact was prepared; retry install",
			},
		)
		return
	}
	if !validatePluginStoreInstallSource(c, h.cfg.Plugins.Configs, currentSources, id, source.ID) {
		h.mu.Unlock()
		return
	}

	pluginsEnabled = h.cfg.Plugins.Enabled
	pluginsDir = currentPluginsDir
	host := h.pluginHost
	previousConfig := pluginRuntimeConfigSnapshot(h.cfg)
	previousGeneration := h.configGeneration
	var installTransaction *pluginstore.InstallTransaction
	runtimePrepared := false
	pluginIsBusy := func() bool { return pluginBusy(host, id) }
	installOptions := pluginstore.InstallOptions{
		PluginsDir:   pluginsDir,
		GOOS:         goos,
		GOARCH:       goarch,
		PluginLoaded: pluginIsBusy,
		BeforePublish: func(path, version string, overwritten bool, digest string) error {
			transaction, errTransaction := pluginstore.PrepareInstallTransactionLocked(
				pluginsDir, id, path, version, digest, overwritten, rootLock,
			)
			if errTransaction != nil {
				return errTransaction
			}
			installTransaction = transaction
			return nil
		},
		RootLock: rootLock,
		BeforeWrite: func() error {
			wasBusy := pluginBusy(host, id)
			if errOverwrite := preparePluginStoreOverwrite(id, host); errOverwrite != nil {
				return errOverwrite
			}
			runtimePrepared = wasBusy
			return nil
		},
	}
	rollbackInstall := func(result pluginstore.InstallResult) {
		if installTransaction != nil {
			if errRollback := installTransaction.Rollback(result); errRollback != nil {
				log.WithError(errRollback).WithField("plugin_id", id).Error("pluginstore: failed to roll back artifact")
			}
		}
	}
	restoreRuntime := func() {
		if runtimePrepared {
			previousCommit := ConfigCommit{Config: previousConfig, Generation: previousGeneration}
			if errSync := h.runPluginRuntimeSyncForCommit(
				context.Background(), previousCommit, h.pluginRuntimeSync,
			); errSync != nil {
				log.WithError(errSync).WithField("plugin_id", id).Error("pluginstore: failed to restore runtime")
			}
		}
	}
	abortInstall := func(result pluginstore.InstallResult) {
		rollbackInstall(result)
		releaseRootLock()
		releaseCommitMu()
		restoreRuntime()
	}

	result, errInstall := prepared.Install(installOptions)
	if errInstall != nil {
		h.mu.Unlock()
		abortInstall(result)
		if errors.Is(errInstall, pluginstore.ErrLoadedPluginLocked) {
			c.JSON(
				http.StatusConflict, gin.H{
					"error":            "plugin_update_requires_restart",
					"message":          "loaded plugin cannot be overwritten while the server is running",
					"restart_required": true,
				},
			)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_install_failed", "message": errInstall.Error()})
		return
	}
	if manifest.ID == "" {
		manifest, errPrepare = pluginStoreManifestForInstall(source, plugin, result)
		if errPrepare != nil {
			h.mu.Unlock()
			abortInstall(result)
			c.JSON(
				http.StatusInternalServerError, gin.H{
					"error": "plugin_manifest_failed",
					"message": fmt.Sprintf(
						"plugin file installed at %s but creating store manifest failed: %s", result.Path,
						errPrepare.Error(),
					),
					"path": result.Path,
				},
			)
			return
		}
	}
	commit, callback, okCommit := h.mutateConfigLocked(
		c, func(next *config.Config) error {
			if errEnable := enablePluginConfig(next, id, manifest); errEnable != nil {
				return newConfigMutationError(
					http.StatusInternalServerError,
					fmt.Sprintf("failed to enable installed plugin: %v", errEnable),
				)
			}
			if installTransaction != nil {
				_, installedConfig, _ := pluginConfigByID(next.Plugins.Configs, id)
				fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(installedConfig)
				if errFingerprint != nil {
					return newConfigMutationError(
						http.StatusInternalServerError,
						fmt.Sprintf("failed to fingerprint installed plugin config: %v", errFingerprint),
					)
				}
				if errExpected := installTransaction.SetExpectedConfigFingerprint(fingerprint); errExpected != nil {
					return newConfigMutationError(
						http.StatusInternalServerError,
						fmt.Sprintf("failed to journal installed plugin config: %v", errExpected),
					)
				}
			}
			return nil
		},
	)
	if !okCommit {
		h.mu.Unlock()
		abortInstall(result)
		return
	}
	if installTransaction != nil {
		if errCommitArtifact := installTransaction.Commit(); errCommitArtifact != nil {
			log.WithError(errCommitArtifact).WithField("plugin_id", id).Warn(
				"pluginstore: failed to remove install transaction",
			)
		}
	}
	h.mu.Unlock()
	releaseRootLock()
	releaseCommitMu()
	runtimeCtx := context.WithoutCancel(c.Request.Context())
	if !h.notifyConfigCommittedContext(c, runtimeCtx, callback, commit) {
		return
	}
	if callback == nil {
		if errSync := h.runPluginRuntimeSyncForCommit(
			runtimeCtx, commit, h.pluginRuntimeSync,
		); errSync != nil {
			log.WithError(errSync).WithField("plugin_id", id).Error("pluginstore: installed plugin runtime sync failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin_runtime_sync_failed"})
			return
		}
	}
	log.WithFields(
		log.Fields{
			"plugin_id":    result.ID,
			"plugin_name":  plugin.Name,
			"source_id":    source.ID,
			"version":      result.Version,
			"install_type": result.InstallType,
			"path":         result.Path,
			"overwritten":  result.Overwritten,
		},
	).Info("pluginstore: plugin installed")

	c.JSON(
		http.StatusOK, pluginInstallResponse{
			Status:          "installed",
			SourceID:        pluginStoreText(source.ID),
			SourceName:      pluginStoreText(source.Name),
			SourceURL:       pluginStoreText(source.URL),
			ID:              pluginStoreText(result.ID),
			Version:         pluginStoreText(result.Version),
			InstallType:     pluginStoreText(result.InstallType),
			Path:            pluginStoreText(result.Path),
			PluginsEnabled:  pluginsEnabled,
			RestartRequired: false,
		},
	)
}

func (h *Handler) DeletePlugin(c *gin.Context) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	if h == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
		return
	}

	h.commitMu.Lock()
	commitMuHeld := true
	releaseCommitMu := func() {
		if commitMuHeld {
			h.commitMu.Unlock()
			commitMuHeld = false
		}
	}
	defer releaseCommitMu()

	h.mu.RLock()
	if h.cfg == nil {
		h.mu.RUnlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
		return
	}
	pluginsDir := normalizedPluginsDir(h.cfg.Plugins.Dir)
	_, item, configured := pluginConfigByID(h.cfg.Plugins.Configs, id)
	host := h.pluginHost
	h.mu.RUnlock()
	var runtimeIdentity pluginhost.PluginRuntimeIdentity
	if host != nil {
		runtimeIdentity = host.PluginRuntimeIdentity(id)
	}

	rootLock, errRootLock := pluginstore.LockRoot(pluginsDir)
	if errRootLock != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_delete_failed", "message": errRootLock.Error()})
		return
	}
	rootLockHeld := true
	releaseRootLock := func() {
		if rootLockHeld {
			rootLock.Unlock()
			rootLockHeld = false
		}
	}
	acquireRootLock := func() error {
		if rootLockHeld {
			return nil
		}
		lock, errLock := pluginstore.LockRoot(pluginsDir)
		if errLock != nil {
			return errLock
		}
		rootLock = lock
		rootLockHeld = true
		return nil
	}
	defer releaseRootLock()

	var desiredVersions map[string]string
	if configured {
		desiredVersions = pluginStoreDesiredVersions(map[string]config.PluginInstanceConfig{id: item})
	}
	path, errPath := pluginFilePath(pluginsDir, id, desiredVersions)
	if errPath != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_discovery_failed", "message": errPath.Error()})
		return
	}
	artifactPaths, errArtifacts := pluginstore.FindPluginArtifacts(pluginsDir, id)
	if errArtifacts != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_discovery_failed", "message": errArtifacts.Error()},
		)
		return
	}
	if path == "" && len(artifactPaths) > 0 {
		path = artifactPaths[0]
	}
	if len(artifactPaths) == 0 && !configured {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
		return
	}

	deletionTransaction, errPrepare := pluginstore.PrepareDeletionTransactionLocked(
		pluginsDir,
		id,
		artifactPaths,
		rootLock,
	)
	if errPrepare != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_delete_failed", "message": errPrepare.Error()},
		)
		return
	}
	defer deletionTransaction.Deactivate()

	runtimeUnloaded := false
	rollbackDelete := func() error {
		var rollbackErrors []error
		if errLock := acquireRootLock(); errLock != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("lock root for rollback: %w", errLock))
		} else if errRollback := deletionTransaction.Rollback(); errRollback != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore deleted artifacts: %w", errRollback))
		}
		releaseRootLock()
		releaseCommitMu()
		return errors.Join(rollbackErrors...)
	}
	respondRollbackFailure := func(status int, code string, cause error) {
		if errRollback := rollbackDelete(); errRollback != nil {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{
					"error":            "plugin_delete_rollback_failed",
					"message":          errors.Join(cause, errRollback).Error(),
					"restart_required": true,
				},
			)
			return
		}
		c.JSON(status, gin.H{"error": code, "message": cause.Error()})
	}
	commitDelete := func() error {
		if errLock := acquireRootLock(); errLock != nil {
			return errLock
		}
		defer releaseRootLock()
		return deletionTransaction.Commit()
	}
	if errHide := deletionTransaction.Hide(); errHide != nil {
		respondRollbackFailure(http.StatusInternalServerError, "plugin_delete_failed", errHide)
		return
	}
	remainingArtifacts, errRescan := pluginstore.FindPluginArtifacts(pluginsDir, id)
	if errRescan != nil || len(remainingArtifacts) > 0 {
		if errRescan == nil {
			errRescan = fmt.Errorf("plugin artifact appeared during deletion: %s", remainingArtifacts[0])
		}
		respondRollbackFailure(http.StatusConflict, "plugin_delete_conflict", errRescan)
		return
	}
	if errMark := deletionTransaction.MarkCommitted(); errMark != nil {
		respondRollbackFailure(http.StatusInternalServerError, "plugin_delete_failed", errMark)
		return
	}
	fileDeleted := deletionTransaction.Removed()
	var runtimeSyncErr error
	var runtimeUnloadErr error
	if configured {
		h.mu.Lock()
		commit, callback, okCommit := h.mutateConfigLocked(
			c, func(next *config.Config) error {
				deletePluginConfigByID(next.Plugins.Configs, id)
				return nil
			},
		)
		h.mu.Unlock()
		if !okCommit {
			if errRollback := rollbackDelete(); errRollback != nil {
				log.WithError(errRollback).WithField("plugin_id", id).Error(
					"pluginstore: failed to rollback deletion after config persistence failure",
				)
			}
			return
		}
		releaseRootLock()
		releaseCommitMu()
		commit.DeferPluginRuntimeSync = true
		if callback != nil && h.configCommitCurrent(commit) {
			runtimeCtx := context.WithoutCancel(c.Request.Context())
			runtimeSyncErr = invokeConfigCommitted(runtimeCtx, callback, commit)
		}
	} else {
		releaseRootLock()
		releaseCommitMu()
	}

	h.commitMu.Lock()
	commitMuHeld = true
	h.mu.RLock()
	currentPluginsDir := ""
	configuredAgain := false
	if h.cfg != nil {
		currentPluginsDir = normalizedPluginsDir(h.cfg.Plugins.Dir)
		_, _, configuredAgain = pluginConfigByID(h.cfg.Plugins.Configs, id)
	}
	h.mu.RUnlock()
	if currentPluginsDir != pluginsDir || configuredAgain {
		errRollback := rollbackDelete()
		if errRollback != nil {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{
					"error":            "plugin_delete_rollback_failed",
					"message":          errRollback.Error(),
					"restart_required": true,
				},
			)
			return
		}
		c.JSON(
			http.StatusConflict,
			gin.H{"error": "plugin_delete_superseded", "message": "plugin configuration changed during deletion"},
		)
		return
	}
	errCommitArtifact := commitDelete()
	if errCommitArtifact == nil && pluginBusy(host, id) {
		runtimeUnloaded, runtimeUnloadErr = host.UnloadPluginIfCurrentWithError(runtimeIdentity)
	}
	releaseCommitMu()
	if errCommitArtifact != nil {
		log.WithError(errCommitArtifact).WithField("plugin_id", id).Error(
			"pluginstore: failed to commit artifact deletion",
		)
		c.JSON(
			http.StatusInternalServerError,
			gin.H{
				"error":            "plugin_delete_failed",
				"message":          errors.Join(errCommitArtifact, runtimeSyncErr, runtimeUnloadErr).Error(),
				"restart_required": runtimeUnloadErr != nil || pluginBusy(host, id),
			},
		)
		return
	}
	if runtimeUnloadErr != nil || pluginBusy(host, id) {
		c.JSON(
			http.StatusConflict,
			gin.H{
				"error": "plugin_delete_requires_restart",
				"message": errors.Join(
					fmt.Errorf("plugin deletion committed but the loaded runtime could not be fully released"),
					runtimeUnloadErr,
				).Error(),
				"restart_required": true,
			},
		)
		return
	}
	if runtimeSyncErr != nil {
		c.JSON(
			http.StatusServiceUnavailable,
			gin.H{
				"error":            "plugin_runtime_sync_failed",
				"message":          runtimeSyncErr.Error(),
				"restart_required": false,
			},
		)
		return
	}

	c.JSON(
		http.StatusOK, gin.H{
			"status":             "deleted",
			"id":                 pluginStoreText(id),
			"path":               pluginStoreText(path),
			"file_deleted":       fileDeleted,
			"configured_removed": configured,
			"runtime_unloaded":   runtimeUnloaded,
			"restart_required":   false,
		},
	)
}

func pluginStoreDirectManifest(
	source pluginstore.Source,
	plugin pluginstore.Plugin,
	requestedVersion string,
) (pluginstore.Manifest, error) {
	version := normalizePluginStoreRequestedVersion(requestedVersion)
	if version == "" {
		version = normalizePluginStoreRequestedVersion(plugin.Version)
	}
	if normalizePluginStoreRequestedVersion(plugin.Version) == version {
		plugin.Version = version
		return pluginstore.ManifestFromPlugin(source, plugin)
	}
	for _, candidate := range plugin.Versions {
		if normalizePluginStoreRequestedVersion(candidate.Version) != version {
			continue
		}
		plugin.Version = version
		plugin.Install = candidate.Install
		if strings.TrimSpace(plugin.Install.Type) == "" {
			plugin.Install.Type = pluginstore.InstallTypeDirect
		}
		return pluginstore.ManifestFromPlugin(source, plugin)
	}
	return pluginstore.Manifest{}, fmt.Errorf("direct plugin version %q not found", version)
}

func preparePluginStoreGitHubRelease(
	ctx context.Context,
	client pluginstore.Client,
	plugin pluginstore.Plugin,
	requestedVersion string,
	options pluginstore.InstallOptions,
) (pluginstore.PreparedInstall, error) {
	version := normalizePluginStoreRequestedVersion(requestedVersion)
	if version == "" {
		return client.Prepare(ctx, plugin, options)
	}
	tags := pluginStoreReleaseTagCandidates(requestedVersion)
	errs := make([]error, 0, len(tags))
	for _, tag := range tags {
		prepared, errPrepare := client.PrepareVersion(ctx, plugin, tag, version, options)
		if errPrepare == nil {
			return prepared, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", tag, errPrepare))
	}
	return pluginstore.PreparedInstall{}, fmt.Errorf("prepare release by tag: %w", errors.Join(errs...))
}

func pluginStoreManifestForInstall(
	source pluginstore.Source,
	plugin pluginstore.Plugin,
	result pluginstore.InstallResult,
) (pluginstore.Manifest, error) {
	installType := strings.TrimSpace(result.InstallType)
	if installType == "" {
		installType = pluginstore.PluginInstallType(plugin)
	}
	switch installType {
	case pluginstore.InstallTypeDirect:
		plugin.Version = strings.TrimSpace(result.Version)
		plugin.Install = pluginstore.NormalizeInstallPlan(plugin.Install)
		return pluginstore.ManifestFromPlugin(source, plugin)
	case pluginstore.InstallTypeGitHubRelease:
		releaseTag := strings.TrimSpace(result.ReleaseTag)
		if releaseTag == "" {
			return pluginstore.Manifest{}, fmt.Errorf("release tag is required")
		}
		return pluginstore.ManifestFromRelease(source, plugin, pluginstore.Release{TagName: releaseTag})
	default:
		return pluginstore.Manifest{}, fmt.Errorf("unsupported install type %q", result.InstallType)
	}
}

func pluginInstallRequestedVersion(c *gin.Context) (string, error) {
	requestedVersion := strings.TrimSpace(c.Query("version"))
	if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return requestedVersion, nil
	}
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, pluginStoreInstallRequestMaxBytes+1))
	if errRead != nil {
		return "", fmt.Errorf("read install request: %w", errRead)
	}
	if len(body) > pluginStoreInstallRequestMaxBytes {
		return "", fmt.Errorf(
			"install request body exceeds maximum allowed size of %d bytes", pluginStoreInstallRequestMaxBytes,
		)
	}
	if strings.TrimSpace(string(body)) == "" {
		return requestedVersion, nil
	}
	var req pluginInstallRequest
	if errDecode := json.Unmarshal(body, &req); errDecode != nil {
		return "", fmt.Errorf("decode install request: %w", errDecode)
	}
	bodyVersion := strings.TrimSpace(req.Version)
	if requestedVersion == "" {
		return bodyVersion, nil
	}
	if bodyVersion == "" || normalizePluginStoreRequestedVersion(bodyVersion) == normalizePluginStoreRequestedVersion(requestedVersion) {
		return requestedVersion, nil
	}
	return "", fmt.Errorf("version query %q does not match request body version %q", requestedVersion, bodyVersion)
}

func pluginStoreReleaseTagCandidates(version string) []string {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(version), "v") {
		return []string{version, strings.TrimSpace(version[1:])}
	}
	return []string{version, "v" + version}
}

func normalizePluginStoreRequestedVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(strings.ToLower(version), "v") {
		return strings.TrimSpace(version[1:])
	}
	return version
}

// enablePluginConfig sets plugins.configs.<id>.enabled and store while
// preserving the rest of the plugin's raw configuration.
func enablePluginConfig(cfg *config.Config, id string, storeManifest pluginstore.Manifest) error {
	ensurePluginConfigMap(cfg)
	_, current, _ := pluginConfigByID(cfg.Plugins.Configs, id)
	node := pluginConfigNode(current)
	storeNode, errStoreNode := pluginStoreManifestYAMLNode(storeManifest)
	if errStoreNode != nil {
		return errStoreNode
	}
	setYAMLMappingValue(node, "enabled", boolYAMLNode(true))
	setYAMLMappingValue(node, "store", storeNode)
	updated, errConfig := pluginInstanceConfigFromNode(node)
	if errConfig != nil {
		return fmt.Errorf("decode plugin config: %w", errConfig)
	}
	setPluginConfigByID(cfg.Plugins.Configs, id, updated)
	return nil
}

func pluginStoreManifestYAMLNode(manifest pluginstore.Manifest) (*yaml.Node, error) {
	var node yaml.Node
	if errEncode := node.Encode(manifest); errEncode != nil {
		return nil, fmt.Errorf("encode store manifest: %w", errEncode)
	}
	return &node, nil
}

func (h *Handler) pluginStoreSnapshot() (
	bool,
	string,
	string,
	[]string,
	[]pluginstore.AuthConfig,
	map[string]config.PluginInstanceConfig,
	pluginRuntimeHost,
	uint64,
) {
	if h == nil {
		return false, "plugins", "", nil, nil, map[string]config.PluginInstanceConfig{}, nil, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return false, "plugins", "", nil, nil, map[string]config.PluginInstanceConfig{}, nil, 0
	}
	pluginsEnabled := h.cfg.Plugins.Enabled
	pluginsDir := normalizedPluginsDir(h.cfg.Plugins.Dir)
	proxyURL := strings.TrimSpace(h.cfg.ProxyURL)
	sourceConfigs := append([]string(nil), h.cfg.Plugins.StoreSources...)
	storeAuth := pluginStoreAuthConfigs(h.cfg.Plugins.StoreAuth)
	configs := make(map[string]config.PluginInstanceConfig, len(h.cfg.Plugins.Configs))
	for id, item := range h.cfg.Plugins.Configs {
		configs[id] = item
	}
	return pluginsEnabled, pluginsDir, proxyURL, sourceConfigs, storeAuth, configs, h.pluginHost, h.configGeneration
}

func pluginStoreAuthConfigs(auth []sdkpluginstore.AuthConfig) []pluginstore.AuthConfig {
	if len(auth) == 0 {
		return nil
	}
	out := make([]pluginstore.AuthConfig, len(auth))
	for i := range auth {
		out[i] = pluginstore.AuthConfig{
			Match:          auth[i].Match,
			ApplyTo:        append([]string(nil), auth[i].ApplyTo...),
			Type:           auth[i].Type,
			TokenEnv:       auth[i].TokenEnv,
			UsernameEnv:    auth[i].UsernameEnv,
			PasswordEnv:    auth[i].PasswordEnv,
			HeaderName:     auth[i].HeaderName,
			HeaderValueEnv: auth[i].HeaderValueEnv,
			AllowInsecure:  auth[i].AllowInsecure,
		}
	}
	return out
}

func (h *Handler) pluginStoreSources(sourceConfigs []string) ([]pluginstore.Source, error) {
	if h != nil && strings.TrimSpace(h.pluginStoreRegistryURL) != "" {
		source := pluginstore.DefaultSource()
		source.URL = strings.TrimSpace(h.pluginStoreRegistryURL)
		return []pluginstore.Source{source}, nil
	}
	return pluginstore.NormalizeSources(sourceConfigs)
}

func (h *Handler) newPluginStoreClient(
	proxyURL string,
	registryURL string,
	storeAuth []pluginstore.AuthConfig,
) pluginstore.Client {
	registryURL = strings.TrimSpace(registryURL)
	var httpClient pluginstore.HTTPDoer
	if h != nil {
		httpClient = h.pluginStoreHTTPClient
	}
	if registryURL == "" {
		registryURL = pluginstore.DefaultRegistryURL
	}
	if httpClient != nil {
		return pluginstore.Client{HTTPClient: httpClient, RegistryURL: registryURL, Auth: storeAuth}
	}
	client := &http.Client{Timeout: pluginStoreHTTPTimeout}
	if strings.TrimSpace(proxyURL) != "" {
		util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}, client)
	}
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}
	client.Transport = proxyutil.PublicHostTransport(client.Transport)
	return pluginstore.Client{HTTPClient: client, RegistryURL: registryURL, Auth: storeAuth}
}

func (h *Handler) fetchSourcedPlugins(
	ctx context.Context,
	proxyURL string,
	storeAuth []pluginstore.AuthConfig,
	sources []pluginstore.Source,
) ([]sourcedPlugin, []pluginStoreSourceErr) {
	plugins := make([]sourcedPlugin, 0)
	sourceErrors := make([]pluginStoreSourceErr, 0)
	for _, source := range sources {
		client := h.newPluginStoreClient(proxyURL, source.URL, storeAuth)
		registry, errRegistry := client.FetchRegistry(ctx)
		if errRegistry != nil {
			sourceErrors = append(
				sourceErrors, pluginStoreSourceErr{
					SourceID:   source.ID,
					SourceName: source.Name,
					SourceURL:  source.URL,
					Message:    errRegistry.Error(),
				},
			)
			continue
		}
		for _, plugin := range registry.Plugins {
			plugins = append(plugins, sourcedPlugin{source: source, plugin: plugin})
		}
	}
	return plugins, sourceErrors
}

func (h *Handler) findPluginStoreInstallTarget(
	ctx context.Context,
	proxyURL string,
	storeAuth []pluginstore.AuthConfig,
	sources []pluginstore.Source,
	id string,
	requestedSourceID string,
	c *gin.Context,
) (pluginstore.Source, pluginstore.Plugin, pluginstore.Client, bool) {
	requestedSourceID = strings.TrimSpace(requestedSourceID)
	if requestedSourceID != "" {
		for _, source := range sources {
			if source.ID != requestedSourceID {
				continue
			}
			client := h.newPluginStoreClient(proxyURL, source.URL, storeAuth)
			registry, errRegistry := client.FetchRegistry(ctx)
			if errRegistry != nil {
				c.JSON(
					http.StatusBadGateway,
					gin.H{"error": "plugin_store_registry_failed", "message": errRegistry.Error()},
				)
				return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
			}
			plugin, okPlugin := registry.PluginByID(id)
			if !okPlugin {
				c.JSON(
					http.StatusNotFound,
					gin.H{"error": "plugin_not_found", "message": "plugin not found in registry source"},
				)
				return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
			}
			return source, plugin, client, true
		}
		c.JSON(
			http.StatusNotFound,
			gin.H{"error": "plugin_store_source_not_found", "message": "plugin store source not found"},
		)
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}

	plugins, sourceErrors := h.fetchSourcedPlugins(ctx, proxyURL, storeAuth, sources)
	matches := make([]sourcedPlugin, 0)
	for _, item := range plugins {
		if item.plugin.ID == id {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		if len(plugins) == 0 && len(sourceErrors) > 0 {
			c.JSON(
				http.StatusBadGateway,
				gin.H{"error": "plugin_store_registry_failed", "message": sourceErrors[0].Message},
			)
			return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found in registry"})
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}
	if len(matches) > 1 {
		c.JSON(
			http.StatusConflict, gin.H{
				"error":   "plugin_store_source_required",
				"message": "multiple plugin store sources contain this plugin id; specify source",
				"sources": sanitizePluginStoreSources(sourcedPluginSources(matches)),
			},
		)
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}
	match := matches[0]
	return match.source, match.plugin, h.newPluginStoreClient(proxyURL, match.source.URL, storeAuth), true
}

func sourcedPluginSources(plugins []sourcedPlugin) []pluginstore.Source {
	sources := make([]pluginstore.Source, 0, len(plugins))
	for _, item := range plugins {
		sources = append(sources, item.source)
	}
	return sources
}

func pluginStoreSourceByID(sources []pluginstore.Source, id string) (pluginstore.Source, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return pluginstore.Source{}, false
}

func pluginStoreText(value string) string {
	return value
}

func pluginStoreTextList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func sanitizePluginStoreSources(sources []pluginstore.Source) []pluginStoreSource {
	out := make([]pluginStoreSource, 0, len(sources))
	for _, source := range sources {
		out = append(
			out, pluginStoreSource{
				ID:   pluginStoreText(source.ID),
				Name: pluginStoreText(source.Name),
				URL:  pluginStoreText(source.URL),
			},
		)
	}
	return out
}

func sanitizePluginStoreSourceErrors(sourceErrors []pluginStoreSourceErr) []pluginStoreSourceErr {
	if len(sourceErrors) == 0 {
		return nil
	}
	out := make([]pluginStoreSourceErr, 0, len(sourceErrors))
	for _, sourceError := range sourceErrors {
		out = append(
			out, pluginStoreSourceErr{
				SourceID:   pluginStoreText(sourceError.SourceID),
				SourceName: pluginStoreText(sourceError.SourceName),
				SourceURL:  pluginStoreText(sourceError.SourceURL),
				Message:    pluginStoreText(sourceError.Message),
			},
		)
	}
	return out
}

func sanitizePluginStorePlatforms(platforms []pluginstore.Platform) []pluginStorePlatform {
	if len(platforms) == 0 {
		return nil
	}
	out := make([]pluginStorePlatform, 0, len(platforms))
	for _, platform := range platforms {
		out = append(
			out, pluginStorePlatform{
				GOOS:   pluginStoreText(platform.GOOS),
				GOARCH: pluginStoreText(platform.GOARCH),
			},
		)
	}
	return out
}

func pluginAuthConfigured(
	source pluginstore.Source,
	plugin pluginstore.Plugin,
	storeAuth []pluginstore.AuthConfig,
) bool {
	return pluginstore.PluginAuthConfigured(source, plugin, storeAuth)
}

// latestPluginVersions resolves the latest release version of each registry
// plugin concurrently, returning results positionally aligned with plugins.
// Unresolved entries are left empty so callers can fall back gracefully.
func (h *Handler) latestPluginVersions(
	ctx context.Context,
	client pluginstore.Client,
	plugins []pluginstore.Plugin,
) []string {
	versions := make([]string, len(plugins))
	if len(plugins) == 0 {
		return versions
	}

	workerCount := pluginLatestVersionLookupConcurrency
	if len(plugins) < workerCount {
		workerCount = len(plugins)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				versions[index] = h.latestPluginVersion(ctx, client, plugins[index])
			}
		}()
	}

sendJobs:
	for index := range plugins {
		select {
		case <-ctx.Done():
			break sendJobs
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()
	return versions
}

// latestPluginVersion returns the plugin's latest release version, caching
// lookups per repository so repeated listings do not exhaust the GitHub API
// rate limit. Failed lookups are cached for a shorter interval and reported
// as an empty version.
func (h *Handler) latestPluginVersion(
	ctx context.Context,
	client pluginstore.Client,
	plugin pluginstore.Plugin,
) string {
	if pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease {
		return ""
	}
	repository := strings.TrimSpace(plugin.Repository)
	if repository == "" {
		return ""
	}
	now := time.Now()
	h.pluginReleaseCacheMu.Lock()
	entry, found := h.pluginReleaseCache[repository]
	h.pluginReleaseCacheMu.Unlock()
	if found && now.Before(entry.expiresAt) {
		return entry.version
	}

	version := ""
	ttl := pluginReleaseFailureCacheTTL
	release, errRelease := client.FetchLatestRelease(ctx, plugin)
	if errRelease != nil {
		log.WithError(errRelease).WithField("plugin_id", plugin.ID).Warn("pluginstore: failed to fetch latest release")
	} else if latestVersion, errVersion := pluginstore.ReleaseVersion(release); errVersion != nil {
		log.WithError(errVersion).WithField("plugin_id", plugin.ID).Warn("pluginstore: invalid latest release tag")
	} else {
		version = latestVersion
		ttl = pluginReleaseCacheTTL
	}

	h.pluginReleaseCacheMu.Lock()
	if h.pluginReleaseCache == nil {
		h.pluginReleaseCache = make(map[string]pluginReleaseCacheEntry)
	}
	h.pluginReleaseCache[repository] = pluginReleaseCacheEntry{version: version, expiresAt: now.Add(ttl)}
	h.pluginReleaseCacheMu.Unlock()
	return version
}

func pluginFilePath(pluginsDir string, id string, desiredVersions ...map[string]string) (string, error) {
	id = pluginstore.CanonicalPluginID(id)
	files, errDiscover := pluginhost.DiscoverPluginFiles(pluginsDir, desiredVersions...)
	if errDiscover != nil {
		return "", errDiscover
	}
	for _, file := range files {
		if file.ID == id {
			return file.Path, nil
		}
	}
	return "", nil
}

func pluginLocalStatuses(
	pluginsEnabled bool,
	pluginsDir string,
	configs map[string]config.PluginInstanceConfig,
	host pluginRuntimeHost,
) (map[string]pluginLocalStatus, error) {
	statuses := map[string]pluginLocalStatus{}
	files, errDiscover := pluginhost.DiscoverPluginFiles(pluginsDir, pluginStoreDesiredVersions(configs))
	if errDiscover != nil {
		return nil, errDiscover
	}
	for _, file := range files {
		status := statuses[file.ID]
		status.Installed = true
		status.Path = file.Path
		if strings.TrimSpace(file.Version) != "" {
			status.InstalledVersion = strings.TrimSpace(file.Version)
		}
		status.Enabled = true
		statuses[file.ID] = status
	}
	for id, item := range configs {
		id = pluginstore.CanonicalPluginID(id)
		status := statuses[id]
		status.Configured = true
		status.Enabled = pluginInstanceEnabled(item)
		status.InstalledSourceID, status.InstalledSourceURL, status.StoreManaged = pluginStoreConfiguredSource(item)
		statuses[id] = status
	}
	if host != nil {
		for _, info := range host.RegisteredPlugins() {
			id := pluginstore.CanonicalPluginID(info.ID)
			status := statuses[id]
			status.Installed = true
			status.Registered = true
			status.InstalledVersion = strings.TrimSpace(info.Metadata.Version)
			if _, _, configured := pluginConfigByID(configs, id); !configured && !status.Enabled {
				status.Enabled = false
			}
			statuses[id] = status
		}
	}
	for id, status := range statuses {
		status.EffectiveEnabled = pluginsEnabled && status.Enabled && status.Registered
		statuses[id] = status
	}
	return statuses, nil
}

func pluginStoreConfiguredSource(item config.PluginInstanceConfig) (sourceID string, sourceURL string, managed bool) {
	storeNode := pluginStoreConfigNode(item)
	if storeNode == nil {
		return "", "", false
	}
	var manifest pluginstore.Manifest
	if errDecode := storeNode.Decode(&manifest); errDecode != nil {
		return "", "", true
	}
	return strings.TrimSpace(manifest.SourceID), strings.TrimSpace(manifest.SourceURL), true
}

func pluginStoreResolveInstalledSource(status pluginLocalStatus, sources []pluginstore.Source) (string, bool) {
	sourceID := strings.TrimSpace(status.InstalledSourceID)
	sourceURL := strings.TrimSpace(status.InstalledSourceURL)
	if sourceID != "" {
		for _, source := range sources {
			if strings.TrimSpace(source.ID) != sourceID {
				continue
			}
			if sourceURL != "" && strings.TrimSpace(source.URL) != sourceURL {
				return "", false
			}
			return sourceID, true
		}
		return sourceID, true
	}
	if sourceURL == "" {
		return "", false
	}
	for _, source := range sources {
		if strings.TrimSpace(source.URL) == sourceURL {
			return strings.TrimSpace(source.ID), true
		}
	}
	return "", false
}

func pluginStoreInstallSourceStatus(
	status pluginLocalStatus,
	sources []pluginstore.Source,
	entrySourceID string,
	sourceCount int,
) (installedSourceID string, sourceStatus string, allowUpdate bool) {
	if !status.Installed && !status.Configured && !status.Registered {
		return "", "", true
	}
	if sourceID, known := pluginStoreResolveInstalledSource(status, sources); known {
		if sourceID == strings.TrimSpace(entrySourceID) {
			return sourceID, "matched", true
		}
		return sourceID, "different", false
	}
	if status.StoreManaged || sourceCount > 1 {
		return "", "unknown", false
	}
	return "", "assumed", true
}

func validatePluginStoreInstallSource(
	c *gin.Context,
	configs map[string]config.PluginInstanceConfig,
	sources []pluginstore.Source,
	id string,
	requestedSourceID string,
) bool {
	canonicalID := pluginstore.CanonicalPluginID(id)
	matchingConfigs := 0
	for configuredID := range configs {
		if pluginstore.CanonicalPluginID(configuredID) == canonicalID {
			matchingConfigs++
		}
	}
	if matchingConfigs > 1 {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "plugin_config_identity_conflict",
			"message": "plugin configuration contains duplicate case-insensitive ids",
		})
		return false
	}
	_, item, configured := pluginConfigByID(configs, canonicalID)
	if !configured {
		return true
	}
	installedSourceID, installedSourceURL, managed := pluginStoreConfiguredSource(item)
	if !managed {
		return true
	}
	status := pluginLocalStatus{
		StoreManaged:       true,
		InstalledSourceID:  installedSourceID,
		InstalledSourceURL: installedSourceURL,
	}
	resolvedSourceID, known := pluginStoreResolveInstalledSource(status, sources)
	if !known {
		c.JSON(http.StatusConflict, gin.H{
			"error":               "plugin_store_installed_source_unknown",
			"message":             "installed plugin source cannot be verified; uninstall it before reinstalling from the store",
			"requested_source_id": strings.TrimSpace(requestedSourceID),
		})
		return false
	}
	if resolvedSourceID != strings.TrimSpace(requestedSourceID) {
		c.JSON(http.StatusConflict, gin.H{
			"error":               "plugin_store_source_conflict",
			"message":             "installed plugin belongs to a different store source; uninstall it before switching sources",
			"installed_source_id": resolvedSourceID,
			"requested_source_id": strings.TrimSpace(requestedSourceID),
		})
		return false
	}
	return true
}

func pluginStoreDesiredVersions(configs map[string]config.PluginInstanceConfig) map[string]string {
	if len(configs) == 0 {
		return nil
	}
	out := make(map[string]string, len(configs))
	for id, item := range configs {
		id = pluginstore.CanonicalPluginID(id)
		version := pluginStoreDesiredVersion(item)
		if id == "" || version == "" {
			continue
		}
		out[id] = version
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pluginStoreDesiredVersion(item config.PluginInstanceConfig) string {
	storeNode := pluginStoreConfigNode(item)
	if storeNode == nil {
		return ""
	}
	if version := pluginStoreNormalizeDesiredVersion(
		pluginStoreYAMLScalar(
			yamlMappingValue(
				storeNode, "version",
			),
		),
	); version != "" {
		return version
	}
	return pluginStoreNormalizeDesiredVersion(pluginStoreYAMLScalar(yamlMappingValue(storeNode, "release-tag")))
}

func pluginStoreConfigNode(item config.PluginInstanceConfig) *yaml.Node {
	if item.Raw.Kind != yaml.MappingNode {
		return nil
	}
	return yamlMappingValue(&item.Raw, "store")
}

func pluginStoreYAMLScalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func pluginStoreNormalizeDesiredVersion(version string) string {
	version = strings.TrimSpace(version)
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') {
		version = version[1:]
	}
	if version == "" || version[0] < '0' || version[0] > '9' {
		return ""
	}
	return version
}

func preparePluginStoreOverwrite(id string, host pluginRuntimeHost) error {
	if !pluginBusy(host, id) {
		return nil
	}
	unloaded, errUnload := host.UnloadPluginWithError(id)
	if errUnload != nil {
		return errors.Join(pluginstore.ErrLoadedPluginLocked, errUnload)
	}
	if unloaded || !pluginBusy(host, id) {
		return nil
	}
	return pluginstore.ErrLoadedPluginLocked
}

func pluginBusy(host pluginRuntimeHost, id string) bool {
	if host == nil {
		return false
	}
	return host.PluginBusy(id)
}
