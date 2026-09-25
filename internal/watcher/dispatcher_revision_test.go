package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/fsnotify/fsnotify"
)

func pauseRevisionTestSnapshot(
	t *testing.T,
	w *Watcher,
	auths []*coreauth.Auth,
) func() {
	t.Helper()
	original := snapshotCoreAuthsFunc
	scanned := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan struct{})
	snapshotCoreAuthsFunc = func(
		*config.Config,
		string,
		synthesizer.PluginAuthParser,
	) ([]*coreauth.Auth, error) {
		close(scanned)
		<-resume
		return auths, nil
	}
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			close(resume)
			<-finished
			snapshotCoreAuthsFunc = original
		})
	}
	t.Cleanup(finish)
	go func() {
		defer close(finished)
		w.refreshAuthState(false)
	}()
	<-scanned
	return finish
}

func newRevisionTestWatcher(auth *coreauth.Auth) *Watcher {
	w := &Watcher{
		config:         &config.Config{},
		authQueue:      make(chan AuthUpdate, 1),
		currentAuths:   make(map[string]*coreauth.Auth),
		lastAuthHashes: make(map[string]string),
	}
	if auth != nil {
		w.currentAuths[auth.ID] = auth.Clone()
	}
	w.dispatchCond = sync.NewCond(&w.dispatchMu)
	return w
}

func TestRefreshAuthStatePreservesConcurrentAuthRevision(t *testing.T) {
	for _, action := range []string{"modify", "delete"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "account.json")
			oldAuth := &coreauth.Auth{
				ID:         "account",
				Provider:   "test",
				ProxyURL:   "http://old.example",
				Attributes: map[string]string{coreauth.AttributePath: path},
			}
			w := newRevisionTestWatcher(oldAuth)
			finish := pauseRevisionTestSnapshot(t, w, []*coreauth.Auth{oldAuth.Clone()})

			w.observeAuthFile(path)
			w.clientsMutex.Lock()
			newByID := make(map[string]*coreauth.Auth)
			if action == "modify" {
				updated := oldAuth.Clone()
				updated.ProxyURL = "http://latest.example"
				newByID[updated.ID] = updated
			}
			updates := w.computePerPathUpdatesLocked(
				map[string]*coreauth.Auth{oldAuth.ID: nil},
				newByID,
			)
			w.clientsMutex.Unlock()
			w.dispatchAuthUpdates(updates)
			finish()

			current := w.currentAuths[oldAuth.ID]
			pending := w.pendingUpdates[oldAuth.ID]
			if action == "delete" {
				if current != nil || pending.Action != AuthUpdateActionDelete {
					t.Fatalf("stale scan restored deleted auth: current=%#v update=%#v", current, pending)
				}
				return
			}
			if current == nil || current.ProxyURL != "http://latest.example" {
				t.Fatalf("current proxy = %#v, want latest", current)
			}
			if pending.Auth == nil || pending.Auth.ProxyURL != "http://latest.example" {
				t.Fatalf("pending proxy = %#v, want latest", pending.Auth)
			}
		})
	}
}

func TestRefreshAuthStatePreservesObservedCurrentAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	current := &coreauth.Auth{
		ID:         "account",
		Provider:   "test",
		ProxyURL:   "http://latest.example",
		Attributes: map[string]string{coreauth.AttributePath: path},
	}
	stale := current.Clone()
	stale.ProxyURL = "http://stale.example"
	w := newRevisionTestWatcher(current)
	finish := pauseRevisionTestSnapshot(t, w, []*coreauth.Auth{stale})
	w.observeAuthFile(path)
	finish()

	got := w.currentAuths[current.ID]
	if got == nil || got.ProxyURL != current.ProxyURL {
		t.Fatalf("current auth = %#v, want observed auth %#v", got, current)
	}
}

func TestRefreshAuthStateDoesNotRegisterObservedDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	stale := &coreauth.Auth{
		ID:         "account",
		Provider:   "test",
		Attributes: map[string]string{coreauth.AttributePath: path},
	}
	w := newRevisionTestWatcher(nil)
	finish := pauseRevisionTestSnapshot(t, w, []*coreauth.Auth{stale})
	w.observeAuthFile(path)
	finish()

	if current := w.currentAuths[stale.ID]; current != nil {
		t.Fatalf("observed deletion was registered from stale scan: %#v", current)
	}
	if len(w.pendingOrder) != 0 {
		t.Fatalf("stale scan queued %d updates after observed deletion", len(w.pendingOrder))
	}
}

func TestRefreshAuthStateDoesNotSuppressHashCachedEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	w := &Watcher{
		authDir:          dir,
		config:           &config.Config{AuthDir: dir},
		authQueue:        make(chan AuthUpdate, 1),
		lastAuthHashes:   make(map[string]string),
		lastAuthVersions: make(map[string]coreauth.PersistedVersion),
		fileAuthsByPath:  make(map[string]map[string]*coreauth.Auth),
	}
	w.dispatchCond = sync.NewCond(&w.dispatchMu)
	w.addOrUpdateClient(path)
	initial := w.currentAuths["account.json"].Clone()
	w.pendingUpdates = nil
	w.pendingOrder = nil

	latestData := []byte(`{"type":"codex","proxy_url":"http://latest.example"}`)
	if errWrite := os.WriteFile(path, latestData, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	latestHash := sha256.Sum256(latestData)
	w.lastAuthHashes[w.normalizeAuthPath(path)] = hex.EncodeToString(latestHash[:])

	original := snapshotCoreAuthsFunc
	t.Cleanup(func() { snapshotCoreAuthsFunc = original })
	snapshotCoreAuthsFunc = func(
		*config.Config,
		string,
		synthesizer.PluginAuthParser,
	) ([]*coreauth.Auth, error) {
		w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Write})
		return []*coreauth.Auth{initial}, nil
	}
	w.refreshAuthState(false)

	current := w.currentAuths["account.json"]
	pending := w.pendingUpdates["account.json"]
	if current == nil || current.ProxyURL != "http://latest.example" {
		t.Fatalf("current auth = %#v, want hash-cached event content", current)
	}
	if pending.Auth == nil || pending.Auth.ProxyURL != "http://latest.example" {
		t.Fatalf("pending auth = %#v, want hash-cached event content", pending.Auth)
	}
}

func TestDispatchAuthUpdatesRejectsDelayedRevision(t *testing.T) {
	oldAuth := &coreauth.Auth{ID: "account", Provider: "test", ProxyURL: "http://old.example"}
	w := newRevisionTestWatcher(oldAuth)
	w.clientsMutex.Lock()
	delayed := w.prepareAuthUpdatesLocked([]*coreauth.Auth{oldAuth.Clone()}, true)
	updated := oldAuth.Clone()
	updated.ProxyURL = "http://latest.example"
	latest := w.computePerPathUpdatesLocked(
		map[string]*coreauth.Auth{oldAuth.ID: nil},
		map[string]*coreauth.Auth{updated.ID: updated},
	)
	w.clientsMutex.Unlock()

	w.dispatchAuthUpdates(latest)
	w.dispatchAuthUpdates(delayed)
	pending := w.pendingUpdates[oldAuth.ID]
	if pending.Revision() != 2 || pending.Auth == nil || pending.Auth.ProxyURL != updated.ProxyURL {
		t.Fatalf("pending update = %#v revision=%d, want latest revision 2", pending.Auth, pending.Revision())
	}

	if _, ok := w.nextPendingBatch(context.Background()); !ok {
		t.Fatal("expected latest pending batch")
	}
	w.dispatchAuthUpdates(delayed)
	if len(w.pendingOrder) != 0 {
		t.Fatal("delayed revision was enqueued after the latest update was dispatched")
	}
}

func TestAuthRevisionsAdvanceWithoutConsumer(t *testing.T) {
	auth := &coreauth.Auth{ID: "account", Provider: "test"}
	w := &Watcher{currentAuths: map[string]*coreauth.Auth{auth.ID: auth}}
	w.clientsMutex.Lock()
	updates := w.prepareAuthUpdatesLocked(nil, false)
	revision := w.authRevisions[auth.ID]
	w.clientsMutex.Unlock()
	if updates != nil {
		t.Fatalf("updates = %#v, want nil without consumer", updates)
	}
	if revision != 1 {
		t.Fatalf("deletion revision = %d, want 1", revision)
	}
}

func TestPersistedAuthUpdateReceivesRevisionAndSupersedesFileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	oldAuth := &coreauth.Auth{
		ID:         "account",
		Provider:   "test",
		ProxyURL:   "http://old.example",
		Attributes: map[string]string{coreauth.AttributePath: path},
	}
	w := newRevisionTestWatcher(oldAuth)
	latest := oldAuth.Clone()
	latest.ProxyURL = "http://latest.example"
	update := AuthUpdate{Action: AuthUpdateActionModify, Auth: latest}

	queued, revision := w.dispatchPersistedAuthUpdateWithRevision(&update)
	if !queued || revision != 1 || update.Revision() != 1 {
		t.Fatalf(
			"persisted update queued=%v revision=%d update revision=%d, want true and 1",
			queued,
			revision,
			update.Revision(),
		)
	}
	current := w.currentAuths[oldAuth.ID]
	if current == nil || current.ProxyURL != latest.ProxyURL {
		t.Fatalf("current auth = %#v, want persisted auth %#v", current, latest)
	}
	normalized := w.normalizeAuthPath(path)
	if pathAuths := w.fileAuthsByPath[normalized]; pathAuths == nil {
		t.Fatal("persisted auth path was not registered")
	} else if _, ok := pathAuths[oldAuth.ID]; !ok {
		t.Fatal("persisted auth identity was not registered for the file")
	}
	pending := w.pendingUpdates[oldAuth.ID]
	if pending.Revision() != 1 || pending.Auth == nil || pending.Auth.ProxyURL != latest.ProxyURL {
		t.Fatalf("pending persisted update = %#v revision=%d", pending.Auth, pending.Revision())
	}
}

func TestRuntimeAuthUpdatesReceiveMonotonicRevisions(t *testing.T) {
	w := newRevisionTestWatcher(nil)
	first := &coreauth.Auth{ID: "runtime", Provider: "test"}
	if !w.dispatchRuntimeAuthUpdate(AuthUpdate{Action: AuthUpdateActionAdd, Auth: first}) {
		t.Fatal("runtime add was not queued")
	}
	second := first.Clone()
	second.ProxyURL = "http://latest.example"
	if !w.dispatchRuntimeAuthUpdate(AuthUpdate{Action: AuthUpdateActionModify, Auth: second}) {
		t.Fatal("runtime modify was not queued")
	}
	pending := w.pendingUpdates[first.ID]
	if pending.Revision() != 2 || pending.Auth == nil || pending.Auth.ProxyURL != second.ProxyURL {
		t.Fatalf("runtime update = %#v revision=%d, want latest revision 2", pending.Auth, pending.Revision())
	}
}
