package pluginstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var activeInstallTransactions sync.Map

const (
	installTransactionPrefix = ".cliproxy-plugin-install-"
	installCleanupPrefix     = ".cliproxy-plugin-install-cleanup-"
	installJournalName       = "journal.json"
	installJournalMaxBytes   = 64 << 10
)

type installJournal struct {
	Version              int    `json:"version"`
	PluginID             string `json:"plugin_id"`
	Target               string `json:"target"`
	Staged               string `json:"staged,omitempty"`
	NextVersion          string `json:"next_version"`
	NextSHA256           string `json:"next_sha256"`
	ConfigFingerprintSHA string `json:"config_fingerprint_sha256,omitempty"`
}

// InstallTransaction preserves the previous artifact until its config commits.
type InstallTransaction struct {
	root        string
	stageDir    string
	journalPath string
	journal     installJournal
}

// PrepareInstallTransactionLocked creates a durable install journal before artifact publication.
func PrepareInstallTransactionLocked(
	root,
	pluginID,
	targetPath,
	nextVersion,
	nextSHA256 string,
	overwritten bool,
	rootLock *RootLock,
) (*InstallTransaction, error) {
	if rootLock == nil || !rootLock.matches(root) {
		return nil, fmt.Errorf("plugin root lock does not match install directory")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	pluginID = CanonicalPluginID(pluginID)
	if !validPluginID(pluginID) {
		return nil, fmt.Errorf("invalid plugin id %q", pluginID)
	}
	nextVersion = normalizeVersion(nextVersion)
	if !validPluginVersion(nextVersion) {
		return nil, fmt.Errorf("invalid plugin version %q", nextVersion)
	}
	nextSHA256 = strings.ToLower(strings.TrimSpace(nextSHA256))
	if len(nextSHA256) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid plugin artifact sha256")
	}
	if _, errDecode := hex.DecodeString(nextSHA256); errDecode != nil {
		return nil, fmt.Errorf("invalid plugin artifact sha256: %w", errDecode)
	}
	pendingInstalls, errPending := PendingInstallPluginIDs(root)
	if errPending != nil {
		return nil, fmt.Errorf("inspect pending plugin installs: %w", errPending)
	}
	if _, pending := pendingInstalls[pluginID]; pending {
		return nil, fmt.Errorf("%w: %s", ErrPendingInstall, pluginID)
	}

	absoluteRoot, errRoot := filepath.Abs(root)
	if errRoot != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", errRoot)
	}
	absoluteRoot, errRoot = filepath.EvalSymlinks(absoluteRoot)
	if errRoot != nil {
		return nil, fmt.Errorf("resolve plugin root links: %w", errRoot)
	}
	absoluteTarget, errTarget := filepath.Abs(targetPath)
	if errTarget != nil {
		return nil, fmt.Errorf("resolve plugin install target: %w", errTarget)
	}
	relativeTarget, errRelative := filepath.Rel(absoluteRoot, absoluteTarget)
	if errRelative != nil || relativeTarget == "." || relativeTarget == ".." ||
		strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeTarget) {
		return nil, fmt.Errorf("plugin install target escapes root: %s", targetPath)
	}
	directoryComponents := strings.Split(filepath.Dir(relativeTarget), string(filepath.Separator))
	targetRoot, errTargetRoot := openOrCreatePluginDirectoryRoot(absoluteRoot, directoryComponents, 0o755)
	if errTargetRoot != nil {
		return nil, fmt.Errorf("open plugin install target directory: %w", errTargetRoot)
	}
	if errClose := targetRoot.Close(); errClose != nil {
		return nil, fmt.Errorf("close plugin install target directory: %w", errClose)
	}
	file, okFile := pluginFileInfoFromPath(absoluteTarget, "")
	if !okFile || file.ID != pluginID || normalizeVersion(file.Version) != nextVersion {
		return nil, fmt.Errorf("plugin install target does not match %s v%s", pluginID, nextVersion)
	}

	stageDir, errStage := os.MkdirTemp(absoluteRoot, installTransactionPrefix+pluginID+"-")
	if errStage != nil {
		return nil, fmt.Errorf("create plugin install transaction: %w", errStage)
	}
	activeInstallTransactions.Store(stageDir, struct{}{})
	cleanup := true
	defer func() {
		if cleanup {
			activeInstallTransactions.Delete(stageDir)
			_ = os.RemoveAll(stageDir)
		}
	}()
	if errChmod := os.Chmod(stageDir, 0o700); errChmod != nil {
		return nil, fmt.Errorf("secure plugin install transaction: %w", errChmod)
	}

	staged := ""
	targetInfo, errTargetInfo := os.Lstat(absoluteTarget)
	if overwritten {
		if errTargetInfo != nil {
			return nil, fmt.Errorf("inspect previous plugin artifact: %w", errTargetInfo)
		}
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("previous plugin artifact is not a regular file")
		}
		staged = "previous-" + filepath.Base(absoluteTarget)
		stagedPath := filepath.Join(stageDir, staged)
		if errLink := os.Link(absoluteTarget, stagedPath); errLink != nil {
			return nil, fmt.Errorf("preserve previous plugin artifact: %w", errLink)
		}
		stagedInfo, errStagedInfo := os.Lstat(stagedPath)
		verifiedInfo, errVerifiedInfo := os.Lstat(absoluteTarget)
		if errStagedInfo != nil || errVerifiedInfo != nil ||
			!stagedInfo.Mode().IsRegular() || !verifiedInfo.Mode().IsRegular() ||
			!os.SameFile(targetInfo, stagedInfo) || !os.SameFile(stagedInfo, verifiedInfo) {
			return nil, fmt.Errorf("previous plugin artifact changed during preservation")
		}
		if errSync := syncDirectory(stageDir); errSync != nil {
			return nil, fmt.Errorf("sync previous plugin artifact: %w", errSync)
		}
	} else if errTargetInfo == nil {
		return nil, fmt.Errorf("plugin install target appeared during preparation")
	} else if !errors.Is(errTargetInfo, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect plugin install target: %w", errTargetInfo)
	}
	transaction := &InstallTransaction{
		root:        absoluteRoot,
		stageDir:    stageDir,
		journalPath: filepath.Join(stageDir, installJournalName),
		journal: installJournal{
			Version:     1,
			PluginID:    pluginID,
			Target:      filepath.Clean(relativeTarget),
			Staged:      staged,
			NextVersion: nextVersion,
			NextSHA256:  nextSHA256,
		},
	}
	if errJournal := transaction.writeJournal(); errJournal != nil {
		return nil, errJournal
	}
	if errSync := syncDirectory(absoluteRoot); errSync != nil {
		return nil, fmt.Errorf("sync plugin root after install preparation: %w", errSync)
	}
	cleanup = false
	return transaction, nil
}

// SetExpectedConfigFingerprint records the config state that commits this artifact.
func (t *InstallTransaction) SetExpectedConfigFingerprint(fingerprint string) error {
	if t == nil || t.stageDir == "" {
		return fmt.Errorf("plugin install transaction is not active")
	}
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	if len(fingerprint) != sha256.Size*2 {
		return fmt.Errorf("invalid plugin config fingerprint")
	}
	if _, errDecode := hex.DecodeString(fingerprint); errDecode != nil {
		return fmt.Errorf("invalid plugin config fingerprint: %w", errDecode)
	}
	t.journal.ConfigFingerprintSHA = fingerprint
	return t.writeJournal()
}

// Commit removes the durable rollback state after the config has persisted.
func (t *InstallTransaction) Commit() error {
	if t == nil || t.stageDir == "" {
		return nil
	}
	stageDir := t.stageDir
	root := filepath.Dir(stageDir)
	cleanupDir := installCleanupPath(stageDir)
	if errRename := os.Rename(stageDir, cleanupDir); errRename != nil {
		return fmt.Errorf("finalize plugin install transaction: %w", errRename)
	}
	activeInstallTransactions.Delete(stageDir)
	t.clear()
	if errSync := syncDirectory(root); errSync != nil {
		return errSync
	}
	if errRemove := os.RemoveAll(cleanupDir); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return fmt.Errorf("remove plugin install transaction: %w", errRemove)
	}
	return syncDirectory(root)
}

// Rollback restores the previous artifact or removes a first-time install.
func (t *InstallTransaction) Rollback(result InstallResult) error {
	if t == nil || t.stageDir == "" || result.Skipped {
		return t.Commit()
	}
	if errRollback := rollbackInstallJournal(t.root, t.stageDir, t.journal); errRollback != nil {
		return errRollback
	}
	return t.Commit()
}

func (t *InstallTransaction) clear() {
	t.root = ""
	t.stageDir = ""
	t.journalPath = ""
	t.journal = installJournal{}
}

// PendingInstallPluginIDs returns plugin IDs hidden by unfinished install journals.
func PendingInstallPluginIDs(root string) (map[string]struct{}, error) {
	transactions, errTransactions := loadInstallTransactions(root)
	if errTransactions != nil {
		return nil, errTransactions
	}
	pending := make(map[string]struct{}, len(transactions))
	for _, transaction := range transactions {
		pending[transaction.journal.PluginID] = struct{}{}
	}
	return pending, nil
}

// RecoverInstallTransactions resolves interrupted installs against persisted desired versions.
func RecoverInstallTransactions(root string, configFingerprints map[string]string) error {
	transactions, errTransactions := loadInstallTransactions(root)
	if errTransactions != nil {
		return errTransactions
	}
	canonicalFingerprints, errFingerprints := canonicalInstallConfigFingerprints(configFingerprints)
	if errFingerprints != nil {
		return errFingerprints
	}
	var recoverErrors []error
	for _, transaction := range transactions {
		configuredFingerprint := canonicalFingerprints[transaction.journal.PluginID]
		if transaction.journal.ConfigFingerprintSHA != "" &&
			configuredFingerprint == transaction.journal.ConfigFingerprintSHA {
			if errFinalize := finalizeInstallJournal(
				transaction.root, transaction.stageDir, transaction.journal,
			); errFinalize != nil {
				recoverErrors = append(recoverErrors, errFinalize)
				continue
			}
		} else if errRollback := rollbackInstallJournal(
			transaction.root, transaction.stageDir, transaction.journal,
		); errRollback != nil {
			recoverErrors = append(recoverErrors, errRollback)
			continue
		}
		if errCommit := transaction.Commit(); errCommit != nil {
			recoverErrors = append(recoverErrors, errCommit)
		}
	}
	return errors.Join(recoverErrors...)
}

func canonicalInstallConfigFingerprints(configFingerprints map[string]string) (map[string]string, error) {
	canonical := make(map[string]string, len(configFingerprints))
	for id, fingerprint := range configFingerprints {
		canonicalID := CanonicalPluginID(id)
		fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
		if existing, duplicate := canonical[canonicalID]; duplicate && existing != fingerprint {
			return nil, fmt.Errorf("conflicting plugin config fingerprints for %q", canonicalID)
		}
		canonical[canonicalID] = fingerprint
	}
	return canonical, nil
}

func loadInstallTransactions(root string) ([]*InstallTransaction, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, errRoot := filepath.Abs(root)
	if errRoot != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", errRoot)
	}
	if errMkdir := os.MkdirAll(absoluteRoot, 0o755); errMkdir != nil {
		return nil, fmt.Errorf("create plugin root: %w", errMkdir)
	}
	absoluteRoot, errRoot = filepath.EvalSymlinks(absoluteRoot)
	if errRoot != nil {
		return nil, fmt.Errorf("resolve plugin root links: %w", errRoot)
	}
	entries, errRead := os.ReadDir(absoluteRoot)
	if errRead != nil {
		return nil, fmt.Errorf("read plugin root: %w", errRead)
	}
	stageDirs := make([]string, 0)
	for _, entry := range entries {
		if entry == nil || !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), installCleanupPrefix) {
			cleanupDir := filepath.Join(absoluteRoot, entry.Name())
			if errCleanup := os.RemoveAll(cleanupDir); errCleanup != nil {
				return nil, fmt.Errorf("remove plugin install cleanup: %w", errCleanup)
			}
			if errSync := syncDirectory(absoluteRoot); errSync != nil {
				return nil, errSync
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), installTransactionPrefix) {
			stageDirs = append(stageDirs, filepath.Join(absoluteRoot, entry.Name()))
		}
	}
	sort.Strings(stageDirs)
	transactions := make([]*InstallTransaction, 0, len(stageDirs))
	for _, stageDir := range stageDirs {
		transaction, errLoad := loadInstallTransaction(absoluteRoot, stageDir)
		if errors.Is(errLoad, os.ErrNotExist) {
			if _, active := activeInstallTransactions.Load(stageDir); active {
				continue
			}
			if errCleanup := cleanupUnjournaledInstallStage(absoluteRoot, stageDir); errCleanup != nil {
				return nil, errCleanup
			}
			continue
		}
		if errLoad != nil {
			return nil, errLoad
		}
		transactions = append(transactions, transaction)
	}
	return transactions, nil
}

func installCleanupPath(stageDir string) string {
	root := filepath.Dir(stageDir)
	return filepath.Join(
		root,
		installCleanupPrefix+strings.TrimPrefix(filepath.Base(stageDir), installTransactionPrefix),
	)
}

func cleanupUnjournaledInstallStage(root, stageDir string) error {
	cleanupDir := installCleanupPath(stageDir)
	if errRename := os.Rename(stageDir, cleanupDir); errRename != nil {
		return fmt.Errorf("quarantine incomplete plugin install transaction: %w", errRename)
	}
	if errSync := syncDirectory(root); errSync != nil {
		return fmt.Errorf("sync incomplete plugin install transaction quarantine: %w", errSync)
	}
	if errRemove := os.RemoveAll(cleanupDir); errRemove != nil {
		return fmt.Errorf("remove incomplete plugin install transaction: %w", errRemove)
	}
	return syncDirectory(root)
}

func loadInstallTransaction(root, stageDir string) (*InstallTransaction, error) {
	journalPath := filepath.Join(stageDir, installJournalName)
	data, errRead := os.ReadFile(journalPath)
	if errRead != nil {
		return nil, fmt.Errorf("read plugin install journal %s: %w", journalPath, errRead)
	}
	if len(data) == 0 || len(data) > installJournalMaxBytes {
		return nil, fmt.Errorf("invalid plugin install journal size: %s", journalPath)
	}
	var journal installJournal
	if errDecode := json.Unmarshal(data, &journal); errDecode != nil {
		return nil, fmt.Errorf("decode plugin install journal %s: %w", journalPath, errDecode)
	}
	journal.PluginID = CanonicalPluginID(journal.PluginID)
	journal.NextSHA256 = strings.ToLower(strings.TrimSpace(journal.NextSHA256))
	journal.ConfigFingerprintSHA = strings.ToLower(strings.TrimSpace(journal.ConfigFingerprintSHA))
	if journal.Version != 1 || !validPluginID(journal.PluginID) ||
		!validPluginVersion(journal.NextVersion) || len(journal.NextSHA256) != sha256.Size*2 ||
		journal.ConfigFingerprintSHA != "" && len(journal.ConfigFingerprintSHA) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid plugin install journal: %s", journalPath)
	}
	if _, errDecode := hex.DecodeString(journal.NextSHA256); errDecode != nil {
		return nil, fmt.Errorf("invalid plugin install artifact digest: %s", journalPath)
	}
	if journal.ConfigFingerprintSHA != "" {
		if _, errDecode := hex.DecodeString(journal.ConfigFingerprintSHA); errDecode != nil {
			return nil, fmt.Errorf("invalid plugin install config fingerprint: %s", journalPath)
		}
	}
	targetPath, errTarget := installJournalTarget(root, journal)
	if errTarget != nil {
		return nil, fmt.Errorf("invalid plugin install journal %s: %w", journalPath, errTarget)
	}
	if journal.Staged != "" && (journal.Staged != filepath.Base(journal.Staged) ||
		journal.Staged != "previous-"+filepath.Base(targetPath)) {
		return nil, fmt.Errorf("invalid plugin install stage: %s", journalPath)
	}
	return &InstallTransaction{root: root, stageDir: stageDir, journalPath: journalPath, journal: journal}, nil
}

func installJournalTarget(root string, journal installJournal) (string, error) {
	if filepath.IsAbs(journal.Target) || journal.Target == "" {
		return "", fmt.Errorf("unsafe plugin install target")
	}
	target := filepath.Join(root, journal.Target)
	relative, errRelative := filepath.Rel(root, target)
	if errRelative != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin install target escapes root")
	}
	file, okFile := pluginFileInfoFromPath(target, "")
	if !okFile || file.ID != journal.PluginID || normalizeVersion(file.Version) != journal.NextVersion {
		return "", fmt.Errorf("plugin install target identity mismatch")
	}
	if errValidate := validateInstallTargetDirectory(root, filepath.Dir(relative)); errValidate != nil {
		return "", errValidate
	}
	if targetInfo, errTarget := os.Lstat(target); errTarget == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return "", fmt.Errorf("plugin install target is not a regular file")
		}
	} else if !errors.Is(errTarget, os.ErrNotExist) {
		return "", fmt.Errorf("inspect plugin install target: %w", errTarget)
	}
	return target, nil
}

func validateInstallTargetDirectory(root string, relativeDir string) error {
	rootHandle, errRoot := os.OpenRoot(root)
	if errRoot != nil {
		return fmt.Errorf("open plugin root: %w", errRoot)
	}
	defer rootHandle.Close()
	current := rootHandle
	for _, component := range strings.Split(filepath.Clean(relativeDir), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		info, errInfo := current.Lstat(component)
		if errInfo != nil {
			if current != rootHandle {
				_ = current.Close()
			}
			return fmt.Errorf("inspect plugin install directory %q: %w", component, errInfo)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if current != rootHandle {
				_ = current.Close()
			}
			return fmt.Errorf("plugin install directory %q is not a regular directory", component)
		}
		next, errOpen := current.OpenRoot(component)
		if errOpen != nil {
			if current != rootHandle {
				_ = current.Close()
			}
			return fmt.Errorf("open plugin install directory %q: %w", component, errOpen)
		}
		openedInfo, errOpened := next.Stat(".")
		verifiedInfo, errVerified := current.Lstat(component)
		if errOpened != nil || errVerified != nil || !os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, verifiedInfo) {
			_ = next.Close()
			if current != rootHandle {
				_ = current.Close()
			}
			if errOpened != nil {
				return fmt.Errorf("inspect opened plugin install directory %q: %w", component, errOpened)
			}
			if errVerified != nil {
				return fmt.Errorf("verify plugin install directory %q: %w", component, errVerified)
			}
			return fmt.Errorf("plugin install directory %q changed during validation", component)
		}
		if current != rootHandle {
			_ = current.Close()
		}
		current = next
	}
	if current != rootHandle {
		return current.Close()
	}
	return nil
}

func rollbackInstallJournal(root, stageDir string, journal installJournal) error {
	targetPath, errTarget := installJournalTarget(root, journal)
	if errTarget != nil {
		return errTarget
	}
	stagedPath := ""
	if journal.Staged != "" {
		stagedPath = filepath.Join(stageDir, journal.Staged)
	}
	targetInfo, errTargetInfo := os.Stat(targetPath)
	if errTargetInfo != nil && !errors.Is(errTargetInfo, os.ErrNotExist) {
		return fmt.Errorf("stat installed plugin artifact: %w", errTargetInfo)
	}
	if stagedPath != "" {
		stagedInfo, errStaged := os.Stat(stagedPath)
		if errStaged != nil {
			return fmt.Errorf("stat previous plugin artifact: %w", errStaged)
		}
		if errTargetInfo == nil && os.SameFile(targetInfo, stagedInfo) {
			return nil
		}
	}
	if errTargetInfo == nil {
		matches, errMatch := fileMatchesSHA256(targetPath, journal.NextSHA256)
		if errMatch != nil {
			return errMatch
		}
		if !matches {
			return fmt.Errorf("installed plugin artifact changed before recovery: %s", targetPath)
		}
	}
	if stagedPath != "" {
		if errRestore := restoreInstallArtifact(stagedPath, targetPath); errRestore != nil {
			return fmt.Errorf("restore previous plugin artifact: %w", errRestore)
		}
		return nil
	}
	if errTargetInfo == nil {
		if errRemove := os.Remove(targetPath); errRemove != nil {
			return fmt.Errorf("remove interrupted plugin install: %w", errRemove)
		}
		return syncDirectory(filepath.Dir(targetPath))
	}
	return nil
}

func restoreInstallArtifact(stagedPath, targetPath string) error {
	stagedInfo, errStaged := os.Lstat(stagedPath)
	if errStaged != nil {
		return errStaged
	}
	if stagedInfo.Mode()&os.ModeSymlink != 0 || !stagedInfo.Mode().IsRegular() {
		return fmt.Errorf("staged plugin artifact is not a regular file")
	}
	targetDir := filepath.Dir(targetPath)
	tempPath := filepath.Join(
		targetDir,
		"."+filepath.Base(targetPath)+".rollback-"+rand.Text(),
	)
	if errLink := os.Link(stagedPath, tempPath); errLink != nil {
		return errLink
	}
	defer os.Remove(tempPath)
	tempInfo, errTemp := os.Lstat(tempPath)
	if errTemp != nil || !tempInfo.Mode().IsRegular() || !os.SameFile(stagedInfo, tempInfo) {
		return fmt.Errorf("temporary rollback artifact does not match staged artifact")
	}
	if errReplace := replaceFile(tempPath, targetPath); errReplace != nil {
		return errReplace
	}
	restoredInfo, errRestored := os.Lstat(targetPath)
	if errRestored != nil || !restoredInfo.Mode().IsRegular() || !os.SameFile(stagedInfo, restoredInfo) {
		return fmt.Errorf("restored plugin artifact does not match staged artifact")
	}
	return syncDirectory(targetDir)
}

func finalizeInstallJournal(root, stageDir string, journal installJournal) error {
	targetPath, errTarget := installJournalTarget(root, journal)
	if errTarget != nil {
		return errTarget
	}
	matches, errMatch := fileMatchesSHA256(targetPath, journal.NextSHA256)
	if errMatch != nil {
		return errMatch
	}
	if !matches {
		return fmt.Errorf("committed plugin artifact does not match install journal: %s", targetPath)
	}
	if journal.Staged != "" {
		if errRemove := os.Remove(filepath.Join(stageDir, journal.Staged)); errRemove != nil &&
			!errors.Is(errRemove, os.ErrNotExist) {
			return fmt.Errorf("remove previous plugin artifact: %w", errRemove)
		}
	}
	return nil
}

func fileMatchesSHA256(path, expected string) (bool, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return false, fmt.Errorf("open plugin artifact: %w", errOpen)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, errCopy := io.Copy(hash, file); errCopy != nil {
		return false, fmt.Errorf("hash plugin artifact: %w", errCopy)
	}
	return hex.EncodeToString(hash.Sum(nil)) == strings.ToLower(strings.TrimSpace(expected)), nil
}

func (t *InstallTransaction) writeJournal() error {
	data, errEncode := json.Marshal(t.journal)
	if errEncode != nil {
		return fmt.Errorf("encode plugin install journal: %w", errEncode)
	}
	if len(data) > installJournalMaxBytes {
		return fmt.Errorf("plugin install journal exceeds %d bytes", installJournalMaxBytes)
	}
	temp, errTemp := os.CreateTemp(t.stageDir, ".journal-*")
	if errTemp != nil {
		return fmt.Errorf("create plugin install journal: %w", errTemp)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	var errWrite error
	if errWrite = temp.Chmod(0o600); errWrite == nil {
		_, errWrite = temp.Write(data)
	}
	if errWrite == nil {
		errWrite = temp.Sync()
	}
	if errWrite == nil {
		errWrite = temp.Close()
	}
	if errWrite != nil {
		return fmt.Errorf("write plugin install journal: %w", errWrite)
	}
	if errWrite = replaceFile(tempPath, t.journalPath); errWrite != nil {
		return fmt.Errorf("publish plugin install journal: %w", errWrite)
	}
	removeTemp = false
	return syncDirectory(t.stageDir)
}
