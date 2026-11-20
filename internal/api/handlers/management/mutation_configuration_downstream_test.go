package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestMutationInspectionScheduleAcknowledgesPersistence(t *testing.T) {
	for _, mode := range []string{"persisted", "persistence_failed", "standalone", "validation"} {
		t.Run(
			mode, func(t *testing.T) {
				h := &Handler{}
				engine := resourceMutationEngine(t, h)
				serviceHandler := h
				if mode == "standalone" {
					serviceHandler = nil
				} else if mode == "persistence_failed" {
					closed, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "closed.db"))
					if err != nil {
						t.Fatal(err)
					}
					if err := closed.Close(); err != nil {
						t.Fatal(err)
					}
					serviceHandler = &Handler{stateStore: closed}
				}
				h.accountInspection = newAccountInspectionService(serviceHandler)
				t.Cleanup(h.accountInspection.close)
				engine.PUT("/v0/management/account-inspection/schedule", h.PutAccountInspectionSchedule)
				body := `{"enabled":false,"interval_seconds":300,"max_concurrency":2,` +
					`"timeout_seconds":20,"retention_runs":` +
					`10}`
				status, outcome := 200, "succeeded"
				kinds := []string{"configuration.runtime_applied", "configuration.persisted"}
				switch mode {
				case "persistence_failed":
					status, outcome, kinds = 503, "partial", kinds[:1]
				case "standalone":
					kinds = kinds[:1]
				case "validation":
					body, status, outcome, kinds = `{"interval_seconds":1}`, 400, "rejected", nil
				}
				callResourceMutation(
					t,
					engine,
					http.MethodPut,
					"/v0/management/account-inspection/schedule",
					body,
					status,
					outcome,
					kinds...,
				)
				if mode == "persisted" {
					stored, err := h.stateStore.loadAccountInspection(t.Context())
					if err != nil || stored == nil || stored.Schedule.IntervalSeconds != 300 {
						t.Fatalf("reported persistence did not reach SQLite: state=%+v err=%v", stored, err)
					}
				}
			},
		)
	}
}

func TestMutationYAMLCleanupWarningRemainsPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{Port: 8317}, configFilePath: path}
	h.configCommitted = func(ctx context.Context, _ ConfigCommit) error {
		ReportConfigCommitCleanupError(ctx, errors.New("synthetic cleanup failure"))
		return nil
	}
	engine := resourceMutationEngine(t, h)
	engine.PUT("/v0/management/config.yaml", h.PutConfigYAML)
	callResourceMutation(
		t, engine, http.MethodPut, "/v0/management/config.yaml", "port: 8318\n", 200, "partial",
		"configuration.persisted", "configuration.published", "configuration.runtime_applied",
	)
}

func TestMutationOAuthCallbackAcknowledgesOnlyLocalFile(t *testing.T) {
	dir := t.TempDir()
	const state = "synthetic-callback-mutation"
	registerOAuthSessionForAuthDir(state, "codex", "", dir)
	t.Cleanup(func() { cancelOAuthSession(state) })
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	engine := resourceMutationEngine(t, h)
	engine.POST("/v0/management/oauth-callback", h.PostOAuthCallback)
	body := `{"provider":"codex","state":"synthetic-callback-mutation","error":"synthetic-denied"}`
	callResourceMutation(
		t, engine, http.MethodPost, "/v0/management/oauth-callback", body, 200, "succeeded", "task.persisted",
	)
	if _, err := os.Stat(filepath.Join(dir, ".oauth-codex-"+state+".oauth")); err != nil {
		t.Fatal(err)
	}
	callResourceMutation(t, engine, http.MethodPost, "/v0/management/oauth-callback", body, 404, "rejected")
}
