package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

const (
	updateGuardStateSchemaVersion = 2
	updateSessionStateKey         = "update_session"
	updateHealthTokenStateKey     = "update_health_token"
	maxUpdateGuardStateBytes      = 64 * 1024

	updatePhaseDownloading         = "downloading"
	updatePhaseVerifying           = "verifying"
	updatePhasePreparing           = "preparing"
	updatePhaseLaunching           = "launching"
	updatePhaseClaimed             = "claimed"
	updatePhaseDraining            = "draining"
	updatePhaseReady               = "ready"
	updatePhaseInstalling          = "installing"
	updatePhaseRestarting          = "restarting"
	updatePhaseHealthCheck         = "health_check"
	updatePhaseRollingBack         = "rolling_back"
	updatePhaseRollbackRestarting  = "rollback_restarting"
	updatePhaseRollbackHealthCheck = "rollback_health_check"
	updatePhaseSucceeded           = "succeeded"
	updatePhaseRolledBack          = "rolled_back"
	updatePhaseError               = "error"
	updatePhaseLaunchFailed        = "launch_failed"
	updatePhaseTerminalFailure     = "terminal_failure"
)

var (
	errUpdateInProgress       = errors.New("update is already in progress")
	errUpdateSessionChanged   = errors.New("update session changed")
	errUpdateRecoveryRequired = errors.New("previous update recovery requires manual verification")
)

type updateGuardSession struct {
	SchemaVersion     int                                  `json:"schema_version"`
	AttemptID         string                               `json:"attempt_id"`
	TargetVersion     string                               `json:"target_version"`
	PreviousVersion   string                               `json:"previous_version"`
	OriginInstance    string                               `json:"origin_instance,omitempty"`
	DrainDeadline     time.Time                            `json:"drain_deadline,omitempty"`
	DrainConfirmation *executionregistry.DrainConfirmation `json:"drain_confirmation,omitempty"`
	TargetIdentity    updateArtifactIdentity               `json:"target_identity,omitempty"`
	PreviousIdentity  updateArtifactIdentity               `json:"previous_identity,omitempty"`
	Automatic         bool                                 `json:"automatic"`
	Phase             string                               `json:"phase"`
	ResultCode        string                               `json:"result_code,omitempty"`
	Percent           int                                  `json:"percent"`
	CandidatePath     string                               `json:"candidate_path,omitempty"`
	ExecutablePath    string                               `json:"executable_path"`
	BackupPath        string                               `json:"backup_path"`
	StateDBPath       string                               `json:"state_db_path"`
	ServiceUnit       string                               `json:"service_unit"`
	GuardUnit         string                               `json:"guard_unit"`
	HealthBaseURL     string                               `json:"health_base_url"`
	HealthTLSCertPath string                               `json:"health_tls_cert_path,omitempty"`
	StartedAt         time.Time                            `json:"started_at"`
	UpdatedAt         time.Time                            `json:"updated_at"`
	Deadline          time.Time                            `json:"deadline"`
	DeadlineUnix      int64                                `json:"deadline_unix"`
	FailureCount      int                                  `json:"failure_count,omitempty"`
	NextRetryAt       *time.Time                           `json:"next_retry_at,omitempty"`
}

type updateHealthTokenState struct {
	SchemaVersion int       `json:"schema_version"`
	AttemptID     string    `json:"attempt_id"`
	Token         string    `json:"token"`
	ExpiresAt     time.Time `json:"expires_at"`
	ExpiresUnix   int64     `json:"expires_unix"`
}

type updateFailureState struct {
	SchemaVersion int       `json:"schema_version"`
	TargetVersion string    `json:"target_version"`
	FailureCount  int       `json:"failure_count"`
	ResultCode    string    `json:"result_code"`
	LastFailureAt time.Time `json:"last_failure_at"`
	NextRetryAt   time.Time `json:"next_retry_at"`
}

func isActiveUpdatePhase(phase string) bool {
	switch phase {
	case updatePhaseDownloading,
		updatePhaseVerifying,
		updatePhasePreparing,
		updatePhaseLaunching,
		updatePhaseClaimed,
		updatePhaseDraining,
		updatePhaseReady,
		updatePhaseInstalling,
		updatePhaseRestarting,
		updatePhaseHealthCheck,
		updatePhaseRollingBack,
		updatePhaseRollbackRestarting,
		updatePhaseRollbackHealthCheck:
		return true
	default:
		return false
	}
}

func encodeUpdateState(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxUpdateGuardStateBytes {
		return nil, fmt.Errorf("encoded update state is %d bytes, maximum is %d", len(raw), maxUpdateGuardStateBytes)
	}
	return raw, nil
}

func (s *managementStateStore) loadUpdateSession(ctx context.Context) (*updateGuardSession, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("management state: update session unavailable")
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key=?", updateSessionStateKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("management state: load update session: %w", err)
	}
	if len(raw) > maxUpdateGuardStateBytes {
		return nil, fmt.Errorf("management state: update session exceeds maximum size")
	}
	var session updateGuardSession
	if err = json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("management state: decode update session: %w", err)
	}
	if session.SchemaVersion != 1 && session.SchemaVersion != updateGuardStateSchemaVersion {
		return nil, fmt.Errorf("management state: unsupported update session schema version %d", session.SchemaVersion)
	}
	return &session, nil
}

func (s *managementStateStore) beginUpdateSession(ctx context.Context, session updateGuardSession) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("management state: update session unavailable")
	}
	raw, err := encodeUpdateState(session)
	if err != nil {
		return fmt.Errorf("management state: encode update session: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("management state: begin update session transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
		 WHERE json_extract(state_items.value, '$.attempt_id') != json_extract(excluded.value, '$.attempt_id')
		 AND COALESCE(json_extract(state_items.value, '$.phase'), '') NOT IN (
			'downloading', 'verifying', 'preparing', 'launching', 'claimed', 'draining', 'ready', 'installing',
			'restarting', 'health_check', 'rolling_back', 'rollback_restarting', 'rollback_health_check'
		 )`,
		updateSessionStateKey,
		raw,
		session.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("management state: begin update session: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("management state: inspect update session insert: %w", err)
	}
	if updated != 1 {
		return errUpdateInProgress
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM state_items WHERE key=?", updateHealthTokenStateKey); err != nil {
		return fmt.Errorf("management state: clear stale update health token: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("management state: commit update session: %w", err)
	}
	return nil
}

func (s *managementStateStore) transitionUpdateSession(
	ctx context.Context,
	attemptID string,
	fromPhase string,
	mutate func(*updateGuardSession),
) (*updateGuardSession, error) {
	session, err := s.loadUpdateSession(ctx)
	if err != nil {
		return nil, err
	}
	if session == nil || session.AttemptID != attemptID || session.Phase != fromPhase {
		return nil, errUpdateSessionChanged
	}
	if mutate != nil {
		mutate(session)
	}
	raw, err := encodeUpdateState(session)
	if err != nil {
		return nil, fmt.Errorf("management state: encode update session transition: %w", err)
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE state_items SET value=?, updated_at=?
		 WHERE key=?
		   AND json_extract(value, '$.attempt_id')=?
		   AND json_extract(value, '$.phase')=?`,
		raw,
		session.UpdatedAt.UTC().Format(time.RFC3339Nano),
		updateSessionStateKey,
		attemptID,
		fromPhase,
	)
	if err != nil {
		return nil, fmt.Errorf("management state: transition update session: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("management state: inspect update session transition: %w", err)
	}
	if updated != 1 {
		return nil, errUpdateSessionChanged
	}
	return session, nil
}

func (s *managementStateStore) armUpdateGuard(
	ctx context.Context,
	session updateGuardSession,
	token updateHealthTokenState,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("management state: update guard unavailable")
	}
	sessionRaw, err := encodeUpdateState(session)
	if err != nil {
		return fmt.Errorf("management state: encode armed update session: %w", err)
	}
	tokenRaw, err := encodeUpdateState(token)
	if err != nil {
		return fmt.Errorf("management state: encode update health token: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("management state: begin update guard transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(
		ctx,
		`UPDATE state_items SET value=?, updated_at=?
		 WHERE key=?
		   AND json_extract(value, '$.attempt_id')=?
		   AND json_extract(value, '$.phase')=?`,
		sessionRaw,
		session.UpdatedAt.UTC().Format(time.RFC3339Nano),
		updateSessionStateKey,
		session.AttemptID,
		updatePhasePreparing,
	)
	if err != nil {
		return fmt.Errorf("management state: arm update session: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("management state: inspect armed update session: %w", err)
	}
	if updated != 1 {
		return errUpdateSessionChanged
	}
	if _, err = tx.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		updateHealthTokenStateKey,
		tokenRaw,
		session.UpdatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("management state: save update health token: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("management state: commit update guard: %w", err)
	}
	return nil
}

func (s *managementStateStore) loadUpdateHealthToken(ctx context.Context) (*updateHealthTokenState, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("management state: update health token unavailable")
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key=?", updateHealthTokenStateKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("management state: load update health token: %w", err)
	}
	if len(raw) > maxUpdateGuardStateBytes {
		return nil, fmt.Errorf("management state: update health token exceeds maximum size")
	}
	var token updateHealthTokenState
	if err = json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("management state: decode update health token: %w", err)
	}
	if token.SchemaVersion != 1 && token.SchemaVersion != updateGuardStateSchemaVersion {
		return nil, fmt.Errorf(
			"management state: unsupported update health token schema version %d", token.SchemaVersion,
		)
	}
	return &token, nil
}

func (s *managementStateStore) extendUpdateHealthToken(
	ctx context.Context,
	attemptID string,
	expiresAt time.Time,
) error {
	token, err := s.loadUpdateHealthToken(ctx)
	if err != nil {
		return err
	}
	if token == nil || token.AttemptID != attemptID {
		return errUpdateSessionChanged
	}
	token.ExpiresAt = expiresAt.UTC()
	token.ExpiresUnix = token.ExpiresAt.Unix()
	raw, err := encodeUpdateState(token)
	if err != nil {
		return fmt.Errorf("management state: encode extended update health token: %w", err)
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE state_items SET value=?, updated_at=?
		 WHERE key=? AND json_extract(value, '$.attempt_id')=?`,
		raw,
		token.ExpiresAt.Format(time.RFC3339Nano),
		updateHealthTokenStateKey,
		attemptID,
	)
	if err != nil {
		return fmt.Errorf("management state: extend update health token: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("management state: inspect update health token extension: %w", err)
	}
	if updated != 1 {
		return errUpdateSessionChanged
	}
	return nil
}

func (s *managementStateStore) deleteUpdateHealthToken(ctx context.Context, attemptID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM state_items
		 WHERE key=? AND json_extract(value, '$.attempt_id')=?`,
		updateHealthTokenStateKey,
		attemptID,
	); err != nil {
		return fmt.Errorf("management state: delete update health token: %w", err)
	}
	return nil
}

func updateFailureStateKey(version string) string {
	return "update_failure:" + strings.ToLower(strings.TrimSpace(version))
}

func (s *managementStateStore) loadUpdateFailure(ctx context.Context, version string) (*updateFailureState, error) {
	if s == nil || s.db == nil || !validVersion.MatchString(strings.TrimSpace(version)) {
		return nil, nil
	}
	var raw []byte
	err := s.db.QueryRowContext(
		ctx, "SELECT value FROM state_items WHERE key=?", updateFailureStateKey(version),
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("management state: load update failure: %w", err)
	}
	if len(raw) > maxUpdateGuardStateBytes {
		return nil, fmt.Errorf("management state: update failure exceeds maximum size")
	}
	var failure updateFailureState
	if err = json.Unmarshal(raw, &failure); err != nil {
		return nil, fmt.Errorf("management state: decode update failure: %w", err)
	}
	if failure.SchemaVersion != 1 && failure.SchemaVersion != updateGuardStateSchemaVersion {
		return nil, fmt.Errorf("management state: unsupported update failure schema version %d", failure.SchemaVersion)
	}
	return &failure, nil
}

func (s *managementStateStore) recordUpdateFailure(
	ctx context.Context,
	version string,
	resultCode string,
	now time.Time,
	baseDelay time.Duration,
	maxDelay time.Duration,
) (*updateFailureState, error) {
	version = strings.TrimSpace(version)
	if s == nil || s.db == nil || !validVersion.MatchString(version) {
		return nil, fmt.Errorf("management state: invalid update failure version")
	}
	failure, err := s.loadUpdateFailure(ctx, version)
	if err != nil {
		return nil, err
	}
	if failure == nil {
		failure = &updateFailureState{
			SchemaVersion: updateGuardStateSchemaVersion,
			TargetVersion: version,
		}
	}
	failure.FailureCount++
	failure.ResultCode = normalizeUpdateResultCode(resultCode)
	failure.LastFailureAt = now.UTC()
	delay := baseDelay
	for count := 1; count < failure.FailureCount && delay < maxDelay; count++ {
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
	failure.NextRetryAt = now.Add(delay).UTC()
	raw, err := encodeUpdateState(failure)
	if err != nil {
		return nil, fmt.Errorf("management state: encode update failure: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		updateFailureStateKey(version),
		raw,
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("management state: save update failure: %w", err)
	}
	return failure, nil
}

func (s *managementStateStore) clearUpdateFailure(ctx context.Context, version string) error {
	if s == nil || s.db == nil || !validVersion.MatchString(strings.TrimSpace(version)) {
		return nil
	}
	if _, err := s.db.ExecContext(
		ctx, "DELETE FROM state_items WHERE key=?", updateFailureStateKey(version),
	); err != nil {
		return fmt.Errorf("management state: clear update failure: %w", err)
	}
	return nil
}

func normalizeUpdateResultCode(code string) string {
	switch strings.TrimSpace(code) {
	case "download_failed",
		"checksum_failed",
		"extract_failed",
		"candidate_failed",
		"state_failed",
		"guard_launch_failed",
		"guard_claim_failed",
		"drain_failed",
		"identity_failed",
		"legacy_recovery_unverified",
		"install_failed",
		"restart_failed",
		"health_failed",
		"rollback_failed",
		"rollback_health_failed",
		"interrupted":
		return strings.TrimSpace(code)
	default:
		return "update_failed"
	}
}
