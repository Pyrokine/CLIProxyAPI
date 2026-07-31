package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	log "github.com/sirupsen/logrus"
)

const (
	objectStoreConfigKey  = "config/config.yaml"
	objectStoreAuthPrefix = "auths"
)

type objectStoreAuthTombstone struct {
	DeletedVersionID string `json:"deleted_version_id"`
	DeletedSHA256    string `json:"deleted_sha256"`
	Tombstone        bool   `json:"_cliproxy_auth_deleted"`
}

// ObjectStoreConfig captures configuration for the object storage-backed token store.
type ObjectStoreConfig struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	Prefix    string
	LocalRoot string
	UseSSL    bool
	PathStyle bool
}

// ObjectTokenStore persists configuration and authentication metadata using an S3-compatible object storage backend.
// Files are mirrored to a local workspace so existing file-based flows continue to operate.
type ObjectTokenStore struct {
	client     *minio.Client
	cfg        ObjectStoreConfig
	spoolRoot  string
	configPath string
	authDir    string
	mu         sync.Mutex
}

// NewObjectTokenStore initializes an object storage backed token store.
func NewObjectTokenStore(cfg ObjectStoreConfig) (*ObjectTokenStore, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("object store: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("object store: bucket is required")
	}
	if cfg.AccessKey == "" {
		return nil, fmt.Errorf("object store: access key is required")
	}
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("object store: secret key is required")
	}

	root := strings.TrimSpace(cfg.LocalRoot)
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = filepath.Join(cwd, "objectstore")
		} else {
			root = filepath.Join(os.TempDir(), "objectstore")
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("object store: resolve spool directory: %w", err)
	}

	configDir := filepath.Join(absRoot, "config")
	authDir := filepath.Join(absRoot, "auths")

	if err = os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("object store: create config directory: %w", err)
	}
	if err = secureAuthDirectory(authDir); err != nil {
		return nil, fmt.Errorf("object store: secure auth directory: %w", err)
	}

	options := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		options.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(cfg.Endpoint, options)
	if err != nil {
		return nil, fmt.Errorf("object store: create client: %w", err)
	}

	return &ObjectTokenStore{
		client:     client,
		cfg:        cfg,
		spoolRoot:  absRoot,
		configPath: filepath.Join(configDir, "config.yaml"),
		authDir:    authDir,
	}, nil
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the object store controls its own workspace.
func (s *ObjectTokenStore) SetBaseDir(_ string) {}

// ConfigPath returns the managed configuration file path inside the spool directory.
func (s *ObjectTokenStore) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

// AuthDir returns the local directory containing mirrored auth files.
func (s *ObjectTokenStore) AuthDir() string {
	if s == nil {
		return ""
	}
	return s.authDir
}

// Bootstrap ensures the target bucket exists and synchronizes data from the object storage backend.
func (s *ObjectTokenStore) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if s == nil {
		return fmt.Errorf("object store: not initialized")
	}
	if err := s.ensureBucket(ctx); err != nil {
		return err
	}
	if err := s.syncConfigFromBucket(ctx, exampleConfigPath); err != nil {
		return err
	}
	if err := s.recoverAuthDeletions(ctx); err != nil {
		return err
	}
	if err := s.syncAuthFromBucket(ctx); err != nil {
		return err
	}
	return nil
}

func (s *ObjectTokenStore) recoverAuthDeletions(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deletions, errRecover := cliproxyauth.RecoverDurablePersistedFileDeletions(s.authDir)
	if errRecover != nil {
		return errRecover
	}
	for _, deletion := range deletions {
		deleted, errDelete := s.deleteAuthObjectIfVersion(ctx, deletion.Path(), deletion.Version())
		if errDelete != nil {
			return fmt.Errorf("object store: resume auth deletion: %w", errDelete)
		}
		if !deleted {
			log.Warnf("object store: interrupted auth deletion preserved a newer credential at %s", deletion.Path())
		}
		if errCommit := deletion.Commit(); errCommit != nil {
			return errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errCommit)
		}
	}
	return nil
}

// Save persists authentication metadata to disk and uploads it to the object storage backend.
func (s *ObjectTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (savedPath string, err error) {
	if auth == nil {
		return "", fmt.Errorf("object store: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("object store: missing file path attribute for %s", auth.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = secureAuthDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("object store: secure auth directory: %w", err)
	}
	fileSnapshot, errSnapshot := captureFileMutationSnapshot(path)
	if errSnapshot != nil {
		return "", errSnapshot
	}
	fileMutated := false
	remoteAttempted := false
	mutationComplete := false
	defer func() {
		if mutationComplete {
			return
		}
		if fileMutated {
			err = errors.Join(err, restoreFileMutationSnapshot(path, fileSnapshot))
		}
		if remoteAttempted || err != nil && fileMutated {
			err = errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, err)
		}
		if err != nil {
			savedPath = ""
		}
	}()

	cliproxyauth.PrepareAuthForPersistence(auth)
	switch {
	case auth.Storage != nil:
		fileMutated = true
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
		if err = secureAuthFilePath(path); err != nil {
			return "", fmt.Errorf("object store: secure stored token: %w", err)
		}
	case auth.Metadata != nil:
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("object store: marshal metadata: %w", errMarshal)
		}
		existing, errRead := os.ReadFile(path)
		if errRead == nil && util.JSONEqual(existing, raw) {
			if err = secureAuthFilePath(path); err != nil {
				return "", fmt.Errorf("object store: secure existing metadata: %w", err)
			}
			break
		}
		if errRead != nil && !errors.Is(errRead, fs.ErrNotExist) {
			return "", fmt.Errorf("object store: read existing metadata: %w", errRead)
		}
		fileMutated = true
		if errWrite := writeAuthFile(path, raw); errWrite != nil {
			return "", fmt.Errorf("object store: write auth file: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("object store: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	remoteAttempted = true
	if err = s.uploadAuth(ctx, path); err != nil {
		return "", err
	}
	mutationComplete = true
	return path, nil
}

// List enumerates auth JSON files from the mirrored workspace.
func (s *ObjectTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := strings.TrimSpace(s.AuthDir())
	if dir == "" {
		return nil, fmt.Errorf("object store: auth directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0, 32)
	err := filepath.WalkDir(
		dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
				return nil
			}
			auth, err := s.readAuthFile(ctx, path, dir)
			if err != nil {
				log.WithError(err).Warnf("object store: skip auth %s", path)
				return nil
			}
			if auth != nil {
				entries = append(entries, auth)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("object store: walk auth directory: %w", err)
	}
	return entries, nil
}

// DeleteIfVersion replaces the expected current object with a conditional tombstone,
// then removes the hidden credential version without exposing older versions.
func (s *ObjectTokenStore) DeleteIfVersion(
	ctx context.Context,
	id string,
	version cliproxyauth.PersistedVersion,
) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("object store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deletion, matched, errPrepare := cliproxyauth.PrepareDurablePersistedFileDeletion(path, version)
	if errPrepare != nil || !matched {
		return false, errPrepare
	}
	deleted, errDelete := s.deleteAuthObjectIfVersion(ctx, path, version)
	if errDelete != nil {
		return false, errors.Join(errDelete, deletion.Rollback())
	}
	if !deleted {
		return false, deletion.Rollback()
	}
	if errCommit := deletion.Commit(); errCommit != nil {
		return false, errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errCommit)
	}
	return true, nil
}

func (s *ObjectTokenStore) deleteAuthObjectIfVersion(
	ctx context.Context,
	path string,
	version cliproxyauth.PersistedVersion,
) (bool, error) {
	versioning, errVersioning := s.client.GetBucketVersioning(ctx, s.cfg.Bucket)
	if errVersioning != nil {
		return false, fmt.Errorf("object store: read bucket versioning: %w", errVersioning)
	}
	if !versioning.Enabled() {
		return false, errors.Join(
			cliproxyauth.ErrAuthDurableStateUncertain,
			fmt.Errorf("object store: enabled bucket versioning is required for conditional auth deletion"),
		)
	}
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return false, fmt.Errorf("object store: resolve auth relative path: %w", err)
	}
	fullKey := s.prefixedKey(objectStoreAuthPrefix + "/" + filepath.ToSlash(rel))
	info, errStat := s.client.StatObject(ctx, s.cfg.Bucket, fullKey, minio.StatObjectOptions{})
	if errStat != nil {
		if isObjectNotFound(errStat) {
			return true, nil
		}
		return false, fmt.Errorf("object store: stat versioned auth %s: %w", fullKey, errStat)
	}
	versionID := strings.TrimSpace(info.VersionID)
	if versionID == "" || strings.EqualFold(versionID, "null") || strings.TrimSpace(info.ETag) == "" {
		return false, errors.Join(
			cliproxyauth.ErrAuthDurableStateUncertain,
			fmt.Errorf("object store: current auth object lacks a conditional version identity"),
		)
	}
	object, errGet := s.client.GetObject(
		ctx,
		s.cfg.Bucket,
		fullKey,
		minio.GetObjectOptions{VersionID: versionID},
	)
	if errGet != nil {
		return false, fmt.Errorf("object store: open versioned auth %s: %w", fullKey, errGet)
	}
	raw, errRead := io.ReadAll(io.LimitReader(object, 50<<20))
	_ = object.Close()
	if errRead != nil {
		return false, fmt.Errorf("object store: read versioned auth %s: %w", fullKey, errRead)
	}
	if tombstone, ok := parseObjectStoreAuthTombstone(raw); ok {
		if !strings.EqualFold(tombstone.DeletedSHA256, fmt.Sprintf("%x", version.SHA256)) {
			return false, nil
		}
		if errRemove := s.removeObjectVersion(ctx, fullKey, tombstone.DeletedVersionID); errRemove != nil {
			return false, errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errRemove)
		}
		return true, nil
	}
	digest, errDigest := cliproxyauth.DigestPersistedJSON(raw)
	if errDigest != nil {
		return false, fmt.Errorf("object store: digest versioned auth %s: %w", fullKey, errDigest)
	}
	if digest != version.SHA256 {
		return false, nil
	}
	tombstoneRaw, errTombstone := json.Marshal(objectStoreAuthTombstone{
		DeletedVersionID: versionID,
		DeletedSHA256:    fmt.Sprintf("%x", version.SHA256),
		Tombstone:        true,
	})
	if errTombstone != nil {
		return false, fmt.Errorf("object store: encode auth tombstone: %w", errTombstone)
	}
	putOptions := minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true}
	putOptions.SetMatchETag(info.ETag)
	if _, errPut := s.client.PutObject(
		ctx,
		s.cfg.Bucket,
		fullKey,
		bytes.NewReader(tombstoneRaw),
		int64(len(tombstoneRaw)),
		putOptions,
	); errPut != nil {
		if isObjectPreconditionFailed(errPut) {
			return false, nil
		}
		return false, errors.Join(
			cliproxyauth.ErrAuthDurableStateUncertain,
			fmt.Errorf("object store: publish conditional auth tombstone %s: %w", fullKey, errPut),
		)
	}
	if errRemove := s.removeObjectVersion(ctx, fullKey, versionID); errRemove != nil {
		return false, errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errRemove)
	}
	return true, nil
}

// Delete removes an auth file locally and remotely.
func (s *ObjectTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("object store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fileSnapshot, errSnapshot := captureFileMutationSnapshot(path)
	if errSnapshot != nil {
		return errSnapshot
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("object store: delete auth file: %w", err)
	}
	if err = s.deleteAuthObject(ctx, path); err != nil {
		errRestore := restoreFileMutationSnapshot(path, fileSnapshot)
		return errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, err, errRestore)
	}
	return nil
}

// PersistAuthFiles uploads the provided auth files to the object storage backend.
func (s *ObjectTokenStore) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		abs := trimmed
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(s.authDir, trimmed)
		}
		if err := s.uploadAuth(ctx, abs); err != nil {
			return err
		}
	}
	return nil
}

// PersistConfig uploads the local configuration file to the object storage backend.
func (s *ObjectTokenStore) PersistConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteObject(ctx, objectStoreConfigKey)
		}
		return fmt.Errorf("object store: read config file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteObject(ctx, objectStoreConfigKey)
	}
	return s.putObject(ctx, objectStoreConfigKey, data, "application/x-yaml")
}

func (s *ObjectTokenStore) ensureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("object store: check bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err = s.client.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{Region: s.cfg.Region}); err != nil {
		return fmt.Errorf("object store: create bucket: %w", err)
	}
	return nil
}

func (s *ObjectTokenStore) syncConfigFromBucket(ctx context.Context, example string) error {
	key := s.prefixedKey(objectStoreConfigKey)
	_, err := s.client.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	switch {
	case err == nil:
		object, errGet := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
		if errGet != nil {
			return fmt.Errorf("object store: fetch config: %w", errGet)
		}
		defer func() { _ = object.Close() }()
		data, errRead := io.ReadAll(io.LimitReader(object, 50<<20))
		if errRead != nil {
			return fmt.Errorf("object store: read config: %w", errRead)
		}
		if errWrite := os.WriteFile(s.configPath, normalizeLineEndingsBytes(data), 0o600); errWrite != nil {
			return fmt.Errorf("object store: write config: %w", errWrite)
		}
	case isObjectNotFound(err):
		if _, statErr := os.Stat(s.configPath); errors.Is(statErr, fs.ErrNotExist) {
			if example != "" {
				if errCopy := misc.CopyConfigTemplate(example, s.configPath); errCopy != nil {
					return fmt.Errorf("object store: copy example config: %w", errCopy)
				}
			} else {
				if errCreate := os.MkdirAll(filepath.Dir(s.configPath), 0o700); errCreate != nil {
					return fmt.Errorf("object store: prepare config directory: %w", errCreate)
				}
				if errWrite := os.WriteFile(s.configPath, []byte{}, 0o600); errWrite != nil {
					return fmt.Errorf("object store: create empty config: %w", errWrite)
				}
			}
		}
		data, errRead := os.ReadFile(s.configPath)
		if errRead != nil {
			return fmt.Errorf("object store: read local config: %w", errRead)
		}
		if len(data) > 0 {
			if errPut := s.putObject(ctx, objectStoreConfigKey, data, "application/x-yaml"); errPut != nil {
				return errPut
			}
		}
	default:
		return fmt.Errorf("object store: stat config: %w", err)
	}
	return nil
}

func (s *ObjectTokenStore) syncAuthFromBucket(ctx context.Context) error {
	// NOTE: We intentionally do NOT use os.RemoveAll here.
	// Wiping the directory triggers file watcher delete events, which then
	// propagate deletions to the remote object store (race condition).
	// Instead, we just ensure the directory exists and overwrite files incrementally.
	if err := secureAuthDirectory(s.authDir); err != nil {
		return fmt.Errorf("object store: secure auth directory: %w", err)
	}

	prefix := s.prefixedKey(objectStoreAuthPrefix + "/")
	objectCh := s.client.ListObjects(
		ctx, s.cfg.Bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		},
	)
	for object := range objectCh {
		if object.Err != nil {
			return fmt.Errorf("object store: list auth objects: %w", object.Err)
		}
		rel := strings.TrimPrefix(object.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		relPath := filepath.FromSlash(rel)
		if filepath.IsAbs(relPath) {
			log.WithField("key", object.Key).Warn("object store: skip auth outside mirror")
			continue
		}
		cleanRel := filepath.Clean(relPath)
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
			log.WithField("key", object.Key).Warn("object store: skip auth outside mirror")
			continue
		}
		local := filepath.Join(s.authDir, cleanRel)
		if err := secureAuthDirectory(filepath.Dir(local)); err != nil {
			return fmt.Errorf("object store: secure auth subdir: %w", err)
		}
		reader, errGet := s.client.GetObject(ctx, s.cfg.Bucket, object.Key, minio.GetObjectOptions{})
		if errGet != nil {
			return fmt.Errorf("object store: download auth %s: %w", object.Key, errGet)
		}
		data, errRead := io.ReadAll(io.LimitReader(reader, 50<<20))
		_ = reader.Close()
		if errRead != nil {
			return fmt.Errorf("object store: read auth %s: %w", object.Key, errRead)
		}
		if _, tombstone := parseObjectStoreAuthTombstone(data); tombstone {
			if errRemove := os.Remove(local); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
				return fmt.Errorf("object store: remove tombstoned auth %s: %w", local, errRemove)
			}
			continue
		}
		if errWrite := writeAuthFile(local, data); errWrite != nil {
			return fmt.Errorf("object store: write auth %s: %w", local, errWrite)
		}
	}
	return nil
}

func (s *ObjectTokenStore) uploadAuth(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return fmt.Errorf("object store: resolve auth relative path: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteAuthObject(ctx, path)
		}
		return fmt.Errorf("object store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthObject(ctx, path)
	}
	key := objectStoreAuthPrefix + "/" + filepath.ToSlash(rel)
	return s.putObject(ctx, key, data, "application/json")
}

func (s *ObjectTokenStore) deleteAuthObject(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return fmt.Errorf("object store: resolve auth relative path: %w", err)
	}
	key := objectStoreAuthPrefix + "/" + filepath.ToSlash(rel)
	return s.deleteObject(ctx, key)
}

func (s *ObjectTokenStore) putObject(ctx context.Context, key string, data []byte, contentType string) error {
	if len(data) == 0 {
		return s.deleteObject(ctx, key)
	}
	fullKey := s.prefixedKey(key)
	reader := bytes.NewReader(data)
	_, err := s.client.PutObject(
		ctx, s.cfg.Bucket, fullKey, reader, int64(len(data)), minio.PutObjectOptions{
			ContentType: contentType,
		},
	)
	if err != nil {
		return fmt.Errorf("object store: put object %s: %w", fullKey, err)
	}
	return nil
}

func parseObjectStoreAuthTombstone(raw []byte) (objectStoreAuthTombstone, bool) {
	var tombstone objectStoreAuthTombstone
	if json.Unmarshal(raw, &tombstone) != nil || !tombstone.Tombstone {
		return objectStoreAuthTombstone{}, false
	}
	return tombstone, true
}

func (s *ObjectTokenStore) removeObjectVersion(ctx context.Context, fullKey, versionID string) error {
	versionID = strings.TrimSpace(versionID)
	if versionID == "" || strings.EqualFold(versionID, "null") {
		return fmt.Errorf("object store: invalid auth version id")
	}
	if err := s.client.RemoveObject(
		ctx,
		s.cfg.Bucket,
		fullKey,
		minio.RemoveObjectOptions{VersionID: versionID},
	); err != nil && !isObjectNotFound(err) {
		return fmt.Errorf("object store: delete auth version %s: %w", fullKey, err)
	}
	return nil
}

func (s *ObjectTokenStore) deleteObject(ctx context.Context, key string) error {
	fullKey := s.prefixedKey(key)
	err := s.client.RemoveObject(ctx, s.cfg.Bucket, fullKey, minio.RemoveObjectOptions{})
	if err != nil {
		if isObjectNotFound(err) {
			return nil
		}
		return fmt.Errorf("object store: delete object %s: %w", fullKey, err)
	}
	return nil
}

func (s *ObjectTokenStore) prefixedKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if s.cfg.Prefix == "" {
		return key
	}
	return strings.TrimLeft(s.cfg.Prefix+"/"+key, "/")
}

func (s *ObjectTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("object store: auth is nil")
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			if filepath.IsAbs(path) {
				return path, nil
			}
			return filepath.Join(s.authDir, path), nil
		}
	}
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(auth.ID)
	}
	if fileName == "" {
		return "", fmt.Errorf("object store: auth %s missing filename", auth.ID)
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	return filepath.Join(s.authDir, fileName), nil
}

func (s *ObjectTokenStore) resolveDeletePath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("object store: id is empty")
	}
	// Absolute paths are honored as-is; callers must ensure they point inside the mirror.
	if filepath.IsAbs(id) {
		return id, nil
	}
	// Treat any non-absolute id (including nested like "team/foo") as relative to the mirror authDir.
	// Normalize separators and guard against path traversal.
	clean := filepath.Clean(filepath.FromSlash(id))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("object store: invalid auth identifier %s", id)
	}
	// Ensure .json suffix.
	if !strings.HasSuffix(strings.ToLower(clean), ".json") {
		clean += ".json"
	}
	return filepath.Join(s.authDir, clean), nil
}

func (s *ObjectTokenStore) readAuthFile(ctx context.Context, path, baseDir string) (*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider := strings.TrimSpace(valueAsString(metadata["type"]))
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat auth file: %w", err)
	}
	id := internalauth.FileAuthID(path, baseDir)
	pluginAuth, handled, errParse := sdkauth.ParsePluginAuthFile(
		ctx, provider, path, id, data,
	)
	if errParse != nil {
		return nil, errParse
	}
	if handled {
		pluginAuth.CreatedAt = info.ModTime()
		pluginAuth.UpdatedAt = info.ModTime()
		return pluginAuth, nil
	}
	if provider == "" {
		provider = "unknown"
	}
	attr := map[string]string{"path": path}
	if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
		attr["email"] = email
	}
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if disabled {
		status = cliproxyauth.StatusDisabled
	}
	auth := &cliproxyauth.Auth{
		ID:               id,
		Provider:         provider,
		FileName:         id,
		Label:            labelFor(metadata),
		Status:           status,
		Disabled:         disabled,
		Attributes:       attr,
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	if err := cliproxyauth.ApplyCustomHeadersFromMetadata(auth); err != nil {
		return nil, err
	}
	return auth, nil
}

func normalizeLineEndingsBytes(data []byte) []byte {
	replaced := bytes.ReplaceAll(data, []byte{'\r', '\n'}, []byte{'\n'})
	return bytes.ReplaceAll(replaced, []byte{'\r'}, []byte{'\n'})
}

func isObjectPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusPreconditionFailed || resp.Code == "PreconditionFailed"
}

func isObjectNotFound(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	switch resp.Code {
	case "NoSuchKey", "NoSuchVersion", "NotFound", "NoSuchBucket":
		return true
	}
	return false
}
