// Package watcher implements auth update dispatching and queue management.
// It batches, deduplicates, and delivers auth updates to registered consumers.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/geminicli"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var snapshotCoreAuthsFunc = snapshotCoreAuths

func (w *Watcher) setAuthUpdateQueue(queue chan<- AuthUpdate) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.authQueue = queue
	if w.dispatchCond == nil {
		w.dispatchCond = sync.NewCond(&w.dispatchMu)
	}
	if w.dispatchCancel != nil {
		w.dispatchCancel()
		if w.dispatchCond != nil {
			w.dispatchMu.Lock()
			w.dispatchCond.Broadcast()
			w.dispatchMu.Unlock()
		}
		w.dispatchCancel = nil
	}
	if queue != nil {
		ctx, cancel := context.WithCancel(context.Background())
		w.dispatchCancel = cancel
		go w.dispatchLoop(ctx)
	}
}

func (w *Watcher) dispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	if w == nil {
		return false
	}
	w.clientsMutex.Lock()
	if w.runtimeAuths == nil {
		w.runtimeAuths = make(map[string]*coreauth.Auth)
	}
	switch update.Action {
	case AuthUpdateActionAdd, AuthUpdateActionModify:
		if update.Auth != nil && update.Auth.ID != "" {
			clone := update.Auth.Clone()
			w.runtimeAuths[clone.ID] = clone
			if w.currentAuths == nil {
				w.currentAuths = make(map[string]*coreauth.Auth)
			}
			w.currentAuths[clone.ID] = clone.Clone()
		}
	case AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id != "" {
			delete(w.runtimeAuths, id)
			if w.currentAuths != nil {
				delete(w.currentAuths, id)
			}
		}
	}
	updates := []AuthUpdate{update}
	w.stampAuthUpdatesLocked(updates)
	w.clientsMutex.Unlock()
	if w.getAuthQueue() == nil {
		return false
	}
	w.dispatchAuthUpdates(updates)
	return true
}

func (w *Watcher) dispatchPersistedAuthUpdateWithRevision(update *AuthUpdate) (bool, uint64) {
	if w == nil || update == nil || update.Auth == nil || strings.TrimSpace(update.Auth.ID) == "" {
		return false, 0
	}
	path := ""
	if update.Auth.Attributes != nil {
		path = update.Auth.Attributes[coreauth.AttributePath]
		if path == "" {
			path = update.Auth.Attributes[coreauth.AttributeSource]
		}
	}
	normalized := w.normalizeAuthPath(path)
	if normalized == "" {
		return false, 0
	}

	clone := update.Auth.Clone()
	w.clientsMutex.Lock()
	if w.fileAuthsByPath == nil {
		w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
	}
	pathAuths := w.fileAuthsByPath[normalized]
	if pathAuths == nil {
		pathAuths = make(map[string]*coreauth.Auth)
		w.fileAuthsByPath[normalized] = pathAuths
	}
	pathAuths[clone.ID] = nil
	if w.currentAuths == nil {
		w.currentAuths = make(map[string]*coreauth.Auth)
	}
	w.currentAuths[clone.ID] = clone
	if update.ID == "" {
		update.ID = clone.ID
	}
	updateCopy := *update
	updateCopy.Auth = clone.Clone()
	updates := []AuthUpdate{updateCopy}
	w.stampAuthUpdatesLocked(updates)
	revision := updates[0].revision
	update.revision = revision
	w.clientsMutex.Unlock()

	if w.getAuthQueue() == nil {
		return false, revision
	}
	w.dispatchAuthUpdates(updates)
	return true, revision
}

func (w *Watcher) refreshAuthState(force bool) {
	w.clientsMutex.Lock()
	w.activeAuthScans++
	cfg := w.config
	authDir := w.authDir
	parser := w.pluginAuthParser
	previousRevisions := maps.Clone(w.authRevisions)
	previousFiles := maps.Clone(w.fileObservations)
	w.clientsMutex.Unlock()

	auths, errSnapshot := snapshotCoreAuthsFunc(cfg, authDir, parser)
	w.clientsMutex.Lock()
	w.activeAuthScans--
	if errSnapshot != nil && w.currentAuths != nil {
		w.clientsMutex.Unlock()
		log.Errorf("failed to refresh auth snapshot; retaining current auth state: %v", errSnapshot)
		return
	}
	changedDuringScan := func(auth *coreauth.Auth) bool {
		if auth == nil {
			return false
		}
		path := auth.Attributes[coreauth.AttributePath]
		if path == "" {
			path = auth.Attributes[coreauth.AttributeSource]
		}
		normalized := w.normalizeAuthPath(path)
		return previousRevisions[auth.ID] != w.authRevisions[auth.ID] ||
			previousFiles[normalized] != w.fileObservations[normalized]
	}
	for index, auth := range auths {
		if changedDuringScan(auth) {
			auths[index] = nil
		}
	}
	for _, auth := range w.currentAuths {
		if changedDuringScan(auth) {
			auths = append(auths, auth.Clone())
		}
	}
	if len(w.runtimeAuths) > 0 {
		for _, a := range w.runtimeAuths {
			if a != nil {
				auths = append(auths, a.Clone())
			}
		}
	}
	updates := w.prepareAuthUpdatesLocked(auths, force)
	w.clientsMutex.Unlock()
	w.dispatchAuthUpdates(updates)
}

func (w *Watcher) prepareAuthUpdatesLocked(auths []*coreauth.Auth, force bool) []AuthUpdate {
	newState := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		newState[auth.ID] = auth.Clone()
	}
	updates := make([]AuthUpdate, 0, len(newState)+len(w.currentAuths))
	for id, auth := range newState {
		if existing, ok := w.currentAuths[id]; !ok {
			updates = append(
				updates,
				AuthUpdate{
					Action:            AuthUpdateActionAdd,
					ID:                id,
					Auth:              auth.Clone(),
					AllowAuthRecreate: allowAuthRecreateForUpdate(auth),
				},
			)
		} else if force || !authEqual(existing, auth) {
			updates = append(
				updates,
				AuthUpdate{
					Action:            AuthUpdateActionModify,
					ID:                id,
					Auth:              auth.Clone(),
					AllowAuthRecreate: allowAuthRecreateForUpdate(auth),
				},
			)
		}
	}
	for id := range w.currentAuths {
		if _, ok := newState[id]; !ok {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionDelete, ID: id})
		}
	}
	w.currentAuths = newState
	w.stampAuthUpdatesLocked(updates)
	if w.authQueue == nil {
		return nil
	}
	return updates
}

// stampAuthUpdatesLocked records revisions even when no consumer is attached.
func (w *Watcher) stampAuthUpdatesLocked(updates []AuthUpdate) {
	if len(updates) == 0 {
		return
	}
	if w.authRevisions == nil {
		w.authRevisions = make(map[string]uint64)
	}
	for index := range updates {
		update := &updates[index]
		if update.ID == "" && update.Auth != nil {
			update.ID = update.Auth.ID
		}
		if update.ID == "" {
			continue
		}
		w.authRevisions[update.ID]++
		update.revision = w.authRevisions[update.ID]
	}
}

func (w *Watcher) dispatchAuthUpdates(updates []AuthUpdate) {
	if len(updates) == 0 {
		return
	}
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	if w.authQueue == nil {
		return
	}
	baseTS := time.Now().UnixNano()
	w.dispatchMu.Lock()
	if w.pendingUpdates == nil {
		w.pendingUpdates = make(map[string]AuthUpdate)
	}
	for idx, update := range updates {
		if update.revision != 0 && update.revision < w.authRevisions[update.ID] {
			continue
		}
		key := w.authUpdateKey(update, baseTS+int64(idx))
		if _, exists := w.pendingUpdates[key]; !exists {
			w.pendingOrder = append(w.pendingOrder, key)
		}
		w.pendingUpdates[key] = update
	}
	if w.dispatchCond != nil {
		w.dispatchCond.Signal()
	}
	w.dispatchMu.Unlock()
}

func (w *Watcher) authUpdateKey(update AuthUpdate, ts int64) string {
	if update.ID != "" {
		return update.ID
	}
	return fmt.Sprintf("%s:%d", update.Action, ts)
}

func (w *Watcher) dispatchLoop(ctx context.Context) {
	for {
		batch, ok := w.nextPendingBatch(ctx)
		if !ok {
			return
		}
		queue := w.getAuthQueue()
		if queue == nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, update := range batch {
			select {
			case queue <- update:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *Watcher) nextPendingBatch(ctx context.Context) ([]AuthUpdate, bool) {
	w.dispatchMu.Lock()
	defer w.dispatchMu.Unlock()
	for len(w.pendingOrder) == 0 {
		if ctx.Err() != nil {
			return nil, false
		}
		w.dispatchCond.Wait()
		if ctx.Err() != nil {
			return nil, false
		}
	}
	batch := make([]AuthUpdate, 0, len(w.pendingOrder))
	for _, key := range w.pendingOrder {
		batch = append(batch, w.pendingUpdates[key])
		delete(w.pendingUpdates, key)
	}
	w.pendingOrder = w.pendingOrder[:0]
	return batch, true
}

func (w *Watcher) getAuthQueue() chan<- AuthUpdate {
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	return w.authQueue
}

func (w *Watcher) stopDispatch() {
	if w.dispatchCancel != nil {
		w.dispatchCancel()
		w.dispatchCancel = nil
	}
	w.dispatchMu.Lock()
	w.pendingOrder = nil
	w.pendingUpdates = nil
	if w.dispatchCond != nil {
		w.dispatchCond.Broadcast()
	}
	w.dispatchMu.Unlock()
	w.clientsMutex.Lock()
	w.authQueue = nil
	w.clientsMutex.Unlock()
}

func authEqual(a, b *coreauth.Auth) bool {
	return reflect.DeepEqual(normalizeAuth(a), normalizeAuth(b)) && geminiRuntimeEqual(a, b)
}

func geminiRuntimeEqual(a, b *coreauth.Auth) bool {
	sharedA := geminicli.ResolveSharedCredential(runtimeValue(a))
	sharedB := geminicli.ResolveSharedCredential(runtimeValue(b))
	if sharedA == nil || sharedB == nil {
		return sharedA == sharedB
	}
	if sharedA.PrimaryID() != sharedB.PrimaryID() || sharedA.Email() != sharedB.Email() ||
		!reflect.DeepEqual(sharedA.ProjectIDs(), sharedB.ProjectIDs()) ||
		!reflect.DeepEqual(sharedA.MetadataSnapshot(), sharedB.MetadataSnapshot()) {
		return false
	}
	virtualA, virtualAOK := runtimeValue(a).(*geminicli.VirtualCredential)
	virtualB, virtualBOK := runtimeValue(b).(*geminicli.VirtualCredential)
	return virtualAOK == virtualBOK && (!virtualAOK || virtualA.ProjectID == virtualB.ProjectID)
}

func runtimeValue(auth *coreauth.Auth) any {
	if auth == nil {
		return nil
	}
	return auth.Runtime
}

func allowAuthRecreateForUpdate(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func normalizeAuth(a *coreauth.Auth) *coreauth.Auth {
	if a == nil {
		return nil
	}
	clone := a.Clone()
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	clone.LastRefreshedAt = time.Time{}
	clone.NextRefreshAfter = time.Time{}
	clone.Runtime = nil
	clone.Quota.NextRecoverAt = time.Time{}
	return clone
}

func snapshotCoreAuths(
	cfg *config.Config,
	authDir string,
	parser synthesizer.PluginAuthParser,
) ([]*coreauth.Auth, error) {
	ctx := &synthesizer.SynthesisContext{
		Config:           cfg,
		AuthDir:          authDir,
		Now:              time.Now(),
		IDGenerator:      synthesizer.NewStableIDGenerator(),
		PluginAuthParser: parser,
	}

	var (
		out         []*coreauth.Auth
		errSnapshot error
	)

	configSynth := synthesizer.NewConfigSynthesizer()
	configAuths, errConfig := configSynth.Synthesize(ctx)
	out = append(out, configAuths...)
	if errConfig != nil {
		errSnapshot = errors.Join(errSnapshot, fmt.Errorf("synthesize config auths: %w", errConfig))
	}

	fileSynth := synthesizer.NewFileSynthesizer()
	fileAuths, errFiles := fileSynth.Synthesize(ctx)
	out = append(out, fileAuths...)
	if errFiles != nil {
		errSnapshot = errors.Join(errSnapshot, fmt.Errorf("synthesize file auths: %w", errFiles))
	}

	return out, errSnapshot
}
