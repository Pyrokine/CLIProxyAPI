package auth

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAuditDurableRecoveryVerifiesVersionActuallyCaptured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.json")
	const oldJSON = `{"token":"synthetic-old"}`
	const newJSON = `{"token":"synthetic-new"}`
	if err := os.WriteFile(path, []byte(oldJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := ReadPersistedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	journalPath, capturedPath := durablePersistedDeletionPaths(path)
	if err = writeDurablePersistedDeletionJournal(
		journalPath,
		durablePersistedDeletionJournal{Target: filepath.Base(path), SHA256: formatPersistedDigest(version)},
	); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, errRecover := RecoverDurablePersistedFileDeletions(dir)
		result <- errRecover
	}()
	var writer *os.File
	deadline := time.After(5 * time.Second)
	for writer == nil {
		fd, errOpen := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if errOpen == nil {
			writer = os.NewFile(uintptr(fd), path)
			break
		}
		if !errors.Is(errOpen, syscall.ENXIO) {
			t.Fatal(errOpen)
		}
		select {
		case errRecover := <-result:
			t.Fatalf("recovery stopped before version read: %v", errRecover)
		case <-deadline:
			t.Fatal("recovery did not begin version read")
		case <-time.After(time.Millisecond):
		}
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err = writer.WriteString(oldJSON); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.json")
	if err = os.WriteFile(replacement, []byte(newJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("recovery accepted a different captured version")
		}
	case <-deadline:
		t.Fatal("recovery did not finish after version replacement")
	}
	captured, err := os.ReadFile(capturedPath)
	if err != nil || string(captured) != newJSON {
		t.Fatalf("replacement capture=%s error=%v", captured, err)
	}
	if _, err = os.Stat(journalPath); err != nil {
		t.Fatalf("recovery journal was lost: %v", err)
	}
}

func TestAuditDurableCommitPreservesChangedCaptureAndJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.json")
	if err := os.WriteFile(path, []byte(`{"token":"synthetic-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := ReadPersistedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	deletion, matched, err := PrepareDurablePersistedFileDeletion(path, version)
	if err != nil || !matched || deletion == nil {
		t.Fatalf("prepare matched=%t error=%v", matched, err)
	}
	const replacement = `{"token":"synthetic-new"}`
	if err = os.WriteFile(deletion.capturedPath, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = deletion.Commit(); err == nil {
		t.Fatal("commit accepted changed capture")
	}
	raw, err := os.ReadFile(deletion.capturedPath)
	if err != nil || string(raw) != replacement {
		t.Fatalf("replacement capture=%s error=%v", raw, err)
	}
	if _, err = os.Stat(deletion.journalPath); err != nil {
		t.Fatalf("recovery journal was lost: %v", err)
	}
}
