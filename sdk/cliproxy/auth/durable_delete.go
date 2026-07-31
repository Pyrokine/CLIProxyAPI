package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const durablePersistedDeletionJournalSuffix = ".auth-delete-journal"

type durablePersistedDeletionJournal struct {
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
}

// DurablePersistedFileDeletion keeps an exact local credential version hidden
// while its backing store deletion is committed. Its journal survives process
// interruption so Bootstrap can finish the same conditional deletion.
type DurablePersistedFileDeletion struct {
	path         string
	capturedPath string
	journalPath  string
	version      PersistedVersion
	active       bool
}

// Path returns the live credential path owned by the deletion transaction.
func (d *DurablePersistedFileDeletion) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Version returns the exact credential version owned by the deletion transaction.
func (d *DurablePersistedFileDeletion) Version() PersistedVersion {
	if d == nil {
		return PersistedVersion{}
	}
	return d.version
}

// PrepareDurablePersistedFileDeletion journals and hides the expected local
// credential before a backing store mutation starts.
func PrepareDurablePersistedFileDeletion(
	path string,
	version PersistedVersion,
) (*DurablePersistedFileDeletion, bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, false, fmt.Errorf("persisted credential path is empty")
	}
	if filepath.Clean(version.Path) != path {
		return nil, false, nil
	}
	journalPath, capturedPath := durablePersistedDeletionPaths(path)
	if _, errJournal := os.Lstat(journalPath); errJournal == nil {
		return nil, false, fmt.Errorf("unfinished credential deletion exists for %s", path)
	} else if !errors.Is(errJournal, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("inspect credential deletion journal: %w", errJournal)
	}
	if _, errCaptured := os.Lstat(capturedPath); errCaptured == nil {
		return nil, false, fmt.Errorf("unfinished credential deletion capture exists for %s", path)
	} else if !errors.Is(errCaptured, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("inspect credential deletion capture: %w", errCaptured)
	}

	liveExists := true
	if _, errStat := os.Lstat(path); errors.Is(errStat, fs.ErrNotExist) {
		liveExists = false
	} else if errStat != nil {
		return nil, false, fmt.Errorf("inspect persisted credential before deletion: %w", errStat)
	}
	if liveExists {
		current, errVersion := ReadPersistedVersion(path)
		if errVersion != nil {
			return nil, false, errVersion
		}
		if current.SHA256 != version.SHA256 {
			return nil, false, nil
		}
	}

	journal := durablePersistedDeletionJournal{
		Target: filepath.Base(path),
		SHA256: hex.EncodeToString(version.SHA256[:]),
	}
	if errWrite := writeDurablePersistedDeletionJournal(journalPath, journal); errWrite != nil {
		return nil, false, errWrite
	}
	deletion := &DurablePersistedFileDeletion{
		path:         path,
		capturedPath: capturedPath,
		journalPath:  journalPath,
		version:      version,
		active:       true,
	}
	if liveExists {
		if errRename := os.Rename(path, capturedPath); errRename != nil {
			return nil, false, errors.Join(
				fmt.Errorf("capture persisted credential for durable deletion: %w", errRename),
				deletion.removeJournal(),
			)
		}
		if errSync := syncPersistedDeletionDirectory(filepath.Dir(path)); errSync != nil {
			return nil, false, errors.Join(errSync, deletion.Rollback())
		}
		captured, errCapturedVersion := ReadPersistedVersion(capturedPath)
		if errCapturedVersion != nil {
			return nil, false, errors.Join(errCapturedVersion, deletion.Rollback())
		}
		if captured.SHA256 != version.SHA256 {
			return nil, false, deletion.Rollback()
		}
	}
	return deletion, true, nil
}

// RecoverDurablePersistedFileDeletions loads interrupted credential deletions
// before a backing store mirrors remote state into the local auth directory.
func RecoverDurablePersistedFileDeletions(
	root string,
) ([]*DurablePersistedFileDeletion, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("persisted credential root is empty")
	}
	deletions := make([]*DurablePersistedFileDeletion, 0)
	errWalk := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), durablePersistedDeletionJournalSuffix) {
			return nil
		}
		deletion, keep, errRecover := recoverDurablePersistedFileDeletion(path)
		if errRecover != nil {
			return errRecover
		}
		if keep {
			deletions = append(deletions, deletion)
		}
		return nil
	})
	if errWalk != nil {
		return nil, fmt.Errorf("recover persisted credential deletions: %w", errWalk)
	}
	return deletions, nil
}

func recoverDurablePersistedFileDeletion(
	journalPath string,
) (*DurablePersistedFileDeletion, bool, error) {
	info, errInfo := os.Lstat(journalPath)
	if errInfo != nil {
		return nil, false, errInfo
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("credential deletion journal is not a regular file: %s", journalPath)
	}
	file, errOpen := os.Open(journalPath)
	if errOpen != nil {
		return nil, false, errOpen
	}
	raw, errRead := io.ReadAll(io.LimitReader(file, 4097))
	errClose := file.Close()
	if errRead != nil || errClose != nil {
		return nil, false, errors.Join(errRead, errClose)
	}
	if len(raw) > 4096 {
		return nil, false, fmt.Errorf("credential deletion journal is too large: %s", journalPath)
	}
	var journal durablePersistedDeletionJournal
	if errDecode := json.Unmarshal(raw, &journal); errDecode != nil {
		return nil, false, fmt.Errorf("decode credential deletion journal: %w", errDecode)
	}
	if journal.Target == "" || filepath.Base(journal.Target) != journal.Target || journal.Target == "." || journal.Target == ".." {
		return nil, false, fmt.Errorf("credential deletion journal has invalid target")
	}
	digest, errDigest := hex.DecodeString(strings.TrimSpace(journal.SHA256))
	if errDigest != nil || len(digest) != sha256.Size {
		return nil, false, fmt.Errorf("credential deletion journal has invalid digest")
	}
	path := filepath.Join(filepath.Dir(journalPath), journal.Target)
	expectedJournal, capturedPath := durablePersistedDeletionPaths(path)
	if filepath.Clean(expectedJournal) != filepath.Clean(journalPath) {
		return nil, false, fmt.Errorf("credential deletion journal name does not match target")
	}
	version := PersistedVersion{Path: path}
	copy(version.SHA256[:], digest)
	deletion := &DurablePersistedFileDeletion{
		path:         path,
		capturedPath: capturedPath,
		journalPath:  journalPath,
		version:      version,
		active:       true,
	}

	capturedInfo, errCaptured := os.Lstat(capturedPath)
	switch {
	case errCaptured == nil:
		if !capturedInfo.Mode().IsRegular() {
			return nil, false, fmt.Errorf("credential deletion capture is not a regular file: %s", capturedPath)
		}
		capturedVersion, errVersion := ReadPersistedVersion(capturedPath)
		if errVersion != nil {
			return nil, false, errVersion
		}
		if capturedVersion.SHA256 != version.SHA256 {
			return nil, false, fmt.Errorf("credential deletion capture digest does not match journal")
		}
		return deletion, true, nil
	case !errors.Is(errCaptured, fs.ErrNotExist):
		return nil, false, errCaptured
	}

	liveVersion, errLive := ReadPersistedVersion(path)
	switch {
	case errLive == nil && liveVersion.SHA256 == version.SHA256:
		if errRename := os.Rename(path, capturedPath); errRename != nil {
			return nil, false, fmt.Errorf("resume credential deletion capture: %w", errRename)
		}
		if errSync := syncPersistedDeletionDirectory(filepath.Dir(path)); errSync != nil {
			return nil, false, errSync
		}
		return deletion, true, nil
	case errLive == nil:
		if errRemove := deletion.removeJournal(); errRemove != nil {
			return nil, false, errRemove
		}
		return nil, false, nil
	case errors.Is(errLive, fs.ErrNotExist):
		return deletion, true, nil
	default:
		return nil, false, errLive
	}
}

// Commit permanently removes the captured local version and its journal.
func (d *DurablePersistedFileDeletion) Commit() error {
	if d == nil || !d.active {
		return nil
	}
	var errCommit error
	if d.capturedPath != "" {
		if errRemove := os.Remove(d.capturedPath); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
			errCommit = fmt.Errorf("remove captured credential version: %w", errRemove)
		}
	}
	if errCommit == nil {
		errCommit = d.removeJournal()
	}
	if errCommit == nil {
		d.active = false
	}
	return errCommit
}

// Rollback restores the captured version only when no replacement owns the live path.
func (d *DurablePersistedFileDeletion) Rollback() error {
	if d == nil || !d.active {
		return nil
	}
	var errRollback error
	if d.capturedPath != "" {
		if _, errStat := os.Lstat(d.capturedPath); errStat == nil {
			errRollback = restoreCapturedFile(d.path, d.capturedPath)
		} else if !errors.Is(errStat, fs.ErrNotExist) {
			errRollback = errStat
		}
	}
	if errRollback == nil {
		errRollback = d.removeJournal()
	}
	if errRollback == nil {
		d.active = false
	}
	return errRollback
}

func durablePersistedDeletionPaths(path string) (string, string) {
	digest := sha256.Sum256([]byte(filepath.Base(path)))
	prefix := ".auth-delete-" + hex.EncodeToString(digest[:8])
	dir := filepath.Dir(path)
	return filepath.Join(dir, prefix+durablePersistedDeletionJournalSuffix),
		filepath.Join(dir, prefix+".capture")
}

func writeDurablePersistedDeletionJournal(
	journalPath string,
	journal durablePersistedDeletionJournal,
) error {
	raw, errMarshal := json.Marshal(journal)
	if errMarshal != nil {
		return errMarshal
	}
	dir := filepath.Dir(journalPath)
	temp, errCreate := os.CreateTemp(dir, ".auth-delete-journal-*")
	if errCreate != nil {
		return fmt.Errorf("create credential deletion journal: %w", errCreate)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		_ = temp.Close()
		return errChmod
	}
	if _, errWrite := temp.Write(raw); errWrite != nil {
		_ = temp.Close()
		return errWrite
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return errSync
	}
	if errClose := temp.Close(); errClose != nil {
		return errClose
	}
	if errLink := os.Link(tempPath, journalPath); errLink != nil {
		return fmt.Errorf("publish credential deletion journal: %w", errLink)
	}
	if errRemove := os.Remove(tempPath); errRemove != nil {
		return errRemove
	}
	removeTemp = false
	return syncPersistedDeletionDirectory(dir)
}

func (d *DurablePersistedFileDeletion) removeJournal() error {
	if d == nil || d.journalPath == "" {
		return nil
	}
	if errRemove := os.Remove(d.journalPath); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
		return fmt.Errorf("remove credential deletion journal: %w", errRemove)
	}
	return syncPersistedDeletionDirectory(filepath.Dir(d.journalPath))
}

func syncPersistedDeletionDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, errOpen := os.Open(dir)
	if errOpen != nil {
		return fmt.Errorf("open credential deletion directory: %w", errOpen)
	}
	errSync := directory.Sync()
	errClose := directory.Close()
	return errors.Join(errSync, errClose)
}
