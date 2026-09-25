package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

// Manager aggregates authenticators and coordinates persistence via a token store.
type Manager struct {
	authenticators map[string]Authenticator
	store          coreauth.Store
}

// NewManager constructs a manager with the provided token store and authenticators.
// If store is nil, the caller must set it later using SetStore.
func NewManager(store coreauth.Store, authenticators ...Authenticator) *Manager {
	mgr := &Manager{
		authenticators: make(map[string]Authenticator),
		store:          store,
	}
	for i := range authenticators {
		mgr.Register(authenticators[i])
	}
	return mgr
}

// Register adds or replaces an authenticator keyed by its provider identifier.
func (m *Manager) Register(a Authenticator) {
	if a == nil {
		return
	}
	if m.authenticators == nil {
		m.authenticators = make(map[string]Authenticator)
	}
	m.authenticators[a.Provider()] = a
}

// SetStore updates the token store used for persistence.
func (m *Manager) SetStore(store coreauth.Store) {
	m.store = store
}

// Login executes the provider login flow and persists the resulting auth record.
func (m *Manager) Login(ctx context.Context, provider string, cfg *config.Config, opts *LoginOptions) (
	*coreauth.Auth,
	string,
	error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	auth, ok := m.authenticators[provider]
	if !ok {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s not registered", provider)
	}

	record, err := auth.Login(ctx, cfg, opts)
	if err != nil {
		return nil, "", err
	}
	if record == nil {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s returned nil record", provider)
	}
	if m.store == nil {
		return record, "", nil
	}
	if err = ctx.Err(); err != nil {
		return record, "", err
	}

	authDir := ""
	if cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	if authDir != "" {
		existingMetadata, errRead := readExistingCredentialMetadata(authDir, record)
		if errRead != nil {
			return record, "", errRead
		}
		if errMerge := coreauth.MergeExistingAuthMetadata(record, existingMetadata); errMerge != nil {
			return record, "", fmt.Errorf("cliproxy auth: preserve existing credential metadata: %w", errMerge)
		}
	}

	var legacyClaudeCredential *coreauth.Auth
	if strings.EqualFold(strings.TrimSpace(record.Provider), "claude") {
		legacyClaudeCredential, err = claudeauth.FindMatchingLegacyCredential(ctx, authDir, record)
		if err != nil {
			return record, "", err
		}
		if legacyClaudeCredential != nil {
			if errMerge := coreauth.MergeExistingAuthMetadata(record, legacyClaudeCredential.Metadata); errMerge != nil {
				return record, "", fmt.Errorf("cliproxy auth: preserve Claude legacy metadata: %w", errMerge)
			}
		}
	}

	if strings.EqualFold(strings.TrimSpace(record.Provider), "devin") {
		existingDevinCredential, errFind := devinauth.FindMatchingCredential(ctx, authDir, record)
		if errFind != nil {
			return record, "", errFind
		}
		if existingDevinCredential != nil {
			if errMerge := coreauth.MergeExistingAuthMetadata(record, existingDevinCredential.Metadata); errMerge != nil {
				return record, "", fmt.Errorf("cliproxy auth: preserve Devin credential metadata: %w", errMerge)
			}
			devinauth.ReuseExistingCredentialIdentity(record, existingDevinCredential)
		} else {
			devinauth.ApplyMetadataIdentity(record)
		}
	}

	if err = ctx.Err(); err != nil {
		return record, "", err
	}
	persistCtx := coreauth.WithAuthCreationIntent(ctx)
	var savedPath string
	if cfg != nil {
		if scopedStore, okScoped := m.store.(interface {
			SaveWithBaseDir(context.Context, string, *coreauth.Auth) (string, error)
		}); okScoped {
			savedPath, err = scopedStore.SaveWithBaseDir(persistCtx, cfg.AuthDir, record)
		} else {
			savedPath, err = m.store.Save(persistCtx, record)
		}
	} else {
		savedPath, err = m.store.Save(persistCtx, record)
	}
	if err != nil {
		return record, "", err
	}

	if legacyClaudeCredential != nil {
		if strings.TrimSpace(savedPath) == "" {
			return record, "", fmt.Errorf(
				"cliproxy auth: canonical Claude credential was not persisted; legacy credential retained",
			)
		}
		legacyID := strings.TrimSpace(legacyClaudeCredential.ID)
		if legacyID == "" {
			legacyID = strings.TrimSpace(legacyClaudeCredential.FileName)
		}
		if errDelete := m.store.Delete(ctx, legacyID); errDelete != nil {
			return record, savedPath, fmt.Errorf(
				"cliproxy auth: canonical Claude credential saved but legacy credential cleanup failed: %w",
				errDelete,
			)
		}
	}
	return record, savedPath, nil
}

func readExistingCredentialMetadata(authDir string, record *coreauth.Auth) (map[string]any, error) {
	if record == nil || strings.TrimSpace(authDir) == "" {
		return nil, nil
	}
	fileName := strings.TrimSpace(record.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(record.ID)
	}
	if fileName == "" {
		return nil, nil
	}
	path := fileName
	if !filepath.IsAbs(path) {
		path = filepath.Join(authDir, path)
	}
	raw, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("cliproxy auth: read existing credential %s: %w", filepath.Base(path), errRead)
	}
	metadata := make(map[string]any)
	if errJSON := json.Unmarshal(raw, &metadata); errJSON != nil {
		return nil, fmt.Errorf("cliproxy auth: decode existing credential %s: %w", filepath.Base(path), errJSON)
	}
	return metadata, nil
}
