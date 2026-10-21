package management

import (
	"context"
	"errors"
	"fmt"
)

func runUpdateGuard(
	ctx context.Context,
	store *managementStateStore,
	attemptID string,
	runtime updateGuardRuntime,
) error {
	session, err := store.loadUpdateSession(ctx)
	if err != nil {
		return err
	}
	if session == nil || session.AttemptID != attemptID || session.Phase != updatePhaseLaunching {
		return errUpdateSessionChanged
	}
	executablePath, err := runtime.currentExecutable()
	if err != nil {
		return err
	}
	if err = validateUpdateGuardSession(*session, store.path, executablePath, true, runtime.now()); err != nil {
		return err
	}
	token, err := store.loadUpdateHealthToken(ctx)
	if err != nil {
		return err
	}
	if token == nil || token.AttemptID != attemptID || !isUpdateHealthToken(token.Token) ||
		token.ExpiresUnix != token.ExpiresAt.Unix() || !runtime.now().Before(token.ExpiresAt) {
		return fmt.Errorf("update health token is unavailable or expired")
	}
	session, err = store.transitionUpdateSession(
		ctx,
		attemptID,
		updatePhaseLaunching,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseClaimed
			next.UpdatedAt = runtime.now().UTC()
		},
	)
	if err != nil {
		return err
	}
	draining, err := store.transitionUpdateSession(ctx, attemptID, updatePhaseClaimed, func(next *updateGuardSession) {
		next.Phase = updatePhaseDraining
		next.Percent = 90
		next.UpdatedAt = runtime.now().UTC()
	})
	if err != nil {
		return finishUpdateGuardFailureCurrent(ctx, store, *session, updatePhaseTerminalFailure, "state_failed", runtime)
	}
	session = draining
	confirmation, drainErr := runtime.drain(ctx, *session, token.Token, "wait")
	if drainErr != nil || confirmation.AttemptID != session.AttemptID ||
		confirmation.InstanceID != session.OriginInstance || !confirmation.Deadline.Equal(session.Deadline) ||
		!runtime.now().Before(confirmation.Deadline) {
		return finishUpdateGuardFailureCurrent(ctx, store, *session, updatePhaseError, "drain_failed", runtime)
	}
	ready, err := store.transitionUpdateSession(ctx, attemptID, updatePhaseDraining, func(next *updateGuardSession) {
		next.Phase = updatePhaseReady
		next.DrainConfirmation = &confirmation
		next.Percent = 95
		next.UpdatedAt = runtime.now().UTC()
		next.Deadline = runtime.now().Add(updateGuardHealthTimeout).UTC()
		next.DeadlineUnix = next.Deadline.Unix()
	})
	if err != nil {
		return finishUpdateGuardFailureCurrent(ctx, store, *session, updatePhaseTerminalFailure, "state_failed", runtime)
	}
	session = ready
	installing, err := store.transitionUpdateSession(
		ctx,
		attemptID,
		updatePhaseReady,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseInstalling
			next.UpdatedAt = runtime.now().UTC()
		},
	)
	if err != nil {
		return finishUpdateGuardFailure(
			ctx, store, *session, updatePhaseReady, updatePhaseTerminalFailure, "state_failed", runtime,
		)
	}
	session = installing
	confirmed, err := runtime.drain(ctx, *session, token.Token, "confirm")
	if err != nil || session.DrainConfirmation == nil || confirmed != *session.DrainConfirmation ||
		!runtime.now().Before(confirmed.Deadline) {
		return finishUpdateGuardFailureCurrent(ctx, store, *session, updatePhaseError, "drain_failed", runtime)
	}
	if err = runtime.install(*session); err != nil {
		return finishUpdateGuardFailure(
			ctx, store, *session, updatePhaseInstalling, updatePhaseTerminalFailure, "install_failed", runtime,
		)
	}
	restarting, err := store.transitionUpdateSession(
		ctx,
		attemptID,
		updatePhaseInstalling,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseRestarting
			next.UpdatedAt = runtime.now().UTC()
		},
	)
	if err != nil {
		return rollbackUpdateGuard(ctx, store, *session, updatePhaseInstalling, "state_failed", token.Token, runtime)
	}
	session = restarting
	if err = runtime.runCommand(ctx, "systemctl", "restart", "--no-block", session.ServiceUnit); err != nil {
		return rollbackUpdateGuard(ctx, store, *session, updatePhaseRestarting, "restart_failed", token.Token, runtime)
	}
	checking, err := store.transitionUpdateSession(
		ctx,
		attemptID,
		updatePhaseRestarting,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseHealthCheck
			next.UpdatedAt = runtime.now().UTC()
		},
	)
	if err != nil {
		return rollbackUpdateGuard(ctx, store, *session, updatePhaseRestarting, "state_failed", token.Token, runtime)
	}
	session = checking
	if err = verifyUpdateGuardHealth(ctx, *session, token.Token, session.TargetVersion, runtime); err != nil {
		return rollbackUpdateGuard(ctx, store, *session, updatePhaseHealthCheck, "health_failed", token.Token, runtime)
	}
	now := runtime.now()
	_, err = store.transitionUpdateSession(
		ctx,
		attemptID,
		updatePhaseHealthCheck,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseSucceeded
			next.ResultCode = ""
			next.Percent = 100
			next.UpdatedAt = now.UTC()
		},
	)
	if err != nil {
		return err
	}
	clearFailureErr := store.clearUpdateFailure(ctx, session.TargetVersion)
	deleteTokenErr := store.deleteUpdateHealthToken(ctx, attemptID)
	return errors.Join(clearFailureErr, deleteTokenErr)
}

func rollbackUpdateGuard(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	fromPhase string,
	resultCode string,
	token string,
	runtime updateGuardRuntime,
) error {
	now := runtime.now()
	rollbackDeadline := now.Add(updateGuardHealthTimeout).UTC()
	currentPhase := fromPhase
	operationErrors := make([]error, 0, 3)
	rollingBack, err := store.transitionUpdateSession(
		ctx,
		session.AttemptID,
		fromPhase,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseRollingBack
			next.ResultCode = normalizeUpdateResultCode(resultCode)
			next.UpdatedAt = now.UTC()
			next.Deadline = rollbackDeadline
			next.DeadlineUnix = rollbackDeadline.Unix()
		},
	)
	if err == nil {
		session = *rollingBack
		currentPhase = updatePhaseRollingBack
	} else {
		operationErrors = append(operationErrors, err)
		if current, loadErr := store.loadUpdateSession(ctx); loadErr == nil && current != nil &&
			current.AttemptID == session.AttemptID && isActiveUpdatePhase(current.Phase) {
			session = *current
			currentPhase = current.Phase
		} else if loadErr != nil {
			operationErrors = append(operationErrors, loadErr)
		}
	}
	session.Deadline = rollbackDeadline
	session.DeadlineUnix = rollbackDeadline.Unix()
	if err = store.extendUpdateHealthToken(ctx, session.AttemptID, rollbackDeadline); err != nil {
		operationErrors = append(operationErrors, err)
	}
	if err = runtime.restore(session); err != nil {
		finishErr := finishUpdateGuardFailureCurrent(
			ctx, store, session, updatePhaseTerminalFailure, "rollback_failed", runtime,
		)
		return errors.Join(append(operationErrors, err, finishErr)...)
	}

	restarting, transitionErr := store.transitionUpdateSession(
		ctx,
		session.AttemptID,
		currentPhase,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseRollbackRestarting
			next.UpdatedAt = runtime.now().UTC()
			next.Deadline = rollbackDeadline
			next.DeadlineUnix = rollbackDeadline.Unix()
		},
	)
	if transitionErr == nil {
		session = *restarting
		currentPhase = updatePhaseRollbackRestarting
	} else {
		operationErrors = append(operationErrors, transitionErr)
	}
	if err = runtime.runCommand(ctx, "systemctl", "restart", "--no-block", session.ServiceUnit); err != nil {
		finishErr := finishUpdateGuardFailureCurrent(
			ctx, store, session, updatePhaseTerminalFailure, "rollback_failed", runtime,
		)
		return errors.Join(append(operationErrors, err, finishErr)...)
	}

	checking, transitionErr := store.transitionUpdateSession(
		ctx,
		session.AttemptID,
		currentPhase,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseRollbackHealthCheck
			next.UpdatedAt = runtime.now().UTC()
			next.Deadline = rollbackDeadline
			next.DeadlineUnix = rollbackDeadline.Unix()
		},
	)
	if transitionErr == nil {
		session = *checking
	} else {
		operationErrors = append(operationErrors, transitionErr)
	}
	if err = verifyUpdateGuardHealth(ctx, session, token, session.PreviousVersion, runtime); err != nil {
		finishErr := finishUpdateGuardFailureCurrent(
			ctx, store, session, updatePhaseTerminalFailure, "rollback_health_failed", runtime,
		)
		return errors.Join(append(operationErrors, err, finishErr)...)
	}
	finishErr := finishUpdateGuardFailureCurrent(
		ctx, store, session, updatePhaseRolledBack, resultCode, runtime,
	)
	return errors.Join(append(operationErrors, finishErr)...)
}

func finishUpdateGuardFailureCurrent(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	finalPhase string,
	resultCode string,
	runtime updateGuardRuntime,
) error {
	current, err := store.loadUpdateSession(ctx)
	if err != nil {
		return err
	}
	if current == nil || current.AttemptID != session.AttemptID || !isActiveUpdatePhase(current.Phase) {
		return errUpdateSessionChanged
	}
	return finishUpdateGuardFailure(ctx, store, *current, current.Phase, finalPhase, resultCode, runtime)
}

func finishUpdateGuardFailure(
	ctx context.Context,
	store *managementStateStore,
	session updateGuardSession,
	fromPhase string,
	finalPhase string,
	resultCode string,
	runtime updateGuardRuntime,
) error {
	var resumeErr error
	if !updatePhaseMayHaveInstalledCandidate(fromPhase) || fromPhase == updatePhaseInstalling {
		if token, loadErr := store.loadUpdateHealthToken(ctx); loadErr != nil {
			resumeErr = loadErr
		} else if token != nil && token.AttemptID == session.AttemptID {
			_, resumeErr = runtime.drain(ctx, session, token.Token, "resume")
		}
	}
	now := runtime.now()
	failure, failureErr := store.recordUpdateFailure(
		ctx,
		session.TargetVersion,
		resultCode,
		now,
		updateFailureBaseDelay,
		updateFailureMaxDelay,
	)
	if failureErr != nil {
		return failureErr
	}
	_, err := store.transitionUpdateSession(
		ctx,
		session.AttemptID,
		fromPhase,
		func(next *updateGuardSession) {
			next.Phase = finalPhase
			next.ResultCode = normalizeUpdateResultCode(resultCode)
			next.FailureCount = failure.FailureCount
			next.NextRetryAt = &failure.NextRetryAt
			next.UpdatedAt = now.UTC()
		},
	)
	if err != nil {
		return err
	}
	deleteTokenErr := store.deleteUpdateHealthToken(ctx, session.AttemptID)
	removeCandidateErr := removeUpdateCandidate(session)
	return errors.Join(
		fmt.Errorf("%s", normalizeUpdateResultCode(resultCode)),
		resumeErr,
		deleteTokenErr,
		removeCandidateErr,
	)
}
