package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateDataDirIncludesPreviousCustomDirectory(t *testing.T) {
	root := t.TempDir()
	previous, next := filepath.Join(root, "custom-a"), filepath.Join(root, "custom-b")
	if err := os.MkdirAll(previous, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(filepath.Join(previous, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importBytes(
		context.Background(), store, nil,
		[]byte(`[{"timestamp":"2026-04-27T10:00:00Z","model":"retained","tokens":{"total_tokens":1}}]`),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDataDirIfNeeded(filepath.Join(root, "config.yaml"), next, previous); err != nil {
		t.Fatal(err)
	}
	migrated, err := openStore(filepath.Join(next, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	var count int
	if err := migrated.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("events=%d error=%v", count, err)
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatalf("previous directory still exists: %v", err)
	}
}

func TestMigrateDataDirRejectsExistingDestinationWithoutChangingEitherDatabase(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		store, err := openStore(filepath.Join(dir, "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := importBytes(
			context.Background(), store, nil,
			[]byte(`[{"timestamp":"2026-04-27T10:00:00Z","model":"retained","tokens":{"total_tokens":1}}]`),
		); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateDataDirIfNeeded(filepath.Join(root, "config.yaml"), b, a); err == nil {
		t.Fatal("existing source and destination were silently accepted")
	}
	for _, dir := range []string{a, b} {
		store, err := openStore(filepath.Join(dir, "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		var count int
		err = store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
		_ = store.Close()
		if err != nil || count != 1 {
			t.Fatalf("directory=%s requests=%d error=%v", dir, count, err)
		}
	}
}
func TestMigrateDataDirCanCompleteAfterRenameBeforeStateAcknowledgement(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.MkdirAll(b, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(filepath.Join(b, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDataDirIfNeeded(filepath.Join(root, "config.yaml"), b, a); err != nil {
		t.Fatal(err)
	}
}
