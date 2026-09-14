package pluginstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateRegistryRejectsCaseInsensitiveDuplicatePluginIDs(t *testing.T) {
	plugin := Plugin{
		ID:          "Alpha",
		Name:        "Alpha",
		Description: "Alpha plugin",
		Author:      "author-name",
		Repository:  "https://github.com/author-name/alpha-plugin",
	}
	duplicate := plugin
	duplicate.ID = "alpha"

	errValidate := validateRegistry(Registry{SchemaVersion: SchemaVersion, Plugins: []Plugin{plugin, duplicate}})
	if errValidate == nil {
		t.Fatal("ValidateRegistry() accepted case-insensitive duplicate plugin ids")
	}
}

func TestPendingDeletionBlocksCaseVariantInstall(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "alpha", "1.0.0")
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	transaction, errPrepare := PrepareDeletionTransactionLocked(root, "alpha", paths, rootLock)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransactionLocked() error = %v", errPrepare)
	}
	defer transaction.Deactivate()
	defer func() {
		if errRollback := transaction.Rollback(); errRollback != nil {
			t.Errorf("Rollback() error = %v", errRollback)
		}
	}()

	_, errInstall := installArchive(
		nil, Plugin{ID: "Alpha", Version: "1.0.0"}, InstallOptions{
			PluginsDir: root,
			GOOS:       runtime.GOOS,
			GOARCH:     runtime.GOARCH,
			RootLock:   rootLock,
		},
	)
	if !errors.Is(errInstall, ErrPendingDeletion) {
		t.Fatalf("InstallArchive() error = %v, want ErrPendingDeletion", errInstall)
	}
}

func TestRecoverInstallTransactionCanonicalizesLegacyJournalID(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "alpha-v1.0.0"+pluginExtension(runtime.GOOS))
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	data := []byte("new")
	transaction, errPrepare := PrepareInstallTransactionLocked(
		root, "alpha", target, "1.0.0", installTestDigest(data), false, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	fingerprint := installTestDigest([]byte("config"))
	if errExpected := transaction.SetExpectedConfigFingerprint(fingerprint); errExpected != nil {
		t.Fatalf("SetExpectedConfigFingerprint() error = %v", errExpected)
	}
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile(target) error = %v", errWrite)
	}
	transaction.journal.PluginID = "Alpha"
	if errJournal := transaction.writeJournal(); errJournal != nil {
		t.Fatalf("writeJournal() error = %v", errJournal)
	}

	if errRecover := RecoverInstallTransactions(root, map[string]string{"Alpha": fingerprint}); errRecover != nil {
		t.Fatalf("RecoverInstallTransactions() error = %v", errRecover)
	}
	if content, errRead := os.ReadFile(target); errRead != nil || string(content) != "new" {
		t.Fatalf("recovered target = %q, error = %v", content, errRead)
	}
}

func TestRecoverDeletionTransactionCanonicalizesLegacyJournalID(t *testing.T) {
	root := t.TempDir()
	paths := writeDeletionTestArtifacts(t, root, "alpha", "1.0.0")
	transaction, errPrepare := PrepareDeletionTransaction(root, "alpha", paths)
	if errPrepare != nil {
		t.Fatalf("PrepareDeletionTransaction() error = %v", errPrepare)
	}
	defer transaction.Deactivate()
	if errHide := transaction.Hide(); errHide != nil {
		t.Fatalf("Hide() error = %v", errHide)
	}
	if errCommitted := transaction.MarkCommitted(); errCommitted != nil {
		t.Fatalf("MarkCommitted() error = %v", errCommitted)
	}
	transaction.journal.PluginID = "Alpha"
	if errJournal := transaction.writeJournal(); errJournal != nil {
		t.Fatalf("writeJournal() error = %v", errJournal)
	}
	activeDeletionTransactions.Delete(transaction.stageDir)

	if errRecover := RecoverDeletionTransactions(root, map[string]struct{}{"Alpha": {}}); errRecover != nil {
		t.Fatalf("RecoverDeletionTransactions() error = %v", errRecover)
	}
	if _, errStat := os.Stat(paths[0]); errStat != nil {
		t.Fatalf("configured artifact was not restored: %v", errStat)
	}
}

func TestPendingInstallBlocksCaseVariantMutation(t *testing.T) {
	root := t.TempDir()
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	defer rootLock.Unlock()
	target := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "alpha-v1.0.0"+pluginExtension(runtime.GOOS))
	transaction, errPrepare := PrepareInstallTransactionLocked(
		root, "alpha", target, "1.0.0", installTestDigest([]byte("new")), false, rootLock,
	)
	if errPrepare != nil {
		t.Fatalf("PrepareInstallTransactionLocked() error = %v", errPrepare)
	}
	defer func() {
		if errRollback := transaction.Rollback(InstallResult{}); errRollback != nil {
			t.Errorf("Rollback() error = %v", errRollback)
		}
	}()

	_, errSecond := PrepareInstallTransactionLocked(
		root, "Alpha", target, "1.0.0", installTestDigest([]byte("new")), false, rootLock,
	)
	if !errors.Is(errSecond, ErrPendingInstall) {
		t.Fatalf("second PrepareInstallTransactionLocked() error = %v, want ErrPendingInstall", errSecond)
	}
	_, errDelete := PrepareDeletionTransactionLocked(root, "Alpha", nil, rootLock)
	if !errors.Is(errDelete, ErrPendingInstall) {
		t.Fatalf("PrepareDeletionTransactionLocked() error = %v, want ErrPendingInstall", errDelete)
	}
	_, errInstall := installArchive(
		nil, Plugin{ID: "Alpha", Version: "1.0.0"}, InstallOptions{
			PluginsDir: root,
			GOOS:       runtime.GOOS,
			GOARCH:     runtime.GOARCH,
			RootLock:   rootLock,
		},
	)
	if !errors.Is(errInstall, ErrPendingInstall) {
		t.Fatalf("InstallArchive() error = %v, want ErrPendingInstall", errInstall)
	}
}
