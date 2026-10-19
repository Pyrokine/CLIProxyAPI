// Package watcher implements debounced configuration hot reload.
// It detects material changes and reloads clients when the config changes.
package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
)

type configSnapshot struct {
	data []byte
	hash string
	cfg  *config.Config
}

func (w *Watcher) stopConfigReloadTimer() {
	w.configReloadMu.Lock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
		w.configReloadTimer = nil
	}
	w.configReloadMu.Unlock()
}

func (w *Watcher) scheduleConfigReload() {
	w.configReloadMu.Lock()
	defer w.configReloadMu.Unlock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
	}
	w.configReloadTimer = time.AfterFunc(
		configReloadDebounce, func() {
			w.configReloadMu.Lock()
			w.configReloadTimer = nil
			w.configReloadMu.Unlock()
			w.reloadConfigIfChanged()
		},
	)
}

func (w *Watcher) readConfigData() ([]byte, error) {
	if w.readConfigFile != nil {
		return w.readConfigFile(w.configPath)
	}
	return os.ReadFile(w.configPath)
}

func configDataHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newConfigSnapshot(data []byte) (*configSnapshot, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config file is empty")
	}
	cfg, err := config.ParseConfigBytes(data)
	if err != nil {
		return nil, err
	}
	return &configSnapshot{
		data: append([]byte(nil), data...),
		hash: configDataHash(data),
		cfg:  cfg,
	}, nil
}

func (w *Watcher) configSnapshotIsCurrent(snapshot *configSnapshot) bool {
	if snapshot == nil {
		return false
	}
	data, err := w.readConfigData()
	if err != nil {
		log.WithError(err).Debug("failed to verify config snapshot")
		return false
	}
	return len(data) > 0 && configDataHash(data) == snapshot.hash
}

func (w *Watcher) reloadConfigIfChanged() {
	w.configApplyMu.Lock()
	defer w.configApplyMu.Unlock()

	data, err := w.readConfigData()
	if err != nil {
		log.Errorf("failed to read config file for hash check: %v", err)
		return
	}
	if len(data) == 0 {
		log.Debugf("ignoring empty config file write event")
		return
	}
	newHash := configDataHash(data)

	w.clientsMutex.RLock()
	currentHash := w.lastConfigHash
	w.clientsMutex.RUnlock()

	if currentHash != "" && currentHash == newHash {
		log.Debugf("config file content unchanged (hash match), skipping reload")
		return
	}

	snapshot, err := newConfigSnapshot(data)
	if err != nil {
		log.Errorf("failed to parse config snapshot: %v", err)
		return
	}
	log.Infof("config file changed, reloading: %s", w.configPath)
	if !w.reloadConfigSnapshot(snapshot) {
		return
	}

	w.clientsMutex.Lock()
	w.lastConfigHash = snapshot.hash
	w.clientsMutex.Unlock()
	w.persistConfigAsync()

	if !w.configSnapshotIsCurrent(snapshot) {
		log.Infof("config changed during reload; scheduling the latest snapshot")
		w.scheduleConfigReload()
	}
}

func (w *Watcher) reloadConfig() bool {
	w.configApplyMu.Lock()
	defer w.configApplyMu.Unlock()

	data, err := w.readConfigData()
	if err != nil {
		log.Errorf("failed to reload config: %v", err)
		return false
	}
	snapshot, err := newConfigSnapshot(data)
	if err != nil {
		log.Errorf("failed to reload config: %v", err)
		return false
	}
	return w.reloadConfigSnapshot(snapshot)
}

func (w *Watcher) reloadConfigSnapshot(snapshot *configSnapshot) bool {
	log.Debug("=========================== CONFIG RELOAD ============================")
	log.Debugf("starting config reload from: %s", w.configPath)

	newConfig := snapshot.cfg
	if w.mirroredAuthDir != "" {
		newConfig.AuthDir = w.mirroredAuthDir
	} else {
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(newConfig.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory from config: %v", errResolveAuthDir)
		} else {
			newConfig.AuthDir = resolvedAuthDir
		}
	}
	modelCatalog, err := modelcatalog.LoadOrInit(w.configPath, newConfig)
	if err != nil {
		log.Errorf("failed to reload model catalog: %v", err)
		return false
	}
	if modelCatalog.LoadMetadata.RecoveredFromPrevious {
		log.WithField("current_error", modelCatalog.LoadMetadata.CurrentError).
			Warn("model-catalog: restored models.json from models.previous.json during config reload")
	}

	w.clientsMutex.RLock()
	previousConfig := w.config
	previousConfigYaml := append([]byte(nil), w.oldConfigYaml...)
	previousAuthDir := w.authDir
	w.clientsMutex.RUnlock()

	var oldConfig *config.Config
	_ = yaml.Unmarshal(previousConfigYaml, &oldConfig)

	var affectedOAuthProviders []string
	if oldConfig != nil {
		_, affectedOAuthProviders = diff.OAuthExcludedModelChanges(
			oldConfig.OAuthExcludedModels, newConfig.OAuthExcludedModels,
		)
	}

	if oldConfig != nil {
		details := diff.BuildConfigChangeDetails(oldConfig, newConfig)
		if len(details) > 0 {
			log.Debugf("config changes detected:")
			for _, d := range details {
				log.Debugf("  %s", d)
			}
		} else {
			log.Debugf("no material config field changes detected")
		}
	}

	authDirChanged := w.normalizeAuthPath(previousAuthDir) != w.normalizeAuthPath(newConfig.AuthDir)
	retryConfigChanged := oldConfig != nil &&
		(oldConfig.RequestRetry != newConfig.RequestRetry || newConfig.MaxRetryInterval != oldConfig.MaxRetryInterval || oldConfig.MaxRetryCredentials != newConfig.MaxRetryCredentials)
	forceAuthRefresh := oldConfig != nil &&
		(oldConfig.ForceModelPrefix != newConfig.ForceModelPrefix || !reflect.DeepEqual(
			oldConfig.OAuthModelAlias, newConfig.OAuthModelAlias,
		) || !reflect.DeepEqual(oldConfig.OAuthSettings, newConfig.OAuthSettings) || retryConfigChanged)

	if !w.configSnapshotIsCurrent(snapshot) {
		log.Infof("config changed before reload publication; scheduling the latest snapshot")
		w.scheduleConfigReload()
		return false
	}

	newAuthDir := strings.TrimSpace(newConfig.AuthDir)
	if authDirChanged && newAuthDir == "" {
		log.Error("cannot switch to an empty auth directory")
		return false
	}
	newAuthWatchAdded := false
	configDir := filepath.Dir(w.configPath)
	if authDirChanged && w.watcher != nil &&
		w.normalizeAuthPath(configDir) != w.normalizeAuthPath(newAuthDir) {
		if errMkdir := os.MkdirAll(newAuthDir, 0o700); errMkdir != nil {
			log.Errorf("failed to create new auth directory %s: %v", newAuthDir, errMkdir)
			return false
		}
		if errAdd := w.watcher.Add(newAuthDir); errAdd != nil {
			log.Errorf("failed to watch new auth directory %s: %v", newAuthDir, errAdd)
			return false
		}
		newAuthWatchAdded = true
	}

	callbackAccepted := true
	switch {
	case w.configReloadCallback != nil:
		log.Debugf("triggering config snapshot callback before publishing config reload")
		callbackAccepted = w.configReloadCallback(newConfig, snapshot.hash)
	case w.reloadCallback != nil:
		log.Debugf("triggering server update callback before publishing config reload")
		callbackAccepted = w.reloadCallback(newConfig)
	}
	if !callbackAccepted {
		log.Errorf("server update callback rejected config reload; keeping previous config")
		if newAuthWatchAdded {
			_ = w.watcher.Remove(newAuthDir)
		}
		if !w.configSnapshotIsCurrent(snapshot) {
			w.scheduleConfigReload()
		}
		return false
	}

	w.clientsMutex.Lock()
	w.oldConfigYaml, _ = yaml.Marshal(newConfig)
	w.config = newConfig
	w.authDir = newAuthDir
	w.clientsMutex.Unlock()

	log.Infof("config successfully reloaded, triggering client reload")
	if !w.reloadClientsWithConfig(newConfig, authDirChanged, affectedOAuthProviders, forceAuthRefresh) {
		w.clientsMutex.Lock()
		w.oldConfigYaml = previousConfigYaml
		w.config = previousConfig
		w.authDir = previousAuthDir
		w.clientsMutex.Unlock()
		if newAuthWatchAdded {
			_ = w.watcher.Remove(newAuthDir)
		}
		return false
	}
	if authDirChanged && w.watcher != nil &&
		w.normalizeAuthPath(configDir) != w.normalizeAuthPath(previousAuthDir) {
		if errRemove := w.watcher.Remove(previousAuthDir); errRemove != nil {
			log.Warnf("failed to remove old auth directory watch %s: %v", previousAuthDir, errRemove)
		}
	}

	util.SetLogLevel(newConfig)
	if oldConfig != nil && oldConfig.Debug != newConfig.Debug {
		log.Debugf("log level updated - debug mode changed from %t to %t", oldConfig.Debug, newConfig.Debug)
	}
	return true
}
