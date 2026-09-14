package management

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestManagementStateFilesUsePrivatePermissions(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "management-state.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatalf("write management state fixture: %v", err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatalf("chmod management state fixture: %v", err)
	}

	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()

	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("stat %s: %v", path, errStat)
		}
		if got := info.Mode().Perm(); got != managementStateFileMode {
			t.Fatalf("%s mode = %o, want %o", path, got, managementStateFileMode)
		}
	}
}

func TestAccountInspectionStatePersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))

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
		ID:               "auth-1",
		Account:          "claude-user.json",
		FileName:         "claude-user.json",
		Provider:         "claude",
		AuthIndex:        "auth-1",
		Status:           accountHealthUnavailable,
		Reason:           "quota_exceeded",
		Advice:           "wait_quota_recovery",
		AutoHandling:     "quota_exceeded_skip_auto_refresh",
		ProbeStatus:      accountProbePassed,
		AuthGeneration:   7,
		CredentialDigest: strings.Repeat("a", 64),
		ProbeRevision:    accountInspectionProbeRevision,
		CheckedAt:        checkedAt,
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

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	gotSchedule := h2.inspectionService().scheduleSnapshot()
	if gotSchedule.IntervalSeconds != schedule.IntervalSeconds || gotSchedule.MaxConcurrency != schedule.MaxConcurrency {
		t.Fatalf("schedule = %+v, want %+v", gotSchedule, schedule)
	}
	results, total, _, _ := h2.inspectionService().resultsSnapshot("", false, true, 1, 10)
	if total != 1 || len(results) != 1 || results[0].Reason != "quota_exceeded" ||
		results[0].ProbeStatus != accountProbePassed || results[0].AuthGeneration != 7 ||
		results[0].CredentialDigest != strings.Repeat("a", 64) ||
		results[0].ProbeRevision != accountInspectionProbeRevision {
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

func TestQuotaStatePersistsProviderAwareEntriesAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return []byte(`{"provider":"` + string(entry.Type) + `"}`), nil
		},
	)
	scheduler.Register("shared.json", quota.TypeClaude, "claude-auth")
	scheduler.Register("shared.json", quota.TypeCodex, "codex-auth")
	h.SetQuotaScheduler(scheduler)
	scheduler.RefreshNow([]string{"shared.json"})
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	scheduler2 := quota.NewScheduler(quota.DefaultConfig(), nil)
	scheduler2.Register("shared.json", quota.TypeClaude, "claude-auth")
	scheduler2.Register("shared.json", quota.TypeCodex, "codex-auth")
	h2.SetQuotaScheduler(scheduler2)

	status := scheduler2.GetStatus()
	for _, credType := range []quota.CredentialType{quota.TypeClaude, quota.TypeCodex} {
		entry := status.Credentials[quota.EntryKey(credType, "shared.json")]
		if entry == nil {
			t.Fatalf("missing %s quota entry", credType)
		}
		if entry.Status != quota.StatusSuccess {
			t.Fatalf("%s status = %s, want %s", credType, entry.Status, quota.StatusSuccess)
		}
		if !bytes.Contains(entry.Data, []byte(credType)) {
			t.Fatalf("%s data = %s", credType, string(entry.Data))
		}
	}
}

func TestManagementStateMigratesQuotaFreshnessFromV3(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "management-state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v3 fixture: %v", err)
	}
	lastRefresh := time.Date(2026, 9, 9, 2, 3, 4, 0, time.UTC).Format(time.RFC3339Nano)
	statements := []string{
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
		"INSERT INTO meta(key, value) VALUES('schema_version', '3')",
		`CREATE TABLE quota_entries_v2 (
			entry_key TEXT PRIMARY KEY,
			file_name TEXT NOT NULL,
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
		`INSERT INTO quota_entries_v2(
			entry_key, file_name, type, auth_index, status, last_refresh,
			next_refresh, error, failure_count, data, disabled, updated_at
		 ) VALUES(
			'claude::claude.json', 'claude.json', 'claude', 'auth-1', 'success',
			'` + lastRefresh + `', NULL, '', 0, '{"remaining":5}', 0, '` + lastRefresh + `'
		)`,
	}
	for _, statement := range statements {
		if _, err = db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("prepare v3 fixture: %v", err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close v3 fixture: %v", err)
	}

	store, err := openManagementStateStorePath(dbPath)
	if err != nil {
		t.Fatalf("migrate v3 management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	entries, err := store.loadQuotaEntries(context.Background())
	if err != nil {
		t.Fatalf("load migrated quota entries: %v", err)
	}
	entry := entries[quota.EntryKey(quota.TypeClaude, "claude.json")]
	if entry.Generation != 1 || entry.StateRevision != 1 || entry.Tombstone ||
		entry.Freshness != quota.FreshnessFresh || entry.ObservedAt == nil ||
		entry.FetchedAt == nil || entry.LastSuccessAt == nil || string(entry.Data) != `{"remaining":5}` {
		t.Fatalf("migrated quota entry = %#v", entry)
	}
	var version string
	if err = store.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != managementStateSchemaVersion {
		t.Fatalf("schema version = %s, want %s", version, managementStateSchemaVersion)
	}
}

func TestQuotaTombstonePersistsAndRejectsOlderGeneration(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}

	key := quota.EntryKey(quota.TypeClaude, "claude.json")
	active := quota.EntryState{
		FileName:           "claude.json",
		Type:               quota.TypeClaude,
		AuthIndex:          "auth-1",
		CredentialRevision: "digest-1",
		Generation:         5,
		StateRevision:      3,
		Status:             quota.StatusSuccess,
		Freshness:          quota.FreshnessFresh,
		Data:               []byte(`{"remaining":5}`),
	}
	if err = store.saveQuotaEntry(context.Background(), active); err != nil {
		t.Fatalf("save active quota: %v", err)
	}
	tombstone := active
	tombstone.Generation = 6
	tombstone.StateRevision = 1
	tombstone.Tombstone = true
	if err = store.deleteQuotaEntry(context.Background(), tombstone); err != nil {
		t.Fatalf("save quota tombstone: %v", err)
	}
	stale := active
	stale.StateRevision = 99
	if err = store.saveQuotaEntry(context.Background(), stale); err != nil {
		t.Fatalf("save stale quota state: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}

	store, err = openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("reopen management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	entries, err := store.loadQuotaEntries(context.Background())
	if err != nil {
		t.Fatalf("load quota entries: %v", err)
	}
	persisted := entries[key]
	if !persisted.Tombstone || persisted.Generation != 6 || persisted.StateRevision != 1 || len(persisted.Data) != 0 {
		t.Fatalf("persisted quota tombstone = %#v", persisted)
	}

	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	scheduler.Restore(entries)
	scheduler.RegisterBinding("claude.json", quota.TypeClaude, "auth-1", "digest-1")
	entry := scheduler.GetStatus().Credentials[key]
	if entry == nil || entry.Generation != 7 || entry.Freshness != quota.FreshnessOutdated {
		t.Fatalf("reactivated quota entry = %#v", entry)
	}
}

func TestRateLimitStatePersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	now := time.Now()
	for i := range managementMaxFailures {
		h.recordFailedManagementAttempt(
			"198.51.100.20",
			managementFailureFingerprint("invalid-credential", strconv.Itoa(i)),
			now.Add(time.Duration(i)*time.Millisecond),
		)
	}
	if _, banned := h.currentRateLimitBan("198.51.100.20", time.Now()); !banned {
		t.Fatal("rate limit ban was not created")
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	if _, banned := h2.currentRateLimitBan("198.51.100.20", time.Now()); !banned {
		t.Fatal("persisted rate limit ban was not restored")
	}
	h2.attemptsMu.Lock()
	events := append([]rateLimitEventEntry(nil), h2.rateLimitEvents...)
	h2.attemptsMu.Unlock()
	if len(events) != 1 || events[0].Type != rateLimitEventBanStarted || events[0].BanCount != 1 {
		t.Fatalf("restored rate limit events = %+v", events)
	}
	if !h2.rateLimitPersistenceAvailable.Load() {
		t.Fatal("rate limit persistence should be available")
	}
}

func TestRateLimitStateRecordsOfflineExpiryOnce(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	now := time.Now()
	bannedUntil := now.Add(-time.Minute)
	state := rateLimitPersistedState{
		Revision: 1,
		ActiveBans: []rateLimitPersistedBan{
			{
				IP:           "198.51.100.21",
				BlockedUntil: bannedUntil,
				LastActivity: now.Add(-time.Hour),
				BanCount:     2,
			},
		},
		Events: []rateLimitEventEntry{
			{
				Type:        rateLimitEventBanStarted,
				IP:          "198.51.100.21",
				OccurredAt:  now.Add(-31 * time.Minute),
				BannedUntil: bannedUntil,
				BanCount:    2,
			},
		},
	}
	if err := store.saveRateLimitState(context.Background(), state); err != nil {
		t.Fatalf("save rate limit state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}

	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	if _, banned := h.currentRateLimitBan("198.51.100.21", time.Now()); banned {
		t.Fatal("expired rate limit ban was restored as active")
	}
	h.attemptsMu.Lock()
	events := append([]rateLimitEventEntry(nil), h.rateLimitEvents...)
	h.attemptsMu.Unlock()
	if len(events) != 2 || events[1].Type != rateLimitEventAutoExpired {
		t.Fatalf("events after restore = %+v", events)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	h2.attemptsMu.Lock()
	autoExpiryEvents := 0
	for _, event := range h2.rateLimitEvents {
		if event.Type == rateLimitEventAutoExpired {
			autoExpiryEvents++
		}
	}
	h2.attemptsMu.Unlock()
	if autoExpiryEvents != 1 {
		t.Fatalf("automatic expiry events = %d, want 1", autoExpiryEvents)
	}
}

func TestRateLimitStateRejectsOlderRevision(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	defer func() { _ = store.Close() }()

	newer := rateLimitPersistedState{
		Revision: 2,
		Attempts: []rateLimitPersistedAttempt{{IP: "198.51.100.30", PendingNoticeFailures: 2}},
	}
	older := rateLimitPersistedState{
		Revision: 1,
		Attempts: []rateLimitPersistedAttempt{{IP: "198.51.100.31", PendingNoticeFailures: 1}},
	}
	if err = store.saveRateLimitState(context.Background(), newer); err != nil {
		t.Fatalf("save newer rate limit state: %v", err)
	}
	if err = store.saveRateLimitState(context.Background(), older); err != nil {
		t.Fatalf("save older rate limit state: %v", err)
	}
	stored, err := store.loadRateLimitState(context.Background())
	if err != nil {
		t.Fatalf("load rate limit state: %v", err)
	}
	if stored == nil || stored.Revision != newer.Revision || len(stored.Attempts) != 1 || stored.Attempts[0].IP != newer.Attempts[0].IP {
		t.Fatalf("stored rate limit state = %#v, want revision %d", stored, newer.Revision)
	}
}

func TestRateLimitPersistenceRecoversCorruptStateOnNextChange(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}
	if _, err = store.db.ExecContext(
		context.Background(),
		`INSERT INTO state_items(key, value, updated_at) VALUES('management_rate_limit', ?, ?)`,
		[]byte("{"),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		_ = store.Close()
		t.Fatalf("insert corrupt rate limit state: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}

	clientIP := "198.51.100.30"
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h.Close() }()
	if h.rateLimitPersistenceAvailable.Load() {
		t.Fatal("corrupt rate limit state was reported as available")
	}
	h.recordFailedManagementAttempt(
		clientIP,
		managementFailureFingerprint("invalid-credential", "bad-key"),
		time.Now(),
	)
	waitForRateLimitCondition(
		t, time.Second, func() bool {
			state, errLoad := h.stateStore.loadRateLimitState(context.Background())
			return errLoad == nil && state != nil && len(state.Attempts) == 1 &&
				state.Attempts[0].IP == clientIP
		},
	)
	if !h.rateLimitPersistenceAvailable.Load() {
		t.Fatal("rate limit persistence did not recover after replacing corrupt state")
	}
}

func TestRateLimitFailureWindowPersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	clientIP := "198.51.100.32"
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	now := time.Now()
	for i := range managementMaxFailures - 1 {
		h.recordFailedManagementAttempt(
			clientIP,
			managementFailureFingerprint("invalid-credential", strconv.Itoa(i)),
			now.Add(time.Duration(i)*time.Millisecond),
		)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close first handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	h2.attemptsMu.Lock()
	restored := h2.failedAttempts[clientIP]
	failureCount := 0
	noticeCount := 0
	if restored != nil {
		failureCount = len(restored.failures)
		noticeCount = restored.pendingNoticeFailures
	}
	h2.attemptsMu.Unlock()
	if failureCount != managementMaxFailures-1 || noticeCount != managementMaxFailures-1 {
		t.Fatalf("restored failures=%d notices=%d, want %d", failureCount, noticeCount, managementMaxFailures-1)
	}

	h2.recordFailedManagementAttempt(
		clientIP,
		managementFailureFingerprint("invalid-credential", "final"),
		time.Now(),
	)
	if _, banned := h2.currentRateLimitBan(clientIP, time.Now()); !banned {
		t.Fatal("restored failure window did not trigger the next ban")
	}
}

func TestRateLimitNoticeConsumptionPersistsAcrossRestart(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	clientIP := "198.51.100.35"
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	h.recordFailedManagementAttempt(
		clientIP,
		managementFailureFingerprint("invalid-credential", "bad-key"),
		time.Now(),
	)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/management/rate-limit?consume_failed_login_notices=true",
		nil,
	)
	h.GetRateLimitStatus(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("consume notices status = %d, want 200", recorder.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close first handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	h2.attemptsMu.Lock()
	restored := h2.failedAttempts[clientIP]
	pendingNotices := 0
	if restored != nil {
		pendingNotices = restored.pendingNoticeFailures
	}
	h2.attemptsMu.Unlock()
	if pendingNotices != 0 {
		t.Fatalf("restored pending notices = %d, want 0", pendingNotices)
	}
}

func TestRateLimitManualUnbanPersistsAcrossRestart(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	clientIP := "198.51.100.33"
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	now := time.Now()
	for i := range managementMaxFailures {
		h.recordFailedManagementAttempt(
			clientIP,
			managementFailureFingerprint("invalid-credential", strconv.Itoa(i)),
			now.Add(time.Duration(i)*time.Millisecond),
		)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodDelete,
		"/management/rate-limit/ban",
		strings.NewReader(`{"ip":"`+clientIP+`"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.DeleteRateLimitBan(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("manual unban status = %d, want 200", recorder.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close first handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	if _, banned := h2.currentRateLimitBan(clientIP, time.Now()); banned {
		t.Fatal("manually removed ban was restored after restart")
	}
	h2.attemptsMu.Lock()
	manualUnbans := 0
	for _, event := range h2.rateLimitEvents {
		if event.Type == rateLimitEventManualUnban && event.IP == clientIP {
			manualUnbans++
		}
	}
	h2.attemptsMu.Unlock()
	if manualUnbans != 1 {
		t.Fatalf("manual unban events = %d, want 1", manualUnbans)
	}
}

func TestRateLimitPersistenceRetriesLatestSnapshot(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(
		&config.Config{},
		configPath,
		nil,
		withEnvSecret(""),
		func(h *Handler) {
			h.rateLimitPersistenceTimeout = 20 * time.Millisecond
			h.rateLimitPersistenceRetryDelay = 10 * time.Millisecond
		},
	)
	defer func() { _ = h.Close() }()
	h.stateStore.db.SetMaxOpenConns(1)
	lockTx, err := h.stateStore.db.Begin()
	if err != nil {
		t.Fatalf("begin locking transaction: %v", err)
	}
	if _, err = lockTx.Exec(
		"INSERT INTO meta(key, value) VALUES('rate_limit_test_lock', '1') ON CONFLICT(key) DO UPDATE SET value=excluded.value",
	); err != nil {
		_ = lockTx.Rollback()
		t.Fatalf("acquire SQLite write lock: %v", err)
	}

	now := time.Now()
	h.recordFailedManagementAttempt(
		"198.51.100.34",
		managementFailureFingerprint("invalid-credential", "first"),
		now,
	)
	waitForRateLimitCondition(
		t, time.Second, func() bool {
			return !h.rateLimitPersistenceAvailable.Load()
		},
	)
	h.recordFailedManagementAttempt(
		"198.51.100.34",
		managementFailureFingerprint("invalid-credential", "second"),
		now.Add(time.Millisecond),
	)
	if err = lockTx.Rollback(); err != nil {
		t.Fatalf("release SQLite write lock: %v", err)
	}

	waitForRateLimitCondition(
		t, time.Second, func() bool {
			state, errLoad := h.stateStore.loadRateLimitState(context.Background())
			return errLoad == nil && state != nil && len(state.Attempts) == 1 && len(state.Attempts[0].Failures) == 2
		},
	)
	if !h.rateLimitPersistenceAvailable.Load() {
		t.Fatal("rate limit persistence did not recover after retry")
	}
}

func waitForRateLimitCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("timed out waiting for rate limit state")
	}
}

func TestQuotaStatePersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return []byte(`{"remaining":42}`), nil
		},
	)
	scheduler.Register("claude-user.json", quota.TypeClaude, "auth-1")
	h.SetQuotaScheduler(scheduler)
	scheduler.RefreshNow([]string{"claude-user.json"})
	scheduler.SetDisabled("claude-user.json", quota.TypeClaude, true)
	if err := h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = h2.Close() }()
	scheduler2 := quota.NewScheduler(quota.DefaultConfig(), nil)
	scheduler2.Register("claude-user.json", quota.TypeClaude, "auth-1")
	h2.SetQuotaScheduler(scheduler2)
	entry := scheduler2.GetStatus().Credentials[quota.EntryKey(quota.TypeClaude, "claude-user.json")]
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
