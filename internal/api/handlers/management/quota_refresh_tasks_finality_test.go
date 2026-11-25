package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestQuotaRefreshTaskFinalStatusFollowsCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "committed"
		if fail {
			name = "failed"
		}
		t.Run(
			name, func(t *testing.T) {
				store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				h := &Handler{stateStore: store}
				s := h.quotaRefreshService()
				const id = "synthetic-quota-task"
				now := time.Now().UTC()
				task := &quotaRefreshTask{
					ID: id, Status: quotaRefreshTaskStatusRunning, CreatedAt: now, StartedAt: &now,
					Targets: []quotaRefreshTaskTarget{
						{
							ID: "account", Status: quotaRefreshTargetStatusSucceeded, CompletedAt: &now,
						},
					},
				}
				task.Summary = summarizeQuotaRefreshTaskTargets(task.Targets)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(context.Canceled)
				s.tasks[id] = task
				s.order = []string{id}
				s.runtimes[id] = &quotaRefreshTaskRuntime{ctx: ctx, cancel: cancel}
				if err := s.persistState(); err != nil {
					t.Fatal(err)
				}

				store.db.SetMaxOpenConns(1)
				conn, err := store.db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				if fail {
					if _, err := conn.ExecContext(t.Context(), "PRAGMA query_only = ON"); err != nil {
						t.Fatal(err)
					}
				}
				router := gin.New()
				router.GET("/quota/refresh-tasks/:task_id", h.GetQuotaRefreshTask)
				get := func() quotaRefreshTask {
					t.Helper()
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/quota/refresh-tasks/"+id, nil))
					var got quotaRefreshTask
					if err := json.Unmarshal(
						recorder.Body.Bytes(), &got,
					); recorder.Code != http.StatusOK || err != nil {
						t.Fatalf("read task: %d %s (%v)", recorder.Code, recorder.Body, err)
					}
					return got
				}

				initialWaits := store.db.Stats().WaitCount
				finished := make(chan struct{})
				go func() {
					s.finishTask(id)
					close(finished)
				}()
				deadline := time.Now().Add(time.Second)
				for store.db.Stats().WaitCount == initialWaits {
					if time.Now().After(deadline) {
						t.Fatal("final write did not wait on the SQLite connection")
					}
					time.Sleep(time.Millisecond)
				}
				if got := get(); got.Status != quotaRefreshTaskStatusRunning || got.CompletedAt != nil || got.Summary.Succeeded != 1 {
					t.Fatalf("blocked write exposed a terminal task: %+v", got)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Fatal("final write did not return")
				}

				persisted, err := store.loadQuotaRefreshTasks(t.Context())
				if err != nil || persisted == nil || len(persisted.Tasks) != 1 {
					t.Fatalf("read SQLite state: %+v (%v)", persisted, err)
				}
				got := get()
				wantStatus, wantPersisted := quotaRefreshTaskStatusSucceeded, quotaRefreshTaskStatusSucceeded
				if fail {
					wantStatus, wantPersisted = quotaRefreshTaskStatusInconclusive, quotaRefreshTaskStatusRunning
				}
				if got.Status != wantStatus || persisted.Tasks[0].Status != wantPersisted || got.CompletedAt == nil ||
					got.Summary.Succeeded != 1 || (got.PersistenceError != "") != fail {
					t.Fatalf("final task: %+v; persisted: %+v", got, persisted.Tasks[0])
				}
			},
		)
	}
}
