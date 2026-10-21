package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
)

func TestUpdateGuardDrainWaitsAndRejectsAConfirmationFromAnotherProcess(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	admission := executionregistry.NewAdmission()
	handler := &Handler{stateStore: store, inferenceAdmission: admission}
	engine := gin.New()
	engine.POST(updateDrainPath, handler.PostUpdateDrain)
	server := httptest.NewServer(engine)
	defer server.Close()
	session, token := prepareArmedUpdateGuardSession(t, store, server.URL, time.Now(), "v1.0.0", "v1.0.1")
	current, err := store.transitionUpdateSession(context.Background(), session.AttemptID, updatePhaseLaunching, func(next *updateGuardSession) {
		next.Phase = updatePhaseDraining
		next.OriginInstance = admission.InstanceID()
	})
	if err != nil {
		t.Fatal(err)
	}
	session = *current
	_, release, _ := admission.Acquire(context.Background(), nil)
	defer release()
	type reply struct {
		confirmation executionregistry.DrainConfirmation
		err          error
	}
	result := make(chan reply, 1)
	go func() {
		confirmation, err := requestUpdateDrain(context.Background(), session, token, "wait")
		result <- reply{confirmation, err}
	}()
	paused := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		_, finish, err := admission.Acquire(context.Background(), nil)
		if errors.Is(err, executionregistry.ErrInferencePaused) {
			paused = true
			break
		}
		finish()
		time.Sleep(time.Millisecond)
	}
	if !paused {
		t.Fatal("guard did not pause new inference")
	}
	select {
	case <-result:
		t.Fatal("guard completed while accepted inference was still active")
	default:
	}
	release()
	var drained reply
	select {
	case drained = <-result:
	case <-time.After(time.Second):
		t.Fatal("guard did not observe inference completion")
	}
	if drained.err != nil || !admission.Confirm(drained.confirmation) {
		t.Fatalf("drain result: %#v", drained)
	}
	ready, err := store.transitionUpdateSession(context.Background(), session.AttemptID, updatePhaseDraining, func(next *updateGuardSession) {
		next.Phase = updatePhaseInstalling
		next.DrainConfirmation = &drained.confirmation
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requestUpdateDrain(context.Background(), *ready, token, "confirm"); err != nil {
		t.Fatal(err)
	}
	handler.inferenceAdmission = executionregistry.NewAdmission()
	if _, err := requestUpdateDrain(context.Background(), *ready, token, "confirm"); err == nil {
		t.Fatal("a new process instance accepted an old drain confirmation")
	}
	admission.Resume(session.AttemptID)
}

func TestUpdateGuardDrainFailureNeverInstallsOrRestarts(t *testing.T) {
	for _, stage := range []string{"timeout", "wrong_instance", "expired", "confirm_failed"} {
		t.Run(stage, func(t *testing.T) {
			store := openUpdateGuardTestStore(t)
			clock := &updateGuardTestClock{current: time.Now().UTC()}
			session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
			runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
			originalDrain := runtime.drain
			installs, restarts, resumes := 0, 0, 0
			runtime.install = func(updateGuardSession) error { installs++; return nil }
			runtime.runCommand = func(_ context.Context, _ string, args ...string) error {
				if len(args) > 0 && args[0] == "restart" {
					restarts++
				}
				return nil
			}
			runtime.drain = func(ctx context.Context, current updateGuardSession, token, action string) (executionregistry.DrainConfirmation, error) {
				if action == "resume" {
					resumes++
					return executionregistry.DrainConfirmation{}, nil
				}
				if stage == "timeout" {
					return executionregistry.DrainConfirmation{}, context.DeadlineExceeded
				}
				if stage == "confirm_failed" && action == "confirm" {
					return executionregistry.DrainConfirmation{}, errors.New("service restarted")
				}
				confirmation, err := originalDrain(ctx, current, token, action)
				if stage == "wrong_instance" {
					confirmation.InstanceID = "previous-process"
				}
				if stage == "expired" {
					confirmation.Deadline = clock.now().Add(-time.Second)
				}
				return confirmation, err
			}
			err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime)
			if err == nil || !strings.Contains(err.Error(), "drain_failed") || installs != 0 || restarts != 0 || resumes != 1 {
				t.Fatalf("failed guard: err=%v installs=%d restarts=%d resumes=%d", err, installs, restarts, resumes)
			}
			if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "old-binary" {
				t.Fatalf("running executable changed: %q, %v", data, err)
			}
		})
	}
}

func TestUpdateGuardRejectsCandidateChangedAfterDrain(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Now().UTC()}
	session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	originalDrain := runtime.drain
	runtime.drain = func(ctx context.Context, current updateGuardSession, token, action string) (executionregistry.DrainConfirmation, error) {
		if action == "confirm" {
			if err := os.WriteFile(current.CandidatePath, []byte("changed-candidate"), 0o755); err != nil {
				return executionregistry.DrainConfirmation{}, err
			}
		}
		return originalDrain(ctx, current, token, action)
	}
	restarts := 0
	runtime.runCommand = func(context.Context, string, ...string) error { restarts++; return nil }
	if err := runUpdateGuard(context.Background(), store, session.AttemptID, runtime); err == nil || !strings.Contains(err.Error(), "install_failed") {
		t.Fatalf("modified candidate accepted: %v", err)
	}
	if restarts != 0 {
		t.Fatalf("restarted after identity failure: %d", restarts)
	}
	if data, err := os.ReadFile(session.ExecutablePath); err != nil || string(data) != "old-binary" {
		t.Fatalf("previous executable changed: %q, %v", data, err)
	}
}

func TestExpiredUpdateDoesNotUseVersionAloneAsSuccessEvidence(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Now().UTC()}
	session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
	_, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.Phase = updatePhaseHealthCheck
		next.Deadline = clock.now().Add(-time.Second)
		next.DeadlineUnix = next.Deadline.Unix()
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	runtime.runCommand = func(context.Context, string, ...string) error { return errors.New("guard stopped") }
	runtime.identify = func(string, string, string) (updateArtifactIdentity, error) {
		wrong := session.TargetIdentity
		wrong.Commit = strings.Repeat("b", 40)
		return wrong, nil
	}
	handler := &Handler{stateStore: store, inferenceAdmission: executionregistry.NewAdmission()}
	if err := handler.reconcileExpiredUpdate(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	current, err := store.loadUpdateSession(context.Background())
	if err != nil || current == nil || current.Phase == updatePhaseSucceeded || current.ResultCode != "interrupted" {
		t.Fatalf("wrong commit was treated as a successful update: %#v, %v", current, err)
	}
}

func TestLegacyUpdateRecoveryDoesNotClaimCompleteIdentityVerification(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Now().UTC()}
	session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
	previousVersion := buildinfo.Version
	buildinfo.Version = session.PreviousVersion
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	legacy, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.SchemaVersion = 1
		next.Phase = updatePhaseInstalling
		next.BackupPath = next.ExecutablePath + ".bak"
		next.Deadline = clock.now().Add(-time.Second)
		next.DeadlineUnix = next.Deadline.Unix()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy.BackupPath, []byte("legacy-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installUpdateCandidate(*legacy); err == nil {
		t.Fatal("legacy session installed a new candidate without drain")
	}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	runtime.runCommand = func(context.Context, string, ...string) error { return errors.New("guard stopped") }
	handler := &Handler{stateStore: store, inferenceAdmission: executionregistry.NewAdmission()}
	if err := handler.reconcileExpiredUpdate(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	current, err := store.loadUpdateSession(context.Background())
	if err != nil || current == nil || current.Phase != updatePhaseRolledBack || current.ResultCode != "legacy_recovery_unverified" {
		t.Fatalf("legacy recovery result: %#v, %v", current, err)
	}
	if status := updateStateFromSession(*current); status.status != "error" || !strings.Contains(status.message, "manual") {
		t.Fatalf("legacy recovery hid missing identity evidence: status=%s message=%s", status.status, status.message)
	}
}

func TestLegacyGuardHealthRestoresAdmissionOnlyAfterGuardExit(t *testing.T) {
	ctx := context.Background()
	store := openUpdateGuardTestStore(t)
	admission := executionregistry.NewAdmission()
	handler := &Handler{stateStore: store, inferenceAdmission: admission}
	engine := gin.New()
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET(updateHealthPath, handler.GetUpdateHealth)
	server := httptest.NewServer(engine)
	defer server.Close()
	clock := &updateGuardTestClock{current: time.Now().UTC()}
	session, token := prepareArmedUpdateGuardSession(t, store, server.URL, clock.now(), "v8.0.23-aug.3", "v8.0.23-aug.4")
	legacy, err := store.transitionUpdateSession(ctx, session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.SchemaVersion = 1
		next.Phase = updatePhaseHealthCheck
		next.OriginInstance = ""
		next.BackupPath = next.ExecutablePath + ".bak"
		next.PreviousIdentity = updateArtifactIdentity{}
		next.TargetIdentity = updateArtifactIdentity{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, "UPDATE state_items SET value=json_set(value, '$.schema_version', 1) WHERE key=?", updateHealthTokenStateKey); err != nil {
		t.Fatal(err)
	}
	previousVersion := buildinfo.Version
	buildinfo.Version = "8.0.23-aug.4"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	if err := handler.restoreUpdateAdmission(ctx); err != nil {
		t.Fatal(err)
	}
	if !admission.Paused(session.AttemptID) {
		t.Fatal("new process accepted inference before the legacy guard completed")
	}
	runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
	if err := verifyUpdateGuardHealth(ctx, *legacy, token, legacy.TargetVersion, runtime); err != nil {
		t.Fatalf("legacy guard health contract failed: %v", err)
	}
	if _, err := store.transitionUpdateSession(ctx, session.AttemptID, legacy.Phase, func(next *updateGuardSession) {
		next.Phase = updatePhaseSucceeded
	}); err != nil {
		t.Fatal(err)
	}
	runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) { return false, nil }
	if err := handler.reconcileExpiredUpdate(ctx, runtime); !errors.Is(err, errUpdateInProgress) || !admission.Paused(session.AttemptID) {
		t.Fatalf("resumed while the legacy guard was still active: %v", err)
	}
	runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) {
		return false, errors.New("systemd unavailable")
	}
	if err := handler.reconcileExpiredUpdate(ctx, runtime); err == nil || !admission.Paused(session.AttemptID) {
		t.Fatal("resumed without proving that the legacy guard had stopped")
	}
	runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) { return true, nil }
	if err := handler.reconcileExpiredUpdate(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	_, release, err := admission.Acquire(ctx, nil)
	if err != nil {
		t.Fatalf("inference remained paused after the legacy guard exited: %v", err)
	}
	release()
	if token, err := store.loadUpdateHealthToken(ctx); err != nil || token != nil {
		t.Fatalf("legacy health authentication survived completed reconciliation: present=%t err=%v", token != nil, err)
	}
}

func TestInstallingUpdateKeepsAdmissionPausedAfterConfirmationExpires(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	admission := executionregistry.NewAdmission()
	handler := &Handler{stateStore: store, inferenceAdmission: admission}
	engine := gin.New()
	engine.POST(updateDrainPath, handler.PostUpdateDrain)
	server := httptest.NewServer(engine)
	defer server.Close()
	session, token := prepareArmedUpdateGuardSession(t, store, server.URL, time.Now(), "v1.0.0", "v1.0.1")
	deadline := time.Now().Add(300 * time.Millisecond)
	confirmation, err := admission.Drain(context.Background(), session.AttemptID, deadline)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.Phase = updatePhaseInstalling
		next.OriginInstance = admission.InstanceID()
		next.DrainConfirmation = &confirmation
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requestUpdateDrain(context.Background(), *current, token, "confirm"); err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Until(deadline) + 20*time.Millisecond)
	_, release, err := admission.Acquire(context.Background(), nil)
	defer release()
	if !errors.Is(err, executionregistry.ErrInferencePaused) {
		t.Fatal("installation accepted inference after its confirmed drain deadline")
	}
	admission.Resume(session.AttemptID)
}

func TestFailedRollbackRetainsItsBackupAndRecoverySession(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	clock := &updateGuardTestClock{current: time.Now().UTC()}
	session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
	if err := installUpdateCandidate(session); err != nil {
		t.Fatal(err)
	}
	_, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.Phase = updatePhaseTerminalFailure
		next.ResultCode = "rollback_failed"
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{stateStore: store, inferenceAdmission: executionregistry.NewAdmission()}
	if err := handler.reconcileStaleUpdateSession(context.Background(), updateGuardTestRuntime(clock, session.ExecutablePath)); !errors.Is(err, errUpdateRecoveryRequired) {
		t.Errorf("recovery did not block a new attempt explicitly: %v", err)
	}
	if data, err := os.ReadFile(session.BackupPath); err != nil || string(data) != "old-binary" {
		t.Fatalf("recovery backup changed: %q, %v", data, err)
	}
	current, err := store.loadUpdateSession(context.Background())
	if err != nil || current == nil || current.AttemptID != session.AttemptID || current.ResultCode != "rollback_failed" {
		t.Fatalf("recovery identity was discarded: %#v, %v", current, err)
	}
}

func TestNewHandlerRestoresInstallationPauseBeforeReturning(t *testing.T) {
	for _, phase := range []string{updatePhaseInstalling, updatePhaseRestarting, updatePhaseHealthCheck,
		updatePhaseRollingBack, updatePhaseRollbackRestarting, updatePhaseRollbackHealthCheck, updatePhaseTerminalFailure} {
		t.Run(phase, func(t *testing.T) {
			store := openUpdateGuardTestStore(t)
			clock := &updateGuardTestClock{current: time.Now().UTC()}
			session, _ := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", clock.now(), "v1.0.0", "v1.0.1")
			_, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
				next.Phase = phase
			})
			if err != nil {
				t.Fatal(err)
			}
			admission := executionregistry.NewAdmission()
			handler := NewHandler(&config.Config{}, filepath.Join(filepath.Dir(store.path), "config.yaml"), nil, WithInferenceAdmission(admission))
			defer func() { _ = handler.Close() }()
			_, release, err := admission.Acquire(context.Background(), nil)
			release()
			if !errors.Is(err, executionregistry.ErrInferencePaused) {
				t.Fatalf("new process accepted inference during %s", phase)
			}
			_, err = store.transitionUpdateSession(context.Background(), session.AttemptID, phase, func(next *updateGuardSession) {
				next.Phase = updatePhaseSucceeded
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := updateGuardTestRuntime(clock, session.ExecutablePath)
			runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) { return false, nil }
			if err := handler.reconcileExpiredUpdate(context.Background(), runtime); !errors.Is(err, errUpdateInProgress) || !admission.Paused(session.AttemptID) {
				t.Fatalf("guard still running or restart queued: paused=%t err=%v", admission.Paused(session.AttemptID), err)
			}
			runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) {
				return false, errors.New("systemd unavailable")
			}
			if err := handler.reconcileExpiredUpdate(context.Background(), runtime); err == nil || !admission.Paused(session.AttemptID) {
				t.Fatalf("unknown systemd state resumed inference: %v", err)
			}
			runtime.quiescent = func(context.Context, updateGuardSession) (bool, error) { return true, nil }
			if err := handler.reconcileExpiredUpdate(context.Background(), runtime); err != nil {
				t.Fatal(err)
			}
			_, release, err = admission.Acquire(context.Background(), nil)
			release()
			if err != nil {
				t.Fatalf("completed guard did not release startup pause: %v", err)
			}
		})
	}
}

func TestUpdateUnitQuiescenceRequiresNoPendingJobs(t *testing.T) {
	for _, test := range []struct {
		name, output string
		guard, ready bool
		invalid      bool
	}{
		{"collected guard", "LoadState=not-found\nActiveState=inactive\nJob=\n", true, true, false},
		{"failed guard", "LoadState=loaded\nActiveState=failed\nJob=\n", true, true, false},
		{"active guard", "LoadState=loaded\nActiveState=active\nJob=\n", true, false, false},
		{"stopping guard", "LoadState=loaded\nActiveState=deactivating\nJob=41\n", true, false, false},
		{"starting guard", "LoadState=loaded\nActiveState=activating\nJob=42\n", true, false, false},
		{"queued guard", "LoadState=loaded\nActiveState=inactive\nJob=43\n", true, false, false},
		{"active service", "LoadState=loaded\nActiveState=active\nJob=\n", false, true, false},
		{"queued restart", "LoadState=loaded\nActiveState=active\nJob=44\n", false, false, false},
		{"missing service", "LoadState=not-found\nActiveState=inactive\nJob=\n", false, false, false},
		{"missing job", "LoadState=loaded\nActiveState=inactive\n", true, false, true},
		{"missing state", "LoadState=loaded\nJob=\n", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, err := updateUnitQuiescent(test.output, test.guard)
			if ready != test.ready || (err != nil) != test.invalid {
				t.Fatalf("ready=%t err=%v", ready, err)
			}
		})
	}
}

func TestUpdateDrainRequiresDirectLoopbackAndItsAttemptToken(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	admission := executionregistry.NewAdmission()
	handler := &Handler{stateStore: store, inferenceAdmission: admission}
	session, token := prepareArmedUpdateGuardSession(t, store, "http://127.0.0.1:8317", time.Now(), "v1.0.0", "v1.0.1")
	_, err := store.transitionUpdateSession(context.Background(), session.AttemptID, session.Phase, func(next *updateGuardSession) {
		next.Phase = updatePhaseDraining
		next.OriginInstance = admission.InstanceID()
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.POST(updateDrainPath, handler.PostUpdateDrain)
	for _, test := range []struct {
		peer, token string
		status      int
	}{
		{"203.0.113.2:1234", token, http.StatusForbidden},
		{"127.0.0.1:1234", "wrong", http.StatusUnauthorized},
		{"127.0.0.1:1234", "", http.StatusUnauthorized},
	} {
		request := httptest.NewRequest(http.MethodPost, updateDrainPath, strings.NewReader(`{"attempt_id":"`+session.AttemptID+`","action":"wait"}`))
		request.RemoteAddr = test.peer
		if test.peer != "127.0.0.1:1234" {
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
		}
		request.Header.Set(updateHealthTokenHeader, test.token)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("peer %s status = %d", test.peer, response.Code)
		}
	}
}
