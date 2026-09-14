package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
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

type pluginAuthParserHolder struct {
	parser PluginAuthParser
}

type pluginAuthParserContextKey struct{}

var pluginAuthParserValue atomic.Value

const authProjectDiscoveryTimeout = 15 * time.Second

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

// ParsePluginAuthFile applies the registered plugin parser to file-backed auth material
func ParsePluginAuthFile(
	ctx context.Context,
	provider,
	path,
	fileName string,
	data []byte,
) (*cliproxyauth.Auth, bool, error) {
	parser := pluginAuthParserForContext(ctx)
	if parser == nil {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auth, handled, errParse := parser.ParseAuth(
		ctx,
		pluginapi.AuthParseRequest{
			Provider: strings.TrimSpace(provider),
			Path:     path,
			FileName: fileName,
			RawJSON:  data,
		},
	)
	if errParse != nil || !handled {
		return auth, handled, errParse
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, true, fmt.Errorf("plugin auth parser returned an invalid auth")
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if path = strings.TrimSpace(path); path != "" {
		auth.Attributes["path"] = path
		auth.Attributes["source"] = path
	}
	if errHeaders := cliproxyauth.ApplyCustomHeadersFromMetadata(auth); errHeaders != nil {
		return nil, true, errHeaders
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil, true, fmt.Errorf("parse plugin auth metadata: %w", errUnmarshal)
	}
	if errWeight := cliproxyauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
		return nil, true, errWeight
	}
	return auth, true, nil
}

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu                      sync.Mutex
	dirLock                 sync.RWMutex
	baseDir                 string
	httpClient              *http.Client
	projectDiscoveryTimeout time.Duration
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

	s.mu.Lock()
	defer s.mu.Unlock()

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

	cliproxyauth.PrepareAuthForPersistence(auth)
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
			auth, err := s.readAuthFile(ctx, path, dir)
			if err != nil {
				if errCtx := ctx.Err(); errCtx != nil {
					return errCtx
				}
				log.WithError(err).Warnf("auth filestore: skip auth %s", path)
				return nil
			}
			if auth != nil {
				entries = append(entries, auth)
			}
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
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
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
	if provider == "antigravity" || provider == "gemini" {
		projectID := ""
		if pid, ok := metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
		if projectID == "" {
			discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, s.discoveryTimeout())
			httpClient := s.discoveryHTTPClient()
			accessToken := extractAccessToken(metadata)
			// For gemini type, the stored access_token is likely expired (~1h lifetime).
			// Refresh it using the long-lived refresh_token before querying.
			if provider == "gemini" {
				if tokenMap, ok := metadata["token"].(map[string]any); ok {
					if refreshed, errRefresh := refreshGeminiAccessToken(
						discoveryCtx, tokenMap, httpClient,
					); errRefresh == nil {
						accessToken = refreshed
					}
				}
			}
			if accessToken != "" {
				fetchedProjectID, errFetch := FetchAntigravityProjectID(
					discoveryCtx, accessToken, httpClient,
				)
				if errFetch == nil && strings.TrimSpace(fetchedProjectID) != "" {
					metadata, err = s.persistProjectIDIfUnchanged(path, data, metadata, fetchedProjectID)
					if err != nil {
						cancelDiscovery()
						return nil, err
					}
					if latestInfo, errLatestInfo := os.Stat(path); errLatestInfo == nil {
						info = latestInfo
					}
				}
			}
			cancelDiscovery()
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
		}
	}
	id := s.idFor(path, baseDir)
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if disabled {
		status = cliproxyauth.StatusDisabled
	}
	auth := &cliproxyauth.Auth{
		ID:               id,
		Provider:         provider,
		FileName:         id,
		Label:            s.labelFor(metadata),
		Status:           status,
		Disabled:         disabled,
		Attributes:       map[string]string{"path": path},
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
	if email, ok := metadata["email"].(string); ok && email != "" {
		auth.Attributes["email"] = email
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

func (s *FileTokenStore) discoveryHTTPClient() *http.Client {
	if s != nil && s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

func (s *FileTokenStore) discoveryTimeout() time.Duration {
	if s != nil && s.projectDiscoveryTimeout > 0 {
		return s.projectDiscoveryTimeout
	}
	return authProjectDiscoveryTimeout
}

func (s *FileTokenStore) persistProjectIDIfUnchanged(
	path string,
	original []byte,
	candidate map[string]any,
	projectID string,
) (map[string]any, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("persist antigravity project ID: project ID is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("persist antigravity project ID: read current credential: %w", errRead)
	}
	metadata := make(map[string]any)
	if errDecode := json.Unmarshal(current, &metadata); errDecode != nil {
		return nil, fmt.Errorf("persist antigravity project ID: decode current credential: %w", errDecode)
	}
	if !bytes.Equal(current, original) {
		return metadata, nil
	}
	if existing, _ := metadata["project_id"].(string); strings.TrimSpace(existing) != "" {
		return metadata, nil
	}
	if candidate == nil {
		candidate = metadata
	}
	candidate["project_id"] = projectID
	raw, errMarshal := json.Marshal(candidate)
	if errMarshal != nil {
		return nil, fmt.Errorf("persist antigravity project ID: encode credential: %w", errMarshal)
	}
	if errWrite := internalauth.WriteTokenFileAtomic(path, raw); errWrite != nil {
		return nil, fmt.Errorf("persist antigravity project ID: %w", errWrite)
	}
	return candidate, nil
}

func extractAccessToken(metadata map[string]any) string {
	if at, ok := metadata["access_token"].(string); ok {
		if v := strings.TrimSpace(at); v != "" {
			return v
		}
	}
	if tokenMap, ok := metadata["token"].(map[string]any); ok {
		if at, ok := tokenMap["access_token"].(string); ok {
			if v := strings.TrimSpace(at); v != "" {
				return v
			}
		}
	}
	return ""
}

func refreshGeminiAccessToken(
	ctx context.Context,
	tokenMap map[string]any,
	httpClient *http.Client,
) (string, error) {
	refreshToken, _ := tokenMap["refresh_token"].(string)
	clientID, _ := tokenMap["client_id"].(string)
	clientSecret, _ := tokenMap["client_secret"].(string)
	tokenURI, _ := tokenMap["token_uri"].(string)

	if refreshToken == "" || clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("missing refresh credentials")
	}
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	request, errRequest := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURI,
		strings.NewReader(data.Encode()),
	)
	if errRequest != nil {
		return "", fmt.Errorf("create refresh request: %w", errRequest)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed: status %d", resp.StatusCode)
	}

	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return "", fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}

	newAccessToken, _ := result["access_token"].(string)
	if newAccessToken == "" {
		return "", fmt.Errorf("no access_token in refresh response")
	}

	tokenMap["access_token"] = newAccessToken
	return newAccessToken, nil
}
