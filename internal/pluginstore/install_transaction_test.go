package pluginstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRecoverInstallTransactionRollsBackUncommittedFirstInstall(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	data := []byte("new")
	transaction, errPrepare := PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", installTestDigest(data), false, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	pending, errPending := PendingInstallPluginIDs(root)
	if errPending != nil {
		t.Fatalf("PendingInstallPluginIDs() error = %v", errPending)
	}
	if _, okPending := pending["sample"]; !okPending {
		t.Fatal("pending install tombstone is missing")
	}

	if errRecover := RecoverInstallTransactions(root, nil); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(target); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("uncommitted first install remains visible: %v", errStat)
	}
	if _, errStat := os.Stat(transaction.stageDir); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("install transaction stage remains: %v", errStat)
	}
}

func TestRecoverInstallTransactionRestoresOverwrittenArtifact(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	if errWrite := os.WriteFile(target, []byte("old"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(old) error = %v", errWrite)
	}
	data := []byte("new")
	_, errPrepare := PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", installTestDigest(data), true, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	replacement := filepath.Join(filepath.Dir(target), ".replacement")
	if errWrite := os.WriteFile(replacement, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile(new) error = %v", errWrite)
	}
	if errReplace := replaceFile(replacement, target); errReplace != nil {
		t.Fatalf("replaceFile(new) error = %v", errReplace)
	}

	if errRecover := RecoverInstallTransactions(root, map[string]string{"sample": "different"}); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	content, errRead := os.ReadFile(target)
	if errRead != nil || string(content) != "old" {
		t.Fatalf("restored artifact = %q, error = %v", content, errRead)
	}
}

func TestRecoverInstallTransactionFinalizesPersistedConfig(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	data := []byte("new")
	transaction, errPrepare := PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", installTestDigest(data), false, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	configFingerprint := installTestDigest([]byte("config"))
	if errExpected := transaction.SetExpectedConfigFingerprint(configFingerprint); errExpected != nil {
		t.Fatalf("SetExpectedConfigFingerprint() error = %v", errExpected)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	if errRecover := RecoverInstallTransactions(
		root, map[string]string{"sample": configFingerprint},
	); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	content, errRead := os.ReadFile(target)
	if errRead != nil || string(content) != "new" {
		t.Fatalf("committed artifact = %q, error = %v", content, errRead)
	}
	pending, errPending := PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending installs after recovery = %v, error = %v", pending, errPending)
	}
}

func TestRecoverInstallTransactionsRemovesUnjournaledStage(t *testing.T) {
	root := t.TempDir()
	stageDir, errStage := os.MkdirTemp(root, installTransactionPrefix+"sample-")
	if errStage != nil {
		t.Fatalf("MkdirTemp() error = %v", errStage)
	}
	if errWrite := os.WriteFile(filepath.Join(stageDir, "previous-sample.so"), []byte("old"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	if errRecover := RecoverInstallTransactions(root, nil); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(stageDir); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("unjournaled install stage remains: %v", errStat)
	}
	pending, errPending := PendingInstallPluginIDs(root)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending installs = %v, error = %v", pending, errPending)
	}
}

func TestRecoverInstallTransactionsRemovesPartialCleanup(t *testing.T) {
	root := t.TempDir()
	cleanupDir := filepath.Join(root, installCleanupPrefix+"sample-partial")
	if errMkdir := os.MkdirAll(cleanupDir, 0o700); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	if errWrite := os.WriteFile(
		filepath.Join(cleanupDir, installJournalName),
		[]byte(`{"incomplete":true}`),
		0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(journal) error = %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(cleanupDir, "previous-sample.so"), []byte("old"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(staged) error = %v", errWrite)
	}
	if errRecover := RecoverInstallTransactions(root, nil); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(cleanupDir); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("partial install cleanup remains: %v", errStat)
	}
}

func TestInstallArchiveCreatesDurableTransactionBeforeWrite(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	var transaction *InstallTransaction
	_, errInstall := InstallArchive(
		makeZip(t, map[string]string{"sample" + pluginExtension(runtime.GOOS): "new"}),
		Plugin{ID: "sample", Version: "1.0.0"},
		InstallOptions{
			PluginsDir: root,
			GOOS:       runtime.GOOS,
			GOARCH:     runtime.GOARCH,
			RootLock:   rootLock,
			BeforePublish: func(path, version string, overwritten bool, digest string) error {
				var errPrepare error
				transaction, errPrepare = PrepareInstallTransactionLocked(
					root, "sample", path, version, digest, overwritten, rootLock,
				)
				return errPrepare
			},
		},
	)
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	if transaction == nil {
		t.Fatal("InstallArchive did not prepare a durable transaction")
	}
	pending, errPending := PendingInstallPluginIDs(root)
	if errPending != nil {
		t.Fatalf("PendingInstallPluginIDs() error = %v", errPending)
	}
	if _, okPending := pending["sample"]; !okPending {
		t.Fatal("installed artifact was published without a tombstone")
	}
	if errRollback := transaction.Rollback(InstallResult{Path: filepath.Join(root, "unused")}); errRollback != nil {
		t.Fatalf("Rollback() error = %v", errRollback)
	}
}

func TestRollbackInstallJournalKeepsStagedArtifactForRetry(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "sample-v1.0.0"+pluginExtension(runtime.GOOS))
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	if errWrite := os.WriteFile(target, []byte("old"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(old) error = %v", errWrite)
	}
	transaction, errPrepare := PrepareInstallTransactionLocked(
		root, "sample", target, "1.0.0", installTestDigest([]byte("new")), true, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	replacement := filepath.Join(filepath.Dir(target), ".replacement")
	if errWrite := os.WriteFile(replacement, []byte("new"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(new) error = %v", errWrite)
	}
	if errReplace := replaceFile(replacement, target); errReplace != nil {
		t.Fatalf("replaceFile(new) error = %v", errReplace)
	}

	if errRollback := rollbackInstallJournal(root, transaction.stageDir, transaction.journal); errRollback != nil {
		t.Fatalf("first rollbackInstallJournal() error = %v", errRollback)
	}
	stagedPath := filepath.Join(transaction.stageDir, transaction.journal.Staged)
	stagedInfo, errStaged := os.Stat(stagedPath)
	if errStaged != nil {
		t.Fatalf("staged artifact was consumed by rollback: %v", errStaged)
	}
	targetInfo, errTarget := os.Stat(target)
	if errTarget != nil || !os.SameFile(stagedInfo, targetInfo) {
		t.Fatalf("restored artifact does not match staged artifact: %v", errTarget)
	}
	if errRollback := rollbackInstallJournal(root, transaction.stageDir, transaction.journal); errRollback != nil {
		t.Fatalf("second rollbackInstallJournal() error = %v", errRollback)
	}
	if _, errStaged = os.Stat(stagedPath); errStaged != nil {
		t.Fatalf("staged artifact missing after repeated rollback: %v", errStaged)
	}
	if errCommit := transaction.Commit(); errCommit != nil {
		t.Fatalf("Commit() error = %v", errCommit)
	}
}

func installTestDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
