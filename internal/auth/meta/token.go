package meta

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	authstore "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// MetaTokenStorage persists the OAuth device token separately from its minted API key.
type MetaTokenStorage struct {
	Type         string `json:"type"`
	AuthKind     string `json:"auth_kind"`
	AccessToken  string `json:"access_token"`
	DCAToken     string `json:"dca_token,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Expired      string `json:"expired,omitempty"`
	DCAExpired   string `json:"dca_expired,omitempty"`
	DCAExpiresAt int64  `json:"dca_expires_at,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`

	Metadata map[string]any `json:"-"`
}

var metaTokenFields = map[string]struct{}{
	"type": {}, "auth_kind": {}, "access_token": {}, "dca_token": {}, "api_key": {},
	"token_type": {}, "expires_in": {}, "expired": {}, "dca_expired": {},
	"dca_expires_at": {}, "last_refresh": {}, "base_url": {}, "email": {}, "name": {},
}

func (storage *MetaTokenStorage) CloneForPersistence() coreauth.TokenStorage {
	if storage == nil {
		return nil
	}
	copyStorage := *storage
	return &copyStorage
}

func (storage *MetaTokenStorage) SetMetadata(metadata map[string]any) {
	if storage == nil {
		return
	}
	storage.Metadata = metadata
}

func (storage *MetaTokenStorage) SaveTokenToFile(path string) error {
	if storage == nil {
		return fmt.Errorf("meta token storage is nil")
	}
	storage.Type = "meta"
	storage.AuthKind = "oauth"
	settings := make(map[string]any, len(storage.Metadata))
	for key, value := range storage.Metadata {
		if _, credentialField := metaTokenFields[key]; !credentialField {
			settings[key] = value
		}
	}
	return authstore.SaveTokenJSON(path, storage, settings)
}

func CredentialFileName(email, dcaToken string) string {
	identity := strings.ToLower(strings.TrimSpace(email))
	if identity == "" {
		identity = strings.TrimSpace(dcaToken)
	}
	digest := sha256.Sum256([]byte(identity))
	return "meta-" + hex.EncodeToString(digest[:12]) + ".json"
}

func (auth *MetaAuth) CreateTokenStorage(bundle *MetaAuthBundle) *MetaTokenStorage {
	if bundle == nil || bundle.TokenData == nil || bundle.MintedKey == nil {
		return nil
	}
	apiKey := strings.TrimSpace(bundle.MintedKey.APIKey)
	if apiKey == "" || strings.HasPrefix(strings.ToLower(apiKey), "dca:") {
		return nil
	}
	baseURL := DefaultAPIBaseURL
	if value := strings.TrimSpace(bundle.MintedKey.BaseURL); value != "" {
		baseURL = value
	}
	storage := &MetaTokenStorage{
		Type:         "meta",
		AuthKind:     "oauth",
		AccessToken:  apiKey,
		DCAToken:     bundle.TokenData.AccessToken,
		APIKey:       apiKey,
		TokenType:    bundle.TokenData.TokenType,
		ExpiresIn:    bundle.TokenData.ExpiresIn,
		DCAExpiresAt: bundle.TokenData.ExpiresAt,
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
		BaseURL:      baseURL,
		Email:        bundle.MintedKey.UserEmail,
		Name:         bundle.MintedKey.UserFullName,
	}
	if storage.DCAExpiresAt > 0 {
		storage.DCAExpired = time.Unix(storage.DCAExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return storage
}
