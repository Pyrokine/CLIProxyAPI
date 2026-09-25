// Package watcher watches config/auth files and triggers hot reloads.
// It supports cross-platform fsnotify event handling.
package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// storePersister captures persistence-capable token store methods used by the watcher.
type storePersister interface {
	PersistConfig(ctx context.Context) error
	PersistAuthFiles(ctx context.Context, message string, paths ...string) error
}

type authDirProvider interface {
	AuthDir() string
}

// Watcher manages file watching for configuration and authentication files
type Watcher struct {
	configPath           string
	authDir              string
	config               *config.Config
	clientsMutex         sync.RWMutex
	authRescanMu         sync.Mutex
	configReloadMu       sync.Mutex
	configReloadTimer    *time.Timer
	configApplyMu        sync.Mutex
	reloadCallback       func(*config.Config) bool
	configReloadCallback func(*config.Config, string) bool
	readConfigFile       func(string) ([]byte, error)
	watcher              *fsnotify.Watcher
	lastAuthHashes       map[string]string
	lastAuthVersions     map[string]coreauth.PersistedVersion
	lastAuthContents     map[string]*coreauth.Auth
	fileAuthsByPath      map[string]map[string]*coreauth.Auth
	lastRemoveTimes      map[string]time.Time
	lastConfigHash       string
	authQueue            chan<- AuthUpdate
	pluginAuthParser     synthesizer.PluginAuthParser
	usageQueue           *redisqueue.Queue
	currentAuths         map[string]*coreauth.Auth
	authRevisions        map[string]uint64
	fileObservations     map[string]uint64
	activeAuthScans      int
	runtimeAuths         map[string]*coreauth.Auth
	dispatchMu           sync.Mutex
	dispatchCond         *sync.Cond
	pendingUpdates       map[string]AuthUpdate
	pendingOrder         []string
	dispatchCancel       context.CancelFunc
	storePersister       storePersister
	mirroredAuthDir      string
	oldConfigYaml        []byte
	serverUpdateMu       sync.Mutex
	serverUpdateTimer    *time.Timer
	serverUpdateLast     time.Time
	serverUpdatePend     bool
}

// authUpdateAction represents the type of change detected in auth sources.
type authUpdateAction string

const (
	AuthUpdateActionAdd    authUpdateAction = "add"
	AuthUpdateActionModify authUpdateAction = "modify"
	AuthUpdateActionDelete authUpdateAction = "delete"
)

// AuthUpdate describes an incremental change to auth configuration.
type AuthUpdate struct {
	Action            authUpdateAction
	ID                string
	Auth              *coreauth.Auth
	AllowAuthRecreate bool
	revision          uint64
}

// Revision returns the watcher-local ordering assigned to this update.
func (u AuthUpdate) Revision() uint64 {
	return u.revision
}

// SetRevision assigns watcher-local ordering to an update bridge.
func (u *AuthUpdate) SetRevision(revision uint64) {
	if u != nil {
		u.revision = revision
	}
}

const (
	// replaceCheckDelay is a short delay to allow atomic replace (rename) to settle
	// before deciding whether a Remove event indicates a real deletion.
	replaceCheckDelay        = 50 * time.Millisecond
	configReloadDebounce     = 150 * time.Millisecond
	authRemoveDebounceWindow = 1 * time.Second
	serverUpdateDebounce     = 500 * time.Millisecond
)

// NewWatcher creates a new file watcher instance
func NewWatcher(configPath, authDir string, reloadCallback func(*config.Config) bool) (*Watcher, error) {
	watcher, errNewWatcher := fsnotify.NewWatcher()
	if errNewWatcher != nil {
		return nil, errNewWatcher
	}
	w := &Watcher{
		configPath:       configPath,
		authDir:          authDir,
		reloadCallback:   reloadCallback,
		watcher:          watcher,
		lastAuthHashes:   make(map[string]string),
		lastAuthVersions: make(map[string]coreauth.PersistedVersion),
		fileAuthsByPath:  make(map[string]map[string]*coreauth.Auth),
	}
	w.dispatchCond = sync.NewCond(&w.dispatchMu)
	if store := sdkAuth.GetTokenStore(); store != nil {
		if persister, ok := store.(storePersister); ok {
			w.storePersister = persister
			log.Debug("persistence-capable token store detected; watcher will propagate persisted changes")
		}
		if provider, ok := store.(authDirProvider); ok {
			if fixed := strings.TrimSpace(provider.AuthDir()); fixed != "" {
				w.mirroredAuthDir = fixed
				log.Debugf("mirrored auth directory locked to %s", fixed)
			}
		}
	}
	return w, nil
}

// Start begins watching the configuration file and authentication directory
func (w *Watcher) Start(ctx context.Context) error {
	return w.start(ctx)
}

// Stop stops the file watcher
func (w *Watcher) Stop() error {
	w.stopDispatch()
	w.stopConfigReloadTimer()
	w.stopServerUpdateTimer()
	return w.watcher.Close()
}

// SetConfig updates the current configuration
func (w *Watcher) SetConfig(cfg *config.Config) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.config = cfg
	w.oldConfigYaml, _ = yaml.Marshal(cfg)
}

func (w *Watcher) SetConfigReloadCallback(callback func(*config.Config, string) bool) {
	w.configReloadCallback = callback
}

func (w *Watcher) AcknowledgeConfigFile(cfg *config.Config, expectedHash string) bool {
	if w == nil || cfg == nil || strings.TrimSpace(expectedHash) == "" {
		return false
	}
	w.configApplyMu.Lock()
	defer w.configApplyMu.Unlock()

	expectedHash = strings.TrimSpace(expectedHash)
	data, errRead := w.readConfigData()
	if errRead != nil || len(data) == 0 || configDataHash(data) != expectedHash {
		return false
	}
	if w.mirroredAuthDir != "" {
		cfg.AuthDir = w.mirroredAuthDir
	} else if strings.TrimSpace(cfg.AuthDir) != "" {
		if resolvedAuthDir, errResolve := util.ResolveAuthDir(cfg.AuthDir); errResolve != nil {
			log.Errorf("failed to resolve committed auth directory: %v", errResolve)
			return false
		} else {
			cfg.AuthDir = resolvedAuthDir
		}
	}

	w.clientsMutex.RLock()
	previousConfig := w.config
	previousConfigYAML := append([]byte(nil), w.oldConfigYaml...)
	previousAuthDir := w.authDir
	previousHash := w.lastConfigHash
	w.clientsMutex.RUnlock()
	newAuthDir := strings.TrimSpace(cfg.AuthDir)
	authDirChanged := w.normalizeAuthPath(previousAuthDir) != w.normalizeAuthPath(newAuthDir)
	if authDirChanged && newAuthDir == "" {
		return false
	}
	newAuthWatchAdded := false
	configDir := filepath.Dir(w.configPath)
	if authDirChanged && w.watcher != nil && w.normalizeAuthPath(configDir) != w.normalizeAuthPath(newAuthDir) {
		if errMkdir := os.MkdirAll(newAuthDir, 0o700); errMkdir != nil {
			log.Errorf("failed to create committed auth directory %s: %v", newAuthDir, errMkdir)
			return false
		}
		if errAdd := w.watcher.Add(newAuthDir); errAdd != nil {
			log.Errorf("failed to watch committed auth directory %s: %v", newAuthDir, errAdd)
			return false
		}
		newAuthWatchAdded = true
	}
	if current, errCurrent := w.readConfigData(); errCurrent != nil || configDataHash(current) != expectedHash {
		if newAuthWatchAdded {
			_ = w.watcher.Remove(newAuthDir)
		}
		return false
	}

	var affectedOAuthProviders []string
	forceAuthRefresh := false
	if previousConfig != nil {
		_, affectedOAuthProviders = diff.OAuthExcludedModelChanges(
			previousConfig.OAuthExcludedModels, cfg.OAuthExcludedModels,
		)
		retryConfigChanged := previousConfig.RequestRetry != cfg.RequestRetry ||
			previousConfig.MaxRetryInterval != cfg.MaxRetryInterval ||
			previousConfig.MaxRetryCredentials != cfg.MaxRetryCredentials
		forceAuthRefresh = previousConfig.ForceModelPrefix != cfg.ForceModelPrefix ||
			!reflect.DeepEqual(previousConfig.OAuthModelAlias, cfg.OAuthModelAlias) || retryConfigChanged
	}

	w.clientsMutex.Lock()
	w.config = cfg
	w.oldConfigYaml, _ = yaml.Marshal(cfg)
	w.authDir = newAuthDir
	w.lastConfigHash = expectedHash
	w.clientsMutex.Unlock()
	if !w.reloadClientsWithConfig(cfg, authDirChanged, affectedOAuthProviders, forceAuthRefresh) {
		w.clientsMutex.Lock()
		w.config = previousConfig
		w.oldConfigYaml = previousConfigYAML
		w.authDir = previousAuthDir
		w.lastConfigHash = previousHash
		w.clientsMutex.Unlock()
		if newAuthWatchAdded {
			_ = w.watcher.Remove(newAuthDir)
		}
		return false
	}
	if authDirChanged && w.watcher != nil && previousAuthDir != "" &&
		w.normalizeAuthPath(configDir) != w.normalizeAuthPath(previousAuthDir) {
		if errRemove := w.watcher.Remove(previousAuthDir); errRemove != nil {
			log.Warnf("failed to remove old auth directory watch %s: %v", previousAuthDir, errRemove)
		}
	}
	return true
}

// SetAuthUpdateQueue sets the queue used to emit auth updates.
func (w *Watcher) SetAuthUpdateQueue(queue chan<- AuthUpdate) {
	w.setAuthUpdateQueue(queue)
}

// SetPluginAuthParser updates the parser used for plugin-owned auth files.
func (w *Watcher) SetPluginAuthParser(parser synthesizer.PluginAuthParser) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.pluginAuthParser = parser
}

// SetUsageQueue assigns the service-owned queue notified after auth changes.
func (w *Watcher) SetUsageQueue(queue *redisqueue.Queue) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.usageQueue = queue
}

func (w *Watcher) notifyUsageRefresh() {
	w.clientsMutex.RLock()
	queue := w.usageQueue
	w.clientsMutex.RUnlock()
	if queue != nil {
		queue.NotifyUsageRefresh()
		return
	}
	redisqueue.NotifyUsageRefresh()
}

// DispatchRuntimeAuthUpdate allows external runtime providers (e.g., websocket-driven auths)
// to push auth updates through the same queue used by file/config watchers.
// Returns true if the update was enqueued; false if no queue is configured.
func (w *Watcher) DispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	return w.dispatchRuntimeAuthUpdate(update)
}

// DispatchPersistedAuthUpdateWithRevision pushes an already-persisted file auth update and returns its revision.
func (w *Watcher) DispatchPersistedAuthUpdateWithRevision(update *AuthUpdate) (bool, uint64) {
	return w.dispatchPersistedAuthUpdateWithRevision(update)
}

// SnapshotCoreAuths converts current clients snapshot into core auth entries.
func (w *Watcher) SnapshotCoreAuths() []*coreauth.Auth {
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	parser := w.pluginAuthParser
	w.clientsMutex.RUnlock()
	auths, errSnapshot := snapshotCoreAuths(cfg, authDir, parser)
	if errSnapshot != nil {
		log.Errorf("failed to build auth snapshot: %v", errSnapshot)
	}
	return auths
}
