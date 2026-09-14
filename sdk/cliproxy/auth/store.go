package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
)

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

const (
	// PersistedSHA256Attribute carries the source file digest through watcher synthesis.
	PersistedSHA256Attribute = "persisted_sha256"
	// PersistedReadAtAttribute carries the source file read time through watcher dispatch.
	PersistedReadAtAttribute = "persisted_read_at"
)

// PersistedVersion identifies the exact file content produced by Store.Save.
type PersistedVersion struct {
	Path   string
	SHA256 [sha256.Size]byte
}

// VersionedStore atomically removes a credential only while its persisted version still matches.
type VersionedStore interface {
	DeleteIfVersion(ctx context.Context, id string, version PersistedVersion) (bool, error)
}

// DigestPersistedJSON returns a whitespace-independent digest of persisted JSON.
func DigestPersistedJSON(raw []byte) ([sha256.Size]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return [sha256.Size]byte{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return [sha256.Size]byte{}, fmt.Errorf("persisted JSON has multiple values")
		}
		return [sha256.Size]byte{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// PrepareAuthForPersistence applies manager-owned metadata to the storage payload.
func PrepareAuthForPersistence(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = auth.Disabled
	if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(auth.Metadata)
	}
}

// PersistedVersionForAuth identifies the content Store.Save was asked to persist.
func PersistedVersionForAuth(path string, auth *Auth) (PersistedVersion, error) {
	if auth == nil {
		return PersistedVersion{}, fmt.Errorf("auth is nil")
	}
	var (
		raw []byte
		err error
	)
	if storage, ok := auth.Storage.(interface{ RawJSON() []byte }); ok {
		raw = storage.RawJSON()
	}
	if len(raw) == 0 && auth.Storage != nil {
		storageRaw, errMarshal := json.Marshal(auth.Storage)
		if errMarshal != nil {
			err = errMarshal
		} else {
			merged := make(map[string]any)
			if errUnmarshal := json.Unmarshal(storageRaw, &merged); errUnmarshal != nil {
				err = errUnmarshal
			} else {
				maps.Copy(merged, auth.Metadata)
				raw, err = json.Marshal(merged)
			}
		}
	}
	if len(raw) == 0 && err == nil {
		raw, err = json.Marshal(auth.Metadata)
	}
	if err != nil {
		return PersistedVersion{}, err
	}
	digest, err := DigestPersistedJSON(raw)
	if err != nil {
		return PersistedVersion{}, err
	}
	return PersistedVersion{Path: path, SHA256: digest}, nil
}

// ReadPersistedVersion reads the exact file content currently stored at path.
func ReadPersistedVersion(path string) (PersistedVersion, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PersistedVersion{}, err
	}
	digest, err := DigestPersistedJSON(raw)
	if err != nil {
		return PersistedVersion{}, err
	}
	return PersistedVersion{Path: path, SHA256: digest}, nil
}

// CapturedPersistedFile holds an expected credential version outside its live path
// until the caller either commits the deletion or restores it without replacing a
// concurrently created newer file.
type CapturedPersistedFile struct {
	path     string
	tempPath string
	active   bool
}

// CaptureFileIfVersion atomically moves the current path aside and verifies its
// digest before any remote deletion starts. A missing path is already deleted and
// therefore matches the requested removal.
func CaptureFileIfVersion(
	path string,
	version PersistedVersion,
) (*CapturedPersistedFile, bool, error) {
	if filepath.Clean(path) != filepath.Clean(version.Path) {
		return nil, false, nil
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".auth-delete-*")
	if err != nil {
		return nil, false, fmt.Errorf("create versioned delete placeholder: %w", err)
	}
	tempPath := temp.Name()
	if errClose := temp.Close(); errClose != nil {
		_ = os.Remove(tempPath)
		return nil, false, fmt.Errorf("close versioned delete placeholder: %w", errClose)
	}
	if errRemove := os.Remove(tempPath); errRemove != nil {
		return nil, false, fmt.Errorf("remove versioned delete placeholder: %w", errRemove)
	}
	if errRename := os.Rename(path, tempPath); errRename != nil {
		if errors.Is(errRename, os.ErrNotExist) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("capture persisted credential version: %w", errRename)
	}
	captured := &CapturedPersistedFile{path: path, tempPath: tempPath, active: true}
	persisted, errRead := ReadPersistedVersion(tempPath)
	if errRead != nil {
		return nil, false, errors.Join(errRead, captured.Rollback())
	}
	if persisted.SHA256 != version.SHA256 {
		return nil, false, captured.Rollback()
	}
	return captured, true, nil
}

// Commit permanently removes the captured credential version.
func (c *CapturedPersistedFile) Commit() error {
	if c == nil || !c.active {
		return nil
	}
	if errRemove := os.Remove(c.tempPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return fmt.Errorf("remove matched credential version: %w", errRemove)
	}
	c.active = false
	return nil
}

// Rollback restores the captured credential only when its live path is still
// absent. A concurrently created replacement is preserved.
func (c *CapturedPersistedFile) Rollback() error {
	if c == nil || !c.active {
		return nil
	}
	if errRestore := restoreCapturedFile(c.path, c.tempPath); errRestore != nil {
		return errRestore
	}
	c.active = false
	return nil
}

// RemoveFileIfVersion removes only the captured matching version. A concurrently
// created newer path is never removed or overwritten.
func RemoveFileIfVersion(path string, version PersistedVersion) (bool, error) {
	captured, matched, errCapture := CaptureFileIfVersion(path, version)
	if errCapture != nil || !matched {
		return false, errCapture
	}
	if errCommit := captured.Commit(); errCommit != nil {
		return false, errCommit
	}
	return true, nil
}

func restoreCapturedFile(path, tempPath string) error {
	if errLink := os.Link(tempPath, path); errLink == nil {
		return os.Remove(tempPath)
	} else if errors.Is(errLink, os.ErrExist) {
		return os.Remove(tempPath)
	} else if _, errStat := os.Lstat(path); errStat == nil {
		return os.Remove(tempPath)
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return errors.Join(errLink, errStat)
	} else {
		return fmt.Errorf("restore captured credential without replacement: %w", errLink)
	}
}
