package management

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestUsageDataDirMigrationPersistsChainAndMatchesStartupDestination(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("port: 8317\nusage-data-dir: %q\n", a)), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{Port: 8317, UsageDataDir: a}, path, nil, withEnvSecret(""))
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close migration handler: %v", err)
		}
	}(h.Close)
	for _, destination := range []string{b, c} {
		ctx, rec := newArtifactCacheTestContext(
			http.MethodPut, "/config.yaml", fmt.Sprintf("port: 8317\nusage-data-dir: %q\n", destination),
		)
		h.PutConfigYAML(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	pending, err := PendingUsageDataDirMigration(context.Background(), path, c)
	if err != nil || pending == nil || pending.From != a || pending.To != c {
		t.Fatalf("pending=%+v error=%v", pending, err)
	}
	mismatch, err := PendingUsageDataDirMigration(context.Background(), path, b)
	if err != nil || mismatch != nil {
		t.Fatalf("mismatch=%+v error=%v", mismatch, err)
	}
	if err := CompleteUsageDataDirMigration(context.Background(), path, *pending); err != nil {
		t.Fatal(err)
	}
	pending, err = PendingUsageDataDirMigration(context.Background(), path, c)
	if err != nil || pending != nil {
		t.Fatalf("completed=%+v error=%v", pending, err)
	}
}
func TestUsageDataDirMigrationFailureRejectsConfigAndFailedWriteRestoresState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.WriteFile(path, []byte("[malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{UsageDataDir: a}, path, nil, withEnvSecret(""))
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close failed migration handler: %v", err)
		}
	}(h.Close)
	if _, err := h.persistConfigCandidate(&config.Config{UsageDataDir: b}); err == nil {
		t.Fatal("malformed source accepted")
	}
	pending, err := PendingUsageDataDirMigration(context.Background(), path, b)
	if err != nil || pending != nil {
		t.Fatalf("failed write left migration=%+v error=%v", pending, err)
	}
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.stateStore.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.persistConfigCandidate(&config.Config{UsageDataDir: b}); err == nil {
		t.Fatal("configuration accepted without durable migration record")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "port: 8317\n" {
		t.Fatalf("configuration changed=%s error=%v", data, err)
	}
}

func TestUsageDataMigrationRollbackRetainsIntentAfterConfigPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("usage-data-dir: %q\n", a)), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{UsageDataDir: a}, path, nil, withEnvSecret(""))
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close migration rollback handler: %v", err)
		}
	}(h.Close)
	rollback, err := h.prepareUsageDataDirMigration(&config.Config{UsageDataDir: b})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("usage-data-dir: %q\n", b)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingUsageDataDirMigration(context.Background(), path, b)
	if err != nil || pending == nil || pending.From != a {
		t.Fatalf("pending=%+v error=%v", pending, err)
	}
}
