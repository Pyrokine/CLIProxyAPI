package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func newMutationResultEngine(t *testing.T, h *Handler, action gin.HandlerFunc) *gin.Engine {
	t.Helper()
	engine := gin.New()
	engine.Use(fakeManagementAuditIdentity(), h.MutationAuditMiddleware())
	engine.POST("/v0/management/config", action)
	engine.GET("/v0/management/mutations/:mutation_id", h.GetMutationResult)
	return engine
}

func mutationResultRequest(engine *gin.Engine, method, path, id string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	if id != "" {
		request.Header.Set(MutationIDHeader, id)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func decodeMutationHeader(t *testing.T, response *httptest.ResponseRecorder) MutationResult {
	t.Helper()
	var result MutationResult
	if err := json.Unmarshal([]byte(response.Result().Header.Get(MutationResultHeader)), &result); err != nil {
		t.Fatalf(
			"physical response header=%q error=%v",
			response.Result().Header.Get(MutationResultHeader),
			err,
		)
	}
	if result.RequestID == "" || uuid.Validate(result.MutationID) != nil {
		t.Fatalf("missing result identity: %+v", result)
	}
	return result
}

func TestMutationResultsUseConfirmedEffectsAndPreserveResponses(t *testing.T) {
	for _, test := range []struct {
		name, effect, outcome  string
		status                 int
		complete, reject, fail bool
	}{
		{name: "unannotated success", status: 200, outcome: "uncertain"},
		{name: "unannotated accepted", status: 202, outcome: "uncertain"},
		{name: "completion without effect", status: 200, complete: true, outcome: "uncertain"},
		{name: "validation rejection", status: 422, reject: true, outcome: "rejected"},
		{name: "committed", status: 200, effect: "configuration.persisted", complete: true, outcome: "succeeded"},
		{name: "partial failure", status: 503, effect: "configuration.persisted", fail: true, outcome: "partial"},
		{name: "late validation", status: 409, effect: "configuration.persisted", reject: true, outcome: "partial"},
		{name: "effect with unknown remainder", status: 200, effect: "configuration.persisted", outcome: "uncertain"},
		{name: "queued asynchronous command", status: 202, effect: "task.queued", complete: true, outcome: "succeeded"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				h := &Handler{stateStore: store}
				engine := newMutationResultEngine(
					t, h, func(c *gin.Context) {
						if test.effect != "" {
							if !RecordMutationEffect(
								c.Request.Context(), test.effect, strings.Split(test.effect, ".")[0],
							) {
								t.Fatal("valid effect rejected")
							}
						}
						if test.complete {
							CompleteMutation(c.Request.Context())
						}
						if test.reject {
							rejectMutation(c.Request.Context())
						}
						if test.fail {
							failMutation(c.Request.Context())
						}
						c.JSON(test.status, gin.H{"legacy": "body"})
					},
				)
				id := uuid.NewString()
				response := mutationResultRequest(engine, http.MethodPost, "/v0/management/config", id)
				if response.Code != test.status || response.Body.String() != `{"legacy":"body"}` {
					t.Fatalf("response=%d %s", response.Code, response.Body.String())
				}
				result := decodeMutationHeader(t, response)
				if result.MutationID != id || result.Outcome != test.outcome || !result.AuditAvailable ||
					result.ConfirmedEffect != (test.effect != "") {
					t.Fatalf("result=%+v", result)
				}
				read := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+id, "")
				var persisted MutationResult
				if err := json.Unmarshal(
					read.Body.Bytes(), &persisted,
				); err != nil || read.Code != 200 || !reflect.DeepEqual(result, persisted) {
					t.Fatalf("read=%d %s error=%v", read.Code, read.Body.String(), err)
				}
				var requestID string
				if err := store.db.QueryRow(
					"SELECT request_id FROM management_mutation_audit WHERE mutation_id=? ORDER BY id DESC LIMIT 1", id,
				).Scan(&requestID); err != nil || requestID != result.RequestID {
					t.Fatalf("audit request_id=%q error=%v", requestID, err)
				}
			},
		)
	}
}

func TestMutationIdentityConflictsDoNotReplayAndAreConcurrentSafe(t *testing.T) {
	store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var executed atomic.Int32
	engine := newMutationResultEngine(
		t, &Handler{stateStore: store}, func(c *gin.Context) {
			executed.Add(1)
			RecordMutationEffect(c.Request.Context(), "configuration.published", "configuration")
			CompleteMutation(c.Request.Context())
			c.JSON(200, gin.H{"ok": true})
		},
	)
	id := uuid.NewString()
	responses := make(chan *httptest.ResponseRecorder, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(
			func() { responses <- mutationResultRequest(engine, http.MethodPost, "/v0/management/config", id) },
		)
	}
	workers.Wait()
	close(responses)
	succeeded, conflicted := 0, 0
	for response := range responses {
		result := decodeMutationHeader(t, response)
		switch response.Code {
		case 200:
			succeeded++
			if result.Outcome != "succeeded" {
				t.Fatalf("first result=%+v", result)
			}
		case 409:
			conflicted++
			if result.Outcome != "rejected" || result.ConfirmedEffect {
				t.Fatalf("conflict result=%+v", result)
			}
		default:
			t.Fatalf("response=%d %s", response.Code, response.Body.String())
		}
	}
	if executed.Load() != 1 || succeeded != 1 || conflicted != 31 {
		t.Fatalf("executed=%d success=%d conflict=%d", executed.Load(), succeeded, conflicted)
	}
	read := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+id, "")
	var result MutationResult
	if err := json.Unmarshal(read.Body.Bytes(), &result); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("conflicts replaced the original result: %s error=%v", read.Body.String(), err)
	}
	invalid := mutationResultRequest(engine, http.MethodPost, "/v0/management/config", "synthetic-invalid-id")
	if invalid.Code != 400 || executed.Load() != 1 {
		t.Fatalf("invalid identity response=%d executions=%d", invalid.Code, executed.Load())
	}
}

func TestMutationResultsRemainHonestWithoutAuditAndOnLateConfirmation(t *testing.T) {
	if _, err := (&managementStateStore{}).claimMutation(
		context.Background(), uuid.NewString(), "synthetic-request",
	); err == nil {
		t.Fatal("missing database did not report unavailable audit")
	}
	for _, closed := range []bool{false, true} {
		t.Run(
			fmt.Sprint("closed=", closed), func(t *testing.T) {
				h := &Handler{}
				if closed {
					store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					h.stateStore = store
				}
				var count int
				engine := newMutationResultEngine(
					t, h, func(c *gin.Context) {
						count++
						RecordMutationEffect(
							c.Request.Context(),
							"credential.published",
							"auth_index:0123456789abcdef",
						)
						CompleteMutation(c.Request.Context())
						c.JSON(202, gin.H{"legacy": true})
					},
				)
				id := uuid.NewString()
				for range 2 {
					response := mutationResultRequest(engine, http.MethodPost, "/v0/management/config", id)
					result := decodeMutationHeader(t, response)
					if response.Code != 202 || response.Body.String() != `{"legacy":true}` ||
						result.AuditAvailable ||
						result.Outcome != "uncertain" ||
						!result.ConfirmedEffect {
						t.Fatalf("legacy result=%+v response=%d", result, response.Code)
					}
				}
				if count != 2 {
					t.Fatal("unavailable persistence introduced a legacy-write precondition")
				}
				read := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+id, "")
				if read.Code != 503 || !strings.Contains(read.Body.String(), "unavailable") {
					t.Fatalf("unavailable read=%d %s", read.Code, read.Body.String())
				}
			},
		)
	}
	store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var operation context.Context
	engine := newMutationResultEngine(
		t, &Handler{stateStore: store}, func(c *gin.Context) { operation = c.Request.Context(); c.Status(504) },
	)
	response := mutationResultRequest(engine, http.MethodPost, "/v0/management/config", "")
	initial := decodeMutationHeader(t, response)
	if initial.Outcome != "uncertain" {
		t.Fatalf("timeout result=%+v", initial)
	}
	RecordMutationEffect(operation, "credential.persisted", "auth_index:0123456789abcdef")
	CompleteMutation(operation)
	read := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+initial.MutationID, "")
	var late MutationResult
	if err := json.Unmarshal(
		read.Body.Bytes(), &late,
	); err != nil || late.Outcome != "succeeded" || late.RequestID != initial.RequestID || !late.ConfirmedEffect {
		t.Fatalf("late result=%s error=%v", read.Body.String(), err)
	}
	unknown := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+uuid.NewString(), "")
	if unknown.Code != 404 || !strings.Contains(unknown.Body.String(), "unknown") {
		t.Fatalf("unknown result=%d %s", unknown.Code, unknown.Body.String())
	}
}

func TestMutationResultMigrationPreservesOldAuditAndRollsBackFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(
			fmt.Sprint("failure=", fail), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "state.db")
				createManagementStateVersionFixture(t, path, "4")
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(
					`CREATE TABLE management_mutation_audit(id INTEGER PRIMARY KEY ` +
						`AUTOINCREMENT,occurred_at TEXT NOT NULL,request_id TEXT NOT NULL,actor_kind TEXT NOT ` +
						`NULL,client_key TEXT NOT NULL,client_source TEXT NOT NULL,method TEXT NOT NULL,route ` +
						`TEXT NOT NULL,resource TEXT NOT NULL,target TEXT NOT NULL DEFAULT '',status_code ` +
						`INTEGER NOT NULL,version TEXT NOT NULL,audit_schema_version INTEGER NOT NULL); INSERT ` +
						`INTO management_mutation_audit VALUES(1,'legacy-time','legacy-request','management_key',` +
						`'127.0.0.1','peer','POST','/v0/management/config','configuration','',200,'legacy-` +
						`version',1)`,
				)
				if err != nil {
					t.Fatal(err)
				}
				if fail {
					if _, err = db.Exec("CREATE VIEW management_mutation_claims AS SELECT 1 AS invalid"); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				store, err := openManagementStateStorePath(path)
				if fail {
					if err == nil {
						_ = store.Close()
						t.Fatal("invalid migration fixture did not fail")
					}
					db, err = sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					defer func(closeResource func() error) {
						if err := closeResource(); err != nil {
							t.Errorf("close migration database: %v", err)
						}
					}(db.Close)
					var version string
					var columns int
					if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err !=
						nil {
						t.Fatal(err)
					}
					if err := db.QueryRow(
						"SELECT COUNT(*) FROM pragma_table_info('management_mutation_audit'" +
							") WHERE name='mutation_id'",
					).Scan(&columns); err !=
						nil {
						t.Fatal(err)
					}
					if version != "4" || columns != 0 {
						t.Fatalf("failed migration version=%s new columns=%d", version, columns)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer func(closeResource func() error) {
					if err := closeResource(); err != nil {
						t.Errorf("close migrated management state: %v", err)
					}
				}(store.Close)
				var requestID string
				var auditVersion int
				if err := store.db.QueryRow(
					"SELECT request_id,audit_schema_version FROM "+
						"management_mutation_audit WHERE "+
						"id=1",
				).Scan(
					&requestID, &auditVersion,
				); err != nil ||
					requestID != "legacy-request" ||
					auditVersion != 1 {
					t.Fatalf("legacy row=%s/%d error=%v", requestID, auditVersion, err)
				}
				for _, statement := range []string{
					"UPDATE management_mutation_audit SET status_code=500", "DELETE FROM management_mutation_audit",
				} {
					if _, err := store.db.Exec(statement); err == nil {
						t.Fatal("audit mutation bypassed append-only protection")
					}
				}
				if got := readManagementStateSchemaVersion(t, store); got != "5" {
					t.Fatalf("version=%s", got)
				}
			},
		)
	}
}
