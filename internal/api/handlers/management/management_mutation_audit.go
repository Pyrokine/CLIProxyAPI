package management

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	managementMutationAuditSchemaVersion = 1
	managementMutationAuditWriteTimeout  = 2 * time.Second

	managementMutationActorContextKey  = "management.mutation-audit.actor-kind"
	managementMutationTargetContextKey = "management.mutation-audit.target"
	managementMutationActorKey         = "management_key"
	managementMutationActorLocal       = "local_password"
	managementMutationActorInternal    = "internal_service"
)

type managementMutationAuditRecord struct {
	OccurredAt    time.Time
	RequestID     string
	ActorKind     string
	ClientKey     string
	ClientSource  string
	Method        string
	Route         string
	Resource      string
	Target        string
	StatusCode    int
	Version       string
	SchemaVersion int
}

type managementMutationAuditSchemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func applyManagementMutationAuditSchema(
	ctx context.Context,
	executor managementMutationAuditSchemaExecutor,
) error {
	if executor == nil {
		return errors.New("management mutation audit schema executor is nil")
	}
	statements := [...]string{
		`CREATE TABLE IF NOT EXISTS management_mutation_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			occurred_at TEXT NOT NULL,
			request_id TEXT NOT NULL,
			actor_kind TEXT NOT NULL,
			client_key TEXT NOT NULL,
			client_source TEXT NOT NULL,
			method TEXT NOT NULL,
			route TEXT NOT NULL,
			resource TEXT NOT NULL,
			target TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL,
			version TEXT NOT NULL,
			audit_schema_version INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_management_mutation_audit_occurred_at
		 ON management_mutation_audit(occurred_at)`,
		`CREATE INDEX IF NOT EXISTS idx_management_mutation_audit_request_id
		 ON management_mutation_audit(request_id)`,
		`CREATE TRIGGER IF NOT EXISTS management_mutation_audit_no_update
		 BEFORE UPDATE ON management_mutation_audit
		 BEGIN SELECT RAISE(ABORT, 'management mutation audit is append-only'); END`,
		`CREATE TRIGGER IF NOT EXISTS management_mutation_audit_no_delete
		 BEFORE DELETE ON management_mutation_audit
		 BEGIN SELECT RAISE(ABORT, 'management mutation audit is append-only'); END`,
	}
	for _, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *managementStateStore) applyManagementMutationAuditSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := applyManagementMutationAuditSchema(ctx, s.db); err != nil {
		return fmt.Errorf("management state: apply mutation audit schema: %w", err)
	}
	return nil
}

func (s *managementStateStore) migrateManagementMutationAuditV3(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("management state: begin v3 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = applyManagementMutationAuditSchema(ctx, tx); err != nil {
		return fmt.Errorf("management state: create mutation audit schema: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		"UPDATE meta SET value='3' WHERE key='schema_version' AND value='2'",
	)
	if err != nil {
		return fmt.Errorf("management state: update v3 schema version: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("management state: inspect v3 schema update: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("management state: v3 migration updated %d schema rows", updated)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("management state: commit v3 migration: %w", err)
	}
	return nil
}

func (s *managementStateStore) appendManagementMutationAudit(
	ctx context.Context,
	record managementMutationAuditRecord,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("management state: mutation audit unavailable")
	}
	if !isManagementMutationActor(record.ActorKind) {
		return fmt.Errorf("management state: invalid mutation audit actor")
	}
	if !isManagementMutationMethod(record.Method) {
		return fmt.Errorf("management state: invalid mutation audit method")
	}
	if record.SchemaVersion != managementMutationAuditSchemaVersion {
		return fmt.Errorf("management state: invalid mutation audit schema version")
	}
	if record.StatusCode < 100 || record.StatusCode > 599 {
		return fmt.Errorf("management state: invalid mutation audit status")
	}
	if record.Target != "" && normalizeManagementMutationTarget(record.Target) != record.Target {
		return fmt.Errorf("management state: invalid mutation audit target")
	}
	record.Route = normalizeManagementMutationRoute(record.Route)
	record.Resource = managementMutationResource(record.Route)
	record.ClientKey = normalizeManagementMutationClientKey(record.ClientKey)
	record.ClientSource = normalizeManagementMutationClientSource(access.ClientAddressSource(record.ClientSource))
	if record.OccurredAt.IsZero() {
		record.OccurredAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO management_mutation_audit(
			occurred_at, request_id, actor_kind, client_key, client_source, method,
			route, resource, target, status_code, version, audit_schema_version
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.OccurredAt.UTC().Format(time.RFC3339Nano),
		record.RequestID,
		record.ActorKind,
		record.ClientKey,
		record.ClientSource,
		record.Method,
		record.Route,
		record.Resource,
		record.Target,
		record.StatusCode,
		record.Version,
		record.SchemaVersion,
	)
	if err != nil {
		return fmt.Errorf("management state: append mutation audit: %w", err)
	}
	return nil
}

func setManagementMutationActor(c *gin.Context, actorKind string) {
	if c != nil && isManagementMutationActor(actorKind) {
		c.Set(managementMutationActorContextKey, actorKind)
	}
}

func managementMutationActor(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(managementMutationActorContextKey)
	if !exists {
		return ""
	}
	actorKind, _ := value.(string)
	if !isManagementMutationActor(actorKind) {
		return ""
	}
	return actorKind
}

func setManagementMutationTarget(c *gin.Context, kind, value string) {
	if c == nil {
		return
	}
	if target := buildManagementMutationTarget(kind, value); target != "" {
		c.Set(managementMutationTargetContextKey, target)
	}
}

func managementMutationTarget(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(managementMutationTargetContextKey)
	if !exists {
		return ""
	}
	target, _ := value.(string)
	return normalizeManagementMutationTarget(target)
}

func buildManagementMutationTarget(kind, value string) string {
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	switch kind {
	case "auth_index":
		value = strings.ToLower(value)
		if len(value) != 16 {
			return ""
		}
		for _, char := range value {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return ""
			}
		}
	case "provider":
		if !isManagementMutationIdentifier(value, 64) {
			return ""
		}
	case "model":
		if !isManagementMutationIdentifier(value, 256) {
			return ""
		}
	case "version":
		if !isManagementMutationIdentifier(value, 64) {
			return ""
		}
	default:
		return ""
	}
	return kind + ":" + value
}

func normalizeManagementMutationTarget(target string) string {
	parts := strings.SplitN(strings.TrimSpace(target), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return buildManagementMutationTarget(parts[0], parts[1])
}

func isManagementMutationIdentifier(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength || strings.Contains(value, "://") {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"authorization", "bearer ", "api_key", "apikey", "cookie", "password", "proxy_url", "secret", "token",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '_', '.', ':', '/', '+', '@':
			continue
		default:
			return false
		}
	}
	return true
}

func isManagementMutationActor(actorKind string) bool {
	switch actorKind {
	case managementMutationActorKey, managementMutationActorLocal, managementMutationActorInternal:
		return true
	default:
		return false
	}
}

func isManagementMutationMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func normalizeManagementMutationRoute(route string) string {
	route = strings.TrimSpace(route)
	if route == "" || len(route) > 256 ||
		(route != "/v0/management" && !strings.HasPrefix(route, "/v0/management/")) {
		return "/v0/management"
	}
	for _, char := range route {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '/', '-', '_', '.', ':', '*':
			continue
		default:
			return "/v0/management"
		}
	}
	return route
}

func managementMutationResource(route string) string {
	relative := strings.TrimPrefix(normalizeManagementMutationRoute(route), "/v0/management")
	segment := strings.TrimPrefix(relative, "/")
	if separator := strings.IndexByte(segment, '/'); separator >= 0 {
		segment = segment[:separator]
	}
	switch {
	case strings.HasPrefix(segment, "usage"):
		return "usage"
	case strings.HasPrefix(segment, "auth"), strings.HasPrefix(segment, "oauth"),
		strings.Contains(segment, "api-key"), strings.Contains(segment, "credential"):
		return "credential"
	case strings.HasPrefix(segment, "quota"), strings.Contains(segment, "inspection"):
		return "quota"
	case strings.HasPrefix(segment, "plugin"):
		return "plugin"
	case strings.HasPrefix(segment, "model"):
		return "model"
	case strings.Contains(segment, "update"), segment == "releases":
		return "update"
	case strings.Contains(segment, "log"), segment == "debug":
		return "logging"
	case strings.Contains(segment, "security"), strings.Contains(segment, "rate-limit"):
		return "security"
	case segment == "config" || segment == "config.yaml":
		return "configuration"
	default:
		return "management"
	}
}

func normalizeManagementMutationClientKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "unknown"
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '.', ':', '-', '_', '%':
			continue
		default:
			return "unknown"
		}
	}
	return value
}

func normalizeManagementMutationClientSource(source access.ClientAddressSource) string {
	switch source {
	case access.ClientAddressSourcePeer,
		access.ClientAddressSourceForwardedFor,
		access.ClientAddressSourceRealIP,
		access.ClientAddressSourceInvalidForwarded:
		return string(source)
	default:
		return "unknown"
	}
}

func managementMutationAuditRecordFromContext(c *gin.Context, statusCode int) (managementMutationAuditRecord, bool) {
	actorKind := managementMutationActor(c)
	client, hasClient := managementClientAddressFromContext(c)
	if actorKind == "" || !hasClient {
		return managementMutationAuditRecord{}, false
	}
	route := normalizeManagementMutationRoute(c.FullPath())
	return managementMutationAuditRecord{
		OccurredAt:    time.Now().UTC(),
		RequestID:     logging.EnsureGinRequestID(c),
		ActorKind:     actorKind,
		ClientKey:     normalizeManagementMutationClientKey(client.resolved.Key),
		ClientSource:  normalizeManagementMutationClientSource(client.source),
		Method:        c.Request.Method,
		Route:         route,
		Resource:      managementMutationResource(route),
		Target:        managementMutationTarget(c),
		StatusCode:    statusCode,
		Version:       buildinfo.Version,
		SchemaVersion: managementMutationAuditSchemaVersion,
	}, true
}

func (h *Handler) recordManagementMutation(c *gin.Context, statusCode int) {
	record, ok := managementMutationAuditRecordFromContext(c, statusCode)
	if !ok {
		log.Error("management mutation audit context unavailable")
		return
	}
	entry := log.WithFields(
		log.Fields{
			"request_id": record.RequestID,
			"method":     record.Method,
			"route":      record.Route,
		},
	)
	if h == nil || h.stateStore == nil {
		entry.Error("management mutation audit unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), managementMutationAuditWriteTimeout)
	defer cancel()
	if err := h.stateStore.appendManagementMutationAudit(ctx, record); err != nil {
		entry.WithError(err).Error("failed to persist management mutation audit")
	}
}

// MutationAuditMiddleware records authenticated management write requests after handler completion.
func (h *Handler) MutationAuditMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || !isManagementMutationMethod(c.Request.Method) {
			if c != nil {
				c.Next()
			}
			return
		}
		defer func() {
			panicValue := recover()
			statusCode := c.Writer.Status()
			if panicValue != nil {
				statusCode = http.StatusInternalServerError
			}
			h.recordManagementMutation(c, statusCode)
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		c.Next()
	}
}
