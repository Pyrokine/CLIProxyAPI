package cliproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
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

	if got := service.usagePersistenceDataDir(); got != "" {
		t.Fatalf("usagePersistenceDataDir() = %q, want empty after migration failure", got)
	}
	if _, errStat := os.Stat(filepath.Join(target, "events.db")); !os.IsNotExist(errStat) {
		t.Fatalf("target events.db was opened after migration failure: %v", errStat)
	}
}
