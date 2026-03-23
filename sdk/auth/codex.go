package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/oauthcommon"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CodexAuthenticator implements the OAuth login flow for Codex accounts.
type CodexAuthenticator struct {
	CallbackPort int
}

// NewCodexAuthenticator constructs a Codex authenticator with default settings.
func NewCodexAuthenticator() *CodexAuthenticator {
	return &CodexAuthenticator{CallbackPort: 1455}
}

func (a *CodexAuthenticator) Provider() string {
	return "codex"
}

func (a *CodexAuthenticator) RefreshLead() *time.Duration {
	return new(5 * 24 * time.Hour)
}

func (a *CodexAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (
	*coreauth.Auth,
	error,
) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	if shouldUseCodexDeviceFlow(opts) {
		return a.loginWithDeviceFlow(ctx, cfg, opts)
	}

	callbackPort := a.CallbackPort
	if opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}

	pkceCodes, err := misc.GeneratePKCECodes()
	if err != nil {
		return nil, fmt.Errorf("codex pkce generation failed: %w", err)
	}

	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, fmt.Errorf("codex state generation failed: %w", err)
	}

	oauthServer := oauthcommon.NewOAuthServer(callbackPort, oauthcommon.ServerConfig{
		CallbackPath:       "/auth/callback",
		DefaultPlatformURL: "https://platform.openai.com",
		ProviderName:       "Codex",
	})
	if err = oauthServer.Start(); err != nil {
		if strings.Contains(err.Error(), "already in use") {
			return nil, oauthcommon.NewAuthenticationError(oauthcommon.ErrPortInUse, err)
		}
		return nil, oauthcommon.NewAuthenticationError(oauthcommon.ErrServerStartFailed, err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if stopErr := oauthServer.Stop(stopCtx); stopErr != nil {
			log.Warnf("codex oauth server stop error: %v", stopErr)
		}
	}()

	authSvc := codex.NewAuth(cfg)

	authURL, err := authSvc.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		return nil, fmt.Errorf("codex authorization url generation failed: %w", err)
	}

	openBrowserForAuth("Codex", authURL, callbackPort, opts.NoBrowser)
	fmt.Println("Waiting for Codex authentication callback...")

	result, manualDescription, err := waitForOAuthCallback(oauthServer, "Codex", opts.Prompt)
	if err != nil {
		return nil, err
	}

	if result.Error != "" {
		return nil, oauthcommon.NewOAuthError(result.Error, manualDescription, http.StatusBadRequest)
	}

	if result.State != state {
		return nil, oauthcommon.NewAuthenticationError(oauthcommon.ErrInvalidState, fmt.Errorf("state mismatch"))
	}

	log.Debug("Codex authorization code received; exchanging for tokens")

	authBundle, err := authSvc.ExchangeCodeForTokens(ctx, result.Code, pkceCodes)
	if err != nil {
		return nil, oauthcommon.NewAuthenticationError(oauthcommon.ErrCodeExchangeFailed, err)
	}

	return a.buildAuthRecord(authSvc, authBundle)
}
