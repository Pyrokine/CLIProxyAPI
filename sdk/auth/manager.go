package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
		authDir, err = util.ResolveAuthDir(authDir)
		if err != nil {
			return record, "", fmt.Errorf("cliproxy auth: %w", err)
		}
	}
	if strings.EqualFold(strings.TrimSpace(record.Provider), "meta") && authDir != "" {
		fileName, errFind := findExistingMetaCredential(ctx, authDir, record)
		if errFind != nil {
			return record, "", errFind
		}
		if fileName != "" {
			record.ID = fileName
			record.FileName = fileName
		}
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
			if errMerge := coreauth.MergeExistingAuthMetadata(
				record, legacyClaudeCredential.Metadata,
			); errMerge != nil {
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
			if errMerge := coreauth.MergeExistingAuthMetadata(
				record, existingDevinCredential.Metadata,
			); errMerge != nil {
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
			savedPath, err = scopedStore.SaveWithBaseDir(persistCtx, authDir, record)
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
		var errDelete error
		if versionedStore, versioned := m.store.(coreauth.VersionedStore); versioned {
			version := coreauth.PersistedVersion{Path: legacyID}
			digest, errDigest := hex.DecodeString(legacyClaudeCredential.Attributes[coreauth.PersistedSHA256Attribute])
			if errDigest != nil || len(digest) != len(version.SHA256) {
				errDelete = fmt.Errorf("matching Claude legacy credential has no valid persisted version")
			} else {
				copy(version.SHA256[:], digest)
				var deleted bool
				deleted, errDelete = versionedStore.DeleteIfVersion(ctx, legacyID, version)
				if errDelete == nil && !deleted {
					errDelete = fmt.Errorf("matching Claude legacy credential changed before cleanup")
				}
			}
		} else {
			log.Warn("auth store has no conditional deletion; legacy Claude cleanup uses the store Delete contract")
			errDelete = m.store.Delete(ctx, legacyID)
		}
		if errDelete != nil {
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
	metadata, errJSON := coreauth.DecodeCredentialMetadata(raw)
	if errJSON != nil {
		return nil, fmt.Errorf("cliproxy auth: decode existing credential %s: %w", filepath.Base(path), errJSON)
	}
	return metadata, nil
}

func findExistingMetaCredential(ctx context.Context, authDir string, target *coreauth.Auth) (string, error) {
	root, errRoot := filepath.Abs(authDir)
	if errRoot != nil {
		return "", fmt.Errorf("find existing Meta credential: %w", errRoot)
	}
	targetEmail, _ := target.Metadata["email"].(string)
	targetDCA, _ := target.Metadata["dca_token"].(string)
	targetEmail = strings.TrimSpace(targetEmail)
	targetDCA = strings.TrimSpace(targetDCA)
	matched := ""
	errWalk := filepath.WalkDir(
		root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if errContext := ctx.Err(); errContext != nil {
				return errContext
			}
			name := strings.ToLower(entry.Name())
			if entry.IsDir() || !strings.HasPrefix(name, "meta-") || !strings.HasSuffix(name, ".json") {
				return nil
			}
			metadata, errRead := readExistingCredentialMetadata(root, &coreauth.Auth{FileName: path})
			if errRead != nil {
				return errRead
			}
			coreauth.NormalizeCredentialMetadata(metadata)
			provider, _ := metadata["type"].(string)
			if !strings.EqualFold(strings.TrimSpace(provider), "meta") {
				return nil
			}
			kind, _ := metadata["auth_kind"].(string)
			if kind = strings.TrimSpace(kind); kind != "" && !strings.EqualFold(kind, "oauth") {
				return nil
			}
			email, _ := metadata["email"].(string)
			dca, _ := metadata["dca_token"].(string)
			email = strings.TrimSpace(email)
			sameIdentity := targetEmail != "" && email != "" && strings.EqualFold(targetEmail, email)
			if targetEmail == "" || email == "" {
				sameIdentity = targetDCA != "" && strings.TrimSpace(dca) == targetDCA
			}
			if !sameIdentity {
				return nil
			}
			if matched != "" {
				return fmt.Errorf("multiple Meta credentials match the login identity")
			}
			var errRelative error
			matched, errRelative = filepath.Rel(root, path)
			return errRelative
		},
	)
	if errors.Is(errWalk, os.ErrNotExist) {
		return "", nil
	}
	if errWalk != nil {
		return "", fmt.Errorf("find existing Meta credential: %w", errWalk)
	}
	return matched, nil
}
