package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestServiceShutdownFlushesSchedulerCheckpoint(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	store := coreauth.NewFileSchedulerCheckpointStore(authDir)
	if errStart := manager.StartSchedulerCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("StartSchedulerCheckpoint() error = %v", errStart)
	}
	service := &Service{coreManager: manager}
	if errShutdown := service.shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("shutdown() error = %v", errShutdown)
	}

	path := filepath.Join(authDir, ".scheduler-state")
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("Stat() error = %v", errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("checkpoint mode = %o, want 600", got)
	}
	if errStop := manager.StopSchedulerCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("second StopSchedulerCheckpoint() error = %v", errStop)
	}
}
