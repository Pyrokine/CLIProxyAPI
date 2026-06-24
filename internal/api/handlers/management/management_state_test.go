package management

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAccountInspectionStatePersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))

	schedule := accountInspectionSchedule{
		Enabled:         false,
		IntervalSeconds: 120,
		Providers:       []string{"claude"},
		MaxConcurrency:  2,
		TimeoutSeconds:  10,
		RetentionRuns:   5,
	}
	if _, err := h.inspectionService().updateSchedule(schedule); err != nil {
		t.Fatalf("update schedule: %v", err)
	}

	checkedAt := time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC)
	endedAt := checkedAt.Add(2 * time.Second)
	run := accountInspectionRun{
		ID:        "run-1",
		StartedAt: checkedAt,
		EndedAt:   &endedAt,
		Duration:  2000,
		Status:    accountInspectionStatusCompleted,
		Checked:   1,
		Summary:   accountInspectionSummary{Total: 1, Abnormal: 1},
	}
	result := accountInspectionResult{
		ID:           "auth-1",
		Account:      "claude-user.json",
		FileName:     "claude-user.json",
		Provider:     "claude",
		AuthIndex:    "auth-1",
		Status:       accountHealthUnavailable,
		Reason:       "quota_exceeded",
		Advice:       "wait_quota_recovery",
		AutoHandling: "quota_exceeded_skip_auto_refresh",
		CheckedAt:    checkedAt,
	}
	svc := h.inspectionService()
	svc.mu.Lock()
	svc.results = []accountInspectionResult{result}
	svc.runs = []accountInspectionRun{run}
	svc.lastRun = &run
	svc.status = accountInspectionStatusCompleted
	svc.updatedAt = endedAt
	svc.addLogLocked("info", "persisted inspection log")
	svc.mu.Unlock()
	svc.persist()
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	defer func() { _ = h2.Close() }()
	gotSchedule := h2.inspectionService().scheduleSnapshot()
	if gotSchedule.IntervalSeconds != schedule.IntervalSeconds || gotSchedule.MaxConcurrency != schedule.MaxConcurrency {
		t.Fatalf("schedule = %+v, want %+v", gotSchedule, schedule)
	}
	results, total, _, _ := h2.inspectionService().resultsSnapshot("", false, true, 1, 10)
	if total != 1 || len(results) != 1 || results[0].Reason != "quota_exceeded" {
		t.Fatalf("results total=%d rows=%+v", total, results)
	}
	logs := h2.inspectionService().logsSnapshot()
	if len(logs) == 0 || logs[len(logs)-1].Message != "persisted inspection log" {
		t.Fatalf("logs = %+v", logs)
	}
	status := h2.inspectionService().statusSnapshot()
	if status.LastRun == nil || status.LastRun.ID != "run-1" {
		t.Fatalf("last run = %+v", status.LastRun)
	}
}

func TestAccountInspectionSkipsDisabledAuthsByDefault(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	disabled := &coreauth.Auth{
		ID:       "disabled-auth",
		Provider: "claude",
		FileName: "disabled.json",
		Status:   coreauth.StatusDisabled,
		Disabled: true,
	}
	active := &coreauth.Auth{
		ID:       "active-auth",
		Provider: "claude",
		FileName: "active.json",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), disabled); err != nil {
		t.Fatalf("register disabled auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register active auth: %v", err)
	}

	h := &Handler{authManager: manager}
	service := h.inspectionService()
	results := service.inspectAccounts(context.Background(), "")
	if len(results) != 1 || results[0].ID != "active-auth" {
		t.Fatalf("default inspection results = %+v, want only active auth", results)
	}

	explicit := service.inspectAccounts(context.Background(), "disabled-auth")
	if len(explicit) != 1 || explicit[0].Status != accountHealthDisabled {
		t.Fatalf("explicit disabled inspection results = %+v", explicit)
	}
}

func TestQuotaStatePersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return []byte(`{"remaining":42}`), nil
		},
	)
	scheduler.Register("claude-user.json", quota.TypeClaude, "auth-1")
	h.SetQuotaScheduler(scheduler)
	scheduler.RefreshNow([]string{"claude-user.json"})
	scheduler.SetDisabled("claude-user.json", true)
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	defer func() { _ = h2.Close() }()
	scheduler2 := quota.NewScheduler(quota.DefaultConfig(), nil)
	scheduler2.Register("claude-user.json", quota.TypeClaude, "auth-1")
	h2.SetQuotaScheduler(scheduler2)
	entry := scheduler2.GetStatus().Credentials["claude-user.json"]
	if entry == nil {
		t.Fatal("missing quota entry")
	}
	if entry.Status != quota.StatusSuccess {
		t.Fatalf("status = %s, want %s", entry.Status, quota.StatusSuccess)
	}
	if !bytes.Contains(entry.Data, []byte(`"remaining":42`)) {
		t.Fatalf("data = %s", string(entry.Data))
	}
	if !entry.Disabled {
		t.Fatal("disabled state was not restored")
	}
}
