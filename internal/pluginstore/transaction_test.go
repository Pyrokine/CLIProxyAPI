package pluginstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestArtifactTransactionRollbackRestoresOriginalFileObject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard-link identity assertion is Unix-specific")
	}
	root := t.TempDir()
	targetPath := filepath.Join(root, "sample-provider.so")
	linkPath := filepath.Join(root, "sample-provider-link.so")
	if err := os.WriteFile(targetPath, []byte("old"), 0o6750); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Link(targetPath, linkPath); err != nil {
		t.Fatalf("Link() error = %v", err)
	}
	originalInfo, errStat := os.Stat(targetPath)
	if errStat != nil {
		t.Fatalf("Stat() error = %v", errStat)
	}

	transaction := &ArtifactTransaction{}
	if errBackup := transaction.Backup(targetPath); errBackup != nil {
		t.Fatalf("Backup() error = %v", errBackup)
	}
	if errWrite := writeFileAtomic(targetPath, []byte("new"), 0o600); errWrite != nil {
		t.Fatalf("writeFileAtomic(replacement) error = %v", errWrite)
	}
	if errRollback := transaction.Rollback(
		InstallResult{Path: targetPath, Overwritten: true},
	); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}

	restoredInfo, errStat := os.Stat(targetPath)
	if errStat != nil {
		t.Fatalf("Stat(restored) error = %v", errStat)
	}
	linkInfo, errStat := os.Stat(linkPath)
	if errStat != nil {
		t.Fatalf("Stat(link) error = %v", errStat)
	}
	if !os.SameFile(originalInfo, restoredInfo) || !os.SameFile(restoredInfo, linkInfo) {
		t.Fatal("rollback did not restore the original inode and hard-link relationship")
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "old" {
		t.Fatalf("restored data = %q, want old", data)
	}
	if restoredInfo.Mode() != originalInfo.Mode() {
		t.Fatalf("restored mode = %v, want %v", restoredInfo.Mode(), originalInfo.Mode())
	}
}

func TestArtifactTransactionRollbackBeforeReplacementRestoresArtifact(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "sample-provider.dll")
	if err := os.WriteFile(targetPath, []byte("old"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	transaction := &ArtifactTransaction{}
	if errBackup := transaction.Backup(targetPath); errBackup != nil {
		t.Fatalf("Backup() error = %v", errBackup)
	}
	dataBeforeReplacement, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile(before replacement) error = %v", errRead)
	}
	if string(dataBeforeReplacement) != "old" {
		t.Fatalf("data before replacement = %q, want old", dataBeforeReplacement)
	}
	if errRollback := transaction.Rollback(
		InstallResult{Path: targetPath, Overwritten: true},
	); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "old" {
		t.Fatalf("restored data = %q, want old", data)
	}
}

func TestStagedRemovalKeepsTargetUntilCommit(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "sample-provider.so")
	if err := os.WriteFile(targetPath, []byte("plugin"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	removal, errStage := StageRemoval(targetPath)
	if errStage != nil {
		t.Fatalf("StageRemoval() error = %v", errStage)
	}
	if data, errRead := os.ReadFile(targetPath); errRead != nil || string(data) != "plugin" {
		t.Fatalf("target before commit = %q, error = %v", data, errRead)
	}
	if errCommit := removal.Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
	if _, errStat := os.Stat(targetPath); !os.IsNotExist(errStat) {
		t.Fatalf("target after commit still exists: %v", errStat)
	}
}

func TestStagedRemovalRollbackKeepsOriginalFileObject(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "sample-provider.so")
	if err := os.WriteFile(targetPath, []byte("plugin"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	originalInfo, errStat := os.Stat(targetPath)
	if errStat != nil {
		t.Fatalf("Stat() error = %v", errStat)
	}
	removal, errStage := StageRemoval(targetPath)
	if errStage != nil {
		t.Fatalf("StageRemoval() error = %v", errStage)
	}
	if errRollback := removal.Rollback(); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
	restoredInfo, errStat := os.Stat(targetPath)
	if errStat != nil {
		t.Fatalf("Stat(after rollback) error = %v", errStat)
	}
	if !os.SameFile(originalInfo, restoredInfo) {
		t.Fatal("rollback changed the original file object")
	}
}

func TestArtifactTransactionCommitKeepsReplacement(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "sample-provider.so")
	if err := os.WriteFile(targetPath, []byte("old"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	transaction := &ArtifactTransaction{}
	if errBackup := transaction.Backup(targetPath); errBackup != nil {
		t.Fatalf("Backup() error = %v", errBackup)
	}
	if errWrite := writeFileAtomic(targetPath, []byte("new"), 0o600); errWrite != nil {
		t.Fatalf("writeFileAtomic(replacement) error = %v", errWrite)
	}
	if errCommit := transaction.Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "new" {
		t.Fatalf("committed data = %q, want new", data)
	}
	entries, errReadDir := os.ReadDir(root)
	if errReadDir != nil {
		t.Fatalf("ReadDir() error = %v", errReadDir)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(targetPath) {
		t.Fatalf("entries after commit = %v, want only replacement", entries)
	}
}
