package cliproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	managementHandlers "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestUsagePersistenceDataDirStaysUnopenedAfterMigrationFailure(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.yaml")
	target := filepath.Join(root, "target", "usage-data")
	service := &Service{
		cfg:        &config.Config{UsageDataDir: target},
		configPath: configPath,
		usageDataDirMigrator: func(gotConfigPath, gotTarget string) error {
			if gotConfigPath != configPath || gotTarget != target {
				t.Fatalf("migration args = (%q, %q), want (%q, %q)", gotConfigPath, gotTarget, configPath, target)
			}
			return errors.New("migration failed")
		},
	}

	if got, pending := service.usagePersistenceDataDir(context.Background()); got != "" || pending != nil {
		t.Fatalf("usagePersistenceDataDir() = (%q, %#v), want unopened target after migration failure", got, pending)
	}
	if _, errStat := os.Stat(filepath.Join(target, "events.db")); !os.IsNotExist(errStat) {
		t.Fatalf("target events.db was opened after migration failure: %v", errStat)
	}
}

func TestUsageStartupConsumesPersistedCustomDirectoryMigration(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	root := t.TempDir()
	t.Setenv("WRITABLE_PATH", root)
	ctx := t.Context()
	path := filepath.Join(root, "config.yaml")
	previous := filepath.Join(root, "custom-a")
	target := filepath.Join(root, "custom-b")
	authDir := filepath.Join(root, "auth")
	oldCfg := &config.Config{Port: 8317, AuthDir: authDir, UsageDataDir: previous}
	oldPersister := internalUsage.NewPersister(previous, oldCfg.UsageRetention)
	oldPersister.Start(ctx)
	t.Cleanup(oldPersister.Stop)
	if !oldPersister.IsReady() {
		t.Fatal("source SQLite did not become ready")
	}
	if !oldPersister.Record(
		internalUsage.FlatDetail{
			Timestamp: time.Now().UTC(), Model: "fixture-model", Provider: "fixture-provider",
			APIKey: "fixture-client-identity", AuthIndex: "fixture-auth", Source: "fixture.json",
			Completed: true, Generate: true,
		},
	) {
		t.Fatal("source usage event was rejected")
	}
	if err := oldPersister.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	oldYAML := fmt.Sprintf("port: 8317\nauth-dir: %q\nusage-data-dir: %q\n", authDir, previous)
	if err := os.WriteFile(path, []byte(oldYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := managementHandlers.NewHandler(oldCfg, path, nil)
	t.Cleanup(
		func() {
			if err := handler.Close(); err != nil {
				t.Error(err)
			}
		},
	)
	body := fmt.Sprintf("port: 8317\nauth-dir: %q\nusage-data-dir: %q\n", authDir, target)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", strings.NewReader(body))
	handler.PutConfigYAML(ginCtx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save target directory returned %d: %s", recorder.Code, recorder.Body.String())
	}
	service := &Service{cfg: &config.Config{UsageDataDir: target}, configPath: path}
	failedService := &Service{
		cfg: service.cfg, configPath: path,
		usageDataDirMigrator: func(string, string) error { return errors.New("fixture migration failure") },
	}
	if resolved, pending := failedService.usagePersistenceDataDir(ctx); resolved != "" || pending != nil {
		t.Fatalf("failed migration opened target: directory=%q pending=%#v", resolved, pending)
	}
	retained, err := managementHandlers.PendingUsageDataDirMigration(ctx, path, target)
	if err != nil || retained == nil || retained.From != previous {
		t.Fatalf("failed migration lost retry source: pending=%#v err=%v", retained, err)
	}
	resolved, pending := service.usagePersistenceDataDir(ctx)
	if resolved != target || pending == nil || pending.From != previous || pending.To != target {
		t.Fatalf("startup migration = (%q, %#v), want %q -> %q", resolved, pending, previous, target)
	}
	retained, err = managementHandlers.PendingUsageDataDirMigration(ctx, path, target)
	if err != nil || retained == nil {
		t.Fatalf("migration intent was cleared before SQLite became ready: pending=%#v err=%v", retained, err)
	}
	databasePath := filepath.Join(target, "events.db")
	preservedPath := filepath.Join(target, "events.db.preserved")
	if err := os.Rename(databasePath, preservedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, []byte("fixture invalid SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	unready := internalUsage.NewPersister(target, service.cfg.UsageRetention)
	unready.Start(ctx)
	t.Cleanup(unready.Stop)
	if unready.IsReady() {
		t.Fatal("invalid target SQLite was accepted")
	}
	unready.Stop()
	retained, err = managementHandlers.PendingUsageDataDirMigration(ctx, path, target)
	if err != nil || retained == nil || *retained != *pending {
		t.Fatalf("SQLite startup failure lost migration intent: pending=%#v err=%v", retained, err)
	}
	if err := os.Rename(preservedPath, databasePath); err != nil {
		t.Fatal(err)
	}
	persister := internalUsage.NewPersister(target, service.cfg.UsageRetention)
	persister.Start(ctx)
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("migrated SQLite did not become ready")
	}
	if err := managementHandlers.CompleteUsageDataDirMigration(ctx, path, *pending); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		},
	)
	var requests int
	if err := db.QueryRow("SELECT COUNT(*) FROM events").Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("custom directory usage history changed: requests=%d err=%v", requests, err)
	}
	remaining, err := managementHandlers.PendingUsageDataDirMigration(ctx, path, target)
	if err != nil || remaining != nil {
		t.Fatalf("successful migration intent remains: pending=%#v err=%v", remaining, err)
	}
}
