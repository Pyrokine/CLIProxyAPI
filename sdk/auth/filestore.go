package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type PluginAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*cliproxyauth.Auth, bool, error)
}

// PluginMultiAuthParser expands one source into multiple plugin runtime auths.
type PluginMultiAuthParser interface {
	ParseAuths(context.Context, pluginapi.AuthParseRequest) ([]*cliproxyauth.Auth, bool, error)
}

type pluginAuthParserHolder struct {
	parser PluginAuthParser
}

type pluginAuthParserContextKey struct{}

var pluginAuthParserValue atomic.Value

func RegisterPluginAuthParser(parser PluginAuthParser) {
	pluginAuthParserValue.Store(pluginAuthParserHolder{parser: parser})
}

func currentPluginAuthParser() PluginAuthParser {
	value := pluginAuthParserValue.Load()
	if value == nil {
		return nil
	}
	holder, ok := value.(pluginAuthParserHolder)
	if !ok {
		return nil
	}
	return holder.parser
}

func WithPluginAuthParser(ctx context.Context, parser PluginAuthParser) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pluginAuthParserContextKey{}, pluginAuthParserHolder{parser: parser})
}

func pluginAuthParserForContext(ctx context.Context) PluginAuthParser {
	if ctx != nil {
		if holder, ok := ctx.Value(pluginAuthParserContextKey{}).(pluginAuthParserHolder); ok {
			return holder.parser
		}
	}
	return currentPluginAuthParser()
}

// ParsePluginAuthFiles applies the registered plugin parser to file-backed auth material.
func ParsePluginAuthFiles(
	ctx context.Context,
	provider,
	path,
	fileName string,
	data []byte,
) ([]*cliproxyauth.Auth, bool, error) {
	parser := pluginAuthParserForContext(ctx)
	if parser == nil {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, errUnmarshal := cliproxyauth.DecodeCredentialMetadata(data)
	if errUnmarshal != nil {
		return nil, true, fmt.Errorf("parse plugin auth metadata: %w", errUnmarshal)
	}
	cliproxyauth.NormalizeCredentialMetadata(metadata)
	if errWeight := cliproxyauth.ValidateAuthWeight(&cliproxyauth.Auth{Metadata: metadata}); errWeight != nil {
		return nil, true, errWeight
	}
	auths, handled, errParse := parsePluginAuthFile(
		ctx,
		parser,
		pluginapi.AuthParseRequest{
			Provider: strings.TrimSpace(provider),
			Path:     path,
			FileName: fileName,
			RawJSON:  data,
		},
	)
	if errParse != nil || !handled {
		return auths, handled, errParse
	}
	auths = compactPluginAuths(auths)
	if len(auths) == 0 {
		return nil, true, nil
	}
	persistedDigest, errDigest := cliproxyauth.DigestPersistedJSON(data)
	if errDigest != nil {
		return nil, true, errDigest
	}
	path = strings.TrimSpace(path)
	disabled, _ := metadata["disabled"].(bool)
	seenIDs := make(map[string]struct{}, len(auths))
	for index, auth := range auths {
		cliproxyauth.NormalizeCredentialMetadata(auth.Metadata)
		if strings.TrimSpace(auth.ID) == "" {
			return nil, true, fmt.Errorf("plugin auth parser returned an auth without an id")
		}
		if _, exists := seenIDs[auth.ID]; exists {
			return nil, true, fmt.Errorf("plugin auth parser returned duplicate auth id %q", auth.ID)
		}
		seenIDs[auth.ID] = struct{}{}
		if len(auths) > 1 {
			cliproxyauth.MarkPluginVirtualAuth(auth, path, index)
		}
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		if path != "" {
			auth.Attributes[cliproxyauth.AttributePath] = path
			auth.Attributes[cliproxyauth.AttributeSource] = path
			auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile
		}
		if disabled {
			auth.Disabled = true
			auth.Status = cliproxyauth.StatusDisabled
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["disabled"] = true
		}
		if proxyURL, ok := metadata["proxy_url"].(string); ok && strings.TrimSpace(auth.ProxyURL) == "" {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
		if prefix, ok := metadata["prefix"].(string); ok && strings.TrimSpace(auth.Prefix) == "" {
			auth.Prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		}
		if errHeaders := cliproxyauth.ApplyCustomHeadersFromMetadata(auth); errHeaders != nil {
			return nil, true, errHeaders
		}
		if errWeight := cliproxyauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
			return nil, true, errWeight
		}
		cliproxyauth.ApplyAuthPriorityMetadata(auth, metadata)
		if _, inherited := auth.Attributes[cliproxyauth.AttributeFilePriority]; inherited {
			if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
				setter.SetMetadata(auth.Metadata)
			}
		}
		if errProfile := cliproxyauth.ApplyClaudeFingerprintProfileMetadata(auth, metadata); errProfile != nil {
			return nil, true, errProfile
		}
		if errBind := cliproxyauth.BindPersistedAuthDigest(auth, persistedDigest); errBind != nil {
			log.WithError(errBind).Warn("auth file: quota material proof unavailable")
		}
	}
	return auths, true, nil
}

// ParsePluginAuthFile preserves the legacy single-auth parser contract.
func ParsePluginAuthFile(
	ctx context.Context,
	provider,
	path,
	fileName string,
	data []byte,
) (*cliproxyauth.Auth, bool, error) {
	auths, handled, errParse := ParsePluginAuthFiles(ctx, provider, path, fileName, data)
	if errParse != nil || !handled || len(auths) == 0 {
		return nil, handled, errParse
	}
	return auths[0], true, nil
}

func parsePluginAuthFile(
	ctx context.Context,
	parser PluginAuthParser,
	req pluginapi.AuthParseRequest,
) ([]*cliproxyauth.Auth, bool, error) {
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(ctx, req)
	}
	auth, handled, errParse := parser.ParseAuth(ctx, req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*cliproxyauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*cliproxyauth.Auth) []*cliproxyauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if errWeight := cliproxyauth.ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu      sync.Mutex
	dirLock sync.RWMutex
	baseDir string
}

// NewFileTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewFileTokenStore() *FileTokenStore {
	return &FileTokenStore{}
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *FileTokenStore) SetBaseDir(dir string) {
	s.dirLock.Lock()
	s.baseDir = strings.TrimSpace(dir)
	s.dirLock.Unlock()
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *FileTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	return s.saveWithBaseDir(ctx, s.baseDirSnapshot(), auth)
}

// SaveWithBaseDir persists an auth record using an operation-scoped base directory.
func (s *FileTokenStore) SaveWithBaseDir(
	ctx context.Context,
	baseDir string,
	auth *cliproxyauth.Auth,
) (string, error) {
	return s.saveWithBaseDir(ctx, strings.TrimSpace(baseDir), auth)
}

func (s *FileTokenStore) saveWithBaseDir(
	ctx context.Context,
	baseDir string,
	auth *cliproxyauth.Auth,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if errWeight := cliproxyauth.ValidateAuthWeight(auth); errWeight != nil {
		return "", errWeight
	}

	path, err := s.resolveAuthPath(auth, baseDir)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}
	if cliproxyauth.IsPluginVirtualAuth(auth) {
		return path, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if auth.Disabled && !cliproxyauth.HasAuthCreationIntent(ctx) {
		if _, errStat := os.Stat(path); errors.Is(errStat, os.ErrNotExist) {
			return "", nil
		} else if errStat != nil {
			return "", fmt.Errorf("auth filestore: stat disabled credential: %w", errStat)
		}
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	root, errRoot := util.OpenOrCreateDirectoryRoot(filepath.Dir(path), 0o700)
	if errRoot != nil {
		return "", fmt.Errorf("auth filestore: secure dir failed: %w", errRoot)
	}
	if errCloseRoot := root.Close(); errCloseRoot != nil {
		return "", fmt.Errorf("auth filestore: close dir root failed: %w", errCloseRoot)
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}

	if errPrepare := cliproxyauth.PrepareAuthForPersistence(auth); errPrepare != nil {
		return "", errPrepare
	}
	switch {
	case auth.Storage != nil:
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if err = internalauth.PublishTokenFile(path, auth.Storage.SaveTokenToFile); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		auth.Metadata["disabled"] = auth.Disabled
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
		}
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if err = ctx.Err(); err != nil {
				return "", err
			}
			if util.JSONEqual(existing, raw) {
				if errSecure := internalauth.SecureTokenFile(path); errSecure != nil {
					return "", fmt.Errorf("auth filestore: secure existing file failed: %w", errSecure)
				}
				break
			}
			if errWrite := internalauth.WriteTokenFileAtomic(path, raw); errWrite != nil {
				return "", fmt.Errorf("auth filestore: write existing failed: %w", errWrite)
			}
			break
		} else if !os.IsNotExist(errRead) {
			return "", fmt.Errorf("auth filestore: read existing failed: %w", errRead)
		}
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if errWrite := internalauth.WriteTokenFileAtomic(path, raw); errWrite != nil {
			return "", fmt.Errorf("auth filestore: write file failed: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSource] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	return path, nil
}

// List enumerates all auth JSON files under the configured directory.
func (s *FileTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entries := make([]*cliproxyauth.Auth, 0)
	err := filepath.WalkDir(
		dir, func(path string, d fs.DirEntry, walkErr error) error {
			if errCtx := ctx.Err(); errCtx != nil {
				return errCtx
			}
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
				return nil
			}
			auths, errRead := s.readAuthFiles(ctx, path, dir)
			if errRead != nil {
				if errCtx := ctx.Err(); errCtx != nil {
					return errCtx
				}
				log.WithError(errRead).Warnf("auth filestore: skip auth %s", path)
				return nil
			}
			entries = append(entries, auths...)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// DeleteIfVersion removes the auth file only while its exact persisted content matches.
func (s *FileTokenStore) DeleteIfVersion(
	_ context.Context,
	id string,
	version cliproxyauth.PersistedVersion,
) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cliproxyauth.RemoveFileIfVersion(path, version)
}

// Delete removes the auth file.
func (s *FileTokenStore) Delete(_ context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	return nil
}

func (s *FileTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *FileTokenStore) readAuthFiles(ctx context.Context, path, baseDir string) ([]*cliproxyauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read file: %w", errRead)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata, errJSON := cliproxyauth.DecodeCredentialMetadata(data)
	if errJSON != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", errJSON)
	}
	cliproxyauth.NormalizeCredentialMetadata(metadata)
	provider, _ := metadata["type"].(string)
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	auths, handled, errParse := ParsePluginAuthFiles(ctx, provider, path, s.idFor(path, baseDir), data)
	if errParse != nil {
		return nil, errParse
	}
	if handled {
		for _, auth := range auths {
			auth.CreatedAt = info.ModTime()
			auth.UpdatedAt = info.ModTime()
		}
		return auths, nil
	}
	auth, errBuiltIn := s.readAuthFile(WithPluginAuthParser(ctx, nil), path, baseDir)
	if errBuiltIn != nil || auth == nil {
		return nil, errBuiltIn
	}
	return []*cliproxyauth.Auth{auth}, nil
}

func (s *FileTokenStore) readAuthFile(ctx context.Context, path, baseDir string) (*cliproxyauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata, err := cliproxyauth.DecodeCredentialMetadata(data)
	if err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	cliproxyauth.NormalizeCredentialMetadata(metadata)
	if errWeight := cliproxyauth.ValidateAuthWeight(&cliproxyauth.Auth{Metadata: metadata}); errWeight != nil {
		return nil, errWeight
	}
	provider, _ := metadata["type"].(string)
	provider = strings.TrimSpace(provider)
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	pluginAuth, handled, errParse := ParsePluginAuthFile(
		ctx,
		provider,
		path,
		s.idFor(path, baseDir),
		data,
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
	id := s.idFor(path, baseDir)
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if disabled {
		status = cliproxyauth.StatusDisabled
	}
	proxyURL, _ := metadata["proxy_url"].(string)
	proxyURL = strings.TrimSpace(proxyURL)
	prefix, _ := metadata["prefix"].(string)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if strings.Contains(prefix, "/") {
		prefix = ""
	}
	auth := &cliproxyauth.Auth{
		ID:               id,
		Provider:         provider,
		FileName:         id,
		Label:            s.labelFor(metadata),
		Status:           status,
		Disabled:         disabled,
		Attributes:       map[string]string{"path": path},
		ProxyURL:         proxyURL,
		Prefix:           prefix,
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	if err := cliproxyauth.ApplyCustomHeadersFromMetadata(auth); err != nil {
		return nil, err
	}
	if errWeight := cliproxyauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
		return nil, errWeight
	}
	cliproxyauth.ApplyAuthPriorityMetadata(auth, metadata)
	if errProfile := cliproxyauth.ApplyClaudeFingerprintProfileMetadata(auth, metadata); errProfile != nil {
		return nil, errProfile
	}
	if email, ok := metadata["email"].(string); ok && email != "" {
		auth.Attributes["email"] = email
	}
	digest, errDigest := cliproxyauth.DigestPersistedJSON(data)
	if errDigest != nil {
		return nil, errDigest
	}
	if errBind := cliproxyauth.BindPersistedAuthDigest(auth, digest); errBind != nil {
		log.WithError(errBind).Warn("auth file: quota material proof unavailable")
	}
	return auth, nil
}

func (s *FileTokenStore) idFor(path, baseDir string) string {
	return internalauth.FileAuthID(path, baseDir)
}

func (s *FileTokenStore) resolveAuthPath(auth *cliproxyauth.Auth, baseDir string) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if baseDir != "" {
			return filepath.Join(baseDir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	if baseDir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(baseDir, auth.ID), nil
}

func (s *FileTokenStore) labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["label"].(string); ok && v != "" {
		return v
	}
	if v, ok := metadata["email"].(string); ok && v != "" {
		return v
	}
	if project, ok := metadata["project_id"].(string); ok && project != "" {
		return project
	}
	return ""
}

func (s *FileTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}
