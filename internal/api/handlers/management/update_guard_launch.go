package management

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) restoreUpdateAdmission(ctx context.Context) error {
	session, err := h.stateStore.loadUpdateSession(ctx)
	if err != nil {
		h.inferenceAdmission.Hold(updateSessionStateKey)
		return err
	}
	if session != nil && (updatePhaseMayHaveInstalledCandidate(session.Phase) || session.Phase == updatePhaseTerminalFailure) {
		if !h.inferenceAdmission.Hold(session.AttemptID) {
			return fmt.Errorf("another update already holds inference admission")
		}
	}
	return nil
}

func (h *Handler) startUpdateReconciliation(ctx context.Context) {
	if h.inferenceAdmission == nil || h.stateStore == nil {
		return
	}
	h.attemptCleanupWG.Go(func() {
		ticker := time.NewTicker(updateGuardReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := h.reconcileExpiredUpdate(checkCtx, realUpdateGuardRuntime())
				cancel()
				if err != nil && !errors.Is(err, errUpdateInProgress) && !errors.Is(err, errUpdateRecoveryRequired) && ctx.Err() == nil {
					log.WithError(err).Warn("failed to reconcile expired update")
				}
			}
		}
	})
}

func (h *Handler) reconcileExpiredUpdate(ctx context.Context, runtime updateGuardRuntime) error {
	if h == nil || h.stateStore == nil || h.inferenceAdmission == nil {
		return nil
	}
	session, err := h.stateStore.loadUpdateSession(ctx)
	if err != nil {
		return err
	}
	if session == nil {
		h.inferenceAdmission.Resume(updateSessionStateKey)
		return nil
	}
	if isActiveUpdatePhase(session.Phase) {
		if runtime.now().Unix() <= session.DeadlineUnix {
			return nil
		}
	} else if !h.inferenceAdmission.Paused(session.AttemptID) && !h.inferenceAdmission.Paused(updateSessionStateKey) {
		return nil
	}
	return h.reconcileStaleUpdateSession(ctx, runtime)
}

func (h *Handler) reconcileStaleUpdateSession(ctx context.Context, runtime updateGuardRuntime) error {
	if h == nil || h.stateStore == nil {
		return fmt.Errorf("update guard state persistence is unavailable")
	}
	session, err := h.stateStore.loadUpdateSession(ctx)
	if err != nil || session == nil {
		return err
	}
	active := isActiveUpdatePhase(session.Phase)
	now := runtime.now()
	if active && now.Unix() <= session.DeadlineUnix {
		return errUpdateInProgress
	}
	quiescent, err := runtime.quiescent(ctx, *session)
	if err != nil {
		return err
	}
	if !quiescent {
		return errUpdateInProgress
	}
	if !active {
		if session.Phase == updatePhaseTerminalFailure {
			if _, backupErr := os.Lstat(session.BackupPath); backupErr == nil {
				return errUpdateRecoveryRequired
			} else if !errors.Is(backupErr, os.ErrNotExist) {
				return backupErr
			}
		}
		deleteTokenErr := h.stateStore.deleteUpdateHealthToken(ctx, session.AttemptID)
		removeCandidateErr := removeUpdateCandidate(*session)
		removeBackupErr := removeUpdateBackup(*session)
		h.inferenceAdmission.Resume(session.AttemptID)
		h.inferenceAdmission.Resume(updateSessionStateKey)
		return errors.Join(deleteTokenErr, removeCandidateErr, removeBackupErr)
	}
	resultPhase := updatePhaseTerminalFailure
	resultCode := "interrupted"
	currentPath, currentErr := runtime.currentExecutable()
	var currentIdentity updateArtifactIdentity
	if currentErr == nil {
		currentIdentity, currentErr = runtime.identify(currentPath, buildinfo.Version, buildinfo.Commit)
	}
	if session.SchemaVersion == 1 {
		resultCode = "legacy_recovery_unverified"
		if strings.EqualFold(normalizeUpdateHealthVersion(buildinfo.Version), normalizeUpdateHealthVersion(session.PreviousVersion)) &&
			(currentPath == session.ExecutablePath || currentPath == session.BackupPath) &&
			session.BackupPath == session.ExecutablePath+".bak" && updatePhaseMayHaveInstalledCandidate(session.Phase) {
			if _, backupErr := os.Lstat(session.BackupPath); backupErr == nil {
				if restoreErr := runtime.restore(*session); restoreErr != nil {
					return fmt.Errorf("restore legacy interrupted update: %w", restoreErr)
				}
				resultPhase = updatePhaseRolledBack
			} else if !errors.Is(backupErr, os.ErrNotExist) {
				return backupErr
			}
		}
	} else if currentErr == nil && currentIdentity == session.TargetIdentity &&
		updatePhaseMayHaveInstalledCandidate(session.Phase) {
		resultPhase = updatePhaseSucceeded
		resultCode = ""
	} else if currentErr == nil && currentIdentity == session.PreviousIdentity {
		if updatePhaseMayHaveInstalledCandidate(session.Phase) &&
			session.CandidatePath == updateCandidatePath(session.ExecutablePath, session.AttemptID) {
			_, executableErr := os.Stat(session.ExecutablePath)
			_, candidateErr := os.Stat(session.CandidatePath)
			_, backupErr := os.Stat(session.BackupPath)
			if backupErr == nil && (errors.Is(executableErr, os.ErrNotExist) || errors.Is(
				candidateErr, os.ErrNotExist,
			)) {
				if restoreErr := runtime.restore(*session); restoreErr != nil {
					return fmt.Errorf("restore interrupted update: %w", restoreErr)
				}
			}
		}
		resultPhase = updatePhaseRolledBack
	}
	updated, err := h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		session.Phase,
		func(next *updateGuardSession) {
			next.Phase = resultPhase
			next.ResultCode = resultCode
			next.UpdatedAt = now.UTC()
		},
	)
	if err != nil {
		return err
	}
	if resultPhase == updatePhaseSucceeded || resultPhase == updatePhaseRolledBack {
		h.inferenceAdmission.Resume(session.AttemptID)
		h.inferenceAdmission.Resume(updateSessionStateKey)
	} else {
		h.inferenceAdmission.Hold(session.AttemptID)
	}
	deleteTokenErr := h.stateStore.deleteUpdateHealthToken(ctx, session.AttemptID)
	removeCandidateErr := removeUpdateCandidate(*updated)
	if resultPhase == updatePhaseSucceeded {
		clearFailureErr := h.stateStore.clearUpdateFailure(ctx, session.TargetVersion)
		return errors.Join(deleteTokenErr, removeCandidateErr, clearFailureErr)
	}
	failure, failureErr := h.stateStore.recordUpdateFailure(
		ctx,
		session.TargetVersion,
		resultCode,
		now,
		updateFailureBaseDelay,
		updateFailureMaxDelay,
	)
	if failureErr != nil {
		return errors.Join(failureErr, deleteTokenErr, removeCandidateErr)
	}
	_, err = h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		resultPhase,
		func(next *updateGuardSession) {
			next.FailureCount = failure.FailureCount
			next.NextRetryAt = &failure.NextRetryAt
			next.UpdatedAt = now.UTC()
		},
	)
	return errors.Join(err, deleteTokenErr, removeCandidateErr)
}

func writeUpdateCandidate(session updateGuardSession, data []byte) error {
	path := updateCandidatePath(session.ExecutablePath, session.AttemptID)
	if path == "" || path != session.CandidatePath {
		return fmt.Errorf("invalid update candidate path")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("create update candidate: %w", err)
	}
	removeOnFailure := true
	defer func() {
		_ = file.Close()
		if removeOnFailure {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("write update candidate: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync update candidate: %w", err)
	}
	if err = file.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod update candidate: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close update candidate: %w", err)
	}
	if err = syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	removeOnFailure = false
	return nil
}

func armUpdateGuardSession(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	now time.Time,
) (updateGuardSession, error) {
	tokenValue, err := generateUpdateHealthToken()
	if err != nil {
		return updateGuardSession{}, err
	}
	session.Phase = updatePhaseLaunching
	session.Percent = 95
	session.UpdatedAt = now.UTC()
	session.DrainDeadline = now.Add(updateGuardDrainTimeout).UTC()
	session.Deadline = session.DrainDeadline.Add(updateGuardHealthTimeout).UTC()
	session.DeadlineUnix = session.Deadline.Unix()
	token := updateHealthTokenState{
		SchemaVersion: updateGuardStateSchemaVersion,
		AttemptID:     session.AttemptID,
		Token:         tokenValue,
		ExpiresAt:     session.Deadline,
		ExpiresUnix:   session.DeadlineUnix,
	}
	if err = store.armUpdateGuard(ctx, session, token); err != nil {
		return session, err
	}
	return session, nil
}

func isUpdateGuardClaimedPhase(phase string) bool {
	switch phase {
	case updatePhaseClaimed,
		updatePhaseDraining,
		updatePhaseReady,
		updatePhaseInstalling,
		updatePhaseRestarting,
		updatePhaseHealthCheck,
		updatePhaseRollingBack,
		updatePhaseRollbackRestarting,
		updatePhaseRollbackHealthCheck,
		updatePhaseSucceeded,
		updatePhaseRolledBack:
		return true
	default:
		return false
	}
}

func launchUpdateGuard(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	runtime updateGuardRuntime,
) error {
	args := []string{
		"--unit=" + session.GuardUnit,
		"--collect",
		"--no-block",
		"--service-type=exec",
		"--property=KillMode=control-group",
		"--property=RuntimeMaxSec=" + strconv.FormatFloat(updateGuardRuntimeLimit.Seconds(), 'f', 0, 64),
		"--property=TimeoutStopSec=5",
		session.ExecutablePath,
		updateGuardCommandFlag,
		updateGuardStateDBFlag,
		session.StateDBPath,
		updateGuardAttemptFlag,
		session.AttemptID,
	}
	if err := runtime.runCommand(ctx, "systemd-run", args...); err != nil {
		current, loadErr := store.loadUpdateSession(ctx)
		if loadErr == nil && current != nil && current.AttemptID == session.AttemptID {
			if isUpdateGuardClaimedPhase(current.Phase) {
				return nil
			}
			if current.Phase != updatePhaseLaunching {
				return fmt.Errorf("update guard failed before claim")
			}
		}
		return markUpdateGuardLaunchFailed(ctx, store, session, "guard_launch_failed", runtime.now())
	}
	deadline := runtime.now().Add(updateGuardClaimTimeout)
	for {
		current, err := store.loadUpdateSession(ctx)
		if err != nil {
			return err
		}
		if current == nil || current.AttemptID != session.AttemptID {
			return errUpdateSessionChanged
		}
		if current.Phase != updatePhaseLaunching {
			if !isUpdateGuardClaimedPhase(current.Phase) {
				return fmt.Errorf("update guard failed before claim")
			}
			return nil
		}
		if !runtime.now().Before(deadline) {
			return markUpdateGuardLaunchFailed(ctx, store, session, "guard_claim_failed", runtime.now())
		}
		if err = runtime.sleep(ctx, updateGuardClaimPollInterval); err != nil {
			markCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			markErr := markUpdateGuardLaunchFailed(
				markCtx, store, session, "guard_claim_failed", runtime.now(),
			)
			cancel()
			return errors.Join(err, markErr)
		}
	}
}

func markUpdateGuardLaunchFailed(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	resultCode string,
	now time.Time,
) error {
	updated, err := store.transitionUpdateSession(
		ctx,
		session.AttemptID,
		updatePhaseLaunching,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseLaunchFailed
			next.ResultCode = normalizeUpdateResultCode(resultCode)
			next.UpdatedAt = now.UTC()
		},
	)
	if errors.Is(err, errUpdateSessionChanged) {
		current, loadErr := store.loadUpdateSession(ctx)
		if loadErr == nil && current != nil && current.AttemptID == session.AttemptID &&
			isUpdateGuardClaimedPhase(current.Phase) {
			return nil
		}
		return errors.Join(errUpdateSessionChanged, loadErr)
	}
	if err != nil {
		return err
	}
	deleteTokenErr := store.deleteUpdateHealthToken(ctx, session.AttemptID)
	removeCandidateErr := removeUpdateCandidate(*updated)
	return errors.Join(
		fmt.Errorf("%s", normalizeUpdateResultCode(resultCode)),
		deleteTokenErr,
		removeCandidateErr,
	)
}
