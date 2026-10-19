package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/browser"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// MetaAuthenticator completes the Meta Muse device authorization flow.
type MetaAuthenticator struct{}

func NewMetaAuthenticator() Authenticator             { return &MetaAuthenticator{} }
func (MetaAuthenticator) Provider() string            { return "meta" }
func (MetaAuthenticator) RefreshLead() *time.Duration { return nil }

func (MetaAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil || cfg == nil {
		return nil, fmt.Errorf("meta authentication requires a context and configuration")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	authSvc := metaauth.NewMetaAuth(cfg)
	device, err := authSvc.StartDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}
	verificationURL := strings.TrimSpace(device.VerificationURIComplete)
	if verificationURL == "" {
		verificationURL = strings.TrimSpace(device.VerificationURI)
	}
	fmt.Printf("Authorize Meta Muse at %s", verificationURL)
	if device.UserCode != "" {
		fmt.Printf(" using code %s", device.UserCode)
	}
	fmt.Println()
	if !opts.NoBrowser && browser.IsAvailable() {
		if err := browser.OpenURL(verificationURL); err != nil {
			log.WithError(err).Warn("could not open Meta device authorization page")
		}
	}
	bundle, err := authSvc.WaitForAuthorization(ctx, device)
	if err != nil {
		return nil, err
	}
	return MetaRecordFromBundle(authSvc, bundle)
}

// MetaRecordFromBundle converts an authorized device flow into a persistable auth record.
func MetaRecordFromBundle(authSvc *metaauth.MetaAuth, bundle *metaauth.MetaAuthBundle) (*coreauth.Auth, error) {
	if authSvc == nil {
		return nil, fmt.Errorf("meta authenticator is missing")
	}
	storage := authSvc.CreateTokenStorage(bundle)
	if storage == nil {
		return nil, fmt.Errorf("meta OAuth authorization did not provide a usable API key")
	}
	fileName := metaauth.CredentialFileName(storage.Email, storage.DCAToken)
	metadata := map[string]any{
		"type":         "meta",
		"auth_kind":    "oauth",
		"access_token": storage.AccessToken,
		"api_key":      storage.APIKey,
		"dca_token":    storage.DCAToken,
		"base_url":     storage.BaseURL,
		"last_refresh": storage.LastRefresh,
	}
	if storage.DCAExpiresAt > 0 {
		metadata["dca_expires_at"] = storage.DCAExpiresAt
		metadata["dca_expired"] = storage.DCAExpired
	}
	if storage.Email != "" {
		metadata["email"] = storage.Email
	}
	if bundle.MintedKey != nil {
		metadata["subs_tier_name"] = bundle.MintedKey.SubsTierName
		metadata["subs_tier_id"] = bundle.MintedKey.SubsTierID
		metadata["is_subs_active"] = bundle.MintedKey.IsSubsActive
		metadata["has_payment_method"] = bundle.MintedKey.HasPaymentMethod
	}
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "meta",
		FileName: fileName,
		Label:    "Meta",
		Storage:  storage,
		Metadata: metadata,
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  storage.BaseURL,
			"api_key":   storage.APIKey,
			"dca_token": storage.DCAToken,
		},
	}, nil
}
