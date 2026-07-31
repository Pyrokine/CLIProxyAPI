package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapturedPersistedFileRollbackPreservesConcurrentReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	original := []byte(`{"type":"test","token":"old"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := ReadPersistedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	captured, matched, err := CaptureFileIfVersion(path, version)
	if err != nil || !matched {
		t.Fatalf("CaptureFileIfVersion() = (%v, %v)", matched, err)
	}
	newer := []byte(`{"type":"test","token":"new"}`)
	if err = os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = captured.Rollback(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(newer) {
		t.Fatalf("rollback replaced concurrent file: %s", stored)
	}
}

func TestCapturedPersistedFileCommitRemovesCapturedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"type":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := ReadPersistedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	captured, matched, err := CaptureFileIfVersion(path, version)
	if err != nil || !matched {
		t.Fatalf("CaptureFileIfVersion() = (%v, %v)", matched, err)
	}
	if err = captured.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("captured path still exists: %v", err)
	}
}
