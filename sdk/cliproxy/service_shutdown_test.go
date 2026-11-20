package cliproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestShutdownContinuesAfterCallerTimeoutAndSharesFinalResult(t *testing.T) {
	service := &Service{}
	service.authQueueWorkers.Add(1)
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelFirst()
	if errShutdown := service.Shutdown(firstCtx); !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want deadline exceeded", errShutdown)
	}
	service.authQueueWorkers.Done()
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("second Shutdown() error = %v", errShutdown)
	}
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("third Shutdown() error = %v", errShutdown)
	}
}

func TestShutdownJoinsAllResourceErrors(t *testing.T) {
	service := &Service{
		watcher: &WatcherWrapper{
			stop: func() error {
				return coreauth.ErrAuthDurableStateUncertain
			},
		},
	}
	service.pluginRuntimeRetryWG.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	errShutdown := service.shutdown(ctx)
	service.pluginRuntimeRetryWG.Done()
	if !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("shutdown() error = %v, want deadline exceeded", errShutdown)
	}
	if !errors.Is(errShutdown, coreauth.ErrAuthDurableStateUncertain) {
		t.Fatalf("shutdown() error = %v, want durable-state uncertainty", errShutdown)
	}
}

func TestServiceShutdownKeepsUsageQueueDisabled(t *testing.T) {
	queue := redisqueue.NewQueue()
	queue.SetEnabled(true)
	service := &Service{
		cfg:        &config.Config{},
		usageQueue: queue,
	}
	if errShutdown := service.shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("shutdown() error = %v", errShutdown)
	}
	if !service.shuttingDown.Load() {
		t.Fatal("service shutdown state was not published")
	}
	if queue.Enabled() {
		t.Fatal("usage queue remained enabled after shutdown")
	}
	if errApply := service.applyConfigUpdateRuntime(
		context.Background(), &config.Config{Home: config.HomeConfig{Enabled: true}},
	); errApply == nil {
		t.Fatal("config update succeeded after shutdown")
	}
	if queue.Enabled() {
		t.Fatal("post-shutdown config update re-enabled the usage queue")
	}
}

func TestShutdownCancelsRepricingBeforeDrainingUsage(t *testing.T) {
	// 保留 5000 条真实写入，竞态检查下由测试进程限制总耗时，独立用例覆盖关闭超时
	original := serviceShutdownTimeout
	serviceShutdownTimeout = 25 * time.Minute
	defer func() { serviceShutdownTimeout = original }()
	root := t.TempDir()
	cfg := &config.Config{}
	persister := internalUsage.NewPersister(root, cfg.UsageRetention)
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) { return 1, 0, 0, true })
	persister.Start(t.Context())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("persister did not become ready")
	}
	if _, err := persister.Import(
		[]byte(`[{"timestamp":"2026-11-01T00:00:00Z","model":"shutdown-history",` +
			`"tokens":{"input_tokens":1000,"total_tokens":1000}}]`),
	); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "events.db")+"?_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	// The recursive SQL holds the actual maintenance write lock until its context is canceled.
	if _, err := db.Exec(
		`CREATE TRIGGER hold_repricing BEFORE UPDATE OF cost_micro ON events
		WHEN NEW.cost_micro<>OLD.cost_micro BEGIN
		SELECT SUM(n) FROM (WITH RECURSIVE seq(n) AS
		(SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<1000000000) SELECT n FROM seq); END`,
	); err != nil {
		t.Fatal(err)
	}
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) { return 2, 0, 0, true })
	if !persister.StartRecalculateCosts() {
		t.Fatal("repricing did not start")
	}
	wait, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := db.ExecContext(wait, "BEGIN IMMEDIATE")
		if err != nil {
			var coded interface{ Code() int }
			if errors.As(err, &coded) && coded.Code()&0xff == 5 {
				break
			}
			t.Fatalf("wait for repricing write lock: %v", err)
		}
		if _, err := db.ExecContext(wait, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-wait.Done():
			t.Fatal("repricing did not acquire its write lock")
		}
	}

	const queued = 5000
	manager := coreusage.NewManager(queued)
	capture := &serviceUsageCapture{records: make(chan string, queued)}
	manager.Register(capture)
	manager.Register(internalUsage.NewLoggerPlugin(persister, nil, func() bool { return true }))
	manager.Start(t.Context())
	t.Cleanup(
		func() {
			persister.Stop()
			manager.Stop()
		},
	)
	base := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	for i := range queued {
		record := coreusage.Record{
			Model: "shutdown-live", Provider: "codex", Completed: true,
			RequestedAt: base.Add(time.Duration(i)), RequestID: fmt.Sprintf("shutdown-%d", i),
			Detail: coreusage.Detail{InputTokens: 317, OutputTokens: 5, TotalTokens: 322},
		}
		if err := manager.Publish(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	// One writer batch and its full input channel leave later dispatcher callbacks waiting.
	for range 4197 {
		select {
		case <-capture.records:
		case <-wait.Done():
			t.Fatal("dispatcher did not reach writer backpressure")
		}
	}
	service := &Service{usageManager: manager, usagePersister: persister}
	shutdownCtx, cancelShutdown := service.newShutdownContext()
	defer cancelShutdown()
	shutdownErr := service.Shutdown(shutdownCtx)
	persister.Stop()
	manager.Stop()
	var persisted int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE model_id=(SELECT id FROM dim_model WHERE name='shutdown-live')",
	).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if shutdownErr != nil || persisted != queued {
		t.Fatalf("shutdown error=%v persisted=%d want=%d", shutdownErr, persisted, queued)
	}
	var historyCost int64
	if err := db.QueryRow(
		"SELECT cost_micro FROM events WHERE model_id=(SELECT id FROM dim_model WHERE name='shutdown-history')",
	).Scan(&historyCost); err != nil || historyCost != 1000 {
		t.Fatalf("canceled repricing changed historical cost: cost=%d error=%v", historyCost, err)
	}
}

func TestShutdownReportsUnavailableDatabaseWithinItsDeadline(t *testing.T) {
	root := t.TempDir()
	persister := internalUsage.NewPersister(root, (&config.Config{}).UsageRetention)
	persister.Start(t.Context())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("persister did not become ready")
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "events.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("ROLLBACK") }()
	if !persister.Record(internalUsage.FlatDetail{Timestamp: time.Now(), Model: "locked-shutdown"}) {
		t.Fatal("live event was not accepted")
	}
	original := serviceShutdownTimeout
	serviceShutdownTimeout = 50 * time.Millisecond
	defer func() { serviceShutdownTimeout = original }()
	service := &Service{usagePersister: persister}
	started := time.Now()
	if err := service.Shutdown(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unavailable database shutdown error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unavailable database ignored shutdown deadline: %v", elapsed)
	}
	if _, err := db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	persister.Stop()
	var events int
	if err := db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("accepted event after database recovery=%d error=%v", events, err)
	}
	if err := service.Shutdown(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shared final shutdown error=%v", err)
	}
}

func TestRunReturnsShutdownFailureInsteadOfNormalCancellation(t *testing.T) {
	original := serviceShutdownTimeout
	serviceShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serviceShutdownTimeout = original })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, AuthDir: t.TempDir()}
	cfg.Plugins.Dir = t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	service, err := NewBuilder().WithConfig(cfg).
		WithConfigPath(configPath).
		WithHooks(Hooks{OnBeforeStart: func(*config.Config) { cancel() }}).Build()
	if err != nil {
		t.Fatal(err)
	}
	manager := coreusage.NewManager(1)
	capture := &serviceUsageCapture{records: make(chan string)}
	manager.Register(capture)
	service.usageManager = manager
	if err := manager.Publish(t.Context(), coreusage.Record{Model: "blocked-shutdown"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			<-capture.records
			manager.Stop()
			<-service.shutdownDone
		},
	)
	if err := service.Run(ctx); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("Run must expose shutdown failure to its CLI caller: %v", err)
	}
}

func TestNewShutdownContextReturnsFreshContext(t *testing.T) {
	originalTimeout := serviceShutdownTimeout
	serviceShutdownTimeout = 20 * time.Millisecond
	defer func() {
		serviceShutdownTimeout = originalTimeout
	}()

	service := &Service{}
	ctx1, cancel1 := service.newShutdownContext()
	defer cancel1()
	<-ctx1.Done()

	ctx2, cancel2 := service.newShutdownContext()
	defer cancel2()
	if err := ctx2.Err(); err != nil {
		t.Fatalf("expected fresh shutdown context, got %v", err)
	}

	deadline1, ok1 := ctx1.Deadline()
	deadline2, ok2 := ctx2.Deadline()
	if !ok1 || !ok2 {
		t.Fatal("expected both shutdown contexts to have deadlines")
	}
	if !deadline2.After(deadline1) {
		t.Fatalf("expected second deadline after first, got first=%v second=%v", deadline1, deadline2)
	}
}
