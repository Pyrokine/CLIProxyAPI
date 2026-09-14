package modelcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

var (
	storeMu       sync.RWMutex
	storeWriterMu sync.Mutex
	storeWriters  = make(map[string]chan struct{})
)

type UpdateOptions struct {
	Backup bool
	Commit func(File) error
}

type fileSnapshot struct {
	data   []byte
	exists bool
}

type UpdateTransaction struct {
	mu               sync.Mutex
	filePath         string
	previousPath     string
	candidatePath    string
	backupPath       string
	doc              File
	baseDoc          File
	stableCurrent    fileSnapshot
	rollbackCurrent  fileSnapshot
	previousSnapshot fileSnapshot
	rollbackPrevious fileSnapshot
	filesCommitted   bool
	finished         bool
	releaseWriter    func()
}

func Path(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

func PreviousPath(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), PreviousFileName)
}

func acquireStoreWriter(filePath string) func() {
	for {
		storeWriterMu.Lock()
		if wait := storeWriters[filePath]; wait != nil {
			storeWriterMu.Unlock()
			<-wait
			continue
		}
		wait := make(chan struct{})
		storeWriters[filePath] = wait
		storeWriterMu.Unlock()
		var once sync.Once
		return func() {
			once.Do(
				func() {
					storeWriterMu.Lock()
					delete(storeWriters, filePath)
					close(wait)
					storeWriterMu.Unlock()
				},
			)
		}
	}
}

func tryAcquireStoreWriter(filePath string) (func(), bool) {
	storeWriterMu.Lock()
	defer storeWriterMu.Unlock()
	if storeWriters[filePath] != nil {
		return nil, false
	}
	wait := make(chan struct{})
	storeWriters[filePath] = wait
	var once sync.Once
	return func() {
		once.Do(
			func() {
				storeWriterMu.Lock()
				delete(storeWriters, filePath)
				close(wait)
				storeWriterMu.Unlock()
			},
		)
	}, true
}

func Load(configPath string) (File, error) {
	return load(configPath, nil, false)
}

func LoadOrInit(configPath string, cfg *config.Config) (File, error) {
	return load(configPath, cfg, true)
}

func load(configPath string, cfg *config.Config, initialize bool) (File, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, fmt.Errorf("config file path not set")
	}
	storeMu.RLock()
	doc, rewrite, err := readCurrentDocumentUnlocked(filePath)
	storeMu.RUnlock()
	if err == nil && !rewrite {
		doc.LoadMetadata = loadMetadata{Source: LoadSourceCurrent}
		if releaseWriter, acquired := tryAcquireStoreWriter(filePath); acquired {
			storeMu.Lock()
			loaded, errLoad := loadStableUnlocked(filePath, cfg, initialize)
			storeMu.Unlock()
			releaseWriter()
			if errLoad != nil {
				return File{}, errLoad
			}
			return loaded.doc, nil
		}
		return doc, nil
	}

	releaseWriter, acquired := tryAcquireStoreWriter(filePath)
	if !acquired {
		return File{}, fmt.Errorf("model catalog update in progress")
	}
	defer releaseWriter()
	storeMu.Lock()
	defer storeMu.Unlock()
	loaded, err := loadStableUnlocked(filePath, cfg, initialize)
	if err != nil {
		return File{}, err
	}
	return loaded.doc, nil
}

func Save(configPath string, doc File) error {
	filePath := Path(configPath)
	if filePath == "" {
		return fmt.Errorf("config file path not set")
	}
	releaseWriter := acquireStoreWriter(filePath)
	defer releaseWriter()
	storeMu.Lock()
	defer storeMu.Unlock()

	candidateData, err := marshalFile(doc)
	if err != nil {
		return err
	}
	current, err := validatedSnapshotUnlocked(filePath)
	if err != nil {
		loaded, errLoad := loadStableUnlocked(filePath, nil, false)
		if errLoad != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return errLoad
			}
			if _, errPrevious := readFileSnapshotUnlocked(PreviousPath(configPath)); errPrevious != nil && !errors.Is(
				errPrevious, os.ErrNotExist,
			) {
				return errLoad
			} else if errPrevious == nil {
				return errLoad
			}
			current = fileSnapshot{}
		} else {
			current = loaded.current
		}
	}
	previous, err := readFileSnapshotUnlocked(PreviousPath(configPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read previous model catalog: %w", err)
	}
	return commitReplacementUnlocked(filePath, candidateData, current, previous)
}

func PrepareUpdate(
	configPath string,
	cfg *config.Config,
	backup bool,
	mutate func(*File) error,
) (*UpdateTransaction, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return nil, fmt.Errorf("config file path not set")
	}
	releaseWriter := acquireStoreWriter(filePath)
	releaseWrite := true
	defer func() {
		if releaseWrite {
			releaseWriter()
		}
	}()

	storeMu.Lock()
	defer storeMu.Unlock()
	loaded, err := loadStableUnlocked(filePath, cfg, true)
	if err != nil {
		return nil, err
	}
	previousPath := PreviousPath(configPath)
	previous, err := readFileSnapshotUnlocked(previousPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read previous model catalog: %w", err)
	}
	rollbackPrevious := previous
	if !loaded.rollbackCurrent.exists {
		rollbackPrevious = fileSnapshot{}
	}
	baseDoc, err := cloneFile(loaded.doc)
	if err != nil {
		return nil, fmt.Errorf("copy base model catalog: %w", err)
	}
	doc, err := cloneFile(loaded.doc)
	if err != nil {
		return nil, fmt.Errorf("copy candidate model catalog: %w", err)
	}
	if mutate != nil {
		if err = mutate(&doc); err != nil {
			return nil, err
		}
	}
	doc.LoadMetadata = loadMetadata{Source: LoadSourceCurrent}
	candidateData, err := marshalFile(doc)
	if err != nil {
		return nil, err
	}
	candidatePath, err := writeTempFileUnlocked(filePath, candidateData)
	if err != nil {
		return nil, err
	}
	backupPath := ""
	if backup {
		backupPath, err = writeBackupUnlocked(filePath, loaded.current.data)
		if err != nil {
			_ = os.Remove(candidatePath)
			return nil, err
		}
	}
	releaseWrite = false
	return &UpdateTransaction{
		filePath:         filePath,
		previousPath:     previousPath,
		candidatePath:    candidatePath,
		backupPath:       backupPath,
		doc:              doc,
		baseDoc:          baseDoc,
		stableCurrent:    loaded.current,
		rollbackCurrent:  loaded.rollbackCurrent,
		previousSnapshot: previous,
		rollbackPrevious: rollbackPrevious,
		releaseWriter:    releaseWriter,
	}, nil
}

func update(
	configPath string,
	cfg *config.Config,
	options UpdateOptions,
	mutate func(*File) error,
) (File, string, error) {
	transaction, err := PrepareUpdate(configPath, cfg, options.Backup, mutate)
	if err != nil {
		return File{}, "", err
	}
	defer func() { _ = transaction.Rollback() }()
	if err = transaction.CommitFiles(); err != nil {
		return File{}, "", err
	}
	doc := transaction.Document()
	if options.Commit != nil {
		if err = options.Commit(doc); err != nil {
			rollbackErr := transaction.Rollback()
			if rollbackErr != nil {
				return File{}, "", errors.Join(err, fmt.Errorf("restore model catalog: %w", rollbackErr))
			}
			return File{}, "", err
		}
	}
	if err = transaction.Finish(); err != nil {
		return File{}, "", err
	}
	return doc, transaction.BackupPath(), nil
}

func (t *UpdateTransaction) Document() File {
	if t == nil {
		return File{}
	}
	return t.doc
}

func (t *UpdateTransaction) BaseDocument() File {
	if t == nil {
		return File{}
	}
	return t.baseDoc
}

func (t *UpdateTransaction) BackupPath() string {
	if t == nil {
		return ""
	}
	return t.backupPath
}

func (t *UpdateTransaction) CommitFiles() error {
	if t == nil {
		return fmt.Errorf("model catalog transaction is nil")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return fmt.Errorf("model catalog transaction is already finished")
	}
	if t.filesCommitted {
		return nil
	}

	storeMu.Lock()
	defer storeMu.Unlock()
	if err := snapshotMatchesUnlocked(t.filePath, t.stableCurrent); err != nil {
		return fmt.Errorf("model catalog changed during transaction: %w", err)
	}
	if err := snapshotMatchesUnlocked(t.previousPath, t.previousSnapshot); err != nil {
		return fmt.Errorf("previous model catalog changed during transaction: %w", err)
	}
	if err := writeBytesAtomic(t.previousPath, t.stableCurrent.data); err != nil {
		return fmt.Errorf("save previous model catalog: %w", err)
	}
	published, err := publishTempFileUnlocked(t.candidatePath, t.filePath)
	if published {
		t.candidatePath = ""
	}
	if err != nil {
		rollbackCurrent := restoreSnapshotUnlocked(t.filePath, t.stableCurrent)
		rollbackPrevious := restoreSnapshotUnlocked(t.previousPath, t.previousSnapshot)
		return errors.Join(
			err,
			wrapRollbackError("restore current model catalog", rollbackCurrent),
			wrapRollbackError("restore previous model catalog", rollbackPrevious),
		)
	}
	t.filesCommitted = true
	return nil
}

func (t *UpdateTransaction) Finish() error {
	if t == nil {
		return fmt.Errorf("model catalog transaction is nil")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	if !t.filesCommitted {
		return fmt.Errorf("model catalog files are not committed")
	}
	t.finished = true
	t.releaseWriter()
	return nil
}

func (t *UpdateTransaction) Rollback() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}

	storeMu.Lock()
	var errs []error
	if t.filesCommitted {
		if err := restoreSnapshotUnlocked(t.filePath, t.rollbackCurrent); err != nil {
			errs = append(errs, fmt.Errorf("restore current model catalog: %w", err))
		}
		if err := restoreSnapshotUnlocked(t.previousPath, t.rollbackPrevious); err != nil {
			errs = append(errs, fmt.Errorf("restore previous model catalog: %w", err))
		}
	} else {
		if err := snapshotMatchesUnlocked(t.filePath, t.stableCurrent); err == nil {
			if err = restoreSnapshotUnlocked(t.filePath, t.rollbackCurrent); err != nil {
				errs = append(errs, fmt.Errorf("restore current model catalog: %w", err))
			}
		} else {
			errs = append(errs, fmt.Errorf("current model catalog changed during rollback: %w", err))
		}
		if err := snapshotMatchesUnlocked(t.previousPath, t.previousSnapshot); err == nil {
			if err = restoreSnapshotUnlocked(t.previousPath, t.rollbackPrevious); err != nil {
				errs = append(errs, fmt.Errorf("restore previous model catalog: %w", err))
			}
		} else {
			errs = append(errs, fmt.Errorf("previous model catalog changed during rollback: %w", err))
		}
	}
	if t.candidatePath != "" {
		if err := os.Remove(t.candidatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove candidate model catalog: %w", err))
		}
		t.candidatePath = ""
	}
	storeMu.Unlock()
	removeBackupUnlocked(t.backupPath)
	t.backupPath = ""
	t.finished = true
	t.releaseWriter()
	return errors.Join(errs...)
}

func wrapRollbackError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func ReplacePrices(configPath string, prices map[string]Price) (File, error) {
	doc, _, err := update(
		configPath,
		nil,
		UpdateOptions{},
		func(doc *File) error {
			SetPrices(doc, prices)
			return nil
		},
	)
	return doc, err
}

func SetPrices(doc *File, prices map[string]Price) {
	if doc == nil {
		return
	}
	doc.Prices = sanitizePrices(prices)
	doc.Sanitize()
}

func readCurrentDocumentUnlocked(filePath string) (File, bool, error) {
	current, err := readFileSnapshotUnlocked(filePath)
	if err != nil {
		return File{}, false, err
	}
	return decodeFileForLoad(current.data)
}

type stableLoad struct {
	doc             File
	current         fileSnapshot
	rollbackCurrent fileSnapshot
}

func loadStableUnlocked(filePath string, cfg *config.Config, initialize bool) (stableLoad, error) {
	current, err := readFileSnapshotUnlocked(filePath)
	if err == nil {
		doc, rewrite, errDecode := decodeFileForLoad(current.data)
		if errDecode == nil {
			doc.LoadMetadata = loadMetadata{Source: LoadSourceCurrent}
			canonicalData, errMarshal := marshalFile(doc)
			if errMarshal != nil {
				return stableLoad{}, errMarshal
			}
			if rewrite {
				backupPath, errBackup := writeBackupUnlocked(filePath, current.data)
				if errBackup != nil {
					return stableLoad{}, errBackup
				}
				previous, errPrevious := readFileSnapshotUnlocked(
					filepath.Join(
						filepath.Dir(filePath), PreviousFileName,
					),
				)
				if errPrevious != nil && !errors.Is(errPrevious, os.ErrNotExist) {
					removeBackupUnlocked(backupPath)
					return stableLoad{}, fmt.Errorf("read previous model catalog: %w", errPrevious)
				}
				if errWrite := commitReplacementUnlocked(filePath, canonicalData, current, previous); errWrite != nil {
					removeBackupUnlocked(backupPath)
					return stableLoad{}, errWrite
				}
				current = fileSnapshot{data: canonicalData, exists: true}
			} else if errPrevious := ensurePreviousValidatedUnlocked(filePath, current); errPrevious != nil {
				return stableLoad{}, errPrevious
			}
			return stableLoad{doc: doc, current: current, rollbackCurrent: current}, nil
		}
		return recoverFromPreviousUnlocked(filePath, fmt.Errorf("invalid current model catalog: %w", errDecode))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return stableLoad{}, fmt.Errorf("read current model catalog: %w", err)
	}

	missingCurrent := fmt.Errorf("current model catalog is missing: %w", os.ErrNotExist)
	loaded, errPrevious := recoverFromPreviousUnlocked(filePath, missingCurrent)
	if errPrevious == nil {
		return loaded, nil
	}
	previous, errSnapshot := readFileSnapshotUnlocked(filepath.Join(filepath.Dir(filePath), PreviousFileName))
	if errSnapshot == nil && previous.exists {
		return stableLoad{}, errPrevious
	}
	if errSnapshot != nil && !errors.Is(errSnapshot, os.ErrNotExist) {
		return stableLoad{}, errPrevious
	}
	if !initialize {
		return stableLoad{}, missingCurrent
	}

	doc := DefaultFile()
	if err = applyLegacyPrices(filepath.Join(filepath.Dir(filePath), "model-prices.json"), &doc); err != nil {
		return stableLoad{}, err
	}
	applyLegacyConfig(cfg, &doc)
	doc.LoadMetadata = loadMetadata{Source: loadSourceInitialized}
	data, err := marshalFile(doc)
	if err != nil {
		return stableLoad{}, err
	}
	if err = commitReplacementUnlocked(filePath, data, fileSnapshot{}, previous); err != nil {
		return stableLoad{}, err
	}
	return stableLoad{
		doc:             doc,
		current:         fileSnapshot{data: data, exists: true},
		rollbackCurrent: fileSnapshot{},
	}, nil
}

func recoverFromPreviousUnlocked(
	filePath string,
	currentErr error,
) (stableLoad, error) {
	previousPath := filepath.Join(filepath.Dir(filePath), PreviousFileName)
	previous, err := readFileSnapshotUnlocked(previousPath)
	if err != nil {
		return stableLoad{}, errors.Join(currentErr, fmt.Errorf("read previous model catalog: %w", err))
	}
	doc, _, err := decodeFileForLoad(previous.data)
	if err != nil {
		return stableLoad{}, errors.Join(currentErr, fmt.Errorf("invalid previous model catalog: %w", err))
	}
	data, err := marshalFile(doc)
	if err != nil {
		return stableLoad{}, errors.Join(currentErr, fmt.Errorf("encode previous model catalog: %w", err))
	}
	if err = writeBytesAtomic(filePath, data); err != nil {
		return stableLoad{}, errors.Join(currentErr, fmt.Errorf("restore current model catalog: %w", err))
	}
	doc.LoadMetadata = loadMetadata{
		Source:                LoadSourcePrevious,
		RecoveredFromPrevious: true,
		CurrentError:          publicLoadError(currentErr),
	}
	repaired := fileSnapshot{data: data, exists: true}
	return stableLoad{doc: doc, current: repaired, rollbackCurrent: repaired}, nil
}

func ensurePreviousValidatedUnlocked(filePath string, current fileSnapshot) error {
	previousPath := filepath.Join(filepath.Dir(filePath), PreviousFileName)
	previous, err := readFileSnapshotUnlocked(previousPath)
	if errors.Is(err, os.ErrNotExist) {
		if err = writeBytesAtomic(previousPath, current.data); err != nil {
			return fmt.Errorf("initialize previous model catalog: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read previous model catalog: %w", err)
	}
	if _, _, err = decodeFileForLoad(previous.data); err == nil {
		return nil
	}
	if err = writeBytesAtomic(previousPath, current.data); err != nil {
		return fmt.Errorf("repair previous model catalog: %w", err)
	}
	return nil
}

func validatedSnapshotUnlocked(filePath string) (fileSnapshot, error) {
	snapshot, err := readFileSnapshotUnlocked(filePath)
	if err != nil {
		return fileSnapshot{}, err
	}
	if _, _, err = decodeFileForLoad(snapshot.data); err != nil {
		return fileSnapshot{}, err
	}
	return snapshot, nil
}

func readFileSnapshotUnlocked(filePath string) (fileSnapshot, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return fileSnapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fileSnapshot{}, fmt.Errorf("symbolic link is not allowed")
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("path is not a regular file")
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{data: data, exists: true}, nil
}

func snapshotMatchesUnlocked(filePath string, expected fileSnapshot) error {
	current, err := readFileSnapshotUnlocked(filePath)
	if errors.Is(err, os.ErrNotExist) && !expected.exists {
		return nil
	}
	if err != nil {
		return err
	}
	if !expected.exists || !bytes.Equal(current.data, expected.data) {
		return fmt.Errorf("file contents differ")
	}
	return nil
}

func publicLoadError(err error) string {
	if pathError, ok := errors.AsType[*os.PathError](err); ok {
		return fmt.Sprintf("%s: %v", pathError.Op, pathError.Err)
	}
	return err.Error()
}

func cloneFile(doc File) (File, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return File{}, err
	}
	var cloned File
	if err = json.Unmarshal(data, &cloned); err != nil {
		return File{}, err
	}
	cloned.LoadMetadata = doc.LoadMetadata
	return cloned, nil
}

func decodeFile(data []byte) (File, error) {
	doc, _, err := decodeFileForLoad(data)
	return doc, err
}

func decodeFileForLoad(data []byte) (File, bool, error) {
	var doc File
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return File{}, false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return File{}, false, err
	}
	if err := validateFileBeforeSanitize(doc); err != nil {
		return File{}, false, err
	}
	rewrite := doc.SchemaVersion < SchemaVersion || strings.TrimSpace(doc.DefaultsVersion) == ""
	if !rewrite {
		for i := range doc.Models {
			if len(doc.Models[i].Aliases) > 0 || len(doc.Models[i].ForceMappedAliases) > 0 || doc.Models[i].Price != nil {
				rewrite = true
				break
			}
		}
	}
	doc.Sanitize()
	if err := doc.Validate(); err != nil {
		return File{}, false, err
	}
	return doc, rewrite, nil
}

func marshalFile(doc File) ([]byte, error) {
	doc.Sanitize()
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeBytesAtomic(filePath string, data []byte) error {
	tempPath, err := writeTempFileUnlocked(filePath, data)
	if err != nil {
		return err
	}
	published, err := publishTempFileUnlocked(tempPath, filePath)
	if !published {
		_ = os.Remove(tempPath)
	}
	return err
}

func writeTempFileUnlocked(filePath string, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(filePath), "."+filepath.Base(filePath)+".tmp-*")
	if err != nil {
		return "", err
	}
	tempPath := file.Name()
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return "", err
	}
	written, err := file.Write(data)
	if err != nil {
		return "", err
	}
	if written != len(data) {
		return "", io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	removeTemp = false
	return tempPath, nil
}

func publishTempFileUnlocked(tempPath, filePath string) (bool, error) {
	if err := rejectSymbolicLinkTarget(filePath); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, filePath); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return false, fmt.Errorf(
				"replace model catalog: %w; mount the config directory instead of a single file",
				err,
			)
		}
		return false, err
	}
	if err := syncDirectory(filepath.Dir(filePath)); err != nil {
		return true, err
	}
	return true, nil
}

func rejectSymbolicLinkTarget(filePath string) error {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symbolic link is not allowed")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file")
	}
	return nil
}

func commitReplacementUnlocked(
	filePath string,
	candidateData []byte,
	current fileSnapshot,
	previous fileSnapshot,
) error {
	previousPath := filepath.Join(filepath.Dir(filePath), PreviousFileName)
	if err := snapshotMatchesUnlocked(filePath, current); err != nil {
		return fmt.Errorf("current model catalog changed before write: %w", err)
	}
	if err := snapshotMatchesUnlocked(previousPath, previous); err != nil {
		return fmt.Errorf("previous model catalog changed before write: %w", err)
	}
	previousData := candidateData
	if current.exists {
		previousData = current.data
	}
	if err := writeBytesAtomic(previousPath, previousData); err != nil {
		return fmt.Errorf("save previous model catalog: %w", err)
	}
	if err := writeBytesAtomic(filePath, candidateData); err != nil {
		rollbackCurrent := restoreSnapshotUnlocked(filePath, current)
		rollbackPrevious := restoreSnapshotUnlocked(previousPath, previous)
		return errors.Join(
			err,
			wrapRollbackError("restore current model catalog", rollbackCurrent),
			wrapRollbackError("restore previous model catalog", rollbackPrevious),
		)
	}
	return nil
}

func restoreSnapshotUnlocked(filePath string, snapshot fileSnapshot) error {
	if snapshot.exists {
		return writeBytesAtomic(filePath, snapshot.data)
	}
	if err := rejectSymbolicLinkTarget(filePath); err != nil {
		return err
	}
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(filePath))
}

func writeBackupUnlocked(filePath string, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return "", err
	}
	pattern := fmt.Sprintf(
		"%s.%s-*.bak",
		filepath.Base(filePath),
		time.Now().Format("20060102-150405"),
	)
	file, err := os.CreateTemp(filepath.Dir(filePath), pattern)
	if err != nil {
		return "", err
	}
	backupPath := file.Name()
	removeBackup := true
	defer func() {
		_ = file.Close()
		if removeBackup {
			_ = os.Remove(backupPath)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return "", err
	}
	written, err := file.Write(data)
	if err != nil {
		return "", err
	}
	if written != len(data) {
		return "", io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = syncDirectory(filepath.Dir(filePath)); err != nil {
		return "", err
	}
	removeBackup = false
	return backupPath, nil
}

func removeBackupUnlocked(backupPath string) {
	if backupPath == "" {
		return
	}
	_ = os.Remove(backupPath)
	_ = syncDirectory(filepath.Dir(backupPath))
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func applyLegacyPrices(filePath string, doc *File) error {
	if doc == nil || strings.TrimSpace(filePath) == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy model-prices.json: %w", err)
	}
	var prices map[string]Price
	if err := json.Unmarshal(data, &prices); err != nil {
		return fmt.Errorf("parse legacy model-prices.json: %w", err)
	}
	for name, price := range prices {
		doc.SetPrice(name, price)
	}
	return nil
}

func ReplaceOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	doc.OAuthModelAlias = nil
	for i := range doc.Models {
		doc.Models[i].Aliases = nil
		doc.Models[i].ForceMappedAliases = nil
	}
	ApplyOAuthModelAlias(doc, aliases)
	doc.Sanitize()
}

func ReplaceModelAliases(doc *File, name string, aliases []string) {
	if doc == nil {
		return
	}
	model, ok := doc.Find(name)
	if !ok {
		return
	}
	key := modelKey(model.Name)
	for channel, entries := range doc.OAuthModelAlias {
		kept := entries[:0]
		for _, entry := range entries {
			if modelKey(entry.Name) != key {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(doc.OAuthModelAlias, channel)
			continue
		}
		doc.OAuthModelAlias[channel] = kept
	}
	model.Aliases = append([]string(nil), aliases...)
	model.ForceMappedAliases = nil
	doc.Sanitize()
}

func ReplaceOAuthModelAliasChannel(doc *File, channel string, aliases []config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	deleteOAuthModelAliasChannel(doc, channel)
	ApplyOAuthModelAlias(doc, map[string][]config.OAuthModelAlias{channel: aliases})
	doc.Sanitize()
}

func DeleteOAuthModelAliasChannel(doc *File, channel string) bool {
	if doc == nil {
		return false
	}
	changed := deleteOAuthModelAliasChannel(doc, channel)
	if changed {
		doc.Sanitize()
	}
	return changed
}

func deleteOAuthModelAliasChannel(doc *File, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if doc == nil || channel == "" {
		return false
	}
	doc.materializeModelAliases()
	changed := false
	if _, ok := doc.OAuthModelAlias[channel]; ok {
		delete(doc.OAuthModelAlias, channel)
		if len(doc.OAuthModelAlias) == 0 {
			doc.OAuthModelAlias = nil
		}
		changed = true
	}
	for i := range doc.Models {
		if !modelMatchesOAuthAliasChannel(doc.Models[i], channel) || len(doc.Models[i].Aliases) == 0 {
			continue
		}
		doc.Models[i].Aliases = nil
		doc.Models[i].ForceMappedAliases = nil
		changed = true
	}
	return changed
}

func modelMatchesOAuthAliasChannel(model Model, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return false
	}
	modelChannel := strings.ToLower(strings.TrimSpace(model.Channel))
	if modelChannel == channel {
		return true
	}
	return slices.Contains(oauthAliasChannelsForModel(model.Provider, modelChannel), channel)
}

func ApplyOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) bool {
	if doc == nil {
		return false
	}
	normalized := config.SanitizeOAuthModelAliasMap(aliases)
	if len(normalized) == 0 {
		return false
	}
	if doc.OAuthModelAlias == nil {
		doc.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	changed := false
	channels := make([]string, 0, len(normalized))
	for channel := range normalized {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	for _, channel := range channels {
		existingAliases := doc.OAuthModelAlias[channel]
		existingByAlias := make(map[string]struct{}, len(existingAliases))
		for _, existing := range existingAliases {
			existingByAlias[strings.ToLower(existing.Alias)] = struct{}{}
		}
		for _, alias := range normalized[channel] {
			if ensureOAuthAliasModel(doc, channel, alias.Name) {
				changed = true
			}
			aliasKey := strings.ToLower(alias.Alias)
			if _, exists := existingByAlias[aliasKey]; exists {
				continue
			}
			doc.OAuthModelAlias[channel] = append(doc.OAuthModelAlias[channel], alias)
			existingByAlias[aliasKey] = struct{}{}
			changed = true
		}
	}
	if changed {
		doc.Sanitize()
	}
	return changed
}

func ensureOAuthAliasModel(doc *File, channel string, name string) bool {
	name = normalizeModelName(name)
	if doc == nil || name == "" {
		return false
	}
	_, existed := doc.Find(name)
	model := doc.EnsureModel(name, true)
	if model == nil {
		return false
	}
	changed := !existed
	if provider := inferProvider(channel, name); model.Provider == "" || model.Provider == "Other" {
		if model.Provider != provider {
			model.Provider = provider
			changed = true
		}
	}
	if inferredChannel := inferChannel(channel, name); model.Channel == "" || model.Channel == "other" {
		if model.Channel != inferredChannel {
			model.Channel = inferredChannel
			changed = true
		}
	}
	return changed
}

func applyLegacyConfig(cfg *config.Config, doc *File) {
	if cfg == nil || doc == nil {
		return
	}
	ApplyOAuthModelAlias(doc, cfg.OAuthModelAlias)
	for _, entry := range cfg.ModelCatalog.Models {
		name := normalizeModelName(entry.Name)
		if name == "" {
			continue
		}
		model := doc.EnsureModel(name, true)
		if model == nil {
			continue
		}
		if provider := strings.TrimSpace(entry.Provider); provider != "" {
			model.Provider = provider
		}
		if channel := strings.TrimSpace(entry.Channel); channel != "" {
			model.Channel = strings.ToLower(channel)
		}
		if group := strings.TrimSpace(entry.Group); group != "" {
			model.Group = group
		}
		if displayName := strings.TrimSpace(entry.DisplayName); displayName != "" {
			model.DisplayName = displayName
		}
		if entry.Enabled != nil {
			model.Enabled = *entry.Enabled
		}
	}
}
