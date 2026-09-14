package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
)

const (
	requestLogIDSQLExpression = "CASE WHEN json_valid(events.metadata) THEN json_extract(events.metadata, '$.request_id') END"
	requestLogArtifactJoinSQL = " LEFT JOIN request_log_artifact AS request_log ON request_log.request_id = " + requestLogIDSQLExpression
	requestLogArtifactColumns = ", request_log.state, request_log.location, request_log.file_name, request_log.home_ip, request_log.reason, request_log.revision, request_log.updated_at_ns "

	upsertRequestLogArtifactSQL = `
INSERT INTO request_log_artifact (
    request_id, state, location, file_name, home_ip, reason, revision, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
    state         = excluded.state,
    location      = excluded.location,
    file_name     = excluded.file_name,
    home_ip       = excluded.home_ip,
    reason        = excluded.reason,
    revision      = excluded.revision,
    updated_at_ns = excluded.updated_at_ns
WHERE excluded.revision > request_log_artifact.revision
`
)

func migrateV4ToV5(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(
		ctx, `
CREATE TABLE IF NOT EXISTS request_log_artifact (
    request_id TEXT PRIMARY KEY
        CHECK (request_id = trim(request_id))
        CHECK (length(request_id) > 0 AND length(request_id) <= 512),
    state TEXT NOT NULL
        CHECK (state IN ('pending', 'available', 'disabled', 'not_captured', 'publish_failed', 'expired', 'legacy_unknown')),
    location TEXT NOT NULL DEFAULT '' CHECK (location IN ('', 'local', 'home')),
    file_name TEXT NOT NULL DEFAULT '' CHECK (file_name = trim(file_name)),
    home_ip TEXT NOT NULL DEFAULT '' CHECK (home_ip = trim(home_ip)),
    reason TEXT NOT NULL DEFAULT '' CHECK (reason = trim(reason)),
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0),
    updated_at_ns INTEGER NOT NULL CHECK (typeof(updated_at_ns) = 'integer'),
    CHECK (state <> 'available' OR location <> ''),
    CHECK (state <> 'available' OR location <> 'local' OR length(file_name) > 0)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_request_log_artifact_state_updated
    ON request_log_artifact(state, updated_at_ns);
CREATE INDEX IF NOT EXISTS idx_request_log_artifact_updated
    ON request_log_artifact(updated_at_ns);
CREATE INDEX IF NOT EXISTS idx_events_request_id ON events(
    CASE WHEN json_valid(metadata) THEN json_extract(metadata, '$.request_id') END
);`,
	); err != nil {
		return fmt.Errorf("create request log artifact schema: %w", err)
	}
	if _, err = tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '5' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	return nil
}

func persistRequestLogArtifact(
	ctx context.Context,
	exec dbExec,
	artifact logging.RequestLogArtifact,
) error {
	if exec == nil {
		return errors.New("usage: request log artifact database is unavailable")
	}
	artifact = logging.PrepareRequestLogArtifact(artifact)
	if err := logging.ValidateRequestLogArtifact(artifact); err != nil {
		return fmt.Errorf("usage: invalid request log artifact: %w", err)
	}
	if _, err := exec.ExecContext(
		ctx,
		upsertRequestLogArtifactSQL,
		artifact.RequestID,
		artifact.State,
		artifact.Location,
		artifact.FileName,
		artifact.HomeIP,
		artifact.Reason,
		artifact.Revision,
		artifact.UpdatedAt.UnixNano(),
	); err != nil {
		return fmt.Errorf("usage: upsert request log artifact: %w", err)
	}
	logging.AdvanceRequestLogArtifactRevision(artifact.Revision)
	return nil
}

func requestLogArtifactFromDetail(detail *FlatDetail) (logging.RequestLogArtifact, bool) {
	if detail == nil || strings.TrimSpace(detail.RequestID) == "" || detail.RequestLogState == "" ||
		detail.RequestLogRevision <= 0 || detail.RequestLogUpdatedAt.IsZero() {
		return logging.RequestLogArtifact{}, false
	}
	return logging.RequestLogArtifact{
		RequestID: strings.TrimSpace(detail.RequestID),
		State:     detail.RequestLogState,
		Location:  detail.RequestLogLocation,
		FileName:  detail.RequestLogFileName,
		HomeIP:    detail.RequestLogHomeIP,
		Reason:    detail.RequestLogReason,
		Revision:  detail.RequestLogRevision,
		UpdatedAt: detail.RequestLogUpdatedAt,
	}, true
}

func scanRequestLogArtifact(
	state sql.NullString,
	location sql.NullString,
	fileName sql.NullString,
	homeIP sql.NullString,
	reason sql.NullString,
	revision sql.NullInt64,
	updatedAtNs sql.NullInt64,
) (logging.RequestLogArtifact, bool, error) {
	if !state.Valid {
		return logging.RequestLogArtifact{}, false, nil
	}
	artifact := logging.RequestLogArtifact{
		State:    logging.RequestLogState(state.String),
		Location: logging.RequestLogLocation(location.String),
		FileName: fileName.String,
		HomeIP:   homeIP.String,
		Reason:   reason.String,
	}
	if revision.Valid {
		artifact.Revision = revision.Int64
	}
	if updatedAtNs.Valid {
		artifact.UpdatedAt = time.Unix(0, updatedAtNs.Int64).UTC()
	}
	return artifact, true, nil
}

func applyRequestLogArtifactToDetail(detail *FlatDetail, artifact logging.RequestLogArtifact, found bool) {
	if detail == nil {
		return
	}
	if !found {
		if strings.TrimSpace(detail.RequestID) != "" {
			detail.RequestLogState = logging.RequestLogLegacyUnknown
		}
		return
	}
	detail.RequestLogState = artifact.State
	detail.RequestLogLocation = artifact.Location
	detail.RequestLogFileName = artifact.FileName
	detail.RequestLogHomeIP = artifact.HomeIP
	detail.RequestLogReason = artifact.Reason
	detail.RequestLogRevision = artifact.Revision
	detail.RequestLogUpdatedAt = artifact.UpdatedAt
}

func (s *Store) queryRequestLogArtifact(
	ctx context.Context,
	requestID string,
) (logging.RequestLogArtifact, bool, error) {
	if s == nil || s.db == nil {
		return logging.RequestLogArtifact{}, false, errors.New("usage: request log artifact store is unavailable")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return logging.RequestLogArtifact{}, false, errors.New("usage: request log artifact request ID is required")
	}
	var (
		state       string
		location    string
		fileName    string
		homeIP      string
		reason      string
		revision    int64
		updatedAtNs int64
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT state, location, file_name, home_ip, reason, revision, updated_at_ns
FROM request_log_artifact
WHERE request_id = ?`,
		requestID,
	).Scan(&state, &location, &fileName, &homeIP, &reason, &revision, &updatedAtNs)
	if errors.Is(err, sql.ErrNoRows) {
		return logging.RequestLogArtifact{}, false, nil
	}
	if err != nil {
		return logging.RequestLogArtifact{}, false, fmt.Errorf("usage: query request log artifact: %w", err)
	}
	artifact := logging.RequestLogArtifact{
		RequestID: requestID,
		State:     logging.RequestLogState(state),
		Location:  logging.RequestLogLocation(location),
		FileName:  fileName,
		HomeIP:    homeIP,
		Reason:    reason,
		Revision:  revision,
		UpdatedAt: time.Unix(0, updatedAtNs).UTC(),
	}
	if err = logging.ValidateRequestLogArtifact(artifact); err != nil {
		return logging.RequestLogArtifact{}, false, fmt.Errorf("usage: stored request log artifact is invalid: %w", err)
	}
	return artifact, true, nil
}

func (s *Store) hasEventRequestID(ctx context.Context, requestID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("usage: request log artifact store is unavailable")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false, nil
	}
	var exists int
	query := "SELECT EXISTS(SELECT 1 FROM events WHERE " + requestLogIDSQLExpression + " = ? LIMIT 1)"
	if err := s.db.QueryRowContext(ctx, query, requestID).Scan(&exists); err != nil {
		return false, fmt.Errorf("usage: query request ID existence: %w", err)
	}
	return exists != 0, nil
}
