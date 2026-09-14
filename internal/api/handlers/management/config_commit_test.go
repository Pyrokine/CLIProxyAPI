package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func newConfigCommitTestContext(method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	return ginCtx, recorder
}

func TestApplyExternalConfigUpdateDoesNotHoldCommitLockWhileWaitingForRuntime(t *testing.T) {
	handler := &Handler{}
	handler.runtimeCommitMu.Lock()
	handler.commitMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- handler.ApplyExternalConfigUpdate(func() error { return nil })
	}()
	handler.commitMu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if !handler.commitMu.TryLock() {
		handler.runtimeCommitMu.Unlock()
		t.Fatal("external config update held commit lock while waiting for runtime")
	}
	handler.commitMu.Unlock()
	handler.runtimeCommitMu.Unlock()
	select {
	case errApply := <-done:
		if errApply != nil {
			t.Fatalf("ApplyExternalConfigUpdate() error = %v", errApply)
		}
	case <-time.After(time.Second):
		t.Fatal("external config update did not complete")
	}
}

func TestApplyExternalConfigCommitReleasesCommitLockBeforeRuntime(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("debug: true\n")
	if errWrite := os.WriteFile(configPath, data, 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	digest := sha256.Sum256(data)
	expectedHash := hex.EncodeToString(digest[:])
	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	t.Cleanup(func() { _ = handler.Close() })

	var got ConfigCommit
	errApply := handler.ApplyExternalConfigCommit(
		&config.Config{Debug: true},
		expectedHash,
		func(commit ConfigCommit) error {
			got = commit
			if !handler.commitMu.TryLock() {
				t.Fatal("external runtime callback held the management commit lock")
			}
			handler.commitMu.Unlock()
			return nil
		},
	)
	if errApply != nil {
		t.Fatalf("ApplyExternalConfigCommit() error = %v", errApply)
	}
	if got.Generation != 1 || got.Config == nil || !got.Config.Debug || got.ConfigFileHash != expectedHash {
		t.Fatalf("external commit = %#v", got)
	}
}

func TestApplyExternalConfigCommitRejectsStaleSnapshot(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	oldData := []byte("debug: true\n")
	if errWrite := os.WriteFile(configPath, oldData, 0o600); errWrite != nil {
		t.Fatalf("write old config: %v", errWrite)
	}
	oldDigest := sha256.Sum256(oldData)
	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	t.Cleanup(func() { _ = handler.Close() })
	if errWrite := os.WriteFile(configPath, []byte("debug: false\n"), 0o600); errWrite != nil {
		t.Fatalf("write new config: %v", errWrite)
	}
	called := false
	errApply := handler.ApplyExternalConfigCommit(
		&config.Config{Debug: true},
		hex.EncodeToString(oldDigest[:]),
		func(ConfigCommit) error {
			called = true
			return nil
		},
	)
	if errApply == nil || called {
		t.Fatalf("ApplyExternalConfigCommit() error = %v, callback called = %v", errApply, called)
	}
	if state := handler.ConfigState(); state.Generation != 0 || state.Config == nil || state.Config.Debug {
		t.Fatalf("handler state changed after stale snapshot: %#v", state)
	}
}

func TestConfigCommitHashRemainsBoundToPersistedCandidate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("debug: false\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	t.Cleanup(func() { _ = handler.Close() })
	candidate := &config.Config{Debug: true}
	persistedHash, errPersist := handler.persistConfigCandidate(candidate)
	if errPersist != nil {
		t.Fatalf("persist config candidate: %v", errPersist)
	}
	if errWrite := os.WriteFile(configPath, []byte("debug: false\nproxy-url: changed\n"), 0o600); errWrite != nil {
		t.Fatalf("replace config externally: %v", errWrite)
	}
	handler.mu.Lock()
	commit, _ := handler.publishConfigLocked(candidate, persistedHash)
	handler.mu.Unlock()
	if commit.ConfigFileHash != persistedHash || commit.ConfigFileHash == handler.configFileHash() {
		t.Fatalf(
			"commit hash = %q, persisted hash = %q, current hash = %q",
			commit.ConfigFileHash,
			persistedHash,
			handler.configFileHash(),
		)
	}
}

func TestPutDebugCreatesMissingConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = handler.Close() }()

	ginCtx, recorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	handler.PutDebug(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	persisted, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load created config: %v", err)
	}
	if !persisted.Debug {
		t.Fatal("created config did not contain the update")
	}
	if _, err = os.Stat(config.LastGoodPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("unexpected last-good file for first config creation: %v", err)
	}
}

func TestConfigCommitCallbackRunsAfterHandlerUnlock(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	original := &config.Config{}
	var (
		handler *Handler
		got     ConfigCommit
	)
	handler = NewHandler(
		original,
		configPath,
		nil,
		withEnvSecret(""),
		WithConfigCommitted(
			func(_ context.Context, commit ConfigCommit) error {
				got = commit
				handler.SetConfig(commit.Config)
				return nil
			},
		),
	)
	defer func() { _ = handler.Close() }()

	ginCtx, recorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	done := make(chan struct{})
	go func() {
		handler.PutDebug(ginCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PutDebug deadlocked while the commit callback called SetConfig")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got.Generation != 1 || got.Config == nil || !got.Config.Debug ||
		got.ConfigFileHash == "" || got.ConfigFileHash != handler.configFileHash() {
		t.Fatalf("commit = %#v", got)
	}
	if original.Debug {
		t.Fatal("original config was modified in place")
	}
	if snapshot := handler.configSnapshot(); snapshot != got.Config {
		t.Fatalf("handler snapshot = %p, want committed config %p", snapshot, got.Config)
	}
}

func TestConfigCommitCallbackFailureKeepsCommittedGeneration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	callbackErr := errors.New("runtime apply failed")
	handler := NewHandler(
		&config.Config{},
		configPath,
		nil,
		withEnvSecret(""),
		WithConfigCommitted(func(context.Context, ConfigCommit) error { return callbackErr }),
	)
	defer func() { _ = handler.Close() }()

	ginCtx, recorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	handler.PutDebug(ginCtx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", recorder.Code, recorder.Body.String())
	}
	snapshot := handler.configSnapshot()
	if snapshot == nil || !snapshot.Debug || handler.configGeneration != 1 {
		t.Fatalf("snapshot = %#v, generation = %d", snapshot, handler.configGeneration)
	}
	persisted, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if !persisted.Debug {
		t.Fatal("committed config was rolled back after callback failure")
	}
}

func TestConfigCommitCallbackPanicReturnsSyncFailure(t *testing.T) {
	handler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		WithConfigCommitted(
			func(context.Context, ConfigCommit) error {
				panic("runtime panic")
			},
		),
	)
	defer func() { _ = handler.Close() }()

	ginCtx, recorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	handler.PutDebug(ginCtx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", recorder.Code, recorder.Body.String())
	}
	if snapshot := handler.configSnapshot(); snapshot == nil || !snapshot.Debug {
		t.Fatalf("committed snapshot = %#v", snapshot)
	}
}

func TestConfigCommitCallbacksAreOrdered(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	generations := make([]uint64, 0, 2)
	handler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		WithConfigCommitted(
			func(_ context.Context, commit ConfigCommit) error {
				generations = append(generations, commit.Generation)
				if commit.Generation == 1 {
					close(firstStarted)
					<-releaseFirst
				}
				return nil
			},
		),
	)
	defer func() { _ = handler.Close() }()

	firstCtx, firstRecorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	firstDone := make(chan struct{})
	go func() {
		handler.PutDebug(firstCtx)
		close(firstDone)
	}()
	<-firstStarted

	secondCtx, secondRecorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":false}`)
	secondDone := make(chan struct{})
	go func() {
		handler.PutDebug(secondCtx)
		close(secondDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for handler.ConfigState().Generation != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if generation := handler.ConfigState().Generation; generation != 2 {
		t.Fatalf("second config generation was not published while the first callback blocked: %d", generation)
	}

	select {
	case <-secondDone:
		t.Fatal("second commit completed before the first callback returned")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone

	if firstRecorder.Code != http.StatusOK || secondRecorder.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d", firstRecorder.Code, secondRecorder.Code)
	}
	if len(generations) != 2 || generations[0] != 1 || generations[1] != 2 {
		t.Fatalf("callback generations = %v", generations)
	}
	if snapshot := handler.configSnapshot(); snapshot == nil || snapshot.Debug {
		t.Fatalf("final snapshot = %#v", snapshot)
	}
}

func TestNotifyConfigCommittedSkipsSupersededGeneration(t *testing.T) {
	calls := 0
	handler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		WithConfigCommitted(
			func(context.Context, ConfigCommit) error {
				calls++
				return nil
			},
		),
	)
	defer func() { _ = handler.Close() }()
	handler.mu.Lock()
	first, callback := handler.publishConfigLocked(&config.Config{Debug: true}, "")
	handler.publishConfigLocked(&config.Config{Debug: false}, "")
	handler.mu.Unlock()

	if !handler.notifyConfigCommittedContext(nil, context.Background(), callback, first) {
		t.Fatal("superseded callback was reported as a failure")
	}
	if calls != 0 {
		t.Fatalf("superseded callback calls = %d, want 0", calls)
	}
}

func TestConfigCommitWarningsCollectConcurrentCleanupErrors(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", nil)
	warnings := beginConfigCommitWarnings(ginCtx)

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			ReportConfigCommitCleanupError(ginCtx.Request.Context(), errors.New("cleanup failed"))
		})
	}
	workers.Wait()
	if message := warnings.cleanupErrorMessage(); message == "" {
		t.Fatal("concurrent cleanup errors were not collected")
	}
}

func TestExternalConfigUpdateSerializesManagementMutation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: false\nproxy-url: https://old.example\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := NewHandler(
		&config.Config{ProxyURL: "https://old.example"},
		configPath,
		nil,
		withEnvSecret(""),
	)
	defer func() { _ = handler.Close() }()

	externalStarted := make(chan struct{})
	releaseExternal := make(chan struct{})
	externalDone := make(chan error, 1)
	go func() {
		externalDone <- handler.ApplyExternalConfigUpdate(
			func() error {
				close(externalStarted)
				<-releaseExternal
				handler.SetConfig(
					&config.Config{ProxyURL: "https://new.example"},
				)
				return nil
			},
		)
	}()
	<-externalStarted

	ginCtx, recorder := newConfigCommitTestContext(http.MethodPut, "/debug", `{"value":true}`)
	mutationDone := make(chan struct{})
	go func() {
		handler.PutDebug(ginCtx)
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
		t.Fatal("management mutation completed before the external update")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseExternal)
	if errExternal := <-externalDone; errExternal != nil {
		t.Fatalf("ApplyExternalConfigUpdate() error = %v", errExternal)
	}
	select {
	case <-mutationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("management mutation did not resume after the external update")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	snapshot := handler.configSnapshot()
	if snapshot == nil || !snapshot.Debug || snapshot.ProxyURL != "https://new.example" {
		t.Fatalf("final config = %#v", snapshot)
	}
	if handler.configGeneration != 2 {
		t.Fatalf("config generation = %d, want 2", handler.configGeneration)
	}
}

func TestPutConfigYAMLPublishesConfigCommit(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\ndebug: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var got ConfigCommit
	handler := NewHandler(
		&config.Config{Port: 8317},
		configPath,
		nil,
		withEnvSecret(""),
		WithConfigCommitted(
			func(_ context.Context, commit ConfigCommit) error {
				got = commit
				return nil
			},
		),
	)
	defer func() { _ = handler.Close() }()

	ginCtx, recorder := newConfigCommitTestContext(
		http.MethodPut,
		"/config.yaml",
		"port: 8317\ndebug: true\n",
	)
	ginCtx.Request.Header.Set("Content-Type", "application/yaml")
	handler.PutConfigYAML(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got.Generation != 1 || got.Config == nil || !got.Config.Debug {
		t.Fatalf("commit = %#v", got)
	}
	if snapshot := handler.configSnapshot(); snapshot != got.Config {
		t.Fatalf("handler snapshot = %p, want committed config %p", snapshot, got.Config)
	}
}
