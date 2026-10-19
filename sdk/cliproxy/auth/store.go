package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	// Storage implementations must not mutate shared nested values in place.
	// Storage that sets metadata must provide an independent CloneForPersistence.
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
	value, err := util.DecodeJSON(raw)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical, err := json.Marshal(normalizeCredentialNumbers(value))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// DecodeCredentialMetadata 无损解析凭证对象，保留大整数及高精度小数
func DecodeCredentialMetadata(raw []byte) (map[string]any, error) {
	value, err := util.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	metadata, ok := value.(map[string]any)
	if !ok || metadata == nil {
		return nil, fmt.Errorf("credential JSON is not an object")
	}
	for key, item := range metadata {
		metadata[key] = normalizeCredentialNumbers(item)
	}
	return metadata, nil
}

func normalizeCredentialNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeCredentialNumbers(item)
		}
	case []any:
		for index, item := range typed {
			typed[index] = normalizeCredentialNumbers(item)
		}
	case json.Number:
		number, err := typed.Float64()
		if err != nil || number >= 1<<53 || number <= -(1<<53) {
			return typed
		}
		encoded, err := json.Marshal(number)
		if err == nil && util.JSONEqual([]byte(typed), encoded) {
			return number
		}
	}
	return value
}

// BindPersistedAuthDigest 保存完整文件摘要，并记录可证明的配额材料对应
// 材料无效时清除旧证明并返回错误，保留文件版本用于 CAS
func BindPersistedAuthDigest(auth *Auth, digest [sha256.Size]byte) error {
	if auth == nil {
		return fmt.Errorf("auth is nil")
	}
	attributes := maps.Clone(auth.Attributes)
	if attributes == nil {
		attributes = make(map[string]string)
	}
	value := hex.EncodeToString(digest[:])
	attributes[PersistedSHA256Attribute] = value
	auth.Attributes = attributes
	auth.quotaPersistedRevision = ""
	auth.quotaPersistedSHA256 = ""
	revision, _, err := QuotaCredentialRevisions(auth)
	if err != nil {
		return err
	}
	auth.quotaPersistedRevision = revision
	auth.quotaPersistedSHA256 = value
	return nil
}

// PrepareAuthForPersistence applies manager-owned metadata to an independent storage snapshot.
func PrepareAuthForPersistence(auth *Auth) error {
	if auth == nil {
		return nil
	}
	if storage, ok := auth.Storage.(PersistenceTokenStorage); ok {
		copyStorage := storage.CloneForPersistence()
		if copyStorage == nil {
			return fmt.Errorf("token storage %T returned a nil persistence copy", auth.Storage)
		}
		auth.Storage = copyStorage
	} else if _, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		return fmt.Errorf("token storage %T must implement CloneForPersistence", auth.Storage)
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	NormalizeCredentialMetadata(auth.Metadata)
	auth.Metadata["disabled"] = auth.Disabled
	if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(auth.Metadata)
	}
	return nil
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
			merged, errUnmarshal := DecodeCredentialMetadata(storageRaw)
			if errUnmarshal != nil {
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
