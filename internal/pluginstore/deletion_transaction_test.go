package pluginstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeletionTransactionRollbackRestoresAllVersions(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0", "0.2.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	for _, path := range paths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("hidden artifact %s remains visible: %v", path, errStat)
		}
	}
	if errRollback := transaction.Rollback(); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
	for _, path := range paths {
		if _, errStat := os.Stat(path); errStat != nil {
			t.Fatalf("restored artifact %s: %v", path, errStat)
		}
	}
}

func TestRecoverDeletionTransactionUsesConfigState(t *testing.T) {
	t.Run(
		"configured restores",
		func(t *testing.T) {
			root := t.TempDir()
			paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
			transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
			if errPrepare != nil {
				t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
			}
			if errHide := transaction.Hide(); errHide != nil {
				t.Fatalf("Hide() error = %v", errHide)
			}
			if errMark := transaction.MarkCommitted(); errMark != nil {
				t.Fatalf("MarkCommitted() error = %v", errMark)
			}
			activeDeletionTransactions.Delete(transaction.stageDir)
			if errRecover := RecoverDeletionTransactions(
				root,
				map[string]struct{}{"sample": {}},
			); errRecover != nil {
				t.Fatalf("RecoverDeletionTransactions() error = %v", errRecover)
			}
			if _, errStat := os.Stat(paths[0]); errStat != nil {
				t.Fatalf("configured artifact was not restored: %v", errStat)
			}
		},
	)
	t.Run(
		"removed finalizes",
		func(t *testing.T) {
			root := t.TempDir()
			paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
			transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
			if errPrepare != nil {
				t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
			}
			if errHide := transaction.Hide(); errHide != nil {
				t.Fatalf("Hide() error = %v", errHide)
			}
			if errMark := transaction.MarkCommitted(); errMark != nil {
				t.Fatalf("MarkCommitted() error = %v", errMark)
			}
			activeDeletionTransactions.Delete(transaction.stageDir)
			if errRecover := RecoverDeletionTransactions(root, nil); errRecover != nil {
				t.Fatalf("RecoverDeletionTransactions() error = %v", errRecover)
			}
			if _, errStat := os.Stat(paths[0]); !os.IsNotExist(errStat) {
				t.Fatalf("removed artifact was restored: %v", errStat)
			}
		},
	)
}

func TestFindPluginArtifactsReturnsAllVersions(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0", "0.2.0")
	otherPlatformDir := filepath.Join(root, "other-os", "other-arch")
	if err := os.MkdirAll(otherPlatformDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(other platform) error = %v", err)
	}
	otherPlatformPath := filepath.Join(otherPlatformDir, "sample-v0.3.0.dll")
	if err := os.WriteFile(otherPlatformPath, []byte("0.3.0"), 0o600); err != nil {
		t.Fatalf("WriteFile(other platform) error = %v", err)
	}
	paths = append(paths, otherPlatformPath)
	found, errFind := FindPluginArtifacts(root, "sample")
	if errFind != nil {
		t.Fatalf("FindPluginArtifacts() error = %v", errFind)
	}
	if len(found) != len(paths) {
		t.Fatalf("FindPluginArtifacts() count = %d, want %d: %v", len(found), len(paths), found)
	}
}

func TestPrepareDeletionTransactionRejectsSymlinkArtifact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	linkPath := filepath.Join(filepath.Dir(paths[0]), "sample-v0.2.0"+pluginExtension(runtime.GOOS))
	if err := os.Symlink(paths[0], linkPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if _, errPrepare := PrepareDeletionTransaction(root, "sample", []string{linkPath}); errPrepare == nil {
		t.Fatal("PrepareDeletionTransaction() accepted a symlink artifact")
	}
	assertNoDeletionTransactionDirectories(t, root)
}

func TestPrepareDeletionTransactionRejectsSymlinkOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "sample-v0.1.0"+pluginExtension(runtime.GOOS))
	if err := os.WriteFile(outsidePath, []byte("outside"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	platformDir := filepath.Join(root, runtime.GOOS)
	if err := os.Symlink(outside, platformDir); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	linkedPath := filepath.Join(platformDir, "sample-v0.1.0"+pluginExtension(runtime.GOOS))
	if _, errPrepare := PrepareDeletionTransaction(root, "sample", []string{linkedPath}); errPrepare == nil {
		t.Fatal("PrepareDeletionTransaction() accepted an artifact outside the plugin root")
	}
	if data, errRead := os.ReadFile(outsidePath); errRead != nil || string(data) != "outside" {
		t.Fatalf("outside artifact changed: data=%q error=%v", data, errRead)
	}
	assertNoDeletionTransactionDirectories(t, root)
}

func TestDeletionTransactionRefusesChangedTarget(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	if errMark := transaction.MarkCommitted(); errMark != nil {
		t.Fatalf("MarkCommitted() error = %v", errMark)
	}
	if errWrite := os.WriteFile(paths[0], []byte("replacement"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(replacement) error = %v", errWrite)
	}
	if errCommit := transaction.Commit(); errCommit == nil {
		t.Fatal("Commit() replaced a changed target")
	}
	if data, errRead := os.ReadFile(paths[0]); errRead != nil || string(data) != "replacement" {
		t.Fatalf("changed target was modified: data=%q error=%v", data, errRead)
	}
	if errRemove := os.Remove(paths[0]); errRemove != nil {
		t.Fatalf("Remove(replacement) error = %v", errRemove)
	}
	if errRollback := transaction.Rollback(); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
	if data, errRead := os.ReadFile(paths[0]); errRead != nil || string(data) != "0.1.0" {
		t.Fatalf("original artifact was not restored: data=%q error=%v", data, errRead)
	}
}

func TestDeletionTransactionHideRestoresConcurrentReplacement(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	defer transaction.Deactivate()

	replacementPath := filepath.Join(filepath.Dir(paths[0]), ".replacement")
	if errWrite := os.WriteFile(replacementPath, []byte("replacement"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(replacement) error = %v", errWrite)
	}
	if errRename := os.Rename(replacementPath, paths[0]); errRename != nil {
		t.Fatalf("Rename(replacement) error = %v", errRename)
	}
	if errHide := transaction.Hide(); errHide == nil {
		t.Fatal("Hide() accepted a concurrently replaced artifact")
	}
	if data, errRead := os.ReadFile(paths[0]); errRead != nil || string(data) != "replacement" {
		t.Fatalf("concurrent replacement changed: data=%q error=%v", data, errRead)
	}
}

func TestDeletionTransactionCommitRemovesLateArtifactVersion(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	defer transaction.Deactivate()
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	if errMark := transaction.MarkCommitted(); errMark != nil {
		t.Fatalf("MarkCommitted() error = %v", errMark)
	}
	latePath := writeDeletionTestArtifacts(t, root, "sample", "0.2.0")[0]
	if errCommit := transaction.Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
	for _, path := range append(paths, latePath) {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("deleted artifact remains at %s: %v", path, errStat)
		}
	}
}

func TestDeletionJournalWriteRejectsOversizedPayload(t *testing.T) {
	root := t.TempDir()
	stageDir, errMkdir := os.MkdirTemp(root, deletionTransactionPrefix+"sample-")
	if errMkdir != nil {
		t.Fatalf("MkdirTemp() error = %v", errMkdir)
	}
	transaction := &DeletionTransaction{
		root:        root,
		stageDir:    stageDir,
		journalPath: filepath.Join(stageDir, deletionJournalName),
		journal: deletionJournal{
			Version:  1,
			PluginID: "sample",
			Entries: []deletionJournalEntry{
				{Target: strings.Repeat("a", deletionJournalMaxBytes), Staged: "0000-sample.so"},
			},
		},
	}
	if errWrite := transaction.writeJournal(); errWrite == nil {
		t.Fatal("writeJournal() accepted an oversized journal")
	}
	if _, errStat := os.Stat(transaction.journalPath); !os.IsNotExist(errStat) {
		t.Fatalf("oversized journal was published: %v", errStat)
	}
}

func TestRecoverDeletionTransactionRemovesUnjournaledPreparation(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	stageDir, errMkdir := os.MkdirTemp(root, deletionTransactionPrefix+"sample-")
	if errMkdir != nil {
		t.Fatalf("MkdirTemp() error = %v", errMkdir)
	}
	stagedPath := filepath.Join(stageDir, "0000-"+filepath.Base(paths[0]))
	if errLink := os.Link(paths[0], stagedPath); errLink != nil {
		t.Fatalf("Link() error = %v", errLink)
	}

	if errRecover := RecoverDeletionTransactions(root, map[string]struct{}{"sample": {}}); errRecover != nil {
		t.Fatalf("RecoverDeletionTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(stageDir); !os.IsNotExist(errStat) {
		t.Fatalf("unjournaled transaction stage remains: %v", errStat)
	}
	if data, errRead := os.ReadFile(paths[0]); errRead != nil || string(data) != "0.1.0" {
		t.Fatalf("visible artifact changed: data=%q error=%v", data, errRead)
	}
}

func TestRecoverDeletionTransactionSkipsActiveUnjournaledPreparation(t *testing.T) {
	root := t.TempDir()
	stageDir, errMkdir := os.MkdirTemp(root, deletionTransactionPrefix+"sample-")
	if errMkdir != nil {
		t.Fatalf("MkdirTemp() error = %v", errMkdir)
	}
	activeDeletionTransactions.Store(stageDir, struct{}{})
	t.Cleanup(func() { activeDeletionTransactions.Delete(stageDir) })

	if errRecover := RecoverDeletionTransactions(root, nil); errRecover != nil {
		t.Fatalf("RecoverDeletionTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(stageDir); errStat != nil {
		t.Fatalf("active transaction stage was removed: %v", errStat)
	}
}

func TestRecoverDeletionTransactionRejectsCorruptJournal(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "sample", "0.1.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "sample", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	activeDeletionTransactions.Delete(transaction.stageDir)
	if errWrite := os.WriteFile(transaction.journalPath, []byte("not-json"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(journal) error = %v", errWrite)
	}
	if errRecover := RecoverDeletionTransactions(root, map[string]struct{}{"sample": {}}); errRecover == nil {
		t.Fatal("RecoverDeletionTransactions() accepted a corrupt journal")
	}
	if _, errStat := os.Stat(paths[0]); !os.IsNotExist(errStat) {
		t.Fatalf("corrupt journal changed the hidden target: %v", errStat)
	}
	if _, errStat := os.Stat(transaction.stageDir); errStat != nil {
		t.Fatalf("corrupt transaction stage was removed: %v", errStat)
	}
}

func assertNoDeletionTransactionDirectories(t *testing.T, root string) {
	t.Helper()
	entries, errRead := os.ReadDir(root)
	if errRead != nil {
		t.Fatalf("ReadDir(plugin root) error = %v", errRead)
	}
	for _, entry := range entries {
		if entry != nil && strings.HasPrefix(entry.Name(), deletionTransactionPrefix) {
			t.Fatalf("plugin deletion transaction directory remains after preparation failure: %s", entry.Name())
		}
	}
}

func writeDeletionTestArtifacts(t *testing.T, root, id string, versions ...string) []string {
	t.Helper()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	extension := pluginExtension(runtime.GOOS)
	paths := make([]string, 0, len(versions))
	for _, version := range versions {
		path := filepath.Join(dir, id+"-v"+version+extension)
		if err := os.WriteFile(path, []byte(version), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		paths = append(paths, path)
	}
	return paths
}
