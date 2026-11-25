package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func openInspectionRepairStore(t *testing.T, path string) *managementStateStore {
	t.Helper()
	store, err := openManagementStateStorePath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func inspectionRepairRouter(h *Handler) *gin.Engine {
	router := inspectionPreconditionRouter(h)
	router.GET("/account-inspection/results", h.GetAccountInspectionResults)
	router.GET("/account-inspection/schedule", h.GetAccountInspectionSchedule)
	router.PUT("/account-inspection/schedule", h.PutAccountInspectionSchedule)
	return router
}

func TestAccountInspectionScheduleKeepsTimerAndLogsWhenWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inspection.db")
	store := openInspectionRepairStore(t, path)
	h := &Handler{stateStore: store}
	s := h.inspectionService()
	t.Cleanup(s.close)
	router := inspectionRepairRouter(h)
	put := func(body string) int {
		return inspectionPreconditionRequest(router, http.MethodPut, "schedule", body).Code
	}
	if status := put(`{"enabled":true,"interval_seconds":120,"providers":[" Codex ","codex"]}`); status != http.StatusOK {
		t.Fatalf("save original plan: %d", status)
	}
	var timerCancelled atomic.Bool
	s.mu.Lock()
	originalCancel := s.scheduleCancel
	s.scheduleCancel = func() {
		timerCancelled.Store(true)
		originalCancel()
	}
	s.mu.Unlock()
	original := s.persistenceSnapshot()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if status := put(`{"enabled":false,"interval_seconds":240}`); status != http.StatusServiceUnavailable {
		t.Fatalf("reject unpersisted plan: %d", status)
	}
	response := inspectionPreconditionRequest(router, http.MethodGet, "schedule", "")
	var live accountInspectionSchedule
	if err := json.Unmarshal(response.Body.Bytes(), &live); err != nil || !reflect.DeepEqual(live, original.Schedule) {
		t.Fatalf("runtime plan = %+v (%v), want %+v", live, err, original.Schedule)
	}
	if timerCancelled.Load() || !reflect.DeepEqual(s.logsSnapshot(), original.Logs) {
		t.Fatal("failed save changed the timer or published a new log")
	}
	persisted, err := openInspectionRepairStore(t, path).loadAccountInspection(t.Context())
	if err != nil || persisted == nil || !reflect.DeepEqual(
		persisted.Schedule, original.Schedule,
	) || len(persisted.Logs) != 1 {
		t.Fatalf("reopened plan: %+v (%v)", persisted, err)
	}
}

func TestAccountInspectionScheduleValidatesIntegerAndDurationBounds(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("duration and JSON integer boundary exceeds a 32-bit config integer")
	}
	path := filepath.Join(t.TempDir(), "inspection.db")
	store := openInspectionRepairStore(t, path)
	h := &Handler{stateStore: store}
	s := h.inspectionService()
	defer s.close()
	router := inspectionRepairRouter(h)
	before := s.persistenceSnapshot()
	for _, body := range []string{
		`{"interval_seconds":59}`, `{"timeout_seconds":-1}`, `{"max_concurrency":1.5}`, `{"retention_runs":-2}`,
		fmt.Sprintf(`{"interval_seconds":%d}`, quota.MaxRefreshIntervalSeconds+1),
		fmt.Sprintf(`{"timeout_seconds":%d}`, quota.MaxRefreshIntervalSeconds+1),
		fmt.Sprintf(`{"max_concurrency":%d}`, maxAccountInspectionInteger+1),
		fmt.Sprintf(`{"retention_runs":%d}`, maxAccountInspectionInteger+1),
	} {
		response := inspectionPreconditionRequest(router, http.MethodPut, "schedule", body)
		if response.Code != http.StatusBadRequest || !reflect.DeepEqual(s.persistenceSnapshot(), before) {
			t.Fatalf("invalid plan %s changed state or returned %d: %s", body, response.Code, response.Body)
		}
	}
	maximum := fmt.Sprintf(
		`{"interval_seconds":%d,"timeout_seconds":%d,"max_concurrency":0}`, quota.MaxRefreshIntervalSeconds,
		quota.MaxRefreshIntervalSeconds,
	)
	response := inspectionPreconditionRequest(router, http.MethodPut, "schedule", maximum)
	if response.Code != http.StatusOK {
		t.Fatalf("valid maximum: %d %s", response.Code, response.Body)
	}
	persisted, err := store.loadAccountInspection(t.Context())
	if err != nil || persisted == nil || int64(persisted.Schedule.IntervalSeconds) != quota.MaxRefreshIntervalSeconds ||
		int64(persisted.Schedule.TimeoutSeconds) != quota.MaxRefreshIntervalSeconds || persisted.Schedule.MaxConcurrency != before.Schedule.MaxConcurrency {
		t.Fatalf("maximum and omitted-field contract: %+v (%v)", persisted, err)
	}

	persisted.Schedule.IntervalSeconds++
	persisted.Schedule.TimeoutSeconds++
	if err := store.saveAccountInspection(t.Context(), *persisted); err != nil {
		t.Fatal(err)
	}
	recovered := newAccountInspectionService(h)
	defer recovered.close()
	if got := recovered.scheduleSnapshot(); got.IntervalSeconds != before.Schedule.IntervalSeconds || got.TimeoutSeconds != before.Schedule.TimeoutSeconds {
		t.Fatalf("reloaded unrepresentable duration: %+v", got)
	}
}

func TestAccountInspectionPreservesUnfinishedResults(t *testing.T) {
	for _, action := range []string{"stop", "timeout"} {
		t.Run(
			action, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				var probes atomic.Int32
				waiting := make(chan string, 1)
				scheduler := quota.NewSchedulerWithContext(
					quota.DefaultConfig(), func(ctx context.Context, entry *quota.Entry) ([]byte, error) {
						if probes.Add(1) == 1 {
							return []byte(`{"usage":"ok"}`), nil
						}
						waiting <- entry.FileName
						<-ctx.Done()
						return nil, ctx.Err()
					},
				)
				store := openInspectionRepairStore(t, filepath.Join(t.TempDir(), "inspection.db"))
				h := &Handler{stateStore: store, authManager: manager, quotaScheduler: scheduler}
				s := h.inspectionService()
				defer s.close()
				oldTime := time.Now().UTC().Add(-time.Hour)
				for _, id := range []string{"first-synthetic", "second-synthetic"} {
					auth, err := manager.Register(
						t.Context(),
						&coreauth.Auth{
							ID: id, FileName: id + ".json", Provider: "codex", Status: coreauth.StatusActive,
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					scheduler.Register(auth.FileName, quota.TypeCodex, auth.Index)
					s.results = append(
						s.results, accountInspectionResult{
							ID: id, Account: id, FileName: auth.FileName, AuthIndex: auth.Index, Provider: "codex",
							CheckedAt: oldTime, ProbeStatus: accountProbePassed,
						},
					)
				}
				schedule := defaultAccountInspectionSchedule()
				schedule.MaxConcurrency = 1
				if action == "timeout" {
					schedule.TimeoutSeconds = 1
				}
				if _, err := s.updateSchedule(schedule); err != nil {
					t.Fatal(err)
				}
				router := inspectionRepairRouter(h)
				if response := inspectionPreconditionRequest(
					router, http.MethodPost, "run", "",
				); response.Code != http.StatusAccepted {
					t.Fatalf("start inspection: %d %s", response.Code, response.Body)
				}
				var untouchedFile string
				select {
				case untouchedFile = <-waiting:
				case <-time.After(3 * time.Second):
					t.Fatal("second check did not start")
				}
				wantReason := "account inspection timed out"
				if action == "stop" {
					wantReason = "account inspection stopped by request"
					if response := inspectionPreconditionRequest(
						router, http.MethodPost, "stop", "",
					); response.Code != http.StatusAccepted {
						t.Fatalf("stop inspection: %d %s", response.Code, response.Body)
					}
				}
				state := waitInspectionPreconditionStatus(t, s, accountInspectionStatusStopped)
				if state.LastRun.Error != wantReason || state.LastRun.Checked != 1 || len(state.Results) != 2 {
					t.Fatalf("incomplete run changed the saved result set: %+v", state)
				}
				for _, row := range state.Results {
					if row.CheckedAt.Equal(oldTime) != (row.FileName == untouchedFile) || row.ProbeStatus != accountProbePassed {
						t.Fatalf(
							"result did not reflect whether the check completed: %+v (unfinished %s)", row,
							untouchedFile,
						)
					}
				}
				persisted, err := store.loadAccountInspection(t.Context())
				if err != nil || persisted == nil || !reflect.DeepEqual(
					persisted.Results, state.Results,
				) || persisted.LastRun.Error != wantReason {
					t.Fatalf("read interrupted run from SQLite: %+v (%v)", persisted, err)
				}
				response := inspectionPreconditionRequest(router, http.MethodGet, "status", "")
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), wantReason) {
					t.Fatalf("operator-visible stop reason: %d %s", response.Code, response.Body)
				}
			},
		)
	}
}

func TestAccountInspectionRowRequiresExactEligibleTarget(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{authManager: manager}
	s := h.inspectionService()
	defer s.close()
	for _, data := range [][3]string{
		{"codex-main", "shared.json", "codex"}, {"codex-other", "shared.json", "codex"},
		{"claude-main", "claude.json", "claude"},
	} {
		if _, err := manager.Register(
			t.Context(), &coreauth.Auth{ID: data[0], FileName: data[1], Provider: data[2]},
		); err != nil {
			t.Fatal(err)
		}
	}
	schedule := defaultAccountInspectionSchedule()
	schedule.Providers = []string{"codex"}
	if _, err := s.updateSchedule(schedule); err != nil {
		t.Fatal(err)
	}
	router := inspectionRepairRouter(h)
	for _, tc := range []struct {
		body  string
		code  int
		cause string
	}{
		{`{"id":"deleted"}`, http.StatusNotFound, "account not found"},
		{`{"file_name":"shared.json"}`, http.StatusConflict, "matches more than one credential"},
		{`{"id":"codex-main","file_name":"different.json"}`, http.StatusNotFound, "account not found"},
		{`{"id":"claude-main"}`, http.StatusUnprocessableEntity, "excluded from the inspection schedule"},
	} {
		response := inspectionPreconditionRequest(router, http.MethodPost, "inspect-one", tc.body)
		if response.Code != tc.code || !strings.Contains(response.Body.String(), tc.cause) {
			t.Fatalf("inspect-one %s = %d %s", tc.body, response.Code, response.Body)
		}
	}
	if current := s.statusSnapshot(); current.LastRun != nil || current.Status != accountInspectionStatusIdle {
		t.Fatalf("rejection queued a run: %+v", current)
	}
	auth, _ := manager.GetByID("codex-main")
	body := fmt.Sprintf(`{"id":%q,"file_name":%q,"auth_index":%q}`, auth.ID, auth.FileName, auth.Index)
	if response := inspectionPreconditionRequest(
		router, http.MethodPost, "inspect-one", body,
	); response.Code != http.StatusAccepted {
		t.Fatalf("exact target: %d %s", response.Code, response.Body)
	}
	state := waitInspectionPreconditionStatus(t, s, accountInspectionStatusCompleted)
	if state.LastRun.Checked != 1 || len(state.Results) != 1 || state.Results[0].ID != auth.ID {
		t.Fatalf("row action probed the wrong credentials: %+v", state)
	}
}
