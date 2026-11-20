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
	"reflect"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	// Direct callers own Storage; Manager persistence requires an independent copy.
	// Storage implementations must not mutate shared nested values in place.
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

// PrepareAuthForPersistence 准备独占保存对象，支持复制时使用独立 Storage 副本
// Manager 在提交共享 Storage 前要求 PersistenceTokenStorage，直接保存由调用方持有对象所有权
func PrepareAuthForPersistence(auth *Auth) error {
	if auth == nil {
		return nil
	}
	if storage, ok := auth.Storage.(PersistenceTokenStorage); ok {
		copyStorage := storage.CloneForPersistence()
		if copyStorage == nil {
			return fmt.Errorf("token storage %T returned a nil persistence copy", auth.Storage)
		}
		value := reflect.ValueOf(copyStorage)
		// Value-kind copies are valid; these reference kinds must also reject typed nils.
		//goland:noinspection GoSwitchMissingCasesForIotaConsts
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return fmt.Errorf("token storage %T returned a nil persistence copy", auth.Storage)
			}
		}
		if original := reflect.ValueOf(auth.Storage); value.Kind() == reflect.Pointer &&
			value.Type().
				Elem().
				Size() !=
				0 && original.Kind() == reflect.Pointer && value.Pointer() == original.Pointer() {
			return fmt.Errorf("token storage %T returned its shared instance as a persistence copy", auth.Storage)
		}
		auth.Storage = copyStorage
	}
	auth.Metadata = clonePersistenceMetadata(auth.Metadata)
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

// clonePersistenceMetadata 复制 JSON 容器并保留别名和循环，扩展对象仍由其所有者管理
func clonePersistenceMetadata(metadata map[string]any) map[string]any {
	type containerKey struct {
		typ     reflect.Type
		pointer uintptr
		length  int
	}
	copies := make(map[containerKey]any)
	var clone func(any) any
	clone = func(value any) any {
		switch typed := value.(type) {
		case map[string]any:
			if typed == nil {
				return typed
			}
			key := containerKey{typ: reflect.TypeOf(typed), pointer: reflect.ValueOf(typed).Pointer()}
			if copied, exists := copies[key]; exists {
				return copied
			}
			copied := make(map[string]any, len(typed))
			copies[key] = copied
			for name, item := range typed {
				copied[name] = clone(item)
			}
			return copied
		case []any:
			if typed == nil {
				return typed
			}
			key := containerKey{
				typ: reflect.TypeOf(typed), pointer: reflect.ValueOf(typed).Pointer(), length: len(typed),
			}
			if copied, exists := copies[key]; exists {
				return copied
			}
			copied := make([]any, len(typed))
			copies[key] = copied
			for index, item := range typed {
				copied[index] = clone(item)
			}
			return copied
		case map[string]string:
			if typed == nil {
				return typed
			}
			key := containerKey{typ: reflect.TypeOf(typed), pointer: reflect.ValueOf(typed).Pointer()}
			if copied, exists := copies[key]; exists {
				return copied
			}
			copied := maps.Clone(typed)
			copies[key] = copied
			return copied
		case []string:
			if typed == nil {
				return typed
			}
			key := containerKey{
				typ: reflect.TypeOf(typed), pointer: reflect.ValueOf(typed).Pointer(), length: len(typed),
			}
			if copied, exists := copies[key]; exists {
				return copied
			}
			copied := append(typed[:0:0], typed...)
			copies[key] = copied
			return copied
		case []byte:
			if typed == nil {
				return typed
			}
			key := containerKey{
				typ: reflect.TypeOf(typed), pointer: reflect.ValueOf(typed).Pointer(), length: len(typed),
			}
			if copied, exists := copies[key]; exists {
				return copied
			}
			copied := append(typed[:0:0], typed...)
			copies[key] = copied
			return copied
		default:
			return value
		}
	}
	return clone(metadata).(map[string]any)
}

type persistedVersionCaptureContextKey struct{}

type persistedVersionCapture struct {
	version PersistedVersion
	err     error
}

// RecordPersistedAuthVersion 在 Store 的保存锁内记录本次实际文件内容，供 Manager 校验后续更新
// Store 应在成功提交后、Save 返回前调用，独立保存不会创建额外的版本读取
func RecordPersistedAuthVersion(ctx context.Context, path string) {
	if ctx == nil {
		return
	}
	capture, _ := ctx.Value(persistedVersionCaptureContextKey{}).(*persistedVersionCapture)
	if capture == nil {
		return
	}
	capture.version, capture.err = ReadPersistedVersion(path)
	capture.version.Path = path
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
