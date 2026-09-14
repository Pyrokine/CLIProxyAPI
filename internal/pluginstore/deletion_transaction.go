package pluginstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var activeDeletionTransactions sync.Map

const (
	deletionTransactionPrefix = ".cliproxy-plugin-delete-"
	deletionCleanupPrefix     = ".cliproxy-plugin-cleanup-"
	deletionJournalName       = "journal.json"
	deletionJournalMaxBytes   = 1 << 20
	deletionJournalMaxEntries = 4096
)

type deletionJournal struct {
	Version   int                    `json:"version"`
	PluginID  string                 `json:"plugin_id"`
	Committed bool                   `json:"committed"`
	Entries   []deletionJournalEntry `json:"entries"`
}

type deletionJournalEntry struct {
	Target string `json:"target"`
	Staged string `json:"staged"`
}

type DeletionTransaction struct {
	root        string
	journalPath string
	stageDir    string
	journal     deletionJournal
	rootLock    *RootLock
}

func FindPluginArtifacts(root, id string) ([]string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	id = CanonicalPluginID(id)
	if !validPluginID(id) {
		return nil, fmt.Errorf("invalid plugin id %q", id)
	}
	paths := make([]string, 0)
	var scan func(string, int) error
	scan = func(dir string, depth int) error {
		entries, errReadDir := os.ReadDir(dir)
		if errReadDir != nil {
			if errors.Is(errReadDir, os.ErrNotExist) {
				return nil
			}
			return errReadDir
		}
		for _, entry := range entries {
			if entry == nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if entry.Type().IsRegular() {
				file, ok := pluginFileInfoFromPath(path, "")
				if ok && file.ID == id {
					paths = append(paths, path)
				}
				continue
			}
			if depth < 2 && entry.IsDir() &&
				!strings.HasPrefix(entry.Name(), deletionTransactionPrefix) &&
				!strings.HasPrefix(entry.Name(), deletionCleanupPrefix) {
				if errScan := scan(path, depth+1); errScan != nil {
					return errScan
				}
			}
		}
		return nil
	}
	if err := scan(root, 0); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return compactStrings(paths), nil
}

func PrepareDeletionTransaction(root, pluginID string, targetPaths []string) (*DeletionTransaction, error) {
	rootLock, errLock := LockRoot(root)
	if errLock != nil {
		return nil, errLock
	}
	transaction, errPrepare := prepareDeletionTransaction(root, pluginID, targetPaths)
	if errPrepare != nil {
		rootLock.Unlock()
		return nil, errPrepare
	}
	transaction.rootLock = rootLock
	return transaction, nil
}

// PrepareDeletionTransactionLocked prepares a deletion while the caller holds rootLock.
func PrepareDeletionTransactionLocked(
	root string,
	pluginID string,
	targetPaths []string,
	rootLock *RootLock,
) (*DeletionTransaction, error) {
	if rootLock == nil || !rootLock.matches(root) {
		return nil, fmt.Errorf("plugin root lock does not match deletion directory")
	}
	return prepareDeletionTransaction(root, pluginID, targetPaths)
}

func prepareDeletionTransaction(root, pluginID string, targetPaths []string) (*DeletionTransaction, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	pluginID = CanonicalPluginID(pluginID)
	if !validPluginID(pluginID) {
		return nil, fmt.Errorf("invalid plugin id %q", pluginID)
	}
	pendingDeletions, errPending := PendingDeletionPluginIDs(root)
	if errPending != nil {
		return nil, fmt.Errorf("inspect pending plugin deletions: %w", errPending)
	}
	if _, pending := pendingDeletions[pluginID]; pending {
		return nil, fmt.Errorf("%w: %s", ErrPendingDeletion, pluginID)
	}
	pendingInstalls, errPending := PendingInstallPluginIDs(root)
	if errPending != nil {
		return nil, fmt.Errorf("inspect pending plugin installs: %w", errPending)
	}
	if _, pending := pendingInstalls[pluginID]; pending {
		return nil, fmt.Errorf("%w: %s", ErrPendingInstall, pluginID)
	}
	targetPaths = compactStrings(targetPaths)
	if len(targetPaths) > deletionJournalMaxEntries {
		return nil, fmt.Errorf("too many plugin artifacts for deletion: %d", len(targetPaths))
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", err)
	}
	if err = os.MkdirAll(absoluteRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create plugin root: %w", err)
	}
	absoluteRoot, err = filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin root links: %w", err)
	}
	stageDir, err := os.MkdirTemp(absoluteRoot, deletionTransactionPrefix+pluginID+"-")
	if err != nil {
		return nil, fmt.Errorf("create plugin deletion transaction: %w", err)
	}
	activeDeletionTransactions.Store(stageDir, struct{}{})
	if err = os.Chmod(stageDir, 0o700); err != nil {
		return nil, cleanupDeletionPreparation(
			absoluteRoot,
			stageDir,
			fmt.Errorf("secure plugin deletion transaction: %w", err),
		)
	}
	transaction := &DeletionTransaction{
		root:        absoluteRoot,
		stageDir:    stageDir,
		journalPath: filepath.Join(stageDir, deletionJournalName),
		journal: deletionJournal{
			Version:  1,
			PluginID: pluginID,
		},
	}
	for index, targetPath := range targetPaths {
		absoluteTarget, errTarget := filepath.Abs(targetPath)
		if errTarget != nil {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("resolve plugin artifact: %w", errTarget),
			)
		}
		inputInfo, errTarget := os.Lstat(absoluteTarget)
		if errTarget != nil {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("stat plugin artifact: %w", errTarget),
			)
		}
		if !inputInfo.Mode().IsRegular() {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("plugin artifact is not a regular file: %s", targetPath),
			)
		}
		absoluteTarget, errTarget = filepath.EvalSymlinks(absoluteTarget)
		if errTarget != nil {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("resolve plugin artifact links: %w", errTarget),
			)
		}
		targetRelative, okTarget := safeRelativePluginPath(absoluteRoot, absoluteTarget, pluginID)
		if !okTarget {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("plugin artifact is outside the plugin root: %s", targetPath),
			)
		}
		targetInfo, errTarget := os.Lstat(absoluteTarget)
		if errTarget != nil {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("stat plugin artifact: %w", errTarget),
			)
		}
		if !targetInfo.Mode().IsRegular() {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("plugin artifact is not a regular file: %s", targetPath),
			)
		}
		stagedName := fmt.Sprintf("%04d-%s", index, filepath.Base(absoluteTarget))
		stagedPath := filepath.Join(stageDir, stagedName)
		if errTarget = os.Link(absoluteTarget, stagedPath); errTarget != nil {
			return nil, cleanupDeletionPreparation(
				absoluteRoot,
				stageDir,
				fmt.Errorf("preserve plugin artifact for deletion: %w", errTarget),
			)
		}
		transaction.journal.Entries = append(
			transaction.journal.Entries,
			deletionJournalEntry{Target: targetRelative, Staged: stagedName},
		)
	}
	if err = transaction.writeJournal(); err != nil {
		return nil, cleanupDeletionPreparation(absoluteRoot, stageDir, err)
	}
	if err = syncDirectory(absoluteRoot); err != nil {
		return nil, cleanupDeletionPreparation(
			absoluteRoot,
			stageDir,
			fmt.Errorf("sync plugin deletion transaction: %w", err),
		)
	}
	return transaction, nil
}

func cleanupDeletionPreparation(root, stageDir string, cause error) error {
	activeDeletionTransactions.Delete(stageDir)
	errRemove := os.RemoveAll(stageDir)
	if errRemove != nil {
		errRemove = fmt.Errorf("remove plugin deletion transaction: %w", errRemove)
	}
	errSync := syncDirectory(root)
	if errSync != nil {
		errSync = fmt.Errorf("sync plugin root after deletion transaction cleanup: %w", errSync)
	}
	return errors.Join(cause, errRemove, errSync)
}

func (t *DeletionTransaction) Removed() bool {
	return t != nil && len(t.journal.Entries) > 0
}

func (t *DeletionTransaction) Deactivate() {
	if t == nil {
		return
	}
	if t.stageDir != "" {
		activeDeletionTransactions.Delete(t.stageDir)
	}
	if t.rootLock != nil {
		t.rootLock.Unlock()
		t.rootLock = nil
	}
}

func (t *DeletionTransaction) MarkCommitted() error {
	if t == nil || t.stageDir == "" {
		return nil
	}
	t.journal.Committed = true
	return t.writeJournal()
}

func (t *DeletionTransaction) Hide() error {
	if t == nil || t.stageDir == "" {
		return nil
	}
	removed := make([]deletionJournalEntry, 0, len(t.journal.Entries))
	for _, entry := range t.journal.Entries {
		targetPath, errTarget := safeDeletionTargetPath(t.root, entry.Target, t.journal.PluginID)
		if errTarget != nil {
			return fmt.Errorf("resolve plugin deletion target: %w", errTarget)
		}
		stagedPath := filepath.Join(t.stageDir, entry.Staged)
		targetExists, _, errState := deletionArtifactState(targetPath, stagedPath)
		if errState != nil {
			errRestore := t.restoreEntries(removed)
			return fmt.Errorf("verify plugin artifact for deletion: %w", errors.Join(errState, errRestore))
		}
		if targetExists {
			hiddenPath := deletionHiddenPath(t.stageDir, entry)
			if errRename := os.Rename(targetPath, hiddenPath); errRename != nil {
				errRestore := t.restoreEntries(removed)
				return fmt.Errorf("hide plugin artifact for deletion: %w", errors.Join(errRename, errRestore))
			}
			hiddenInfo, errHidden := os.Lstat(hiddenPath)
			stagedInfo, errStaged := os.Lstat(stagedPath)
			if errHidden != nil || errStaged != nil || !hiddenInfo.Mode().IsRegular() ||
				!stagedInfo.Mode().IsRegular() || !os.SameFile(hiddenInfo, stagedInfo) {
				errRestoreTarget := os.Rename(hiddenPath, targetPath)
				errRestore := t.restoreEntries(removed)
				return fmt.Errorf(
					"plugin artifact changed during deletion: %w",
					errors.Join(
						fmt.Errorf("target %s no longer matches the preserved artifact", targetPath),
						errHidden,
						errStaged,
						errRestoreTarget,
						errRestore,
					),
				)
			}
		}
		removed = append(removed, entry)
		if errSync := syncDirectory(filepath.Dir(targetPath)); errSync != nil {
			errRestore := t.restoreEntries(removed)
			return fmt.Errorf("sync hidden plugin artifact: %w", errors.Join(errSync, errRestore))
		}
		if errSync := syncDirectory(t.stageDir); errSync != nil {
			errRestore := t.restoreEntries(removed)
			return fmt.Errorf("sync preserved plugin artifact: %w", errors.Join(errSync, errRestore))
		}
	}
	return nil
}

func deletionHiddenPath(stageDir string, entry deletionJournalEntry) string {
	return filepath.Join(stageDir, entry.Staged+".hidden")
}

func (t *DeletionTransaction) Rollback() error {
	if t == nil || t.stageDir == "" {
		return nil
	}
	if err := t.restoreEntries(t.journal.Entries); err != nil {
		return err
	}
	return t.removeStage()
}

func (t *DeletionTransaction) Commit() error {
	if t == nil || t.stageDir == "" {
		return nil
	}
	for _, entry := range t.journal.Entries {
		targetPath, errTarget := safeDeletionTargetPath(t.root, entry.Target, t.journal.PluginID)
		if errTarget != nil {
			return fmt.Errorf("resolve plugin deletion target: %w", errTarget)
		}
		stagedPath := filepath.Join(t.stageDir, entry.Staged)
		targetExists, same, errState := deletionArtifactState(targetPath, stagedPath)
		if errState != nil {
			return fmt.Errorf("verify plugin artifact deletion: %w", errState)
		}
		if targetExists && !same {
			return fmt.Errorf("plugin artifact changed during deletion: %s", targetPath)
		}
		if targetExists {
			if errRemove := os.Remove(targetPath); errRemove != nil {
				return fmt.Errorf("remove plugin artifact: %w", errRemove)
			}
		}
		if errSync := syncDirectory(filepath.Dir(targetPath)); errSync != nil {
			return errSync
		}
	}
	if errLate := t.quarantineLateArtifacts(); errLate != nil {
		return errLate
	}
	return t.removeStage()
}

func (t *DeletionTransaction) quarantineLateArtifacts() error {
	for attempt := range 4 {
		paths, errFind := FindPluginArtifacts(t.root, t.journal.PluginID)
		if errFind != nil {
			return fmt.Errorf("rescan plugin artifacts before deletion commit: %w", errFind)
		}
		if len(paths) == 0 {
			return nil
		}
		for index, artifactPath := range paths {
			absolutePath, errAbs := filepath.Abs(artifactPath)
			if errAbs != nil {
				return fmt.Errorf("resolve late plugin artifact: %w", errAbs)
			}
			if _, safe := safeRelativePluginPath(t.root, absolutePath, t.journal.PluginID); !safe {
				return fmt.Errorf("late plugin artifact is outside the plugin root: %s", artifactPath)
			}
			quarantinePath := filepath.Join(
				t.stageDir,
				fmt.Sprintf(".late-%d-%04d-%s", attempt, index, filepath.Base(absolutePath)),
			)
			if errRename := os.Rename(absolutePath, quarantinePath); errRename != nil {
				return fmt.Errorf("quarantine late plugin artifact: %w", errRename)
			}
			if errSync := syncDirectory(filepath.Dir(absolutePath)); errSync != nil {
				return errSync
			}
		}
		if errSync := syncDirectory(t.stageDir); errSync != nil {
			return errSync
		}
	}
	return fmt.Errorf("plugin artifacts continued to appear during deletion commit")
}

// PendingDeletionPluginIDs returns plugin IDs hidden by durable deletion journals.
func PendingDeletionPluginIDs(root string) (map[string]struct{}, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, errAbs := filepath.Abs(root)
	if errAbs != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", errAbs)
	}
	resolvedRoot, errResolve := filepath.EvalSymlinks(absoluteRoot)
	if errors.Is(errResolve, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if errResolve != nil {
		return nil, fmt.Errorf("resolve plugin root links: %w", errResolve)
	}
	entries, errReadDir := os.ReadDir(resolvedRoot)
	if errReadDir != nil {
		return nil, fmt.Errorf("read plugin root: %w", errReadDir)
	}
	pending := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || !entry.IsDir() || !strings.HasPrefix(entry.Name(), deletionTransactionPrefix) {
			continue
		}
		transaction, errLoad := loadDeletionTransaction(resolvedRoot, filepath.Join(resolvedRoot, entry.Name()))
		if errors.Is(errLoad, os.ErrNotExist) {
			continue
		}
		if errLoad != nil {
			return nil, errLoad
		}
		pending[transaction.journal.PluginID] = struct{}{}
	}
	return pending, nil
}

func RecoverDeletionTransactions(root string, configuredPluginIDs map[string]struct{}) error {
	canonicalConfiguredPluginIDs := make(map[string]struct{}, len(configuredPluginIDs))
	for id := range configuredPluginIDs {
		canonicalConfiguredPluginIDs[CanonicalPluginID(id)] = struct{}{}
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve plugin root: %w", err)
	}
	if _, errStat := os.Stat(absoluteRoot); errStat != nil {
		if errors.Is(errStat, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat plugin root: %w", errStat)
	}
	absoluteRoot, err = filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return fmt.Errorf("resolve plugin root links: %w", err)
	}
	entries, errReadDir := os.ReadDir(absoluteRoot)
	if errReadDir != nil {
		if errors.Is(errReadDir, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read plugin root: %w", errReadDir)
	}
	var recoveryErrors []error
	for _, entry := range entries {
		if entry == nil || !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), deletionCleanupPrefix) {
			cleanupDir := filepath.Join(absoluteRoot, entry.Name())
			if errCleanup := os.RemoveAll(cleanupDir); errCleanup != nil {
				recoveryErrors = append(
					recoveryErrors,
					fmt.Errorf("remove plugin deletion cleanup: %w", errCleanup),
				)
			} else if errSync := syncDirectory(absoluteRoot); errSync != nil {
				recoveryErrors = append(recoveryErrors, errSync)
			}
			continue
		}
		if !strings.HasPrefix(entry.Name(), deletionTransactionPrefix) {
			continue
		}
		stageDir := filepath.Join(absoluteRoot, entry.Name())
		if _, active := activeDeletionTransactions.Load(stageDir); active {
			continue
		}
		transaction, errLoad := loadDeletionTransaction(absoluteRoot, stageDir)
		if errors.Is(errLoad, os.ErrNotExist) {
			if errCleanup := cleanupUnjournaledDeletionStage(absoluteRoot, stageDir); errCleanup != nil {
				recoveryErrors = append(recoveryErrors, errCleanup)
			}
			continue
		}
		if errLoad != nil {
			recoveryErrors = append(recoveryErrors, errLoad)
			continue
		}
		_, configured := canonicalConfiguredPluginIDs[transaction.journal.PluginID]
		if configured || !transaction.journal.Committed {
			errLoad = transaction.Rollback()
		} else {
			errLoad = transaction.Commit()
		}
		if errLoad != nil {
			recoveryErrors = append(recoveryErrors, errLoad)
		}
	}
	return errors.Join(recoveryErrors...)
}

func cleanupUnjournaledDeletionStage(root, stageDir string) error {
	cleanupDir := deletionCleanupPath(stageDir)
	if errRename := os.Rename(stageDir, cleanupDir); errRename != nil {
		return fmt.Errorf("quarantine incomplete plugin deletion transaction: %w", errRename)
	}
	if errSync := syncDirectory(root); errSync != nil {
		return fmt.Errorf("sync incomplete plugin deletion transaction quarantine: %w", errSync)
	}
	if errRemove := os.RemoveAll(cleanupDir); errRemove != nil {
		return fmt.Errorf("remove incomplete plugin deletion transaction: %w", errRemove)
	}
	if errSync := syncDirectory(root); errSync != nil {
		return fmt.Errorf("sync incomplete plugin deletion transaction cleanup: %w", errSync)
	}
	return nil
}

func deletionCleanupPath(stageDir string) string {
	root := filepath.Dir(stageDir)
	return filepath.Join(
		root,
		deletionCleanupPrefix+strings.TrimPrefix(filepath.Base(stageDir), deletionTransactionPrefix),
	)
}

func loadDeletionTransaction(root, stageDir string) (*DeletionTransaction, error) {
	journalPath := filepath.Join(stageDir, deletionJournalName)
	journalInfo, err := os.Lstat(journalPath)
	if err != nil {
		return nil, fmt.Errorf("stat plugin deletion journal: %w", err)
	}
	if !journalInfo.Mode().IsRegular() || journalInfo.Size() > deletionJournalMaxBytes {
		return nil, fmt.Errorf("invalid plugin deletion journal: %s", journalPath)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		return nil, fmt.Errorf("read plugin deletion journal: %w", err)
	}
	var journal deletionJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		return nil, fmt.Errorf("decode plugin deletion journal: %w", err)
	}
	journal.PluginID = CanonicalPluginID(journal.PluginID)
	if journal.Version != 1 || !validPluginID(journal.PluginID) ||
		len(journal.Entries) > deletionJournalMaxEntries {
		return nil, fmt.Errorf("invalid plugin deletion journal: %s", journalPath)
	}
	seenTargets := make(map[string]struct{}, len(journal.Entries))
	seenStaged := make(map[string]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		if filepath.IsAbs(entry.Target) {
			return nil, fmt.Errorf("unsafe plugin deletion target in %s", journalPath)
		}
		targetPath, errTarget := safeDeletionTargetPath(root, entry.Target, journal.PluginID)
		if errTarget != nil {
			return nil, fmt.Errorf("unsafe plugin deletion target in %s: %w", journalPath, errTarget)
		}
		if entry.Staged != filepath.Base(entry.Staged) || entry.Staged == "" ||
			!strings.HasSuffix(entry.Staged, "-"+filepath.Base(targetPath)) {
			return nil, fmt.Errorf("unsafe plugin deletion stage in %s", journalPath)
		}
		cleanTarget := filepath.Clean(entry.Target)
		if _, duplicate := seenTargets[cleanTarget]; duplicate {
			return nil, fmt.Errorf("duplicate plugin deletion target in %s", journalPath)
		}
		if _, duplicate := seenStaged[entry.Staged]; duplicate {
			return nil, fmt.Errorf("duplicate plugin deletion stage in %s", journalPath)
		}
		seenTargets[cleanTarget] = struct{}{}
		seenStaged[entry.Staged] = struct{}{}
	}
	return &DeletionTransaction{
		root:        root,
		journalPath: journalPath,
		stageDir:    stageDir,
		journal:     journal,
	}, nil
}

func (t *DeletionTransaction) writeJournal() error {
	data, err := json.Marshal(t.journal)
	if err != nil {
		return fmt.Errorf("encode plugin deletion journal: %w", err)
	}
	if len(data) > deletionJournalMaxBytes {
		return fmt.Errorf("plugin deletion journal exceeds %d bytes", deletionJournalMaxBytes)
	}
	temp, err := os.CreateTemp(t.stageDir, ".journal-*")
	if err != nil {
		return fmt.Errorf("create plugin deletion journal: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if err == nil {
		err = temp.Close()
	}
	if err != nil {
		return fmt.Errorf("write plugin deletion journal: %w", err)
	}
	if err = replaceFile(tempPath, t.journalPath); err != nil {
		return fmt.Errorf("publish plugin deletion journal: %w", err)
	}
	removeTemp = false
	return syncDirectory(t.stageDir)
}

func (t *DeletionTransaction) restoreEntries(entries []deletionJournalEntry) error {
	var restoreErrors []error
	for _, entry := range entries {
		targetPath, errTarget := safeDeletionTargetPath(t.root, entry.Target, t.journal.PluginID)
		if errTarget != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("resolve plugin deletion target: %w", errTarget))
			continue
		}
		stagedPath := filepath.Join(t.stageDir, entry.Staged)
		targetExists, same, errState := deletionArtifactState(targetPath, stagedPath)
		if errState != nil {
			restoreErrors = append(restoreErrors, errState)
			continue
		}
		hiddenPath := deletionHiddenPath(t.stageDir, entry)
		_, errHidden := os.Lstat(hiddenPath)
		hiddenExists := errHidden == nil
		if errHidden != nil && !errors.Is(errHidden, os.ErrNotExist) {
			restoreErrors = append(restoreErrors, fmt.Errorf("stat hidden plugin artifact: %w", errHidden))
			continue
		}
		if targetExists {
			if !same {
				restoreErrors = append(
					restoreErrors,
					fmt.Errorf("refuse to overwrite changed plugin artifact: %s", targetPath),
				)
			}
			if hiddenExists {
				if errRemove := os.Remove(hiddenPath); errRemove != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("remove redundant hidden artifact: %w", errRemove))
				}
			}
			continue
		}
		if hiddenExists {
			if errRename := os.Rename(hiddenPath, targetPath); errRename != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore hidden plugin artifact: %w", errRename))
				continue
			}
		} else if errLink := os.Link(stagedPath, targetPath); errLink != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore plugin artifact: %w", errLink))
			continue
		}
		if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	return errors.Join(restoreErrors...)
}

func safeDeletionTargetPath(root, relative, pluginID string) (string, error) {
	relative = filepath.Clean(relative)
	targetPath := filepath.Join(root, relative)
	if _, ok := safeRelativePluginPath(root, targetPath, pluginID); !ok {
		return "", fmt.Errorf("plugin artifact is outside the plugin root: %s", relative)
	}
	parentRelative := filepath.Dir(relative)
	if parentRelative == "." {
		return targetPath, nil
	}
	current := root
	for component := range strings.SplitSeq(parentRelative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("stat plugin artifact directory: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("plugin artifact directory is not a directory: %s", current)
		}
	}
	return targetPath, nil
}

func deletionArtifactState(targetPath, stagedPath string) (bool, bool, error) {
	stagedInfo, err := os.Lstat(stagedPath)
	if err != nil {
		return false, false, fmt.Errorf("stat preserved plugin artifact: %w", err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return false, false, fmt.Errorf("preserved plugin artifact is not a regular file: %s", stagedPath)
	}
	targetInfo, err := os.Lstat(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("stat plugin artifact: %w", err)
	}
	if !targetInfo.Mode().IsRegular() {
		return true, false, nil
	}
	return true, os.SameFile(targetInfo, stagedInfo), nil
}

func (t *DeletionTransaction) removeStage() error {
	stageDir := t.stageDir
	root := filepath.Dir(stageDir)
	cleanupDir := deletionCleanupPath(stageDir)
	if err := os.Rename(stageDir, cleanupDir); err != nil {
		return fmt.Errorf("finalize plugin deletion transaction: %w", err)
	}
	activeDeletionTransactions.Delete(stageDir)
	t.root = ""
	t.journalPath = ""
	t.stageDir = ""
	t.journal = deletionJournal{}
	if err := syncDirectory(root); err != nil {
		return err
	}
	if err := os.RemoveAll(cleanupDir); err != nil {
		return fmt.Errorf("remove plugin deletion transaction: %w", err)
	}
	errSync := syncDirectory(root)
	t.Deactivate()
	return errSync
}

func safeRelativePluginPath(root, target, pluginID string) (string, bool) {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return "", false
	}
	cleaned := filepath.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false
	}
	file, ok := pluginFileInfoFromPath(target, "")
	return cleaned, ok && file.ID == pluginID
}

func compactStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
