// Package watcher implements watcher client lifecycle logic and persistence helpers.
// It reloads clients, handles incremental auth file changes, and persists updates when supported.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (w *Watcher) reloadClients(rescanAuth bool, affectedOAuthProviders []string, forceAuthRefresh bool) bool {
	w.clientsMutex.RLock()
	cfg := w.config
	w.clientsMutex.RUnlock()

	if cfg != nil && w.reloadCallback != nil {
		log.Debugf("triggering server update callback before auth refresh")
		if !w.reloadCallback(cfg) {
			log.Errorf("server update callback rejected config reload; skipping auth refresh")
			return false
		}
	}
	return w.reloadClientsWithConfig(cfg, rescanAuth, affectedOAuthProviders, forceAuthRefresh)
}

func (w *Watcher) reloadClientsWithConfig(
	cfg *config.Config,
	rescanAuth bool,
	affectedOAuthProviders []string,
	forceAuthRefresh bool,
) bool {
	log.Debugf("starting full client load process")

	if cfg == nil {
		log.Error("config is nil, cannot reload clients")
		return false
	}

	if len(affectedOAuthProviders) > 0 {
		w.clientsMutex.Lock()
		if w.currentAuths != nil {
			filtered := make(map[string]*coreauth.Auth, len(w.currentAuths))
			for id, auth := range w.currentAuths {
				if auth == nil {
					continue
				}
				provider := strings.ToLower(strings.TrimSpace(auth.Provider))
				if _, match := matchProvider(provider, affectedOAuthProviders); match {
					continue
				}
				filtered[id] = auth
			}
			w.currentAuths = filtered
			log.Debugf("applying oauth-excluded-models to providers %v", affectedOAuthProviders)
		} else {
			w.currentAuths = nil
		}
		w.clientsMutex.Unlock()
	}

	geminiAPIKeyCount, vertexCompatAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, xaiAPIKeyCount, openAICompatCount := BuildAPIKeyClients(
		cfg,
	)
	totalAPIKeyClients := geminiAPIKeyCount + vertexCompatAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + xaiAPIKeyCount + openAICompatCount
	log.Debugf("loaded %d API key clients", totalAPIKeyClients)

	var authFileCount int
	if rescanAuth {
		authFileCount = w.loadFileClients(cfg)
		log.Debugf("loaded %d file-based clients", authFileCount)
	} else {
		w.clientsMutex.RLock()
		authFileCount = len(w.lastAuthHashes)
		w.clientsMutex.RUnlock()
		log.Debugf("skipping auth directory rescan; retaining %d existing auth files", authFileCount)
	}

	if rescanAuth {
		w.authRescanMu.Lock()
		w.clientsMutex.Lock()
		parser := w.pluginAuthParser

		w.lastAuthHashes = make(map[string]string)
		w.lastAuthVersions = make(map[string]coreauth.PersistedVersion)
		cacheAuthContents := log.IsLevelEnabled(log.DebugLevel)
		if cacheAuthContents {
			w.lastAuthContents = make(map[string]*coreauth.Auth)
		} else {
			w.lastAuthContents = nil
		}
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory for hash cache: %v", errResolveAuthDir)
		} else if resolvedAuthDir != "" {
			_ = filepath.Walk(
				resolvedAuthDir, func(path string, info fs.FileInfo, err error) error {
					if err != nil {
						return nil
					}
					if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".json") {
						readAt := time.Now()
						if data, errReadFile := os.ReadFile(path); errReadFile == nil && len(data) > 0 {
							sum := sha256.Sum256(data)
							normalizedPath := w.normalizeAuthPath(path)
							w.lastAuthHashes[normalizedPath] = hex.EncodeToString(sum[:])
							if digest, errDigest := coreauth.DigestPersistedJSON(data); errDigest == nil {
								w.lastAuthVersions[normalizedPath] = coreauth.PersistedVersion{
									Path: path, SHA256: digest,
								}
							} else {
								log.WithError(errDigest).Warnf(
									"failed to capture auth version for %s", filepath.Base(path),
								)
							}
							if cacheAuthContents {
								var auth coreauth.Auth
								if errParse := json.Unmarshal(data, &auth); errParse == nil {
									w.lastAuthContents[normalizedPath] = &auth
								}
							}
							ctx := &synthesizer.SynthesisContext{
								Config:           cfg,
								AuthDir:          resolvedAuthDir,
								Now:              readAt,
								IDGenerator:      synthesizer.NewStableIDGenerator(),
								PluginAuthParser: parser,
							}
							generated, errSynthesize := synthesizer.SynthesizeAuthFile(ctx, path, data)
							if errSynthesize != nil {
								log.Errorf("failed to synthesize auth file %s: %v", filepath.Base(path), errSynthesize)
							} else if len(generated) > 0 {
								if pathAuths := authSliceToMap(generated); len(pathAuths) > 0 {
									w.fileAuthsByPath[normalizedPath] = authIDSet(pathAuths)
								}
							}
						}
					}
					return nil
				},
			)
		}
		w.clientsMutex.Unlock()
		w.authRescanMu.Unlock()
	}

	totalNewClients := authFileCount + geminiAPIKeyCount + vertexCompatAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + xaiAPIKeyCount + openAICompatCount

	w.refreshAuthState(forceAuthRefresh)
	w.notifyUsageRefresh()

	log.Infof(
		"full client load complete - %d clients (%d auth files + %d Gemini API keys + %d Vertex API keys + %d Claude API keys + %d Codex keys + %d xAI keys + %d OpenAI-compat)",
		totalNewClients,
		authFileCount,
		geminiAPIKeyCount,
		vertexCompatAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		xaiAPIKeyCount,
		openAICompatCount,
	)
	return true
}

func (w *Watcher) addOrUpdateClient(path string) {
	unlockFile := authfile.LockFileMutation(path)
	defer unlockFile()
	readAt := time.Now()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		log.Errorf("failed to read auth file %s: %v", filepath.Base(path), errRead)
		return
	}
	if len(data) == 0 {
		log.Debugf("ignoring empty auth file: %s", filepath.Base(path))
		return
	}

	sum := sha256.Sum256(data)
	curHash := hex.EncodeToString(sum[:])
	normalized := w.normalizeAuthPath(path)

	// Parse new auth content for diff comparison
	var newAuth coreauth.Auth
	if errParse := json.Unmarshal(data, &newAuth); errParse != nil {
		log.Errorf("failed to parse auth file %s: %v", filepath.Base(path), errParse)
		return
	}
	persistedDigest, errDigest := coreauth.DigestPersistedJSON(data)
	if errDigest != nil {
		log.Errorf("failed to capture auth version for %s: %v", filepath.Base(path), errDigest)
		return
	}

	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	parser := w.pluginAuthParser
	w.clientsMutex.RUnlock()
	if cfg == nil {
		log.Error("config is nil, cannot add or update client")
		return
	}
	synthesisContext := &synthesizer.SynthesisContext{
		Config:           cfg,
		AuthDir:          authDir,
		Now:              readAt,
		IDGenerator:      synthesizer.NewStableIDGenerator(),
		PluginAuthParser: parser,
	}
	generated, errSynthesize := synthesizer.SynthesizeAuthFile(synthesisContext, path, data)
	if errSynthesize != nil {
		log.Errorf("failed to synthesize auth file %s: %v", filepath.Base(path), errSynthesize)
		return
	}
	newByID := authSliceToMap(generated)

	w.authRescanMu.Lock()
	defer w.authRescanMu.Unlock()
	currentVersion, errCurrentVersion := coreauth.ReadPersistedVersion(path)
	if errCurrentVersion != nil || currentVersion.SHA256 != persistedDigest {
		log.Debugf("auth file changed again before publication, skipping stale update: %s", filepath.Base(path))
		return
	}

	w.clientsMutex.Lock()
	if w.config != cfg || w.authDir != authDir {
		w.clientsMutex.Unlock()
		log.Debugf("auth configuration changed before publication, skipping stale update: %s", filepath.Base(path))
		return
	}
	if w.fileAuthsByPath == nil {
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
	}
	if prev, ok := w.lastAuthHashes[normalized]; ok && prev == curHash {
		log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(path))
		w.clientsMutex.Unlock()
		return
	}

	cacheAuthContents := log.IsLevelEnabled(log.DebugLevel)
	var oldAuth *coreauth.Auth
	if cacheAuthContents && w.lastAuthContents != nil {
		oldAuth = w.lastAuthContents[normalized]
	}
	if cacheAuthContents {
		if changes := diff.BuildAuthChangeDetails(oldAuth, &newAuth); len(changes) > 0 {
			log.Debugf("auth field changes for %s:", filepath.Base(path))
			for _, c := range changes {
				log.Debugf("  %s", c)
			}
		}
	}

	w.lastAuthHashes[normalized] = curHash
	if w.lastAuthVersions == nil {
		w.lastAuthVersions = make(map[string]coreauth.PersistedVersion)
	}
	w.lastAuthVersions[normalized] = currentVersion
	if cacheAuthContents {
		if w.lastAuthContents == nil {
			w.lastAuthContents = make(map[string]*coreauth.Auth)
		}
		w.lastAuthContents[normalized] = &newAuth
	}

	oldByID := make(map[string]*coreauth.Auth, len(w.fileAuthsByPath[normalized]))
	maps.Copy(oldByID, w.fileAuthsByPath[normalized])
	if len(newByID) > 0 {
		w.fileAuthsByPath[normalized] = authIDSet(newByID)
	} else {
		delete(w.fileAuthsByPath, normalized)
	}
	updates := w.computePerPathUpdatesLocked(oldByID, newByID)
	queue := w.authQueue
	w.clientsMutex.Unlock()

	if queue == nil && w.reloadCallback != nil {
		log.Debugf("triggering server update callback after add/update")
		w.triggerServerUpdate(cfg)
	}
	w.persistAuthVersionAsync(fmt.Sprintf("Sync auth %s", filepath.Base(path)), path, currentVersion)
	w.dispatchAuthUpdates(updates)
	w.notifyUsageRefresh()
}

func (w *Watcher) removeClient(path string) {
	unlockFile := authfile.LockFileMutation(path)
	defer unlockFile()
	if _, errStat := os.Stat(path); errStat == nil {
		log.Debugf("auth file exists after remove event, skipping stale removal: %s", filepath.Base(path))
		return
	}
	w.authRescanMu.Lock()
	defer w.authRescanMu.Unlock()

	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.Lock()
	cfg := w.config
	oldByID := make(map[string]*coreauth.Auth, len(w.fileAuthsByPath[normalized]))
	maps.Copy(oldByID, w.fileAuthsByPath[normalized])
	version, hasVersion := w.lastAuthVersions[normalized]
	delete(w.lastAuthHashes, normalized)
	delete(w.lastAuthVersions, normalized)
	delete(w.lastAuthContents, normalized)
	delete(w.fileAuthsByPath, normalized)

	updates := w.computePerPathUpdatesLocked(oldByID, map[string]*coreauth.Auth{})
	queue := w.authQueue
	w.clientsMutex.Unlock()

	if queue == nil && w.reloadCallback != nil {
		log.Debugf("triggering server update callback after removal")
		w.triggerServerUpdate(cfg)
	}
	w.persistAuthDeletionAsync(fmt.Sprintf("Remove auth %s", filepath.Base(path)), path, version, hasVersion)
	w.dispatchAuthUpdates(updates)
	w.notifyUsageRefresh()
}

func (w *Watcher) computePerPathUpdatesLocked(oldByID, newByID map[string]*coreauth.Auth) []AuthUpdate {
	if w.currentAuths == nil {
		w.currentAuths = make(map[string]*coreauth.Auth)
	}
	updates := make([]AuthUpdate, 0, len(oldByID)+len(newByID))
	for id, newAuth := range newByID {
		existing, ok := w.currentAuths[id]
		if !ok {
			w.currentAuths[id] = newAuth.Clone()
			updates = append(
				updates,
				AuthUpdate{
					Action:            AuthUpdateActionAdd,
					ID:                id,
					Auth:              newAuth.Clone(),
					AllowAuthRecreate: allowAuthRecreateForUpdate(newAuth),
				},
			)
			continue
		}
		if !authEqual(existing, newAuth) {
			w.currentAuths[id] = newAuth.Clone()
			updates = append(
				updates,
				AuthUpdate{
					Action:            AuthUpdateActionModify,
					ID:                id,
					Auth:              newAuth.Clone(),
					AllowAuthRecreate: allowAuthRecreateForUpdate(newAuth),
				},
			)
		}
	}
	for id := range oldByID {
		if _, stillExists := newByID[id]; stillExists {
			continue
		}
		delete(w.currentAuths, id)
		updates = append(updates, AuthUpdate{Action: AuthUpdateActionDelete, ID: id})
	}
	return updates
}

func authSliceToMap(auths []*coreauth.Auth) map[string]*coreauth.Auth {
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, a := range auths {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		byID[a.ID] = a
	}
	return byID
}

func authIDSet(auths map[string]*coreauth.Auth) map[string]*coreauth.Auth {
	set := make(map[string]*coreauth.Auth, len(auths))
	for id := range auths {
		set[id] = nil
	}
	return set
}

func (w *Watcher) loadFileClients(cfg *config.Config) int {
	authFileCount := 0
	successfulAuthCount := 0

	authDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errResolveAuthDir != nil {
		log.Errorf("failed to resolve auth directory: %v", errResolveAuthDir)
		return 0
	}
	if authDir == "" {
		return 0
	}

	errWalk := filepath.Walk(
		authDir, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				log.Debugf("error accessing path %s: %v", path, err)
				return err
			}
			if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".json") {
				authFileCount++
				log.Debugf("processing auth file %d: %s", authFileCount, filepath.Base(path))
				if data, errCreate := os.ReadFile(path); errCreate == nil && len(data) > 0 {
					successfulAuthCount++
				}
			}
			return nil
		},
	)

	if errWalk != nil {
		log.Errorf("error walking auth directory: %v", errWalk)
	}
	log.Debugf("auth directory scan complete - found %d .json files, %d readable", authFileCount, successfulAuthCount)
	return authFileCount
}

func BuildAPIKeyClients(cfg *config.Config) (int, int, int, int, int, int) {
	geminiAPIKeyCount := 0
	vertexCompatAPIKeyCount := 0
	claudeAPIKeyCount := 0
	codexAPIKeyCount := 0
	xaiAPIKeyCount := 0
	openAICompatCount := 0

	if len(cfg.GeminiKey) > 0 {
		geminiAPIKeyCount += len(cfg.GeminiKey)
	}
	if len(cfg.InteractionsKey) > 0 {
		geminiAPIKeyCount += len(cfg.InteractionsKey)
	}
	if len(cfg.VertexCompatAPIKey) > 0 {
		vertexCompatAPIKeyCount += len(cfg.VertexCompatAPIKey)
	}
	if len(cfg.ClaudeKey) > 0 {
		claudeAPIKeyCount += len(cfg.ClaudeKey)
	}
	if len(cfg.CodexKey) > 0 {
		codexAPIKeyCount += len(cfg.CodexKey)
	}
	if len(cfg.XAIKey) > 0 {
		xaiAPIKeyCount += len(cfg.XAIKey)
	}
	if len(cfg.OpenAICompatibility) > 0 {
		for _, compatConfig := range cfg.OpenAICompatibility {
			if compatConfig.Disabled {
				continue
			}
			openAICompatCount += len(compatConfig.APIKeyEntries)
		}
	}
	return geminiAPIKeyCount, vertexCompatAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, xaiAPIKeyCount, openAICompatCount
}

func (w *Watcher) persistConfigAsync() {
	if w == nil || w.storePersister == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.storePersister.PersistConfig(ctx); err != nil {
			log.Errorf("failed to persist config change: %v", err)
		}
	}()
}

func (w *Watcher) persistAuthVersionAsync(
	message,
	path string,
	version coreauth.PersistedVersion,
) {
	if w == nil || w.storePersister == nil || strings.TrimSpace(path) == "" {
		return
	}
	go func() {
		unlockFile := authfile.LockFileMutation(path)
		defer unlockFile()
		currentVersion, errVersion := coreauth.ReadPersistedVersion(path)
		if errVersion != nil || currentVersion.SHA256 != version.SHA256 {
			log.Debugf("skipping stale auth persistence: %s", filepath.Base(path))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if errPersist := w.storePersister.PersistAuthFiles(ctx, message, path); errPersist != nil {
			log.Errorf("failed to persist auth changes: %v", errPersist)
		}
	}()
}

func (w *Watcher) persistAuthAsync(message string, paths ...string) {
	if w == nil || w.storePersister == nil {
		return
	}
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.storePersister.PersistAuthFiles(ctx, message, filtered...); err != nil {
			log.Errorf("failed to persist auth changes: %v", err)
		}
	}()
}

func (w *Watcher) persistAuthDeletionAsync(
	message string,
	path string,
	version coreauth.PersistedVersion,
	hasVersion bool,
) {
	if w == nil || w.storePersister == nil || strings.TrimSpace(path) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if versionedStore, ok := w.storePersister.(coreauth.VersionedStore); ok {
			if !hasVersion {
				log.Errorf("cannot persist auth deletion without captured version: %s", filepath.Base(path))
				return
			}
			deleted, errDelete := versionedStore.DeleteIfVersion(ctx, path, version)
			if errDelete != nil {
				log.Errorf("failed to persist versioned auth deletion: %v", errDelete)
				return
			}
			if !deleted {
				log.Warnf("skipped stale auth deletion: %s", filepath.Base(path))
			}
			return
		}
		if errPersist := w.storePersister.PersistAuthFiles(ctx, message, path); errPersist != nil {
			log.Errorf("failed to persist auth changes: %v", errPersist)
		}
	}()
}

func (w *Watcher) stopServerUpdateTimer() {
	w.serverUpdateMu.Lock()
	defer w.serverUpdateMu.Unlock()
	if w.serverUpdateTimer != nil {
		w.serverUpdateTimer.Stop()
		w.serverUpdateTimer = nil
	}
	w.serverUpdatePend = false
}

func (w *Watcher) triggerServerUpdate(cfg *config.Config) {
	if w == nil || w.reloadCallback == nil || cfg == nil {
		return
	}

	now := time.Now()

	w.serverUpdateMu.Lock()
	if w.serverUpdateLast.IsZero() || now.Sub(w.serverUpdateLast) >= serverUpdateDebounce {
		w.serverUpdateLast = now
		if w.serverUpdateTimer != nil {
			w.serverUpdateTimer.Stop()
			w.serverUpdateTimer = nil
		}
		w.serverUpdatePend = false
		w.serverUpdateMu.Unlock()
		w.reloadCallback(cfg)
		return
	}

	if w.serverUpdatePend {
		w.serverUpdateMu.Unlock()
		return
	}

	delay := max(serverUpdateDebounce-now.Sub(w.serverUpdateLast), 10*time.Millisecond)
	w.serverUpdatePend = true
	if w.serverUpdateTimer != nil {
		w.serverUpdateTimer.Stop()
		w.serverUpdateTimer = nil
	}
	var timer *time.Timer
	timer = time.AfterFunc(
		delay, func() {
			w.clientsMutex.RLock()
			latestCfg := w.config
			w.clientsMutex.RUnlock()

			w.serverUpdateMu.Lock()
			if w.serverUpdateTimer != timer || !w.serverUpdatePend {
				w.serverUpdateMu.Unlock()
				return
			}
			w.serverUpdateTimer = nil
			w.serverUpdatePend = false
			if latestCfg == nil || w.reloadCallback == nil {
				w.serverUpdateMu.Unlock()
				return
			}

			w.serverUpdateLast = time.Now()
			w.serverUpdateMu.Unlock()
			w.reloadCallback(latestCfg)
		},
	)
	w.serverUpdateTimer = timer
	w.serverUpdateMu.Unlock()
}
