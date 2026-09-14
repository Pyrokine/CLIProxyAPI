package management

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type storedManagementMutationAudit struct {
	OccurredAt    string
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

func readManagementMutationAudits(t *testing.T, store *managementStateStore) []storedManagementMutationAudit {
	t.Helper()
	rows, err := store.db.QueryContext(
		context.Background(),
		`SELECT occurred_at, request_id, actor_kind, client_key, client_source, method,
		        route, resource, target, status_code, version, audit_schema_version
		 FROM management_mutation_audit ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("query management mutation audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var records []storedManagementMutationAudit
	for rows.Next() {
		var record storedManagementMutationAudit
		if err = rows.Scan(
			&record.OccurredAt,
			&record.RequestID,
			&record.ActorKind,
			&record.ClientKey,
			&record.ClientSource,
			&record.Method,
			&record.Route,
			&record.Resource,
			&record.Target,
			&record.StatusCode,
			&record.Version,
			&record.SchemaVersion,
		); err != nil {
			t.Fatalf("scan management mutation audit: %v", err)
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("iterate management mutation audit: %v", err)
	}
	return records
}

func readManagementStateSchemaVersion(t *testing.T, store *managementStateStore) string {
	t.Helper()
	var version string
	if err := store.db.QueryRowContext(
		context.Background(),
		"SELECT value FROM meta WHERE key='schema_version'",
	).Scan(&version); err != nil {
		t.Fatalf("read management schema version: %v", err)
	}
	return version
}

func createManagementStateVersionFixture(t *testing.T, dbPath, version string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open management state fixture: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(
		context.Background(),
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
	); err != nil {
		t.Fatalf("create management state meta fixture: %v", err)
	}
	if _, err = db.ExecContext(
		context.Background(),
		"INSERT INTO meta(key, value) VALUES('schema_version', ?)",
		version,
	); err != nil {
		t.Fatalf("insert management state version fixture: %v", err)
	}
}

func TestManagementStateCreatesMutationAuditSchemaV3(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	if got := readManagementStateSchemaVersion(t, store); got != managementStateSchemaVersion {
		t.Fatalf("schema version = %q, want %q", got, managementStateSchemaVersion)
	}
	for _, object := range []string{
		"management_mutation_audit",
		"idx_management_mutation_audit_occurred_at",
		"idx_management_mutation_audit_request_id",
		"management_mutation_audit_no_update",
		"management_mutation_audit_no_delete",
	} {
		var count int
		if err = store.db.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM sqlite_master WHERE name=?",
			object,
		).Scan(&count); err != nil {
			t.Fatalf("inspect schema object %s: %v", object, err)
		}
		if count != 1 {
			t.Fatalf("schema object %s count = %d, want 1", object, count)
		}
	}
}

func TestManagementStateMigratesV2ToCurrent(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "management-state.db")
	createManagementStateVersionFixture(t, dbPath, "2")

	store, err := openManagementStateStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("open migrated management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	if got := readManagementStateSchemaVersion(t, store); got != managementStateSchemaVersion {
		t.Fatalf("schema version = %q, want %s", got, managementStateSchemaVersion)
	}
	if records := readManagementMutationAudits(t, store); len(records) != 0 {
		t.Fatalf("new mutation audit records = %+v, want empty", records)
	}
}

func TestManagementStateMigratesV1ThroughCurrent(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "management-state.db")
	createManagementStateVersionFixture(t, dbPath, "1")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v1 fixture: %v", err)
	}
	if _, err = db.ExecContext(
		context.Background(),
		`CREATE TABLE quota_entries (
			file_name TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			auth_index TEXT NOT NULL,
			status TEXT NOT NULL,
			last_refresh TEXT,
			next_refresh TEXT,
			error TEXT NOT NULL DEFAULT '',
			failure_count INTEGER NOT NULL DEFAULT 0,
			data BLOB,
			disabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		) WITHOUT ROWID`,
	); err != nil {
		_ = db.Close()
		t.Fatalf("create v1 quota fixture: %v", err)
	}
	if _, err = db.ExecContext(
		context.Background(),
		`INSERT INTO quota_entries(
			file_name, type, auth_index, status, error, failure_count, data, disabled, updated_at
		 ) VALUES('legacy.json', 'claude', '0123456789abcdef', 'success', '', 0, '{}', 0, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		_ = db.Close()
		t.Fatalf("insert v1 quota fixture: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close v1 fixture: %v", err)
	}

	store, err := openManagementStateStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("open migrated management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	if got := readManagementStateSchemaVersion(t, store); got != managementStateSchemaVersion {
		t.Fatalf("schema version = %q, want %s", got, managementStateSchemaVersion)
	}
	entries, err := store.loadQuotaEntries(context.Background())
	if err != nil {
		t.Fatalf("load migrated quota entries: %v", err)
	}
	if entry := entries["claude::legacy.json"]; entry.FileName != "legacy.json" ||
		entry.AuthIndex != "0123456789abcdef" || entry.Generation != 1 || entry.StateRevision != 1 ||
		entry.Freshness != quota.FreshnessFresh {
		t.Fatalf("migrated quota entry = %+v", entry)
	}
}

func TestManagementMutationAuditIsAppendOnly(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	record := managementMutationAuditRecord{
		RequestID:     "0123456789abcdef0123456789abcdef",
		ActorKind:     managementMutationActorKey,
		ClientKey:     "198.51.100.10",
		ClientSource:  string(access.ClientAddressSourcePeer),
		Method:        http.MethodPost,
		Route:         "/v0/management/config",
		Resource:      "configuration",
		StatusCode:    http.StatusNoContent,
		Version:       buildinfo.Version,
		SchemaVersion: managementMutationAuditSchemaVersion,
	}
	if err = store.appendManagementMutationAudit(context.Background(), record); err != nil {
		t.Fatalf("append mutation audit: %v", err)
	}
	if _, err = store.db.ExecContext(
		context.Background(),
		"UPDATE management_mutation_audit SET status_code=200",
	); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update append-only audit error = %v", err)
	}
	if _, err = store.db.ExecContext(
		context.Background(),
		"DELETE FROM management_mutation_audit",
	); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete append-only audit error = %v", err)
	}
	if records := readManagementMutationAudits(
		t, store,
	); len(records) != 1 || records[0].StatusCode != http.StatusNoContent {
		t.Fatalf("records after rejected mutations = %+v", records)
	}
}

func TestManagementMutationAuditConcurrentAppend(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	const count = 32
	errorsByAppend := make(chan error, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errorsByAppend <- store.appendManagementMutationAudit(
				context.Background(),
				managementMutationAuditRecord{
					RequestID:     fmt.Sprintf("%032x", index+1),
					ActorKind:     managementMutationActorKey,
					ClientKey:     "198.51.100.20",
					ClientSource:  string(access.ClientAddressSourcePeer),
					Method:        http.MethodPatch,
					Route:         "/v0/management/config",
					Resource:      "configuration",
					StatusCode:    http.StatusOK,
					Version:       buildinfo.Version,
					SchemaVersion: managementMutationAuditSchemaVersion,
				},
			)
		}(index)
	}
	wait.Wait()
	close(errorsByAppend)
	for err = range errorsByAppend {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	if records := readManagementMutationAudits(t, store); len(records) != count {
		t.Fatalf("concurrent mutation audit count = %d, want %d", len(records), count)
	}
}

func configureManagementAuditTestHandler(
	t *testing.T,
	actorKind string,
) (*Handler, string, string, string, access.ClientAddressSource) {
	t.Helper()
	cfg := &config.Config{}
	credential := "management-secret"
	remoteAddr := "198.51.100.30:12345"
	expectedClient := "198.51.100.30"
	expectedSource := access.ClientAddressSourcePeer
	options := []Option{withEnvSecret("")}
	switch actorKind {
	case managementMutationActorInternal:
		remoteAddr = "127.0.0.1:12345"
		expectedClient = "127.0.0.1"
	case managementMutationActorLocal:
		remoteAddr = "127.0.0.1:12345"
		expectedClient = "127.0.0.1"
	case managementMutationActorKey:
		cfg.RemoteManagement.AllowRemote = true
		options = []Option{withEnvSecret(credential)}
	default:
		t.Fatalf("unsupported actor kind %q", actorKind)
	}
	handler := NewHandler(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		nil,
		options...,
	)
	switch actorKind {
	case managementMutationActorInternal:
		credential = handler.InternalToken()
	case managementMutationActorLocal:
		handler.SetLocalPassword(credential)
	}
	return handler, credential, remoteAddr, expectedClient, expectedSource
}

func mutationAuditTestEngine(handler *Handler) *gin.Engine {
	engine := gin.New()
	group := engine.Group("/v0/management")
	group.Use(handler.middleware(), handler.MutationAuditMiddleware())
	group.Any(
		"/config/:id", func(c *gin.Context) {
			setManagementMutationTarget(c, "auth_index", "0123456789abcdef")
			c.Status(http.StatusNoContent)
		},
	)
	return engine
}

func TestManagementMutationAuditRecordsAuthenticatedActorAndSafeFields(t *testing.T) {
	for _, actorKind := range []string{
		managementMutationActorInternal,
		managementMutationActorLocal,
		managementMutationActorKey,
	} {
		t.Run(
			actorKind, func(t *testing.T) {
				handler, credential, remoteAddr, expectedClient, expectedSource := configureManagementAuditTestHandler(
					t, actorKind,
				)
				defer func() { _ = handler.Close() }()
				engine := mutationAuditTestEngine(handler)
				request := httptest.NewRequest(
					http.MethodPost,
					"/v0/management/config/private-token?api_key=query-secret",
					strings.NewReader(`{"password":"body-secret"}`),
				)
				request.RemoteAddr = remoteAddr
				request.Header.Set("Authorization", "Bearer "+credential)
				request.Header.Set("X-Management-Key", "unused-header-secret")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if response.Code != http.StatusNoContent {
					t.Fatalf("mutation status = %d, want 204: %s", response.Code, response.Body.String())
				}

				records := readManagementMutationAudits(t, handler.stateStore)
				if len(records) != 1 {
					t.Fatalf("mutation audit records = %+v", records)
				}
				record := records[0]
				if _, err := time.Parse(time.RFC3339Nano, record.OccurredAt); err != nil {
					t.Fatalf("occurred_at = %q: %v", record.OccurredAt, err)
				}
				if len(record.RequestID) != 32 {
					t.Fatalf("request id = %q, want 32 hex characters", record.RequestID)
				}
				if record.ActorKind != actorKind || record.ClientKey != expectedClient || record.ClientSource != string(expectedSource) {
					t.Fatalf("actor/client = %+v", record)
				}
				if record.Method != http.MethodPost || record.Route != "/v0/management/config/:id" ||
					record.Resource != "configuration" || record.Target != "auth_index:0123456789abcdef" ||
					record.StatusCode != http.StatusNoContent || record.Version != buildinfo.Version ||
					record.SchemaVersion != managementMutationAuditSchemaVersion {
					t.Fatalf("mutation audit record = %+v", record)
				}
				storedText := strings.Join(
					[]string{
						record.OccurredAt,
						record.RequestID,
						record.ActorKind,
						record.ClientKey,
						record.ClientSource,
						record.Method,
						record.Route,
						record.Resource,
						record.Target,
						record.Version,
					}, "\n",
				)
				for _, secret := range []string{
					"private-token", "query-secret", "body-secret", credential, "unused-header-secret",
				} {
					if strings.Contains(storedText, secret) {
						t.Fatalf("mutation audit retained secret %q: %+v", secret, record)
					}
				}
			},
		)
	}
}

func TestManagementMutationAuditUsesCanonicalForwardedClient(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	handler := NewHandler(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		nil,
		withEnvSecret("management-secret"),
	)
	defer func() { _ = handler.Close() }()
	request := httptest.NewRequest(http.MethodPut, "/v0/management/config/value", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Forwarded-For", "203.0.113.44")
	request.Header.Set("X-Management-Key", "management-secret")
	response := httptest.NewRecorder()
	mutationAuditTestEngine(handler).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("mutation status = %d, want 204: %s", response.Code, response.Body.String())
	}
	records := readManagementMutationAudits(t, handler.stateStore)
	if len(records) != 1 || records[0].ClientKey != "203.0.113.44" ||
		records[0].ClientSource != string(access.ClientAddressSourceForwardedFor) {
		t.Fatalf("forwarded mutation audit records = %+v", records)
	}
}

func TestManagementMutationAuditRecordsWriteMethodsOnlyAfterAuthentication(t *testing.T) {
	handler, credential, remoteAddr, _, _ := configureManagementAuditTestHandler(t, managementMutationActorKey)
	defer func() { _ = handler.Close() }()
	engine := mutationAuditTestEngine(handler)
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet,
	} {
		request := httptest.NewRequest(method, "/v0/management/config/value", nil)
		request.RemoteAddr = remoteAddr
		request.Header.Set("X-Management-Key", credential)
		engine.ServeHTTP(httptest.NewRecorder(), request)
	}
	failed := httptest.NewRequest(http.MethodPost, "/v0/management/config/value", nil)
	failed.RemoteAddr = remoteAddr
	failed.Header.Set("X-Management-Key", "wrong-secret")
	failedResponse := httptest.NewRecorder()
	engine.ServeHTTP(failedResponse, failed)
	if failedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("failed auth status = %d, want 401", failedResponse.Code)
	}
	records := readManagementMutationAudits(t, handler.stateStore)
	if len(records) != 4 {
		t.Fatalf("mutation audit count = %d, want 4: %+v", len(records), records)
	}
	for index, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if records[index].Method != method {
			t.Fatalf("record %d method = %q, want %q", index, records[index].Method, method)
		}
	}
}

func fakeManagementAuditIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		client := resolveManagementClientAddress(c.Request, &config.Config{})
		c.Set(managementClientAddressContextKey, client)
		setManagementMutationActor(c, managementMutationActorInternal)
		c.Next()
	}
}

func TestManagementMutationAuditFailureDoesNotChangeResponse(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}
	handler := &Handler{stateStore: store}
	hook := logtest.NewGlobal()
	defer hook.Reset()
	engine := gin.New()
	engine.Use(fakeManagementAuditIdentity(), handler.MutationAuditMiddleware())
	engine.POST(
		"/v0/management/config", func(c *gin.Context) {
			c.Status(http.StatusAccepted)
		},
	)
	request := httptest.NewRequest(http.MethodPost, "/v0/management/config", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("response status = %d, want 202", response.Code)
	}
	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Message == "failed to persist management mutation audit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit failure log entries = %+v", hook.AllEntries())
	}
}

func TestManagementMutationAuditRecordsPanicsAsServerErrors(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	handler := &Handler{stateStore: store}
	engine := gin.New()
	engine.Use(logging.GinLogrusRecovery(), fakeManagementAuditIdentity(), handler.MutationAuditMiddleware())
	engine.POST(
		"/v0/management/config", func(*gin.Context) {
			panic("audit-test-panic")
		},
	)
	request := httptest.NewRequest(http.MethodPost, "/v0/management/config", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic response status = %d, want 500", response.Code)
	}
	records := readManagementMutationAudits(t, store)
	if len(records) != 1 || records[0].StatusCode != http.StatusInternalServerError {
		t.Fatalf("panic mutation audit records = %+v", records)
	}
}

func TestConfiguredManagementKeyUsesManagementActor(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("configured-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash management key: %v", err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.SecretKey = string(hash)
	handler := NewHandler(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		nil,
		withEnvSecret(""),
	)
	defer func() { _ = handler.Close() }()
	request := httptest.NewRequest(http.MethodPatch, "/v0/management/config/value", nil)
	request.RemoteAddr = "198.51.100.40:12345"
	request.Header.Set("X-Management-Key", "configured-secret")
	response := httptest.NewRecorder()
	mutationAuditTestEngine(handler).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("configured key mutation status = %d, want 204", response.Code)
	}
	records := readManagementMutationAudits(t, handler.stateStore)
	if len(records) != 1 || records[0].ActorKind != managementMutationActorKey {
		t.Fatalf("configured key mutation audit records = %+v", records)
	}
}

func TestManagementMutationTargetAllowsOnlySafeClassifiedValues(t *testing.T) {
	for _, testCase := range []struct {
		kind  string
		value string
		want  string
	}{
		{kind: "provider", value: "xai", want: "provider:xai"},
		{kind: "model", value: "grok-4.1-fast", want: "model:grok-4.1-fast"},
		{kind: "auth_index", value: "0123456789ABCDEF", want: "auth_index:0123456789abcdef"},
		{kind: "version", value: "v6.2.1+build.4", want: "version:v6.2.1+build.4"},
		{kind: "provider", value: "https://private.example", want: ""},
		{kind: "model", value: "model-private-token", want: ""},
		{kind: "auth_index", value: "credential.json", want: ""},
		{kind: "path", value: "/private/file", want: ""},
	} {
		if got := buildManagementMutationTarget(testCase.kind, testCase.value); got != testCase.want {
			t.Errorf(
				"buildManagementMutationTarget(%q, %q) = %q, want %q", testCase.kind, testCase.value, got, testCase.want,
			)
		}
	}
}

func TestManagementMutationAuditSharesRequestIDWithRequestLog(t *testing.T) {
	handler, credential, remoteAddr, _, _ := configureManagementAuditTestHandler(t, managementMutationActorKey)
	defer func() { _ = handler.Close() }()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	engine := gin.New()
	engine.Use(logging.GinLogrusLogger())
	group := engine.Group("/v0/management")
	group.Use(handler.middleware(), handler.MutationAuditMiddleware())
	group.POST(
		"/config", func(c *gin.Context) {
			c.Status(http.StatusNoContent)
		},
	)
	request := httptest.NewRequest(http.MethodPost, "/v0/management/config", nil)
	request.RemoteAddr = remoteAddr
	request.Header.Set("X-Management-Key", credential)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("mutation status = %d, want 204", response.Code)
	}
	records := readManagementMutationAudits(t, handler.stateStore)
	if len(records) != 1 {
		t.Fatalf("mutation audit records = %+v", records)
	}
	found := false
	for _, entry := range hook.AllEntries() {
		if requestID, ok := entry.Data["request_id"].(string); ok && requestID == records[0].RequestID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("request log does not contain audit request id %q", records[0].RequestID)
	}
}
