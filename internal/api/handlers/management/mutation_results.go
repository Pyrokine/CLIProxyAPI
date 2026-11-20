package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	MutationIDHeader     = "X-CPA-Mutation-ID"
	MutationResultHeader = "X-CPA-Mutation-Result"
)

//goland:noinspection GoUnnecessarilyExportedIdentifiers
type MutationEffect struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

type MutationResult struct {
	MutationID      string           `json:"mutation_id"`
	RequestID       string           `json:"request_id"`
	Outcome         string           `json:"outcome"`
	ConfirmedEffect bool             `json:"confirmed_effect"`
	Effects         []MutationEffect `json:"effects"`
	AuditAvailable  bool             `json:"audit_available"`
}

type mutationContextKey struct{}

type mutationState struct {
	mu             sync.Mutex
	store          *managementStateStore
	record         managementMutationAuditRecord
	id             string
	effects        []MutationEffect
	complete       bool
	rejected       bool
	failed         bool
	finished       bool
	claimed        bool
	contextValid   bool
	lastRecordJSON string
	lastStatus     int
	result         MutationResult
}

func mutationRequestContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return nil
	}
	return c.Request.Context()
}

func mutationFromContext(ctx context.Context) *mutationState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(mutationContextKey{}).(*mutationState)
	return state
}

func validMutationEffect(effect MutationEffect) bool {
	parts := strings.Split(effect.Kind, ".")
	if len(parts) != 2 {
		return false
	}
	if effect.Kind != "quota.consumed" {
		switch parts[0] {
		case "configuration", "credential", "task", "plugin", "model", "usage", "logging", "update":
		default:
			return false
		}
		switch parts[1] {
		case "persisted", "published", "deleted", "created", "queued", "runtime_applied":
		default:
			return false
		}
	}
	return effect.Target == parts[0] ||
		effect.Target != "" && normalizeManagementMutationTarget(effect.Target) == effect.Target
}

// RecordMutationEffect records an effect confirmed by a commit, publication or queue operation.
func RecordMutationEffect(ctx context.Context, kind, target string) bool {
	state := mutationFromContext(ctx)
	if state == nil {
		return false
	}
	effect := MutationEffect{Kind: kind, Target: target}
	if !validMutationEffect(effect) {
		log.Error("invalid management mutation effect")
		return false
	}
	state.mu.Lock()
	if slices.Contains(state.effects, effect) {
		state.mu.Unlock()
		return true
	}
	state.effects = append(state.effects, effect)
	finished, status := state.finished, state.lastStatus
	state.mu.Unlock()
	if finished {
		state.persist(status)
	}
	return true
}

// CompleteMutation confirms that all effects required by this operation have completed.
func CompleteMutation(ctx context.Context) {
	setMutationCompletion(ctx, true, false, false)
}

// rejectMutation records a validation rejection before any effect is committed.
func rejectMutation(ctx context.Context) {
	setMutationCompletion(ctx, false, true, false)
}

// failMutation records a failed stage; confirmed earlier effects remain visible.
func failMutation(ctx context.Context) {
	setMutationCompletion(ctx, false, false, true)
}

func setMutationCompletion(ctx context.Context, complete, rejected, failed bool) {
	state := mutationFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.complete, state.rejected, state.failed = complete, rejected, failed
	finished, status := state.finished, state.lastStatus
	state.mu.Unlock()
	if finished {
		state.persist(status)
	}
}

func (state *mutationState) snapshotLocked() MutationResult {
	result := MutationResult{
		MutationID: state.id, RequestID: state.record.RequestID, Outcome: "uncertain",
		ConfirmedEffect: len(state.effects) > 0, Effects: append([]MutationEffect{}, state.effects...),
		AuditAvailable: state.claimed && state.contextValid,
	}
	if !result.AuditAvailable {
		return result
	}
	switch {
	case state.complete && result.ConfirmedEffect:
		result.Outcome = "succeeded"
	case (state.failed || state.rejected) && result.ConfirmedEffect:
		result.Outcome = "partial"
	case state.rejected:
		result.Outcome = "rejected"
	}
	return result
}

func (state *mutationState) persist(status int) MutationResult {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastStatus = status
	if status >= http.StatusBadRequest && !state.complete {
		state.failed = true
	}
	result := state.snapshotLocked()
	encoded, _ := json.Marshal(result)
	if state.lastRecordJSON == string(encoded) && state.lastStatus == state.record.StatusCode {
		return state.result
	}
	if state.contextValid && state.store != nil {
		record := state.record
		record.OccurredAt = time.Now().UTC()
		record.StatusCode = status
		record.MutationID = state.id
		record.Result = &result
		ctx, cancel := context.WithTimeout(context.Background(), managementMutationAuditWriteTimeout)
		err := state.store.appendManagementMutationAudit(ctx, record)
		cancel()
		if err != nil {
			log.WithError(err).Error("failed to persist management mutation audit")
			result.AuditAvailable = false
			result.Outcome = "uncertain"
		} else {
			state.lastRecordJSON = string(encoded)
			state.record.StatusCode = status
		}
	} else {
		log.Error("management mutation audit unavailable")
		result.AuditAvailable = false
		result.Outcome = "uncertain"
	}
	state.result = result
	return result
}

func (state *mutationState) finish(status int, panicked bool) MutationResult {
	state.mu.Lock()
	state.finished = true
	if (panicked || status >= http.StatusBadRequest) && !state.complete {
		state.failed = true
	}
	state.mu.Unlock()
	return state.persist(status)
}

type mutationResponseWriter struct {
	gin.ResponseWriter
	state   *mutationState
	context *gin.Context
	emitted bool
}

func (writer *mutationResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *mutationResponseWriter) applyResult() {
	if writer.emitted || writer.ResponseWriter.Written() {
		return
	}
	writer.state.mu.Lock()
	writer.state.record.Target = managementMutationTarget(writer.context)
	writer.state.mu.Unlock()
	result := writer.state.persist(writer.ResponseWriter.Status())
	encoded, _ := json.Marshal(result)
	writer.Header().Set(MutationResultHeader, string(encoded))
	writer.emitted = true
}

func (writer *mutationResponseWriter) WriteHeaderNow() {
	writer.applyResult()
	writer.ResponseWriter.WriteHeaderNow()
}

func (writer *mutationResponseWriter) Write(data []byte) (int, error) {
	writer.applyResult()
	return writer.ResponseWriter.Write(data)
}

func (writer *mutationResponseWriter) WriteString(data string) (int, error) {
	writer.applyResult()
	return writer.ResponseWriter.WriteString(data)
}

func (writer *mutationResponseWriter) Flush() {
	writer.applyResult()
	writer.ResponseWriter.Flush()
}

func (h *Handler) mutationResultMiddleware(c *gin.Context) {
	if c == nil || c.Request == nil {
		if c != nil {
			c.Next()
		}
		return
	}
	h.AuditMutation(c, c.FullPath(), c.Next)
}

// AuditMutation 使用不含敏感值的路由模板记录管理操作和已确认效果
func (h *Handler) AuditMutation(c *gin.Context, route string, action func()) {
	if !isManagementMutationMethod(c.Request.Method, route) {
		action()
		return
	}
	record, contextValid := managementMutationAuditRecordFromContext(c, http.StatusOK)
	record.Route = normalizeManagementMutationRoute(route)
	record.Resource = managementMutationResource(record.Route)
	id := uuid.NewString()
	invalidID := false
	if requested := strings.TrimSpace(c.GetHeader(MutationIDHeader)); requested != "" {
		parsed, err := uuid.Parse(requested)
		if err != nil {
			invalidID = true
		} else {
			id = parsed.String()
		}
	}
	if record.RequestID == "" {
		record.RequestID = logging.EnsureGinRequestID(c)
	}
	state := &mutationState{record: record, id: id, contextValid: contextValid}
	if h != nil {
		state.store = h.stateStore
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), mutationContextKey{}, state))
	if state.store != nil && contextValid {
		ctx, cancel := context.WithTimeout(context.Background(), managementMutationAuditWriteTimeout)
		claimed, err := state.store.claimMutation(ctx, id, record.RequestID)
		cancel()
		if err != nil {
			log.WithError(err).Error("failed to reserve management mutation identity")
		} else {
			if !claimed {
				result := MutationResult{
					MutationID: id, RequestID: record.RequestID, Outcome: "rejected", Effects: []MutationEffect{},
					AuditAvailable: true,
				}
				record.MutationID, record.StatusCode = id, http.StatusConflict
				auditCtx, cancelAudit := context.WithTimeout(context.Background(), managementMutationAuditWriteTimeout)
				errAudit := state.store.appendManagementMutationAudit(auditCtx, record)
				cancelAudit()
				if errAudit != nil {
					log.WithError(errAudit).Error("failed to persist management mutation audit")
					result.AuditAvailable, result.Outcome = false, "uncertain"
				}
				encoded, _ := json.Marshal(result)
				c.Header(MutationResultHeader, string(encoded))
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "mutation_id_conflict", "mutation_id": id})
				return
			}
			state.claimed = true
		}
	}
	original := c.Writer
	writer := &mutationResponseWriter{ResponseWriter: original, state: state, context: c}
	c.Writer = writer
	defer func() {
		panicValue := recover()
		status := original.Status()
		if panicValue != nil {
			status = http.StatusInternalServerError
			original.WriteHeader(status)
		}
		state.mu.Lock()
		state.record.Target = managementMutationTarget(c)
		state.mu.Unlock()
		state.finish(status, panicValue != nil)
		writer.applyResult()
		c.Writer = original
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	if invalidID {
		rejectMutation(c.Request.Context())
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_mutation_id"})
		return
	}
	action()
}

func validateMutationResult(result MutationResult) error {
	id, err := uuid.Parse(result.MutationID)
	if err != nil || id.String() != result.MutationID || result.RequestID == "" {
		return errors.New("invalid management mutation identity")
	}
	if result.ConfirmedEffect != (len(result.Effects) > 0) {
		return errors.New("management mutation effect confirmation does not match effects")
	}
	switch result.Outcome {
	case "succeeded", "partial":
		if !result.ConfirmedEffect || !result.AuditAvailable {
			return errors.New("management mutation outcome requires durable confirmed effects")
		}
	case "rejected":
		if result.ConfirmedEffect || !result.AuditAvailable {
			return errors.New("invalid rejected management mutation")
		}
	case "uncertain":
	default:
		return errors.New("invalid management mutation outcome")
	}
	for _, effect := range result.Effects {
		if !validMutationEffect(effect) {
			return errors.New("invalid management mutation effect")
		}
	}
	return nil
}

type mutationSchemaExecutor interface {
	managementMutationAuditSchemaExecutor
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ensureMutationResultSchema(ctx context.Context, executor mutationSchemaExecutor) error {
	for _, column := range []struct{ name, definition string }{
		{"mutation_id", "mutation_id TEXT NOT NULL DEFAULT ''"},
		{"result_json", "result_json TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := executor.QueryRowContext(
			ctx, "SELECT COUNT(*) FROM pragma_table_info('management_mutation_audit') WHERE name=?", column.name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := executor.ExecContext(
				ctx, "ALTER TABLE management_mutation_audit ADD COLUMN "+column.definition,
			); err != nil {
				return err
			}
		}
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS management_mutation_claims (mutation_id TEXT PRIMARY KEY, request_id ` +
			`TEXT NOT NULL, occurred_at TEXT NOT NULL) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_management_mutation_audit_mutation ON m` +
			`anagement_mutation_audit(mutation_id,id) WHERE mutation_id<>''`,
		`CREATE TRIGGER IF NOT EXISTS management_mutation_claims_no_update BEFORE UPDATE ON ` +
			`management_mutation_claims BEGIN SELECT RAISE(ABORT, 'management mutation claims are append-` +
			`only'); END`,
		`CREATE TRIGGER IF NOT EXISTS management_mutation_claims_no_delete BEFORE DELETE ON ` +
			`management_mutation_claims BEGIN SELECT RAISE(ABORT, 'management mutation claims are append-` +
			`only'); END`,
	} {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *managementStateStore) migrateMutationResultsV5(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = applyManagementMutationAuditSchema(ctx, tx); err != nil {
		return err
	}
	if err = ensureMutationResultSchema(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE meta SET value='5' WHERE key='schema_version' AND value='4'")
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("management mutation migration changed %d schema rows", count)
	}
	return tx.Commit()
}

func (s *managementStateStore) claimMutation(ctx context.Context, id, requestID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("management mutation audit unavailable")
	}
	result, err := s.db.ExecContext(
		ctx,
		"INSERT INTO management_mutation_claims(mutation_id,request_id,occurred_at) SELECT ?,?,? WHERE NOT "+
			"EXISTS(SELECT 1 FROM management_mutation_audit WHERE mutation_id=?) ON CONFLICT(mutation_id) DO "+
			"NOTHING",
		id, requestID, time.Now().UTC().Format(time.RFC3339Nano), id,
	)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// GetMutationResult returns the recorded outcome; resource GET endpoints perform actual reconciliation.
func (h *Handler) GetMutationResult(c *gin.Context) {
	parsed, err := uuid.Parse(strings.TrimSpace(c.Param("mutation_id")))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_mutation_id"})
		return
	}
	if h == nil || h.stateStore == nil || h.stateStore.db == nil {
		c.JSON(
			http.StatusServiceUnavailable,
			gin.H{"status": "unavailable", "error": "mutation_audit_unavailable"},
		)
		return
	}
	var encoded string
	err = h.stateStore.db.QueryRowContext(
		c.Request.Context(),
		"SELECT result_json FROM management_mutation_audit WHERE mutation_id=? "+
			"AND result_json<>'' ORDER BY id DESC LIMIT "+
			"1",
		parsed.String(),
	).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"status": "unknown", "mutation_id": parsed.String()})
		return
	}
	if err != nil {
		log.WithError(err).Error("failed to read management mutation result")
		c.JSON(
			http.StatusServiceUnavailable,
			gin.H{"status": "unavailable", "error": "mutation_audit_unavailable"},
		)
		return
	}
	var result MutationResult
	if err = json.Unmarshal(
		[]byte(encoded), &result,
	); err != nil || result.MutationID != parsed.String() || validateMutationResult(result) != nil {
		log.Error("invalid persisted management mutation result")
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": "mutation_audit_invalid"})
		return
	}
	c.JSON(http.StatusOK, result)
}
