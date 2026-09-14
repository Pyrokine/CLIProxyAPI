package management

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
)

func (h *Handler) reconcileStaleUpdateSession(ctx context.Context, runtime updateGuardRuntime) error {
	if h == nil || h.stateStore == nil {
		return fmt.Errorf("update guard state persistence is unavailable")
	}
	session, err := h.stateStore.loadUpdateSession(ctx)
	if err != nil || session == nil {
		return err
	}
	if !isActiveUpdatePhase(session.Phase) {
		deleteTokenErr := h.stateStore.deleteUpdateHealthToken(ctx, session.AttemptID)
		removeCandidateErr := removeUpdateCandidate(*session)
		return errors.Join(deleteTokenErr, removeCandidateErr)
	}
	now := runtime.now()
	if now.Unix() <= session.DeadlineUnix {
		return errUpdateInProgress
	}
	if session.GuardUnit != "" && runtime.runCommand(
		ctx, "systemctl", "is-active", "--quiet", session.GuardUnit,
	) == nil {
		return errUpdateInProgress
	}
	resultPhase := updatePhaseTerminalFailure
	resultCode := "interrupted"
	if strings.EqualFold(strings.TrimSpace(buildinfo.Version), session.TargetVersion) {
		resultPhase = updatePhaseSucceeded
		resultCode = ""
	} else if strings.EqualFold(strings.TrimSpace(buildinfo.Version), session.PreviousVersion) {
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
	session.Deadline = now.Add(updateGuardHealthTimeout).UTC()
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
		"--property=KillMode=process",
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
