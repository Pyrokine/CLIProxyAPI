package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/browser"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// kimiRefreshLead is the duration before token expiry when refresh should occur.
var kimiRefreshLead = 5 * time.Minute

// KimiAuthenticator implements the OAuth device flow for Kimi Code and Kimi.ai.
type KimiAuthenticator struct {
	domain string
}

// NewKimiAuthenticator constructs a Kimi Code authenticator.
func NewKimiAuthenticator() Authenticator {
	return &KimiAuthenticator{domain: kimi.KimiDefaultDomain}
}

// NewKimiAIAuthenticator constructs a Kimi.ai authenticator.
func NewKimiAIAuthenticator() Authenticator {
	return &KimiAuthenticator{domain: kimi.KimiAIDomain}
}

// Provider returns the provider key for the selected Kimi service.
func (a KimiAuthenticator) Provider() string {
	if kimi.IsKimiAIDomain(a.domain) {
		return "kimi-ai"
	}
	return "kimi"
}

// RefreshLead returns the duration before token expiry when refresh should occur.
// Kimi tokens expire and need to be refreshed before expiry.
func (KimiAuthenticator) RefreshLead() *time.Duration {
	return &kimiRefreshLead
}

// Login initiates the Kimi device flow authentication.
func (a KimiAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	domain := kimi.KimiDefaultDomain
	displayName := "Kimi"
	if kimi.IsKimiAIDomain(a.domain) {
		domain = kimi.KimiAIDomain
		displayName = "Kimi.ai"
	}
	authSvc := kimi.NewAuthWithDomain(cfg, domain)

	// Start the device flow
	fmt.Printf("Starting %s authentication...\n", displayName)
	deviceCode, err := authSvc.StartDeviceFlow(ctx)
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to start device flow: %w", err)
	}

	// Display the verification URL
	verificationURL := deviceCode.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = deviceCode.VerificationURI
	}

	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", verificationURL)
	if deviceCode.UserCode != "" {
		fmt.Printf("User code: %s\n\n", deviceCode.UserCode)
	}

	// Try to open the browser automatically
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(verificationURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			} else {
				fmt.Println("Browser opened automatically.")
			}
		}
	}

	fmt.Println("Waiting for authorization...")
	if deviceCode.ExpiresIn > 0 {
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", deviceCode.ExpiresIn)
	}

	// Wait for user authorization
	authBundle, err := authSvc.WaitForAuthorization(ctx, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("kimi: %w", err)
	}

	record, err := KimiRecordFromBundle(authSvc, authBundle)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\n%s authentication successful!\n", displayName)
	return record, nil
}

// KimiRecordFromBundle creates a persistable credential for Kimi Code or Kimi.ai.
func KimiRecordFromBundle(authSvc *kimi.Auth, bundle *kimi.AuthBundle) (*coreauth.Auth, error) {
	if authSvc == nil || bundle == nil || bundle.TokenData == nil || strings.TrimSpace(bundle.TokenData.AccessToken) == "" {
		return nil, fmt.Errorf("kimi: device authorization did not yield an access token")
	}
	storage := authSvc.CreateTokenStorage(bundle)
	domain := storage.Domain
	provider := "kimi"
	displayName := "Kimi"
	filePrefix := "kimi"
	if kimi.IsKimiAIDomain(domain) {
		provider = "kimi-ai"
		displayName = "Kimi.ai"
		filePrefix = "kimi-ai"
	}
	fileName := fmt.Sprintf("%s-%d.json", filePrefix, time.Now().UnixMilli())
	return &coreauth.Auth{
		ID:       fileName,
		Provider: provider,
		FileName: fileName,
		Label:    displayName + " User",
		Storage:  storage,
		Metadata: kimi.BuildMetadataForDomain(bundle, domain),
		Attributes: map[string]string{
			"domain":   domain,
			"base_url": kimi.ResolveKimiAPIBaseURL(domain),
		},
	}, nil
}
