package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverDurablePersistedFileDeletionResumesCapturedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if errWrite := os.WriteFile(path, []byte(`{"token":"old"}`), 0o600); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	version, errVersion := ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	deletion, matched, errPrepare := PrepareDurablePersistedFileDeletion(path, version)
	if errPrepare != nil || !matched || deletion == nil {
		t.Fatalf("PrepareDurablePersistedFileDeletion() = %#v, %v, %v", deletion, matched, errPrepare)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("live credential still exists: %v", errStat)
	}

	recovered, errRecover := RecoverDurablePersistedFileDeletions(dir)
	if errRecover != nil {
		t.Fatalf("RecoverDurablePersistedFileDeletions() error = %v", errRecover)
	}
	if len(recovered) != 1 || recovered[0].Path() != path || recovered[0].Version() != version {
		t.Fatalf("recovered deletions = %#v", recovered)
	}
	if errCommit := recovered[0].Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
	recovered, errRecover = RecoverDurablePersistedFileDeletions(dir)
	if errRecover != nil || len(recovered) != 0 {
		t.Fatalf("recover after commit = %#v, %v", recovered, errRecover)
	}
}

func TestRecoverDurablePersistedFileDeletionPreservesReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if errWrite := os.WriteFile(path, []byte(`{"token":"old"}`), 0o600); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	version, errVersion := ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if _, matched, errPrepare := PrepareDurablePersistedFileDeletion(path, version); errPrepare != nil || !matched {
		t.Fatalf("PrepareDurablePersistedFileDeletion() matched = %v, error = %v", matched, errPrepare)
	}
	if errWrite := os.WriteFile(path, []byte(`{"token":"new"}`), 0o600); errWrite != nil {
		t.Fatalf("write replacement: %v", errWrite)
	}

	recovered, errRecover := RecoverDurablePersistedFileDeletions(dir)
	if errRecover != nil || len(recovered) != 1 {
		t.Fatalf("RecoverDurablePersistedFileDeletions() = %#v, %v", recovered, errRecover)
	}
	if errCommit := recovered[0].Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil || string(raw) != `{"token":"new"}` {
		t.Fatalf("replacement = %q, %v", raw, errRead)
	}
}

func TestRecoverDurablePersistedFileDeletionCapturesJournaledLiveVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if errWrite := os.WriteFile(path, []byte(`{"token":"old"}`), 0o600); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	version, errVersion := ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	journalPath, _ := durablePersistedDeletionPaths(path)
	journal := durablePersistedDeletionJournal{
		Target: filepath.Base(path),
		SHA256: formatPersistedDigest(version),
	}
	if errWrite := writeDurablePersistedDeletionJournal(journalPath, journal); errWrite != nil {
		t.Fatalf("writeDurablePersistedDeletionJournal() error = %v", errWrite)
	}

	recovered, errRecover := RecoverDurablePersistedFileDeletions(dir)
	if errRecover != nil || len(recovered) != 1 {
		t.Fatalf("RecoverDurablePersistedFileDeletions() = %#v, %v", recovered, errRecover)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("journaled live credential was not captured: %v", errStat)
	}
	if errRollback := recovered[0].Rollback(); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("credential was not restored: %v", errStat)
	}
}

func formatPersistedDigest(version PersistedVersion) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(version.SHA256)*2)
	for i, value := range version.SHA256 {
		out[i*2] = digits[value>>4]
		out[i*2+1] = digits[value&0x0f]
	}
	return string(out)
}
