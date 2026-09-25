package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type loadedPlugin struct {
	id              string
	path            string
	version         string
	name            string
	retirementWatch bool
	configYAML      []byte
	client          pluginClient
}

type pluginUnloadTarget struct {
	id      string
	name    string
	path    string
	version string
	client  pluginClient
	plugin  *loadedPlugin
}

func pluginUnloadTargetFromLoaded(lp *loadedPlugin) pluginUnloadTarget {
	if lp == nil {
		return pluginUnloadTarget{}
	}
	return pluginUnloadTarget{
		id: lp.id, name: lp.name, path: lp.path, version: lp.version,
		client: lp.client, plugin: lp,
	}
}

type pendingPluginLoad struct {
	plugin   *loadedPlugin
	previous *loadedPlugin
}

type pluginInitialization struct {
	done chan struct{}
}

type modelExecutor interface {
	ExecuteModel(context.Context, handlers.ModelExecutionRequest) (
		handlers.ModelExecutionResponse,
		*handlers.ErrorMessage,
	)
	ExecuteModelStream(context.Context, handlers.ModelExecutionRequest) (
		handlers.ModelExecutionStream,
		*handlers.ErrorMessage,
	)
}

type pluginPublicationToken struct {
	snapshot          *Snapshot
	applyGeneration   uint64
	pluginGenerations map[string]uint64
}

type pluginOperationLease struct {
	epoch uint64
}

type pluginOperationToken struct {
	pluginID string
	lease    *pluginOperationLease
}

type pluginLoadAttemptRegistry struct {
	attempts sync.Map
}

func (r *pluginLoadAttemptRegistry) claim(id string) bool {
	if r == nil {
		return false
	}
	_, attempted := r.attempts.LoadOrStore(id, struct{}{})
	return !attempted
}

// PluginRuntimeIdentity identifies one plugin runtime owner for conditional unload.
type PluginRuntimeIdentity struct {
	ID               string
	Path             string
	Version          string
	PluginGeneration uint64
	OperationEpoch   uint64
}

type accessProviderManager interface {
	SetProviders([]sdkaccess.Provider)
}

type Host struct {
	mu                           sync.Mutex
	applyMu                      sync.Mutex
	publicationMu                sync.Mutex
	usageMu                      sync.Mutex
	usageManagerMu               sync.RWMutex
	loader                       pluginLoader
	loaded                       map[string]*loadedPlugin
	retired                      map[string][]*loadedPlugin
	loading                      map[string]struct{}
	initializing                 map[string]*pluginInitialization
	pendingLoaded                map[string]*loadedPlugin
	fused                        map[string]string
	pluginFileVersions           map[string]string
	activePluginVersions         map[string]string
	activePluginPaths            map[string]string
	cleanupFilesPending          bool
	runtimeConfig                *config.Config
	modelExecutor                modelExecutor
	affinityManager              affinityLookupManager
	modelClientIDs               map[string]struct{}
	modelClientOwners            map[string]string
	modelRegistry                modelRegistry
	executorModelClientIDs       map[string]struct{}
	executorModelClientOwners    map[string]string
	executorModelRegistry        modelProviderRegistry
	modelProviders               map[string]string
	modelRegistrations           map[string]pluginModelRegistration
	providerModels               map[string][]*registryModelInfo
	executorProviders            map[string]struct{}
	executorProviderOwners       map[string]string
	executorManager              executorManager
	accessManager                accessProviderManager
	accessProviderKeys           map[string]string
	exclusiveAccessProviderKey   string
	exclusiveAccessProviderOwner string
	usageManager                 *coreusage.Manager
	usagePluginKeys              map[string]string
	usageRetirements             map[string][]*coreusage.Retirement
	commandLineFlags             map[string]commandLineFlagRecord
	commandLineHits              map[string]struct{}
	managementRoutes             map[string]managementRouteRecord
	resourceRoutes               map[string]resourceRouteRecord
	applyGeneration              uint64
	pluginGenerations            map[string]uint64
	pluginOperationEpochs        map[string]uint64
	pluginOperationLeases        sync.Map
	pluginLoadAttempts           *pluginLoadAttemptRegistry
	streams                      *streamBridge
	httpStreams                  *hostHTTPStreamBridge
	modelStreams                 *modelStreamBridge
	callbackContexts             *callbackContextRegistry
	shutdownContext              context.Context
	shutdownCancel               context.CancelFunc
	snapshot                     atomic.Value
}

func New() *Host {
	shutdownContext, shutdownCancel := context.WithCancel(context.Background())
	h := &Host{
		loader:                    defaultPluginLoader(),
		loaded:                    make(map[string]*loadedPlugin),
		retired:                   make(map[string][]*loadedPlugin),
		loading:                   make(map[string]struct{}),
		initializing:              make(map[string]*pluginInitialization),
		pendingLoaded:             make(map[string]*loadedPlugin),
		fused:                     make(map[string]string),
		pluginFileVersions:        make(map[string]string),
		activePluginVersions:      make(map[string]string),
		activePluginPaths:         make(map[string]string),
		cleanupFilesPending:       true,
		modelClientIDs:            make(map[string]struct{}),
		modelClientOwners:         make(map[string]string),
		executorModelClientIDs:    make(map[string]struct{}),
		executorModelClientOwners: make(map[string]string),
		modelProviders:            make(map[string]string),
		modelRegistrations:        make(map[string]pluginModelRegistration),
		providerModels:            make(map[string][]*registryModelInfo),
		executorProviders:         make(map[string]struct{}),
		executorProviderOwners:    make(map[string]string),
		accessProviderKeys:        make(map[string]string),
		usagePluginKeys:           make(map[string]string),
		usageRetirements:          make(map[string][]*coreusage.Retirement),
		commandLineFlags:          make(map[string]commandLineFlagRecord),
		commandLineHits:           make(map[string]struct{}),
		managementRoutes:          make(map[string]managementRouteRecord),
		resourceRoutes:            make(map[string]resourceRouteRecord),
		pluginGenerations:         make(map[string]uint64),
		pluginOperationEpochs:     make(map[string]uint64),
		pluginLoadAttempts:        defaultPluginLoadAttemptRegistry(),
		streams:                   newStreamBridge(),
		httpStreams:               newHostHTTPStreamBridge(),
		modelStreams:              newModelStreamBridge(),
		callbackContexts:          newCallbackContextRegistry(),
		shutdownContext:           shutdownContext,
		shutdownCancel:            shutdownCancel,
	}
	h.snapshot.Store(emptySnapshot())
	return h
}

func newForTest(loader pluginLoader) *Host {
	h := New()
	h.loader = loader
	h.pluginLoadAttempts = &pluginLoadAttemptRegistry{}
	return h
}

// SetUsageManager assigns the service-owned usage dispatcher.
func (h *Host) SetUsageManager(manager *coreusage.Manager) {
	if h == nil {
		return
	}
	h.usageManagerMu.Lock()
	h.usageManager = manager
	h.usageManagerMu.Unlock()
}

func (h *Host) usageDispatcher() *coreusage.Manager {
	if h == nil {
		return coreusage.DefaultManager()
	}
	h.usageManagerMu.RLock()
	manager := h.usageManager
	h.usageManagerMu.RUnlock()
	if manager != nil {
		return manager
	}
	return coreusage.DefaultManager()
}

func (h *Host) SetModelExecutor(executor modelExecutor) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.modelExecutor = executor
	h.mu.Unlock()
}

func (h *Host) currentModelExecutor() modelExecutor {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	executor := h.modelExecutor
	h.mu.Unlock()
	return executor
}

func (h *Host) Snapshot() *Snapshot {
	if h == nil {
		return emptySnapshot()
	}
	raw := h.snapshot.Load()
	if snap, ok := raw.(*Snapshot); ok && snap != nil {
		return snap
	}
	return emptySnapshot()
}

func (h *Host) capturePublicationToken() pluginPublicationToken {
	if h == nil {
		return pluginPublicationToken{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	generations := make(map[string]uint64, len(h.pluginGenerations))
	maps.Copy(generations, h.pluginGenerations)
	return pluginPublicationToken{
		snapshot:          h.Snapshot(),
		applyGeneration:   h.applyGeneration,
		pluginGenerations: generations,
	}
}

func (h *Host) publicationTokenCurrentLocked(token pluginPublicationToken) bool {
	if h == nil || token.snapshot == nil || h.Snapshot() != token.snapshot ||
		h.applyGeneration != token.applyGeneration {
		return false
	}
	for id, generation := range token.pluginGenerations {
		if h.pluginGenerations[id] != generation {
			return false
		}
	}
	return true
}

func (h *Host) currentApplyGeneration() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	generation := h.applyGeneration
	h.mu.Unlock()
	return generation
}

func (h *Host) capturePluginOperationToken(
	record capabilityRecord,
	expectedApplyGeneration uint64,
) (pluginOperationToken, bool) {
	if h == nil || expectedApplyGeneration == 0 {
		return pluginOperationToken{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.applyGeneration != expectedApplyGeneration {
		return pluginOperationToken{}, false
	}
	id := pluginstore.CanonicalPluginID(record.id)
	if id == "" {
		return pluginOperationToken{}, false
	}
	if _, fused := h.fused[id]; fused {
		return pluginOperationToken{}, false
	}
	path := cleanPluginPath(record.path)
	version := strings.TrimSpace(record.version)
	if path != "" || version != "" {
		if h.activePluginPaths[id] != path ||
			h.activePluginVersions[id] != version ||
			h.pluginFileVersions[path] != version {
			return pluginOperationToken{}, false
		}
	}
	return pluginOperationToken{
		pluginID: id,
		lease:    h.pluginOperationLeaseLocked(id),
	}, true
}

func (h *Host) pluginOperationTokenCurrent(token pluginOperationToken) bool {
	if h == nil || token.pluginID == "" || token.lease == nil {
		return false
	}
	current, ok := h.pluginOperationLeases.Load(token.pluginID)
	if !ok {
		return false
	}
	lease, ok := current.(*pluginOperationLease)
	return ok && lease == token.lease
}

func (h *Host) pluginOperationEpochLocked(id string) uint64 {
	if h.pluginOperationEpochs == nil {
		h.pluginOperationEpochs = make(map[string]uint64)
	}
	epoch := h.pluginOperationEpochs[id]
	if epoch == 0 {
		epoch = 1
		h.pluginOperationEpochs[id] = epoch
	}
	return epoch
}

func (h *Host) pluginOperationLeaseLocked(id string) *pluginOperationLease {
	epoch := h.pluginOperationEpochLocked(id)
	if current, ok := h.pluginOperationLeases.Load(id); ok {
		if lease, okLease := current.(*pluginOperationLease); okLease && lease != nil && lease.epoch == epoch {
			return lease
		}
	}
	lease := &pluginOperationLease{epoch: epoch}
	h.pluginOperationLeases.Store(id, lease)
	return lease
}

func (h *Host) advancePluginOperationLocked(id string) uint64 {
	epoch := h.pluginOperationEpochLocked(id) + 1
	h.pluginOperationEpochs[id] = epoch
	h.pluginOperationLeases.Store(id, &pluginOperationLease{epoch: epoch})
	return epoch
}

func (h *Host) advanceActivePluginOperationsLocked() {
	ids := make(map[string]struct{})
	for id := range h.loaded {
		ids[id] = struct{}{}
	}
	for id := range h.pendingLoaded {
		ids[id] = struct{}{}
	}
	for id := range h.retired {
		ids[id] = struct{}{}
	}
	if snapshot := h.Snapshot(); snapshot != nil {
		for _, record := range snapshot.records {
			ids[record.id] = struct{}{}
		}
	}
	for id := range ids {
		h.advancePluginOperationLocked(id)
	}
}

// PluginRuntimeIdentity returns the current lifecycle identity for one plugin ID.
func (h *Host) PluginRuntimeIdentity(id string) PluginRuntimeIdentity {
	id = pluginstore.CanonicalPluginID(id)
	if h == nil || id == "" {
		return PluginRuntimeIdentity{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return PluginRuntimeIdentity{
		ID:               id,
		Path:             h.activePluginPaths[id],
		Version:          h.activePluginVersions[id],
		PluginGeneration: h.pluginGenerations[id],
		OperationEpoch:   h.pluginOperationEpochLocked(id),
	}
}

func (h *Host) pluginRuntimeIdentityCurrentLocked(identity PluginRuntimeIdentity) bool {
	id := pluginstore.CanonicalPluginID(identity.ID)
	if h == nil || id == "" || identity.OperationEpoch == 0 ||
		h.pluginGenerations[id] != identity.PluginGeneration ||
		h.pluginOperationEpochs[id] != identity.OperationEpoch {
		return false
	}
	return h.activePluginPaths[id] == cleanPluginPath(identity.Path) &&
		h.activePluginVersions[id] == strings.TrimSpace(identity.Version)
}

func (h *Host) ApplyConfig(ctx context.Context, cfg *config.Config) (err error) {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h.applyMu.Lock()
	defer h.applyMu.Unlock()

	generation, previousRuntimeConfig := h.beginConfigApply(cfg)
	defer func() {
		if err == nil {
			return
		}
		h.mu.Lock()
		if h.configApplyCurrentLocked(generation) && h.runtimeConfig == cfg {
			h.runtimeConfig = previousRuntimeConfig
		}
		h.mu.Unlock()
	}()
	rc, errRuntimeConfig := runtimeConfigFromConfig(cfg)
	if errRuntimeConfig != nil {
		return fmt.Errorf("pluginhost: resolve runtime config: %w", errRuntimeConfig)
	}
	rootLock, errRootLock := pluginstore.LockRoot(rc.Dir)
	if errRootLock != nil {
		return fmt.Errorf("pluginhost: lock plugin root: %w", errRootLock)
	}
	unlockRoot := func() {
		if rootLock != nil {
			rootLock.Unlock()
			rootLock = nil
		}
	}
	defer unlockRoot()
	desiredVersions := desiredPluginVersions(rc.Items)
	configFingerprints, errFingerprints := pluginConfigFingerprints(cfg)
	if errFingerprints != nil {
		return fmt.Errorf("pluginhost: fingerprint plugin configs: %w", errFingerprints)
	}
	if errRecover := pluginstore.RecoverInstallTransactions(rc.Dir, configFingerprints); errRecover != nil {
		return fmt.Errorf("pluginhost: recover plugin install transactions: %w", errRecover)
	}
	configuredPluginIDs := make(map[string]struct{}, len(rc.Items))
	for id := range rc.Items {
		configuredPluginIDs[id] = struct{}{}
	}
	if errRecover := pluginstore.RecoverDeletionTransactions(rc.Dir, configuredPluginIDs); errRecover != nil {
		return fmt.Errorf("pluginhost: recover plugin deletion transactions: %w", errRecover)
	}
	if !rc.Enabled {
		h.mu.Lock()
		if h.configApplyCurrentLocked(generation) {
			h.runtimeConfig = cfg
		}
		h.mu.Unlock()
		h.clearRuntimeRoutesForApply(generation)
		return nil
	}

	files, cleanupCandidates, errSelect := selectPluginFilesWithCandidates(rc.Dir, desiredVersions)
	if errSelect != nil {
		return fmt.Errorf("pluginhost: select plugin files: %w", errSelect)
	}
	cleanupCandidates, errCleanupSnapshots := preparePluginCleanupSnapshots(cleanupCandidates)
	if errCleanupSnapshots != nil {
		return fmt.Errorf("pluginhost: snapshot cleanup candidates: %w", errCleanupSnapshots)
	}
	files = h.withLoadedPluginFallbacks(files, rc.Items, desiredVersions)
	files, snapshotDir, errSnapshots := preparePluginFileSnapshots(files, rc.Items)
	if errSnapshots != nil {
		return fmt.Errorf("pluginhost: snapshot plugin files: %w", errSnapshots)
	}
	defer removePluginFileSnapshots(snapshotDir)
	unlockRoot()

	records := make([]capabilityRecord, 0, len(files))
	loadedFiles := make([]pluginFile, 0, len(files))
	recordPlugins := make(map[string]*loadedPlugin, len(files))
	recordGenerations := make(map[string]uint64, len(files))
	pendingLoads := make(map[*loadedPlugin]pendingPluginLoad)
	quiescedReplacements := make(map[*loadedPlugin]struct{})
	publishedRecords := make(map[string]capabilityRecord)
	for _, record := range h.activeRecords() {
		publishedRecords[record.id] = record
	}
	defer func() {
		for candidate := range pendingLoads {
			h.mu.Lock()
			if h.pendingLoaded[candidate.id] == candidate {
				delete(h.pendingLoaded, candidate.id)
				delete(h.loading, candidate.id)
			}
			retirementDone := h.trackRetiredPluginLocked(candidate)
			h.mu.Unlock()
			h.shutdownPreparedDiscardedPlugin(candidate, retirementDone)
		}
		if err != nil {
			h.mu.Lock()
			if h.configApplyCurrentLocked(generation) && h.runtimeConfig == cfg {
				h.runtimeConfig = previousRuntimeConfig
			}
			h.mu.Unlock()
		}
		for previous := range quiescedReplacements {
			if errRestore := h.restoreQuiescedPlugin(previous); errRestore != nil {
				err = errors.Join(err, errRestore)
			}
		}
	}()
	for _, file := range files {
		item, ok := rc.Items[file.ID]
		if !ok {
			item = defaultRuntimeItemConfig(file.ID)
		}
		if !item.Enabled {
			continue
		}

		h.mu.Lock()
		if !h.configApplyCurrentLocked(generation) {
			h.mu.Unlock()
			return nil
		}
		pluginGeneration := h.pluginGenerations[file.ID]
		previous := h.loaded[file.ID]
		lp := previous
		if lp != nil && cleanPluginPath(lp.path) != cleanPluginPath(file.Path) {
			lp = nil
		}
		_, disabled := h.fused[file.ID]
		if disabled && lp == previous {
			h.mu.Unlock()
			continue
		}
		if lp != nil && bytes.Equal(lp.configYAML, item.ConfigYAML) {
			record, published := publishedRecords[file.ID]
			if published && cleanPluginPath(record.path) == cleanPluginPath(file.Path) &&
				strings.TrimSpace(record.version) == strings.TrimSpace(file.Version) {
				h.mu.Unlock()
				record.priority = item.Priority
				records = append(records, record)
				loadedFiles = append(loadedFiles, file)
				recordPlugins[file.ID] = lp
				recordGenerations[file.ID] = pluginGeneration
				continue
			}
		}
		if lp != nil {
			lp = nil
		}
		if pluginReloadRequiresRestart() &&
			(previous != nil || !h.pluginLoadAttempts.claim(file.ID)) {
			h.mu.Unlock()
			return fmt.Errorf("pluginhost: plugin %s reload requires process restart", file.ID)
		}
		h.loading[file.ID] = struct{}{}
		initialization := &pluginInitialization{done: make(chan struct{})}
		h.initializing[file.ID] = initialization
		h.mu.Unlock()

		if previous != nil {
			restoreRequired, errQuiesce := h.callQuiesce(ctx, previous)
			if restoreRequired {
				quiescedReplacements[previous] = struct{}{}
			}
			if errQuiesce != nil || ctx.Err() != nil {
				h.finishAbortedInitialization(file.ID, initialization)
				if errQuiesce != nil {
					return fmt.Errorf("pluginhost: quiesce replacement plugin %s: %w", file.ID, errQuiesce)
				}
				return ctx.Err()
			}
		}

		loaded, errLoad := h.load(file)
		if errLoad == nil {
			artifactCurrent, errCurrent := pluginFileSnapshotCurrentUnderRoot(rc.Dir, file)
			if errCurrent != nil {
				errLoad = fmt.Errorf("verify plugin artifact after load: %w", errCurrent)
			} else if !artifactCurrent {
				errLoad = fmt.Errorf("plugin artifact changed while loading: %s", file.Path)
			}
		}
		h.mu.Lock()
		if h.initializing[file.ID] == initialization {
			delete(h.initializing, file.ID)
			close(initialization.done)
		}
		applyCurrent := h.configApplyCurrentLocked(generation)
		pluginCurrent := h.pluginGenerationCurrentLocked(file.ID, pluginGeneration)
		loadedCurrent := h.loaded[file.ID] == previous
		var retirementDone <-chan struct{}
		if errLoad == nil && applyCurrent && pluginCurrent && loadedCurrent {
			h.pendingLoaded[file.ID] = loaded
		} else {
			delete(h.loading, file.ID)
			retirementDone = h.trackRetiredPluginLocked(loaded)
		}
		h.mu.Unlock()
		if errLoad != nil {
			h.shutdownPreparedDiscardedPlugin(loaded, retirementDone)
			log.Warnf("pluginhost: failed to load plugin %s from %s: %v", file.ID, file.Path, errLoad)
			if !applyCurrent {
				return nil
			}
			if previous != nil {
				return fmt.Errorf("pluginhost: load replacement plugin %s: %w", file.ID, errLoad)
			}
			continue
		}
		if !applyCurrent || !pluginCurrent || !loadedCurrent {
			h.shutdownPreparedDiscardedPlugin(loaded, retirementDone)
			if !applyCurrent {
				return nil
			}
			continue
		}
		lp = loaded
		pendingLoads[lp] = pendingPluginLoad{plugin: lp, previous: previous}
		log.WithFields(pluginLogFields(file.ID, "", file.Version, file.Path)).Info("pluginhost: plugin loaded")
		plugin, okCall := h.callRegister(ctx, lp, item)
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		h.mu.Lock()
		applyCurrent = h.configApplyCurrentLocked(generation)
		pluginCurrent = h.pluginGenerationCurrentLocked(file.ID, pluginGeneration)
		loadedCurrent = h.loaded[file.ID] == previous
		pendingCurrent := h.pendingLoaded[file.ID] == lp
		loadedCurrent = loadedCurrent && pendingCurrent
		if okCall && applyCurrent && pluginCurrent && loadedCurrent {
			plugin.Metadata = clonePluginMetadata(plugin.Metadata)
			lp.name = strings.TrimSpace(plugin.Metadata.Name)
			lp.configYAML = bytes.Clone(item.ConfigYAML)
			if strings.TrimSpace(lp.version) == "" {
				lp.version = strings.TrimSpace(plugin.Metadata.Version)
			}
		}
		discardCandidate := false
		var discardRetirementDone <-chan struct{}
		if !okCall || !applyCurrent || !pluginCurrent || !loadedCurrent {
			if h.pendingLoaded[file.ID] == lp {
				delete(h.pendingLoaded, file.ID)
				delete(h.loading, file.ID)
				delete(pendingLoads, lp)
				discardRetirementDone = h.trackRetiredPluginLocked(lp)
				discardCandidate = true
			} else if !pendingCurrent {
				delete(pendingLoads, lp)
			}
		}
		h.mu.Unlock()
		if discardCandidate {
			h.shutdownPreparedDiscardedPlugin(lp, discardRetirementDone)
		}
		if !applyCurrent {
			return nil
		}
		if !okCall {
			if previous != nil {
				return fmt.Errorf("pluginhost: register replacement plugin %s", file.ID)
			}
			continue
		}
		if !pluginCurrent || !loadedCurrent {
			continue
		}
		records = append(
			records, capabilityRecord{
				id:       file.ID,
				path:     file.Path,
				version:  file.Version,
				priority: item.Priority,
				meta:     plugin.Metadata,
				plugin:   plugin,
			},
		)
		loadedFiles = append(loadedFiles, file)
		recordPlugins[file.ID] = lp
		recordGenerations[file.ID] = pluginGeneration
	}

	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	publicationRootLock, errPublicationRootLock := pluginstore.LockRoot(rc.Dir)
	if errPublicationRootLock != nil {
		return fmt.Errorf("pluginhost: lock plugin root before publication: %w", errPublicationRootLock)
	}
	for _, file := range loadedFiles {
		current, errCurrent := pluginFileSnapshotCurrent(file)
		if errCurrent != nil {
			publicationRootLock.Unlock()
			return fmt.Errorf("pluginhost: verify plugin %s before publication: %w", file.ID, errCurrent)
		}
		if !current {
			publicationRootLock.Unlock()
			return fmt.Errorf("pluginhost: plugin artifact changed before publication: %s", file.Path)
		}
	}
	h.publicationMu.Lock()
	h.mu.Lock()
	if errContext := ctx.Err(); errContext != nil {
		h.mu.Unlock()
		h.publicationMu.Unlock()
		publicationRootLock.Unlock()
		return errContext
	}
	if !h.configApplyCurrentLocked(generation) {
		h.mu.Unlock()
		h.publicationMu.Unlock()
		publicationRootLock.Unlock()
		return nil
	}
	previousPluginIDs := make(map[string]struct{})
	if previousSnapshot := h.Snapshot(); previousSnapshot != nil {
		for _, record := range previousSnapshot.records {
			previousPluginIDs[record.id] = struct{}{}
		}
	}
	currentRecords := records[:0]
	currentFiles := loadedFiles[:0]
	retiredTargets := make([]pluginUnloadTarget, 0)
	publishedHotReloadLogs := make([]log.Fields, 0)
	loadedNowPlugins := make(map[string]struct{})
	for index, record := range records {
		candidate, loadedNow := pendingLoads[recordPlugins[record.id]]
		expectedLoaded := recordPlugins[record.id]
		if loadedNow {
			expectedLoaded = candidate.previous
		}
		if !h.pluginGenerationCurrentLocked(record.id, recordGenerations[record.id]) ||
			h.loaded[record.id] != expectedLoaded {
			continue
		}
		if loadedNow {
			if candidate.previous != nil {
				h.retireLoadedPluginLocked(candidate.previous)
				h.removePluginRuntimeStateLocked(record.id)
				publishedHotReloadLogs = append(
					publishedHotReloadLogs,
					pluginHotReloadLogFields(
						record.id, record.version, record.path,
						candidate.previous.version, candidate.previous.path,
					),
				)
			}
			delete(h.fused, record.id)
			h.advancePluginOperationLocked(record.id)
			h.loaded[record.id] = candidate.plugin
			delete(quiescedReplacements, candidate.previous)
			delete(h.pendingLoaded, record.id)
			delete(h.loading, record.id)
			delete(pendingLoads, candidate.plugin)
			loadedNowPlugins[record.id] = struct{}{}
		}
		currentRecords = append(currentRecords, record)
		currentFiles = append(currentFiles, loadedFiles[index])
		if loadedNow && candidate.previous != nil {
			retiredTargets = append(retiredTargets, h.takeRetiredPluginTargetsLocked(record.id)...)
		}
		delete(previousPluginIDs, record.id)
	}
	for id := range previousPluginIDs {
		h.advancePluginOperationLocked(id)
	}
	records = currentRecords
	loadedFiles = currentFiles
	sortRecords(records)
	cleanupFiles := h.cleanupFilesPending
	if len(loadedFiles) > 0 {
		h.cleanupFilesPending = false
	}
	h.runtimeConfig = cfg
	h.managementRoutes = make(map[string]managementRouteRecord)
	h.resourceRoutes = make(map[string]resourceRouteRecord)
	h.rebuildActivePluginMapsLocked(records)
	h.snapshot.Store(&Snapshot{enabled: true, records: records})
	h.mu.Unlock()
	publicationRootLock.Unlock()
	h.refreshUsagePlugins(records)
	h.publicationMu.Unlock()

	for _, record := range records {
		if _, loadedNow := loadedNowPlugins[record.id]; !loadedNow {
			continue
		}
		log.WithFields(
			pluginLogFieldsFromMetadata(record.id, record.meta, record.path),
		).Info("pluginhost: plugin registered")
	}
	h.refreshThinkingProviders(records)
	h.republishExternalAdapters(ctx)
	shutdownCtx, cancelShutdown := boundedPluginShutdownContext(ctx)
	if errWait := h.waitUsageRetirementsForTargets(shutdownCtx, retiredTargets); errWait != nil {
		log.WithError(errWait).Warn("pluginhost: usage retirement wait timed out")
	}
	completedRetirements, errShutdown := shutdownPluginTargets(shutdownCtx, retiredTargets)
	h.removeRetiredPluginTargets(completedRetirements)
	h.trackRetiredPluginTargets(pendingPluginTargets(retiredTargets, completedRetirements))
	if errShutdown != nil {
		log.WithError(errShutdown).Warn("pluginhost: retired plugin shutdown deferred")
	}
	cancelShutdown()
	for _, fields := range publishedHotReloadLogs {
		log.WithFields(fields).Info("pluginhost: plugin hot reloaded")
	}
	if cleanupFiles && len(loadedFiles) > 0 {
		cleanupRootLock, errCleanupLock := pluginstore.LockRoot(rc.Dir)
		if errCleanupLock != nil {
			log.Warnf("pluginhost: lock plugin root for cleanup: %v", errCleanupLock)
		} else {
			if errCleanup := cleanupUnselectedPluginFiles(cleanupCandidates, loadedFiles); errCleanup != nil {
				log.Warnf("pluginhost: failed to clean old plugin files: %v", errCleanup)
			}
			cleanupRootLock.Unlock()
		}
	}
	return nil
}

func (h *Host) republishExternalAdapters(ctx context.Context) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.RegisterFrontendAuthProviders()
	h.mu.Lock()
	modelRegistry := h.modelRegistry
	executorManager := h.executorManager
	executorModelRegistry := h.executorModelRegistry
	h.mu.Unlock()
	if modelRegistry != nil {
		h.RegisterModels(ctx, modelRegistry)
	}
	if executorManager != nil {
		h.RegisterExecutors(executorManager, executorModelRegistry)
	}
}

func pluginConfigFingerprints(cfg *config.Config) (map[string]string, error) {
	if cfg == nil || len(cfg.Plugins.Configs) == 0 {
		return nil, nil
	}
	fingerprints := make(map[string]string, len(cfg.Plugins.Configs))
	configuredIDs := make(map[string]string, len(cfg.Plugins.Configs))
	for id, item := range cfg.Plugins.Configs {
		canonicalID := pluginstore.CanonicalPluginID(id)
		if existingID, duplicate := configuredIDs[canonicalID]; duplicate {
			return nil, fmt.Errorf("duplicate plugin config ids %q and %q", existingID, id)
		}
		fingerprint, errFingerprint := config.PluginInstanceConfigFingerprint(item)
		if errFingerprint != nil {
			return nil, fmt.Errorf("fingerprint plugin %s config: %w", id, errFingerprint)
		}
		configuredIDs[canonicalID] = id
		fingerprints[canonicalID] = fingerprint
	}
	return fingerprints, nil
}

func (h *Host) beginConfigApply(cfg *config.Config) (uint64, *config.Config) {
	h.publicationMu.Lock()
	h.mu.Lock()
	previousRuntimeConfig := h.runtimeConfig
	h.applyGeneration++
	generation := h.applyGeneration
	h.runtimeConfig = cfg
	h.mu.Unlock()
	h.publicationMu.Unlock()
	return generation, previousRuntimeConfig
}

func (h *Host) configApplyCurrentLocked(generation uint64) bool {
	return h != nil && generation != 0 && h.applyGeneration == generation
}

func (h *Host) pluginGenerationCurrentLocked(id string, generation uint64) bool {
	return h != nil && h.pluginGenerations[id] == generation
}

func (h *Host) clearRuntimeRoutesForApply(generation uint64) bool {
	h.publicationMu.Lock()
	defer h.publicationMu.Unlock()
	h.mu.Lock()
	if !h.configApplyCurrentLocked(generation) {
		h.mu.Unlock()
		return false
	}
	h.advanceActivePluginOperationsLocked()
	h.managementRoutes = make(map[string]managementRouteRecord)
	h.resourceRoutes = make(map[string]resourceRouteRecord)
	h.rebuildActivePluginMapsLocked(nil)
	h.snapshot.Store(emptySnapshot())
	externalPublications := h.takeAllExternalPublicationsLocked()
	h.mu.Unlock()
	externalPublications.unpublish()
	h.refreshUsagePlugins(nil)
	h.clearThinkingProviders()
	h.clearAccessProvidersForPublication()
	return true
}

// InvalidateRuntime removes plugin capabilities without waiting for plugin lifecycle calls.
func (h *Host) InvalidateRuntime() {
	if h == nil {
		return
	}
	h.publicationMu.Lock()
	h.mu.Lock()
	h.advanceActivePluginOperationsLocked()
	h.applyGeneration++
	h.runtimeConfig = nil
	h.managementRoutes = make(map[string]managementRouteRecord)
	h.resourceRoutes = make(map[string]resourceRouteRecord)
	h.rebuildActivePluginMapsLocked(nil)
	h.snapshot.Store(emptySnapshot())
	externalPublications := h.takeAllExternalPublicationsLocked()
	h.mu.Unlock()
	externalPublications.unpublish()
	h.refreshUsagePlugins(nil)
	h.clearThinkingProviders()
	h.clearAccessProvidersForPublication()
	h.publicationMu.Unlock()
}

func (h *Host) shutdownPreparedDiscardedPlugin(lp *loadedPlugin, retirementDone <-chan struct{}) {
	if h == nil || lp == nil || lp.client == nil {
		return
	}
	h.watchRetiredPlugin(lp, retirementDone)
	shutdownCtx, cancelShutdown := boundedPluginShutdownContext(context.Background())
	if errShutdown := shutdownPluginClient(shutdownCtx, lp.client); errShutdown != nil {
		log.WithError(errShutdown).WithField("plugin_id", lp.id).Warn(
			"pluginhost: discarded plugin shutdown deferred",
		)
	} else {
		h.removeRetiredPluginTargets([]pluginUnloadTarget{pluginUnloadTargetFromLoaded(lp)})
	}
	cancelShutdown()
}

func (h *Host) pluginLoaded(id string) bool {
	if h == nil {
		return false
	}
	id = pluginstore.CanonicalPluginID(id)
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.loaded[id]
	if ok {
		return true
	}
	return len(h.retired[id]) > 0
}

// PluginBusy reports whether a plugin dynamic library is loaded or being loaded.
func (h *Host) PluginBusy(id string) bool {
	if h == nil {
		return false
	}
	id = pluginstore.CanonicalPluginID(id)
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.loaded[id]; ok {
		return true
	}
	if len(h.retired[id]) > 0 {
		return true
	}
	if h.initializing[id] != nil {
		return true
	}
	_, ok := h.loading[id]
	return ok
}

func (h *Host) load(file pluginFile) (*loadedPlugin, error) {
	loadFile := file
	if strings.TrimSpace(file.loadPath) != "" {
		loadFile.Path = file.loadPath
	}
	client, errOpen := h.loader.Open(loadFile, h)
	if errOpen != nil {
		return nil, errOpen
	}

	return &loadedPlugin{
		id:      file.ID,
		path:    file.Path,
		version: file.Version,
		client:  newGuardedPluginClient(client),
	}, nil
}

func (h *Host) withLoadedPluginFallbacks(
	files []pluginFile,
	items map[string]runtimeItemConfig,
	desired map[string]string,
) []pluginFile {
	if h == nil || len(desired) == 0 {
		return files
	}
	selected := make(map[string]struct{}, len(files))
	for _, file := range files {
		id := pluginstore.CanonicalPluginID(file.ID)
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if _, ok := selected[id]; ok {
			continue
		}
		if item, ok := items[id]; ok && !item.Enabled {
			continue
		}
		lp := h.loaded[id]
		if lp == nil || strings.TrimSpace(lp.path) == "" {
			continue
		}
		files = append(
			files, pluginFile{
				ID:      id,
				Path:    lp.path,
				Version: strings.TrimSpace(lp.version),
			},
		)
		selected[id] = struct{}{}
	}
	return files
}

type pluginExternalPublications struct {
	host                   *Host
	pluginID               string
	modelRegistry          modelRegistry
	modelClientIDs         []string
	executorModelRegistry  modelRegistry
	executorModelClientIDs []string
	executorManager        executorManager
	executorProviders      []string
}

func (h *Host) takePluginExternalPublicationsLocked(id string) pluginExternalPublications {
	publications := pluginExternalPublications{
		host:                  h,
		pluginID:              id,
		modelRegistry:         h.modelRegistry,
		executorModelRegistry: h.executorModelRegistry,
		executorManager:       h.executorManager,
	}
	for clientID, owner := range h.modelClientOwners {
		if owner != id {
			continue
		}
		publications.modelClientIDs = append(publications.modelClientIDs, clientID)
		delete(h.modelClientOwners, clientID)
		delete(h.modelClientIDs, clientID)
	}
	for clientID, owner := range h.executorModelClientOwners {
		if owner != id {
			continue
		}
		publications.executorModelClientIDs = append(publications.executorModelClientIDs, clientID)
		delete(h.executorModelClientOwners, clientID)
		delete(h.executorModelClientIDs, clientID)
	}
	for provider, owner := range h.executorProviderOwners {
		if owner != id {
			continue
		}
		publications.executorProviders = append(publications.executorProviders, provider)
		delete(h.executorProviderOwners, provider)
		delete(h.executorProviders, provider)
		delete(h.providerModels, provider)
	}
	sort.Strings(publications.modelClientIDs)
	sort.Strings(publications.executorModelClientIDs)
	sort.Strings(publications.executorProviders)
	return publications
}

func (h *Host) takeAllExternalPublicationsLocked() pluginExternalPublications {
	publications := pluginExternalPublications{
		host:                  h,
		modelRegistry:         h.modelRegistry,
		executorModelRegistry: h.executorModelRegistry,
		executorManager:       h.executorManager,
		modelClientIDs:        make([]string, 0, len(h.modelClientIDs)),
		executorModelClientIDs: make(
			[]string, 0, len(h.executorModelClientIDs),
		),
		executorProviders: make([]string, 0, len(h.executorProviders)),
	}
	for clientID := range h.modelClientIDs {
		publications.modelClientIDs = append(publications.modelClientIDs, clientID)
	}
	for clientID := range h.executorModelClientIDs {
		publications.executorModelClientIDs = append(publications.executorModelClientIDs, clientID)
	}
	for provider := range h.executorProviders {
		publications.executorProviders = append(publications.executorProviders, provider)
	}
	sort.Strings(publications.modelClientIDs)
	sort.Strings(publications.executorModelClientIDs)
	sort.Strings(publications.executorProviders)
	h.modelClientIDs = make(map[string]struct{})
	h.modelClientOwners = make(map[string]string)
	h.executorModelClientIDs = make(map[string]struct{})
	h.executorModelClientOwners = make(map[string]string)
	h.executorProviders = make(map[string]struct{})
	h.executorProviderOwners = make(map[string]string)
	h.providerModels = make(map[string][]*registryModelInfo)
	return publications
}

func (p pluginExternalPublications) unpublish() {
	if p.modelRegistry != nil {
		for _, clientID := range p.modelClientIDs {
			p.modelRegistry.UnregisterClient(clientID)
		}
	}
	if p.executorModelRegistry != nil {
		for _, clientID := range p.executorModelClientIDs {
			p.executorModelRegistry.UnregisterClient(clientID)
		}
	}
	if p.executorManager == nil {
		return
	}
	for _, provider := range p.executorProviders {
		existing, okExecutor := p.executorManager.Executor(provider)
		if !okExecutor || existing == nil {
			continue
		}
		adapter, okAdapter := existing.(*executorAdapter)
		if !okAdapter || adapter.host != p.host || p.pluginID != "" && adapter.pluginID != p.pluginID {
			continue
		}
		p.executorManager.UnregisterExecutorIfSame(provider, existing)
	}
}

var errPluginInitializationInProgress = errors.New(
	"plugin initialization is still in progress; process restart required",
)

// ErrPluginRuntimeSuperseded reports that a conditional unload no longer owns the runtime.
var ErrPluginRuntimeSuperseded = errors.New("plugin runtime identity changed")

// UnloadPlugin removes one plugin from the active runtime and closes its dynamic library.
func (h *Host) UnloadPlugin(id string) bool {
	unloaded, errUnload := h.UnloadPluginWithError(id)
	if errUnload != nil {
		log.WithError(errUnload).WithField("plugin_id", strings.TrimSpace(id)).Warn(
			"pluginhost: plugin shutdown deferred",
		)
	}
	return unloaded
}

// UnloadPluginWithError removes one plugin and reports deferred shutdown errors.
func (h *Host) UnloadPluginWithError(id string) (bool, error) {
	shutdownCtx, cancelShutdown := boundedPluginShutdownContext(context.Background())
	defer cancelShutdown()
	return h.unloadPluginContext(shutdownCtx, id, nil)
}

// UnloadPluginIfCurrentWithError removes a plugin only while identity still owns the runtime.
func (h *Host) UnloadPluginIfCurrentWithError(identity PluginRuntimeIdentity) (bool, error) {
	shutdownCtx, cancelShutdown := boundedPluginShutdownContext(context.Background())
	defer cancelShutdown()
	return h.unloadPluginContext(shutdownCtx, identity.ID, &identity)
}

func (h *Host) unloadPluginContext(
	ctx context.Context,
	id string,
	expectedIdentity *PluginRuntimeIdentity,
) (bool, error) {
	if h == nil {
		return false, nil
	}
	id = pluginstore.CanonicalPluginID(id)
	if id == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h.publicationMu.Lock()
	targets := make([]pluginUnloadTarget, 0)
	h.mu.Lock()
	if expectedIdentity != nil && !h.pluginRuntimeIdentityCurrentLocked(*expectedIdentity) {
		h.mu.Unlock()
		h.publicationMu.Unlock()
		return false, ErrPluginRuntimeSuperseded
	}
	if h.pluginGenerations == nil {
		h.pluginGenerations = make(map[string]uint64)
	}
	h.advancePluginOperationLocked(id)
	h.pluginGenerations[id]++
	_, loading := h.loading[id]
	initializationPending := h.initializing[id] != nil
	pending := h.pendingLoaded[id]
	lp := h.loaded[id]
	if pending != nil {
		targets = append(targets, pluginUnloadTargetFromLoaded(pending))
	}
	if lp != nil {
		targets = append(
			targets,
			pluginUnloadTargetFromLoaded(lp),
		)
	}
	for _, retired := range h.retired[id] {
		if retired == nil {
			continue
		}
		targets = append(targets, pluginUnloadTargetFromLoaded(retired))
	}
	if len(targets) == 0 && !loading && !initializationPending {
		h.mu.Unlock()
		h.publicationMu.Unlock()
		return false, nil
	}
	delete(h.loaded, id)
	h.retired[id] = loadedPluginsFromTargets(targets)
	if !initializationPending {
		delete(h.loading, id)
	}
	delete(h.pendingLoaded, id)
	delete(h.fused, id)
	delete(h.activePluginVersions, id)
	delete(h.activePluginPaths, id)
	for _, target := range targets {
		delete(h.pluginFileVersions, cleanPluginPath(target.path))
	}
	records, enabled := h.snapshotWithoutPluginLocked(id)
	h.removePluginRuntimeStateLocked(id)
	h.snapshot.Store(&Snapshot{enabled: enabled, records: records})
	externalPublications := h.takePluginExternalPublicationsLocked(id)
	h.mu.Unlock()
	externalPublications.unpublish()
	h.refreshUsagePlugins(records)
	h.unregisterThinkingProvider(id)
	h.unregisterAccessProviders(id)
	h.publicationMu.Unlock()

	waitErr := h.waitUsageRetirementsForTargets(ctx, targets)
	completedTargets, shutdownErr := shutdownPluginTargets(ctx, targets)
	h.removeRetiredPluginTargets(completedTargets)
	h.trackRetiredPluginTargets(pendingPluginTargets(targets, completedTargets))
	for _, target := range completedTargets {
		log.WithFields(
			pluginLogFields(
				target.id, target.name, target.version, target.path,
			),
		).Info("pluginhost: plugin unloaded")
	}
	var initializationErr error
	if initializationPending {
		initializationErr = fmt.Errorf("plugin %s: %w", id, errPluginInitializationInProgress)
	}
	return true, errors.Join(waitErr, shutdownErr, initializationErr)
}

// shutdownAll removes active plugin capabilities and closes all loaded dynamic libraries.
func (h *Host) shutdownAll() {
	shutdownCtx, cancelShutdown := boundedPluginShutdownContext(context.Background())
	defer cancelShutdown()
	if errShutdown := h.ShutdownAllContext(shutdownCtx); errShutdown != nil {
		log.WithError(errShutdown).Warn("pluginhost: plugin shutdown deferred")
	}
}

// ShutdownAllContext removes all plugin capabilities and waits up to ctx for
// in-flight callbacks before releasing dynamic libraries.
func (h *Host) ShutdownAllContext(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if h.shutdownCancel != nil {
		h.shutdownCancel()
	}

	h.publicationMu.Lock()
	targets := make([]pluginUnloadTarget, 0)
	h.mu.Lock()
	h.advanceActivePluginOperationsLocked()
	h.applyGeneration++
	h.runtimeConfig = nil
	for _, lp := range h.loaded {
		if lp == nil || lp.client == nil {
			continue
		}
		targets = append(targets, pluginUnloadTargetFromLoaded(lp))
	}
	for _, lp := range h.pendingLoaded {
		if lp == nil || lp.client == nil {
			continue
		}
		targets = append(targets, pluginUnloadTargetFromLoaded(lp))
	}
	for _, retiredPlugins := range h.retired {
		for _, lp := range retiredPlugins {
			if lp == nil || lp.client == nil {
				continue
			}
			targets = append(targets, pluginUnloadTargetFromLoaded(lp))
		}
	}
	h.loaded = make(map[string]*loadedPlugin)
	h.retired = make(map[string][]*loadedPlugin)
	for _, plugin := range loadedPluginsFromTargets(targets) {
		h.retired[plugin.id] = append(h.retired[plugin.id], plugin)
	}
	initializingIDs := make([]string, 0, len(h.initializing))
	h.loading = make(map[string]struct{}, len(h.initializing))
	for id := range h.initializing {
		initializingIDs = append(initializingIDs, id)
		h.loading[id] = struct{}{}
	}
	sort.Strings(initializingIDs)
	h.pendingLoaded = make(map[string]*loadedPlugin)
	externalPublications := h.takeAllExternalPublicationsLocked()
	h.modelClientIDs = make(map[string]struct{})
	h.executorModelClientIDs = make(map[string]struct{})
	h.modelProviders = make(map[string]string)
	h.modelRegistrations = make(map[string]pluginModelRegistration)
	h.providerModels = make(map[string][]*registryModelInfo)
	h.executorProviders = make(map[string]struct{})
	h.commandLineFlags = make(map[string]commandLineFlagRecord)
	h.commandLineHits = make(map[string]struct{})
	h.managementRoutes = make(map[string]managementRouteRecord)
	h.resourceRoutes = make(map[string]resourceRouteRecord)
	h.pluginFileVersions = make(map[string]string)
	h.activePluginVersions = make(map[string]string)
	h.activePluginPaths = make(map[string]string)
	h.snapshot.Store(emptySnapshot())
	h.mu.Unlock()

	externalPublications.unpublish()
	h.refreshUsagePlugins(nil)
	h.clearThinkingProviders()
	h.clearAccessProvidersForPublication()
	h.publicationMu.Unlock()
	waitErr := h.waitAllUsageRetirementsContext(ctx)
	completedTargets, shutdownErr := shutdownPluginTargets(ctx, targets)
	h.removeRetiredPluginTargets(completedTargets)
	h.trackRetiredPluginTargets(pendingPluginTargets(targets, completedTargets))
	for _, target := range completedTargets {
		log.WithFields(
			pluginLogFields(
				target.id, target.name, target.version, target.path,
			),
		).Info("pluginhost: plugin unloaded")
	}
	var initializationErr error
	if len(initializingIDs) > 0 {
		initializationErr = fmt.Errorf(
			"plugins %s: %w",
			strings.Join(initializingIDs, ", "),
			errPluginInitializationInProgress,
		)
	}
	return errors.Join(waitErr, shutdownErr, initializationErr)
}

func cleanPluginPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func (h *Host) retireLoadedPluginLocked(lp *loadedPlugin) {
	if h == nil || lp == nil {
		return
	}
	if slices.Contains(h.retired[lp.id], lp) {
		return
	}
	h.retired[lp.id] = append(h.retired[lp.id], lp)
}

func (h *Host) takeRetiredPluginTargetsLocked(id string) []pluginUnloadTarget {
	if h == nil {
		return nil
	}
	retired := h.retired[id]
	if len(retired) == 0 {
		return nil
	}
	targets := make([]pluginUnloadTarget, 0, len(retired))
	for _, lp := range retired {
		if lp == nil {
			continue
		}
		targets = append(targets, pluginUnloadTargetFromLoaded(lp))
	}
	return targets
}

func shutdownPluginTargets(
	ctx context.Context,
	targets []pluginUnloadTarget,
) ([]pluginUnloadTarget, error) {
	completed := make([]pluginUnloadTarget, 0, len(targets))
	var shutdownErrors []error
	for _, target := range targets {
		if errShutdown := shutdownPluginClient(ctx, target.client); errShutdown != nil {
			shutdownErrors = append(
				shutdownErrors,
				fmt.Errorf("shutdown plugin %s: %w", target.id, errShutdown),
			)
			continue
		}
		completed = append(completed, target)
		log.WithFields(
			pluginLogFields(
				target.id, target.name, target.version, target.path,
			),
		).Info("pluginhost: retired plugin unloaded")
	}
	return completed, errors.Join(shutdownErrors...)
}

func pendingPluginTargets(
	targets,
	completed []pluginUnloadTarget,
) []pluginUnloadTarget {
	completedPlugins := make(map[*loadedPlugin]struct{}, len(completed))
	for _, target := range completed {
		if target.plugin != nil {
			completedPlugins[target.plugin] = struct{}{}
		}
	}
	pending := make([]pluginUnloadTarget, 0, len(targets)-len(completedPlugins))
	for _, target := range targets {
		if _, ok := completedPlugins[target.plugin]; ok {
			continue
		}
		pending = append(pending, target)
	}
	return pending
}

func loadedPluginsFromTargets(targets []pluginUnloadTarget) []*loadedPlugin {
	plugins := make([]*loadedPlugin, 0, len(targets))
	seen := make(map[*loadedPlugin]struct{}, len(targets))
	for _, target := range targets {
		if target.plugin == nil {
			continue
		}
		if _, ok := seen[target.plugin]; ok {
			continue
		}
		seen[target.plugin] = struct{}{}
		plugins = append(plugins, target.plugin)
	}
	return plugins
}

func (h *Host) trackRetiredPlugin(plugin *loadedPlugin) {
	if h == nil || plugin == nil {
		return
	}
	h.mu.Lock()
	done := h.trackRetiredPluginLocked(plugin)
	h.mu.Unlock()
	h.watchRetiredPlugin(plugin, done)
}

func (h *Host) trackRetiredPluginLocked(plugin *loadedPlugin) <-chan struct{} {
	if h == nil || plugin == nil {
		return nil
	}
	h.retireLoadedPluginLocked(plugin)
	guarded, ok := plugin.client.(*guardedPluginClient)
	if !ok || guarded == nil || plugin.retirementWatch {
		return nil
	}
	plugin.retirementWatch = true
	return guarded.shutdownDone
}

func (h *Host) watchRetiredPlugin(plugin *loadedPlugin, done <-chan struct{}) {
	if h == nil || plugin == nil || done == nil {
		return
	}
	go func() {
		<-done
		h.removeRetiredPluginTargets([]pluginUnloadTarget{pluginUnloadTargetFromLoaded(plugin)})
	}()
}

func (h *Host) trackRetiredPluginTargets(targets []pluginUnloadTarget) {
	for _, target := range targets {
		h.trackRetiredPlugin(target.plugin)
	}
}

func (h *Host) removeRetiredPluginTargets(targets []pluginUnloadTarget) {
	if h == nil || len(targets) == 0 {
		return
	}
	completed := make(map[*loadedPlugin]struct{}, len(targets))
	for _, target := range targets {
		if target.plugin != nil {
			completed[target.plugin] = struct{}{}
		}
	}
	if len(completed) == 0 {
		return
	}
	h.mu.Lock()
	for id, retired := range h.retired {
		remaining := retired[:0]
		for _, plugin := range retired {
			if _, ok := completed[plugin]; ok {
				continue
			}
			remaining = append(remaining, plugin)
		}
		if len(remaining) == 0 {
			delete(h.retired, id)
		} else {
			h.retired[id] = remaining
		}
	}
	h.mu.Unlock()
}

func (h *Host) recordCurrent(record capabilityRecord) bool {
	if strings.TrimSpace(record.path) == "" && strings.TrimSpace(record.version) == "" {
		return true
	}
	return h.pluginIdentityCurrent(record.id, record.path, record.version)
}

func (h *Host) pluginIdentityCurrent(id string, path string, version string) bool {
	if h == nil {
		return false
	}
	version = strings.TrimSpace(version)
	h.mu.Lock()
	defer h.mu.Unlock()
	id = pluginstore.CanonicalPluginID(id)
	if id == "" {
		return false
	}
	path = cleanPluginPath(path)
	if path == "" || h.activePluginPaths[id] != path {
		return false
	}
	activePathVersion, okVersion := h.pluginFileVersions[path]
	if !okVersion || activePathVersion != version {
		return false
	}
	return h.activePluginVersions[id] == version
}

func (h *Host) snapshotWithoutPluginLocked(id string) ([]capabilityRecord, bool) {
	raw := h.snapshot.Load()
	snap, _ := raw.(*Snapshot)
	if snap == nil || len(snap.records) == 0 {
		return nil, snap != nil && snap.enabled
	}
	records := make([]capabilityRecord, 0, len(snap.records))
	for _, record := range snap.records {
		if record.id == id {
			continue
		}
		records = append(records, record)
	}
	return records, snap.enabled
}

func (h *Host) removePluginRuntimeStateLocked(id string) {
	for key, record := range h.managementRoutes {
		if record.pluginID == id {
			delete(h.managementRoutes, key)
		}
	}
	for key, record := range h.resourceRoutes {
		if record.pluginID == id {
			delete(h.resourceRoutes, key)
		}
	}
	for name, record := range h.commandLineFlags {
		if record.pluginID == id {
			delete(h.commandLineFlags, name)
			delete(h.commandLineHits, name)
		}
	}
	if registration, ok := h.modelRegistrations[id]; ok {
		delete(h.providerModels, registration.provider)
	}
	delete(h.modelProviders, id)
	delete(h.modelRegistrations, id)
}

func (h *Host) rebuildActivePluginMapsLocked(records []capabilityRecord) {
	h.pluginFileVersions = make(map[string]string, len(records))
	h.activePluginVersions = make(map[string]string, len(records))
	h.activePluginPaths = make(map[string]string, len(records))
	for _, record := range records {
		id := strings.TrimSpace(record.id)
		path := cleanPluginPath(record.path)
		if id == "" || path == "" {
			continue
		}
		h.pluginFileVersions[path] = strings.TrimSpace(record.version)
		h.activePluginVersions[id] = strings.TrimSpace(record.version)
		h.activePluginPaths[id] = path
	}
}

func (h *Host) finishAbortedInitialization(id string, initialization *pluginInitialization) {
	if h == nil || initialization == nil {
		return
	}
	h.mu.Lock()
	if h.initializing[id] == initialization {
		delete(h.initializing, id)
		close(initialization.done)
	}
	if h.pendingLoaded[id] == nil {
		delete(h.loading, id)
	}
	h.mu.Unlock()
}

func (h *Host) callQuiesce(ctx context.Context, lp *loadedPlugin) (bool, error) {
	if h == nil || lp == nil || lp.client == nil {
		return false, nil
	}
	actionStarted := false
	errQuiesce, okCall := h.safePluginAction(ctx, lp.id, pluginabi.MethodPluginQuiesce, func() error {
		actionStarted = true
		_, errCall := callPlugin[rpcEmptyResponse](ctx, lp.client, pluginabi.MethodPluginQuiesce, rpcEmptyResponse{})
		return errCall
	})
	if !okCall {
		return actionStarted, errQuiesce
	}
	if errQuiesce == nil {
		return true, nil
	}
	entry := log.WithError(errQuiesce).WithField("plugin_id", lp.id)
	switch {
	case quiesceUnsupported(errQuiesce):
		entry.Debug("pluginhost: plugin quiesce unsupported")
		return false, nil
	case errors.Is(errQuiesce, context.Canceled), errors.Is(errQuiesce, context.DeadlineExceeded):
		entry.Debug("pluginhost: plugin quiesce canceled")
	default:
		entry.Warn("pluginhost: plugin quiesce failed")
	}
	return true, errQuiesce
}

func quiesceUnsupported(errQuiesce error) bool {
	if errQuiesce == nil {
		return false
	}
	if errors.Is(errQuiesce, errors.ErrUnsupported) {
		return true
	}
	var errRPC rpcError
	if errors.As(errQuiesce, &errRPC) {
		switch strings.ToLower(strings.TrimSpace(errRPC.Code)) {
		case "unknown_method", "method_not_found", "unsupported_method":
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(errQuiesce.Error()))
	for _, indication := range []string{
		"unknown method",
		"method not found",
		"unsupported method",
		"method unsupported",
		"method is not supported",
		"method not supported",
	} {
		if strings.Contains(message, indication) {
			return true
		}
	}
	return false
}

func (h *Host) restoreQuiescedPlugin(lp *loadedPlugin) error {
	if h == nil || lp == nil || lp.client == nil {
		return nil
	}
	h.mu.Lock()
	current := h.loaded[lp.id] == lp
	configYAML := bytes.Clone(lp.configYAML)
	h.mu.Unlock()
	if !current {
		return nil
	}
	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), pluginShutdownTimeout)
	defer cancelRestore()
	plugin, errRestore := registerRPCPlugin(
		restoreCtx,
		h,
		lp.id,
		lp.client,
		pluginabi.MethodPluginReconfigure,
		configYAML,
	)
	if errRestore != nil {
		return fmt.Errorf("pluginhost: restore quiesced plugin %s: %w", lp.id, errRestore)
	}
	if !validPlugin(plugin) {
		return fmt.Errorf("pluginhost: restore quiesced plugin %s returned invalid registration", lp.id)
	}
	h.mu.Lock()
	restored := h.loaded[lp.id] == lp
	if restored {
		delete(h.fused, lp.id)
	}
	h.mu.Unlock()
	if restored {
		records := h.activeRecords()
		h.refreshUsagePlugins(records)
		h.refreshThinkingProviders(records)
	}
	return nil
}

func (h *Host) safePluginAction(
	ctx context.Context,
	id string,
	method string,
	fn func() error,
) (err error, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(id, method, recovered)
			err = fmt.Errorf("plugin %s %s panic: %v", id, method, recovered)
			ok = false
		}
	}()
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err(), false
		default:
		}
	}
	return fn(), true
}

func (h *Host) callRegister(ctx context.Context, lp *loadedPlugin, item runtimeItemConfig) (pluginapi.Plugin, bool) {
	if lp == nil {
		return pluginapi.Plugin{}, false
	}

	plugin, okCall := h.safePluginCall(
		ctx, lp.id, pluginabi.MethodPluginRegister, func() pluginapi.Plugin {
			plugin, errRegister := registerRPCPlugin(
				ctx,
				h,
				lp.id,
				lp.client,
				pluginabi.MethodPluginRegister,
				item.ConfigYAML,
			)
			if errRegister != nil {
				log.Warnf(
					"pluginhost: plugin %s %s failed: %v",
					lp.id,
					pluginabi.MethodPluginRegister,
					errRegister,
				)
				return pluginapi.Plugin{}
			}
			return plugin
		},
	)
	if !okCall {
		return pluginapi.Plugin{}, false
	}
	if !validPlugin(plugin) {
		log.Warnf("pluginhost: plugin %s returned invalid metadata or no capabilities", lp.id)
		return pluginapi.Plugin{}, false
	}
	return plugin, true
}

func (h *Host) safePluginCall(ctx context.Context, id, method string, fn func() pluginapi.Plugin) (
	out pluginapi.Plugin,
	ok bool,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(id, method, recovered)
			out = pluginapi.Plugin{}
			ok = false
		}
	}()

	if ctx != nil {
		select {
		case <-ctx.Done():
			return pluginapi.Plugin{}, false
		default:
		}
	}
	return fn(), true
}

func validPlugin(plugin pluginapi.Plugin) bool {
	if strings.TrimSpace(plugin.Metadata.Name) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.Version) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.Author) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.GitHubRepository) == "" {
		return false
	}
	caps := plugin.Capabilities
	return caps.ModelRegistrar != nil ||
		caps.ModelProvider != nil ||
		caps.AuthProvider != nil ||
		caps.FrontendAuthProvider != nil ||
		caps.Scheduler != nil ||
		caps.Executor != nil ||
		caps.RequestTranslator != nil ||
		caps.RequestNormalizer != nil ||
		caps.RequestInterceptor != nil ||
		caps.RequestLifecyclePlugin != nil ||
		caps.ResponseTranslator != nil ||
		caps.ResponseBeforeTranslator != nil ||
		caps.ResponseAfterTranslator != nil ||
		caps.ResponseInterceptor != nil ||
		caps.StreamChunkInterceptor != nil ||
		caps.WebSocketResponseObserver != nil ||
		caps.ThinkingApplier != nil ||
		caps.UsagePlugin != nil ||
		caps.CommandLinePlugin != nil ||
		caps.ManagementAPI != nil ||
		caps.QuotaProvider != nil
}
