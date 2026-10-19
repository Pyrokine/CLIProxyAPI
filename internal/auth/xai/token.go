package xai

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TokenStorage stores xAI OAuth credentials on disk.
type TokenStorage struct {
	Type          string `json:"type"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	ExpiresIn     int    `json:"expires_in,omitempty"`
	Expire        string `json:"expired,omitempty"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Email         string `json:"email,omitempty"`
	Subject       string `json:"sub,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	AuthKind      string `json:"auth_kind,omitempty"`

	Metadata map[string]any `json:"-"`
}

func (ts *TokenStorage) CloneForPersistence() coreauth.TokenStorage {
	if ts == nil {
		return nil
	}
	copyStorage := *ts
	return &copyStorage
}

// SetMetadata allows the token store to merge status fields before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes xAI credentials to a JSON auth file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	if ts == nil {
		return fmt.Errorf("xai token storage: storage is nil")
	}
	authFilePath = strings.TrimSpace(authFilePath)
	if authFilePath == "" {
		return fmt.Errorf("xai token storage: auth file path is empty")
	}
	if hasTrailingPathSeparator(authFilePath) {
		return fmt.Errorf("xai token storage: auth file path identifies a directory")
	}
	absoluteAuthFilePath, err := filepath.Abs(authFilePath)
	if err != nil {
		return fmt.Errorf("xai token storage: resolve auth file path: %w", err)
	}
	authFilePath = filepath.Clean(absoluteAuthFilePath)
	name := filepath.Base(authFilePath)
	if name == "." || name == ".." || name == "" {
		return fmt.Errorf("xai token storage: auth file name is invalid")
	}
	ts.Type = "xai"
	ts.AuthKind = "oauth"
	data, err := misc.MergeMetadata(ts, ts.Metadata)
	if err != nil {
		return fmt.Errorf("xai token storage: merge metadata: %w", err)
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("xai token storage: marshal token file: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(authFilePath)
	root, err := util.OpenOrCreateDirectoryRoot(dir, 0o700)
	if err != nil {
		return fmt.Errorf("xai token storage: open directory root: %w", err)
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("xai token storage: open directory: %w", err)
	}
	if err = secureTokenFile(directory, true); err != nil {
		_ = directory.Close()
		return fmt.Errorf("xai token storage: secure directory: %w", err)
	}
	if err = directory.Close(); err != nil {
		return fmt.Errorf("xai token storage: close directory: %w", err)
	}
	return saveTokenPayload(root, name, payload)
}

func hasTrailingPathSeparator(path string) bool {
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		return true
	}
	return runtime.GOOS == "windows" && strings.HasSuffix(path, "/")
}

func saveTokenPayload(root *os.Root, name string, payload []byte) error {
	return withTokenFileLock(
		root,
		name,
		func() error {
			if info, errInspect := root.Lstat(name); errInspect == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("xai token storage: token path is not a regular file")
				}
			} else if !os.IsNotExist(errInspect) {
				return fmt.Errorf("xai token storage: inspect token path: %w", errInspect)
			}
			file, tempName, errCreate := createTokenCandidate(root)
			if errCreate != nil {
				return errCreate
			}
			fileOpen := true
			defer func() {
				if fileOpen {
					_ = file.Close()
				}
				if tempName != "" {
					_ = root.Remove(tempName)
				}
			}()
			if err := secureTokenFile(file, false); err != nil {
				return fmt.Errorf("xai token storage: secure token candidate: %w", err)
			}
			written, errWrite := file.Write(payload)
			if errWrite != nil {
				return fmt.Errorf("xai token storage: write token candidate: %w", errWrite)
			}
			if written != len(payload) {
				return io.ErrShortWrite
			}
			if err := file.Sync(); err != nil {
				return fmt.Errorf("xai token storage: sync token candidate: %w", err)
			}
			candidateInfo, err := file.Stat()
			if err != nil {
				return fmt.Errorf("xai token storage: inspect token candidate: %w", err)
			}
			if err = file.Close(); err != nil {
				fileOpen = false
				return fmt.Errorf("xai token storage: close token candidate: %w", err)
			}
			fileOpen = false
			pathInfo, err := root.Lstat(tempName)
			if err != nil {
				return fmt.Errorf("xai token storage: verify token candidate: %w", err)
			}
			if !pathInfo.Mode().IsRegular() || !os.SameFile(candidateInfo, pathInfo) {
				return fmt.Errorf("xai token storage: token candidate changed before publication")
			}
			if err = root.Rename(tempName, name); err != nil {
				return fmt.Errorf("xai token storage: publish token file: %w", err)
			}
			tempName = ""
			if err := syncTokenRoot(root); err != nil {
				return fmt.Errorf("xai token storage: sync token directory: %w", err)
			}
			return nil
		},
	)
}

func createTokenCandidate(root *os.Root) (*os.File, string, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("xai token storage: generate candidate name: %w", err)
		}
		tempName := fmt.Sprintf(".xai-token-%x.tmp", suffix)
		file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return file, tempName, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("xai token storage: create token candidate: %w", err)
		}
	}
	return nil, "", fmt.Errorf("xai token storage: candidate name collision limit exceeded")
}

func syncTokenRoot(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

// CredentialFileName returns the filename used for xAI credentials.
func CredentialFileName(email, subject string) string {
	email = sanitizeFileSegment(email)
	if email != "" {
		return fmt.Sprintf("xai-%s.json", email)
	}
	subject = sanitizeFileSegment(subject)
	if subject != "" {
		return fmt.Sprintf("xai-%s.json", subject)
	}
	return fmt.Sprintf("xai-%d.json", time.Now().UnixMilli())
}

func sanitizeFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@' || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
