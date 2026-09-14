package management

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

type updateGuardTestClock struct {
	current time.Time
}

type updateGuardRoundTripFunc func(*http.Request) (*http.Response, error)

func (f updateGuardRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (c *updateGuardTestClock) now() time.Time {
	return c.current
}

func (c *updateGuardTestClock) sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		c.current = c.current.Add(delay)
		return nil
	}
}

func openUpdateGuardTestStore(t *testing.T) *managementStateStore {
	t.Helper()
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func prepareArmedUpdateGuardSession(
	t *testing.T,
	store *managementStateStore,
	baseURL string,
	now time.Time,
	previousVersion string,
	targetVersion string,
) (updateGuardSession, string) {
	t.Helper()
	dir := t.TempDir()
	executablePath := filepath.Join(dir, "cli-proxy-api")
	if err := os.WriteFile(executablePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write current executable: %v", err)
	}
	attemptID := "0123456789abcdef0123456789abcdef"
	host := updateGuardHost{
		ExecutablePath: executablePath,
		BackupPath:     executablePath + ".bak",
		StateDBPath:    store.path,
		ServiceUnit:    defaultUpdateServiceUnit,
		HealthBaseURL:  baseURL,
	}
	session := newUpdateGuardSession(host, attemptID, targetVersion, false, now)
	session.PreviousVersion = previousVersion
	session.Phase = updatePhasePreparing
	session.CandidatePath = updateCandidatePath(executablePath, attemptID)
	if err := os.WriteFile(session.CandidatePath, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write candidate executable: %v", err)
	}
	if err := store.beginUpdateSession(context.Background(), session); err != nil {
		t.Fatalf("begin update session: %v", err)
	}
	armed, err := armUpdateGuardSession(context.Background(), store, session, now)
	if err != nil {
		t.Fatalf("arm update guard: %v", err)
	}
	token, err := store.loadUpdateHealthToken(context.Background())
	if err != nil || token == nil {
		t.Fatalf("load update health token: token=%#v err=%v", token, err)
	}
	return armed, token.Token
}

func updateGuardTestRuntime(clock *updateGuardTestClock, executablePath string) updateGuardRuntime {
	return updateGuardRuntime{
		now:   clock.now,
		sleep: clock.sleep,
		runCommand: func(context.Context, string, ...string) error {
			return nil
		},
		currentExecutable: func() (string, error) {
			return executablePath, nil
		},
		install:       installUpdateCandidate,
		restore:       restoreUpdateBackup,
		newHTTPClient: newUpdateGuardHTTPClient,
	}
}

func TestRunUpdateGuardSucceedsAfterThreeHealthSamples(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	attemptID := "0123456789abcdef0123456789abcdef"
	targetVersion := "v9.9.9"
	var tokenValue atomic.Value
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					w.WriteHeader(http.StatusOK)
				case updateHealthPath:
					if token, _ := tokenValue.Load().(string); token == "" || r.Header.Get(updateHealthTokenHeader) != token {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode(
						updateHealthResponse{
							Status: "ok", Version: targetVersion, AttemptID: attemptID,
						},
					)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			},
		),
	)
	defer server.Close()

	session, token := prepareArmedUpdateGuardSession(
		t, store, server.URL, clock.now(), "v9.9.8", targetVersion,
	)
	tokenValue.Store(token)
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	var restartCount int
	var activeCount int
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name != "systemctl" {
			return errors.New("unexpected command")
		}
		if len(args) > 0 && args[0] == "restart" {
			restartCount++
			return nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			activeCount++
			return nil
		}
		return errors.New("unexpected systemctl command")
	}
	if err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime); err != nil {
		t.Fatalf("run update guard: %v", err)
	}
	if restartCount != 1 || activeCount != updateGuardRequiredSuccesses {
		t.Fatalf("commands: restart=%d active=%d", restartCount, activeCount)
	}
	got, err := store.loadUpdateSession(context.Background())
	if err != nil || got == nil || got.Phase != updatePhaseSucceeded || got.Percent != 100 {
		t.Fatalf("completed session = %#v, err=%v", got, err)
	}
	if tokenState, errLoad := store.loadUpdateHealthToken(context.Background()); errLoad != nil || tokenState != nil {
		t.Fatalf("health token after success = %#v, err=%v", tokenState, errLoad)
	}
	if failure, errLoad := store.loadUpdateFailure(
		context.Background(), targetVersion,
	); errLoad != nil || failure != nil {
		t.Fatalf("failure after success = %#v, err=%v", failure, errLoad)
	}
	if data, errRead := os.ReadFile(session.ExecutablePath); errRead != nil || string(data) != "new-binary" {
		t.Fatalf("installed executable = %q, err=%v", data, errRead)
	}
	if data, errRead := os.ReadFile(session.BackupPath); errRead != nil || string(data) != "old-binary" {
		t.Fatalf("update backup = %q, err=%v", data, errRead)
	}
}

func TestRunUpdateGuardRollsBackAfterTargetVersionFailure(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC)}
	attemptID := "0123456789abcdef0123456789abcdef"
	previousVersion := "v9.9.8"
	targetVersion := "v9.9.9"
	var tokenValue atomic.Value
	var restored atomic.Bool
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path != updateHealthPath || r.Header.Get(updateHealthTokenHeader) != tokenValue.Load() {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				version := "wrong-version"
				if restored.Load() {
					version = previousVersion
				}
				_ = json.NewEncoder(w).Encode(
					updateHealthResponse{
						Status: "ok", Version: version, AttemptID: attemptID,
					},
				)
			},
		),
	)
	defer server.Close()

	session, token := prepareArmedUpdateGuardSession(
		t, store, server.URL, clock.now(), previousVersion, targetVersion,
	)
	tokenValue.Store(token)
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	realRestore := runtime.restore
	runtime.restore = func(current updateGuardSession) error {
		if err := realRestore(current); err != nil {
			return err
		}
		restored.Store(true)
		return nil
	}
	var restartCount int
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) > 0 && args[0] == "restart" {
			restartCount++
		}
		return nil
	}
	err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime)
	if err == nil || !strings.Contains(err.Error(), "health_failed") {
		t.Fatalf("run update guard error = %v", err)
	}
	if restartCount != 2 || !restored.Load() {
		t.Fatalf("rollback state: restart=%d restored=%t", restartCount, restored.Load())
	}
	got, errLoad := store.loadUpdateSession(context.Background())
	if errLoad != nil || got == nil || got.Phase != updatePhaseRolledBack || got.ResultCode != "health_failed" {
		t.Fatalf("rolled back session = %#v, err=%v", got, errLoad)
	}
	if got.FailureCount != 1 || got.NextRetryAt == nil {
		t.Fatalf("rolled back retry state = %#v", got)
	}
	if tokenState, tokenErr := store.loadUpdateHealthToken(context.Background()); tokenErr != nil || tokenState != nil {
		t.Fatalf("health token after rollback = %#v, err=%v", tokenState, tokenErr)
	}
	if data, errRead := os.ReadFile(session.ExecutablePath); errRead != nil || string(data) != "old-binary" {
		t.Fatalf("restored executable = %q, err=%v", data, errRead)
	}
	if _, errStat := os.Stat(session.BackupPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("backup should be consumed by rollback: %v", errStat)
	}
}

func TestRunUpdateGuardStopsAfterRollbackHealthFailure(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)}
	attemptID := "0123456789abcdef0123456789abcdef"
	var tokenValue atomic.Value
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path == updateHealthPath && r.Header.Get(updateHealthTokenHeader) == tokenValue.Load() {
					_ = json.NewEncoder(w).Encode(
						updateHealthResponse{
							Status: "ok", Version: "wrong-version", AttemptID: attemptID,
						},
					)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			},
		),
	)
	defer server.Close()

	session, token := prepareArmedUpdateGuardSession(t, store, server.URL, clock.now(), "v9.9.8", "v9.9.9")
	tokenValue.Store(token)
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	var restartCount int
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) > 0 && args[0] == "restart" {
			restartCount++
		}
		return nil
	}
	err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime)
	if err == nil || !strings.Contains(err.Error(), "rollback_health_failed") {
		t.Fatalf("run update guard error = %v", err)
	}
	if restartCount != 2 {
		t.Fatalf("restart count = %d, want 2", restartCount)
	}
	got, errLoad := store.loadUpdateSession(context.Background())
	if errLoad != nil || got == nil || got.Phase != updatePhaseTerminalFailure ||
		got.ResultCode != "rollback_health_failed" {
		t.Fatalf("terminal session = %#v, err=%v", got, errLoad)
	}
}

func TestRunUpdateGuardRestoresBinaryWhenStateFailsAfterInstall(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Date(2026, 9, 5, 14, 30, 0, 0, time.UTC)}
	attemptID := "0123456789abcdef0123456789abcdef"
	previousVersion := "v9.9.8"
	var tokenValue atomic.Value
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path == updateHealthPath && r.Header.Get(updateHealthTokenHeader) == tokenValue.Load() {
					_ = json.NewEncoder(w).Encode(
						updateHealthResponse{
							Status: "ok", Version: previousVersion, AttemptID: attemptID,
						},
					)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			},
		),
	)
	defer server.Close()

	session, token := prepareArmedUpdateGuardSession(
		t, store, server.URL, clock.now(), previousVersion, "v9.9.9",
	)
	tokenValue.Store(token)
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	realInstall := runtime.install
	runtime.install = func(current updateGuardSession) error {
		if err := realInstall(current); err != nil {
			return err
		}
		return store.db.Close()
	}
	var restartCount int
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) > 0 && args[0] == "restart" {
			restartCount++
		}
		return nil
	}
	if err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime); err == nil {
		t.Fatal("expected state failure after install")
	}
	if restartCount != 1 {
		t.Fatalf("rollback restart count = %d, want 1", restartCount)
	}
	if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "old-binary" {
		t.Fatalf("executable after state failure = %q, err=%v", data, err)
	}
}

func TestLaunchUpdateGuardFailureDoesNotReplaceExecutable(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	session, token := prepareArmedUpdateGuardSession(
		t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1",
	)
	clock := &updateGuardTestClock{current: now}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name != "systemd-run" {
			t.Fatalf("launch command = %q", name)
		}
		command := strings.Join(args, " ")
		if strings.Contains(command, token) {
			t.Fatal("systemd-run arguments contain update health token")
		}
		if !strings.Contains(command, session.GuardUnit) || !strings.Contains(command, session.AttemptID) {
			t.Fatalf("systemd-run arguments = %q", command)
		}
		return errors.New("systemd-run failed")
	}
	err := launchUpdateGuard(context.Background(), store, session, runtime)
	if err == nil || !strings.Contains(err.Error(), "guard_launch_failed") {
		t.Fatalf("launch error = %v", err)
	}
	got, errLoad := store.loadUpdateSession(context.Background())
	if errLoad != nil || got == nil || got.Phase != updatePhaseLaunchFailed {
		t.Fatalf("launch failure session = %#v, err=%v", got, errLoad)
	}
	if data, errRead := os.ReadFile(session.ExecutablePath); errRead != nil || string(data) != "old-binary" {
		t.Fatalf("current executable changed = %q, err=%v", data, errRead)
	}
	if _, errStat := os.Stat(session.CandidatePath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("candidate remains after launch failure: %v", errStat)
	}
}

func TestLaunchUpdateGuardClaimTimeoutDoesNotReplaceExecutable(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC)
	session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1")
	clock := &updateGuardTestClock{current: now}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	err := launchUpdateGuard(context.Background(), store, session, runtime)
	if err == nil || !strings.Contains(err.Error(), "guard_claim_failed") {
		t.Fatalf("claim timeout error = %v", err)
	}
	got, errLoad := store.loadUpdateSession(context.Background())
	if errLoad != nil || got == nil || got.Phase != updatePhaseLaunchFailed {
		t.Fatalf("claim timeout session = %#v, err=%v", got, errLoad)
	}
	if data, errRead := os.ReadFile(session.ExecutablePath); errRead != nil || string(data) != "old-binary" {
		t.Fatalf("current executable changed = %q, err=%v", data, errRead)
	}
}

func TestLaunchUpdateGuardReturnsAfterGuardClaimsSession(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Date(2026, 9, 5, 16, 30, 0, 0, time.UTC)
	session, token := prepareArmedUpdateGuardSession(
		t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1",
	)
	clock := &updateGuardTestClock{current: now}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	var launched bool
	runtime.runCommand = func(_ context.Context, name string, args ...string) error {
		if name != "systemd-run" {
			t.Fatalf("launch command = %q", name)
		}
		command := strings.Join(args, " ")
		if strings.Contains(command, token) {
			t.Fatal("systemd-run arguments contain update health token")
		}
		if !strings.Contains(command, session.GuardUnit) || !strings.Contains(command, session.AttemptID) {
			t.Fatalf("systemd-run arguments = %q", command)
		}
		launched = true
		return nil
	}
	runtime.sleep = func(ctx context.Context, delay time.Duration) error {
		if _, err := store.transitionUpdateSession(
			ctx,
			session.AttemptID,
			updatePhaseLaunching,
			func(next *updateGuardSession) {
				next.Phase = updatePhaseClaimed
				next.UpdatedAt = clock.now().UTC()
			},
		); err != nil {
			return err
		}
		return clock.sleep(ctx, delay)
	}
	if err := launchUpdateGuard(context.Background(), store, session, runtime); err != nil {
		t.Fatalf("launch update guard: %v", err)
	}
	if !launched {
		t.Fatal("systemd-run was not invoked")
	}
	got, err := store.loadUpdateSession(context.Background())
	if err != nil || got == nil || got.Phase != updatePhaseClaimed {
		t.Fatalf("claimed session = %#v, err=%v", got, err)
	}
	if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "old-binary" {
		t.Fatalf("current executable before guard install = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(session.CandidatePath); err != nil || string(data) != "new-binary" {
		t.Fatalf("candidate after guard claim = %q, err=%v", data, err)
	}
}

func TestLaunchUpdateGuardRejectsUnclaimedSessionPhase(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Date(2026, 9, 5, 16, 45, 0, 0, time.UTC)
	session, _ := prepareArmedUpdateGuardSession(
		t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1",
	)
	clock := &updateGuardTestClock{current: now}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	runtime.sleep = func(ctx context.Context, delay time.Duration) error {
		if _, err := store.transitionUpdateSession(
			ctx,
			session.AttemptID,
			updatePhaseLaunching,
			func(next *updateGuardSession) {
				next.Phase = updatePhaseError
				next.UpdatedAt = clock.now().UTC()
			},
		); err != nil {
			return err
		}
		return clock.sleep(ctx, delay)
	}
	err := launchUpdateGuard(context.Background(), store, session, runtime)
	if err == nil || !strings.Contains(err.Error(), "failed before claim") {
		t.Fatalf("unclaimed session phase error = %v", err)
	}
}

func TestUpdateGuardStateSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := openManagementStateStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	now := time.Now().UTC()
	session, token := prepareArmedUpdateGuardSession(
		t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1",
	)
	path := store.path
	if err = store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}
	reopened, err := openManagementStateStorePath(path)
	if err != nil {
		t.Fatalf("reopen management state: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.loadUpdateSession(context.Background())
	if err != nil || got == nil || got.AttemptID != session.AttemptID || got.Phase != updatePhaseLaunching {
		t.Fatalf("reopened session = %#v, err=%v", got, err)
	}
	gotToken, err := reopened.loadUpdateHealthToken(context.Background())
	if err != nil || gotToken == nil || gotToken.Token != token {
		t.Fatalf("reopened token = %#v, err=%v", gotToken, err)
	}
	var sessionRaw []byte
	if err = reopened.db.QueryRowContext(
		context.Background(), "SELECT value FROM state_items WHERE key=?", updateSessionStateKey,
	).Scan(&sessionRaw); err != nil {
		t.Fatalf("read raw update session: %v", err)
	}
	if strings.Contains(string(sessionRaw), token) {
		t.Fatal("update session manifest contains update health token")
	}
}

func TestUpdateSessionAllowsOnlyOnePendingAttempt(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Date(2026, 9, 5, 16, 50, 0, 0, time.UTC)
	host := updateGuardHost{
		ExecutablePath: filepath.Join(t.TempDir(), "cli-proxy-api"),
		StateDBPath:    store.path,
		ServiceUnit:    defaultUpdateServiceUnit,
		HealthBaseURL:  "http://127.0.0.1:8317",
	}
	host.BackupPath = host.ExecutablePath + ".bak"
	first := newUpdateGuardSession(
		host, "0123456789abcdef0123456789abcdef", "v1.0.1", false, now,
	)
	second := newUpdateGuardSession(
		host, "fedcba9876543210fedcba9876543210", "v1.0.2", true, now,
	)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, session := range []updateGuardSession{first, second} {
		go func() {
			<-start
			results <- store.beginUpdateSession(context.Background(), session)
		}()
	}
	close(start)
	var successCount int
	var inProgressCount int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, errUpdateInProgress):
			inProgressCount++
		default:
			t.Fatalf("begin concurrent update session: %v", err)
		}
	}
	if successCount != 1 || inProgressCount != 1 {
		t.Fatalf("concurrent begin results: success=%d in_progress=%d", successCount, inProgressCount)
	}
	active, err := store.loadUpdateSession(context.Background())
	if err != nil || active == nil {
		t.Fatalf("load active update session: %#v, %v", active, err)
	}
	if _, err = store.transitionUpdateSession(
		context.Background(),
		active.AttemptID,
		updatePhaseDownloading,
		func(next *updateGuardSession) { next.Phase = updatePhaseError },
	); err != nil {
		t.Fatalf("finish active update session: %v", err)
	}
	third := newUpdateGuardSession(
		host, "00112233445566778899aabbccddeeff", "v1.0.3", false, now,
	)
	if err = store.beginUpdateSession(context.Background(), third); err != nil {
		t.Fatalf("begin replacement update session: %v", err)
	}
	if _, err = store.transitionUpdateSession(
		context.Background(), active.AttemptID, updatePhaseError, nil,
	); !errors.Is(err, errUpdateSessionChanged) {
		t.Fatalf("stale update transition error = %v", err)
	}
}

func TestGetUpdateStatusUsesPersistentSessionAfterMemoryReset(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	previous := snapshotSelfUpdate()
	t.Cleanup(
		func() {
			setSelfUpdate(previous.status, previous.message, previous.version, previous.attemptID, previous.percent)
		},
	)
	setSelfUpdate("idle", "", "", "", 0)
	now := time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC)
	session := newUpdateGuardSession(
		updateGuardHost{
			ExecutablePath: filepath.Join(t.TempDir(), "cli-proxy-api"),
			StateDBPath:    store.path,
			ServiceUnit:    defaultUpdateServiceUnit,
			HealthBaseURL:  "http://127.0.0.1:8317",
		},
		"0123456789abcdef0123456789abcdef",
		"v1.0.1",
		false,
		now,
	)
	session.BackupPath = session.ExecutablePath + ".bak"
	session.Phase = updatePhaseSucceeded
	session.Percent = 100
	if err := store.beginUpdateSession(context.Background(), session); err != nil {
		t.Fatalf("save completed update session: %v", err)
	}

	handler := &Handler{stateStore: store}
	engine := gin.New()
	engine.GET("/update-status", handler.GetUpdateStatus)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/update-status", nil)
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status code = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Status         string `json:"status"`
		TargetVersion  string `json:"target_version"`
		AttemptID      string `json:"attempt_id"`
		GuardPhase     string `json:"guard_phase"`
		Percent        int    `json:"percent"`
		StateAvailable bool   `json:"state_available"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update status: %v", err)
	}
	if payload.Status != "done" || payload.TargetVersion != session.TargetVersion || !payload.StateAvailable ||
		payload.AttemptID != session.AttemptID || payload.GuardPhase != updatePhaseSucceeded || payload.Percent != 100 {
		t.Fatalf("persistent update status = %#v", payload)
	}
}

func TestGetUpdateStatusSignalsUnavailablePersistentState(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}
	handler := &Handler{stateStore: store}
	engine := gin.New()
	engine.GET("/update-status", handler.GetUpdateStatus)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/update-status", nil)
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status code = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		StateAvailable bool   `json:"state_available"`
		StateError     string `json:"state_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update status: %v", err)
	}
	if payload.StateAvailable || payload.StateError != "update_state_unavailable" {
		t.Fatalf("unavailable update state = %#v", payload)
	}
}

func TestUpdateHealthRequiresDirectLoopbackSessionToken(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Now().UTC()
	session, token := prepareArmedUpdateGuardSession(
		t, store, "http://127.0.0.1:8317", now, "v1.0.0", "v1.0.1",
	)
	handler := &Handler{stateStore: store}
	engine := gin.New()
	engine.GET(updateHealthPath, handler.GetUpdateHealth)

	request := httptest.NewRequest(http.MethodGet, updateHealthPath, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set(updateHealthTokenHeader, token)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("direct update health status = %d: %s", response.Code, response.Body.String())
	}
	var payload updateHealthResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil ||
		payload.AttemptID != session.AttemptID || payload.Version != buildinfo.Version {
		t.Fatalf("update health payload = %#v, err=%v", payload, err)
	}

	for _, testCase := range []struct {
		name       string
		remoteAddr string
		forwarded  string
		token      string
		want       int
	}{
		{
			name: "wrong token", remoteAddr: "127.0.0.1:12345", token: strings.Repeat("0", 64),
			want: http.StatusUnauthorized,
		},
		{
			name: "forwarded loopback", remoteAddr: "127.0.0.1:12345", forwarded: "203.0.113.9", token: token,
			want: http.StatusForbidden,
		},
		{name: "remote peer", remoteAddr: "203.0.113.9:12345", token: token, want: http.StatusForbidden},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, updateHealthPath, nil)
				req.RemoteAddr = testCase.remoteAddr
				req.Header.Set(updateHealthTokenHeader, testCase.token)
				if testCase.forwarded != "" {
					req.Header.Set("X-Forwarded-For", testCase.forwarded)
				}
				res := httptest.NewRecorder()
				engine.ServeHTTP(res, req)
				if res.Code != testCase.want {
					t.Fatalf("status = %d, want %d", res.Code, testCase.want)
				}
			},
		)
	}

	if err := store.extendUpdateHealthToken(
		context.Background(), session.AttemptID, time.Now().Add(-time.Second),
	); err != nil {
		t.Fatalf("expire update health token: %v", err)
	}
	expired := httptest.NewRequest(http.MethodGet, updateHealthPath, nil)
	expired.RemoteAddr = "127.0.0.1:12345"
	expired.Header.Set(updateHealthTokenHeader, token)
	expiredResponse := httptest.NewRecorder()
	engine.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", expiredResponse.Code)
	}
}

func TestUpdateHealthTokenCannotAuthenticateManagementRoute(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret("management-secret"))
	defer func() { _ = handler.Close() }()
	_, token := prepareArmedUpdateGuardSession(
		t, handler.stateStore, "http://127.0.0.1:8317", time.Now(), "v1.0.0", "v1.0.1",
	)
	engine := gin.New()
	group := engine.Group("/v0/management")
	group.Use(handler.middleware())
	group.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/v0/management/protected", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set(updateHealthTokenHeader, token)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("management route status = %d, want 401", response.Code)
	}
}

func TestUpdateGuardTLSClientUsesConfiguredCertificate(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		),
	)
	defer server.Close()
	certificatePath := filepath.Join(t.TempDir(), "server.crt")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificatePath, certificatePEM, 0o600); err != nil {
		t.Fatalf("write TLS certificate: %v", err)
	}
	session := updateGuardSession{
		HealthBaseURL:     server.URL,
		HealthTLSCertPath: certificatePath,
	}
	client, err := newUpdateGuardHTTPClient(session)
	if err != nil {
		t.Fatalf("new TLS update guard client: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig == nil ||
		transport.TLSClientConfig.MinVersion < tls.VersionTLS12 || transport.TLSClientConfig.ServerName == "" {
		t.Fatalf("TLS update transport = %#v", client.Transport)
	}
	if err = probeUpdatePublicHealth(context.Background(), client, server.URL); err != nil {
		t.Fatalf("TLS public health: %v", err)
	}
}

func TestUpdateFailureBackoffPersistsAndDoesNotBlockManualCompatibility(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	now := time.Now().UTC()
	first, err := store.recordUpdateFailure(
		context.Background(), "v2.0.0", "health_failed", now, updateFailureBaseDelay, updateFailureMaxDelay,
	)
	if err != nil || first.FailureCount != 1 || !first.NextRetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("first backoff = %#v, err=%v", first, err)
	}
	second, err := store.recordUpdateFailure(
		context.Background(), "v2.0.0", "health_failed", now, updateFailureBaseDelay, updateFailureMaxDelay,
	)
	if err != nil || second.FailureCount != 2 || !second.NextRetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("second backoff = %#v, err=%v", second, err)
	}
	handler := &Handler{stateStore: store}
	compat := handler.buildUpdateCompatibilityFromConfig(context.Background(), "v2.0.0", &config.Config{})
	if !compat.Compatible || !compat.AutomaticRetryBlocked || compat.LastFailure == nil ||
		compat.LastFailure.FailureCount != 2 {
		t.Fatalf("manual compatibility with backoff = %#v", compat)
	}
}

func TestTryAutoUpdateSkipsVersionInRetryBackoff(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	previousVersion := buildinfo.Version
	buildinfo.Version = "v1.0.0"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	previousState := snapshotSelfUpdate()
	setSelfUpdate("idle", "", "", "", 0)
	t.Cleanup(
		func() {
			setSelfUpdate(
				previousState.status,
				previousState.message,
				previousState.version,
				previousState.attemptID,
				previousState.percent,
			)
		},
	)
	now := time.Now().UTC()
	if _, err := store.recordUpdateFailure(
		context.Background(),
		"v2.0.0",
		"health_failed",
		now,
		updateFailureBaseDelay,
		updateFailureMaxDelay,
	); err != nil {
		t.Fatalf("record update failure: %v", err)
	}

	originalTransport := http.DefaultTransport
	var requestCount atomic.Int32
	http.DefaultTransport = updateGuardRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			requestCount.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v2.0.0"}`)),
				Request:    request,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	cfg := &config.Config{}
	cfg.RemoteManagement.AutoCheckUpdate = true
	cfg.RemoteManagement.AutoUpdateCPA = true
	handler := &Handler{cfg: cfg, stateStore: store}
	handler.tryAutoUpdateCPA(context.Background(), cfg)
	if requestCount.Load() != 1 {
		t.Fatalf("latest release requests = %d, want 1", requestCount.Load())
	}
	session, err := store.loadUpdateSession(context.Background())
	if err != nil {
		t.Fatalf("load update session: %v", err)
	}
	if session != nil {
		t.Fatalf("auto-update started during retry backoff: %#v", session)
	}
}

func TestReconcileStaleDownloadDoesNotRestorePreviousBackup(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	oldBuildVersion := buildinfo.Version
	buildinfo.Version = "v1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldBuildVersion })
	dir := t.TempDir()
	executablePath := filepath.Join(dir, "cli-proxy-api")
	if err := os.WriteFile(executablePath, []byte("current-binary"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	backupPath := executablePath + ".bak"
	if err := os.WriteFile(backupPath, []byte("older-backup"), 0o755); err != nil {
		t.Fatalf("write previous backup: %v", err)
	}
	started := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	session := newUpdateGuardSession(
		updateGuardHost{
			ExecutablePath: executablePath,
			BackupPath:     backupPath,
			StateDBPath:    store.path,
			ServiceUnit:    defaultUpdateServiceUnit,
			HealthBaseURL:  "http://127.0.0.1:8317",
		},
		"0123456789abcdef0123456789abcdef",
		"v1.0.1",
		false,
		started,
	)
	if err := store.beginUpdateSession(context.Background(), session); err != nil {
		t.Fatalf("begin stale update: %v", err)
	}
	clock := &updateGuardTestClock{current: started.Add(updateGuardDownloadTimeout + time.Second)}
	runtime := updateGuardTestRuntime(clock, executablePath)
	var restoreCalls int
	runtime.restore = func(updateGuardSession) error {
		restoreCalls++
		return nil
	}
	runtime.runCommand = func(context.Context, string, ...string) error {
		return errors.New("inactive")
	}
	handler := &Handler{stateStore: store}
	if err := handler.reconcileStaleUpdateSession(context.Background(), runtime); err != nil {
		t.Fatalf("reconcile stale update: %v", err)
	}
	if restoreCalls != 0 {
		t.Fatalf("stale download restored unrelated backup %d times", restoreCalls)
	}
	if data, err := os.ReadFile(backupPath); err != nil || string(data) != "older-backup" {
		t.Fatalf("previous backup changed = %q, err=%v", data, err)
	}
	got, err := store.loadUpdateSession(context.Background())
	if err != nil || got == nil || got.Phase != updatePhaseRolledBack || got.ResultCode != "interrupted" {
		t.Fatalf("reconciled session = %#v, err=%v", got, err)
	}
}

func TestInstallAndRestoreUpdateCandidate(t *testing.T) {
	dir := t.TempDir()
	executablePath := filepath.Join(dir, "cli-proxy-api")
	session := updateGuardSession{
		ExecutablePath: executablePath,
		CandidatePath:  executablePath + ".candidate",
		BackupPath:     executablePath + ".bak",
	}
	if err := os.WriteFile(session.ExecutablePath, []byte("old"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	if err := os.WriteFile(session.CandidatePath, []byte("new"), 0o755); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := os.WriteFile(session.BackupPath, []byte("previous"), 0o755); err != nil {
		t.Fatalf("write previous backup: %v", err)
	}
	if err := installUpdateCandidate(session); err != nil {
		t.Fatalf("install update candidate: %v", err)
	}
	if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "new" {
		t.Fatalf("installed executable = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(session.BackupPath); err != nil || string(data) != "old" {
		t.Fatalf("installed backup = %q, err=%v", data, err)
	}
	if err := restoreUpdateBackup(session); err != nil {
		t.Fatalf("restore update backup: %v", err)
	}
	if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "old" {
		t.Fatalf("restored executable = %q, err=%v", data, err)
	}
}

func TestParseUpdateGuardCommand(t *testing.T) {
	validAttempt := "0123456789abcdef0123456789abcdef"
	statePath := filepath.Join(t.TempDir(), "management-state.db")
	gotPath, gotAttempt, handled, err := parseUpdateGuardCommand(
		[]string{
			updateGuardCommandFlag,
			updateGuardStateDBFlag,
			statePath,
			updateGuardAttemptFlag + "=" + validAttempt,
		},
	)
	if err != nil || !handled || gotPath != statePath || gotAttempt != validAttempt {
		t.Fatalf("valid guard command = (%q, %q, %t, %v)", gotPath, gotAttempt, handled, err)
	}
	if _, _, handled, err = parseUpdateGuardCommand([]string{"--config", "config.yaml"}); err != nil || handled {
		t.Fatalf("normal command handled=%t err=%v", handled, err)
	}
	for _, args := range [][]string{
		{"--config", "config.yaml", updateGuardCommandFlag},
		{updateGuardCommandFlag, updateGuardStateDBFlag, "relative.db", updateGuardAttemptFlag, validAttempt},
		{updateGuardCommandFlag, updateGuardStateDBFlag, statePath, updateGuardAttemptFlag, "invalid"},
		{
			updateGuardCommandFlag, updateGuardStateDBFlag, statePath, updateGuardStateDBFlag, statePath,
			updateGuardAttemptFlag, validAttempt,
		},
		{updateGuardCommandFlag, "--unknown"},
	} {
		if _, _, handled, err = parseUpdateGuardCommand(args); !handled || err == nil {
			t.Errorf("invalid args %#v handled=%t err=%v", args, handled, err)
		}
	}
}

func TestRunUpdateGuardCommandDoesNotCreateStateDatabase(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "missing", "management-state.db")
	handled, exitCode := RunUpdateGuardCommand(
		[]string{
			updateGuardCommandFlag,
			updateGuardStateDBFlag,
			statePath,
			updateGuardAttemptFlag,
			"0123456789abcdef0123456789abcdef",
		},
	)
	if !handled || exitCode != 1 {
		t.Fatalf("missing state database command = (%t, %d)", handled, exitCode)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guard command created missing state database: %v", err)
	}
}

func TestProbeUpdateHealthRejectsOversizedOrWrongIdentity(t *testing.T) {
	const attemptID = "0123456789abcdef0123456789abcdef"
	validPayload := `{"status":"ok","version":"v2.0.0","attempt_id":"` + attemptID + `"}`
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "wrong identity", body: `{"status":"ok","version":"v2.0.0","attempt_id":"other"}`},
		{name: "oversized response", body: validPayload + strings.Repeat(" ", maxUpdateHealthResponseBytes)},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/healthz" {
								w.WriteHeader(http.StatusOK)
								return
							}
							_, _ = io.WriteString(w, testCase.body)
						},
					),
				)
				defer server.Close()
				session := updateGuardSession{AttemptID: attemptID, HealthBaseURL: server.URL}
				client, err := newUpdateGuardHTTPClient(session)
				if err != nil {
					t.Fatalf("new update guard client: %v", err)
				}
				if err = probeUpdatePublicHealth(context.Background(), client, server.URL); err != nil {
					t.Fatalf("public health: %v", err)
				}
				if err = probeUpdatePrivateHealth(
					context.Background(), client, session, "token", "v2.0.0",
				); err == nil {
					t.Fatalf("private health accepted %s", testCase.name)
				}
			},
		)
	}
}
