package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/oauthcommon"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// LoginOptions contains options for the login processes.
// It provides configuration for authentication flows including browser behavior
// and interactive prompting capabilities.
type LoginOptions struct {
	// NoBrowser indicates whether to skip opening the browser automatically.
	NoBrowser bool

	// CallbackPort overrides the local OAuth callback port when set (>0).
	CallbackPort int

	// Prompt allows the caller to provide interactive input when needed.
	Prompt func(prompt string) (string, error)
}

// doLogin performs the common setup for all OAuth login flows and calls manager.Login.
func doLogin(
	cfg *config.Config,
	options *LoginOptions,
	provider string,
	metadata map[string]string,
) (*cliproxyauth.Auth, string, error) {
	if options == nil {
		options = &LoginOptions{}
	}
	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}
	manager := newAuthManager()
	if metadata == nil {
		metadata = map[string]string{}
	}
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		CallbackPort: options.CallbackPort,
		Metadata:     metadata,
		Prompt:       promptFn,
	}
	return manager.Login(context.Background(), provider, cfg, authOpts)
}

// handleOAuthError handles authentication errors with oauthcommon unwrapping.
func handleOAuthError(err error, providerLabel string) {
	if authErr, ok := errors.AsType[*oauthcommon.AuthenticationError](err); ok {
		log.Error(oauthcommon.GetUserFriendlyMessage(authErr))
		if authErr.Type == oauthcommon.ErrPortInUse.Type {
			os.Exit(oauthcommon.ErrPortInUse.Code)
		}
		return
	}
	fmt.Printf("%s authentication failed: %v\n", providerLabel, err)
}

// printLoginSuccess prints the standard success messages after a login completes.
func printLoginSuccess(savedPath, providerLabel string) {
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	fmt.Printf("%s authentication successful!\n", providerLabel)
}

// DoCodexLogin triggers the Codex OAuth flow through the shared authentication manager.
func DoCodexLogin(cfg *config.Config, options *LoginOptions) {
	_, savedPath, err := doLogin(cfg, options, "codex", nil)
	if err != nil {
		handleOAuthError(err, "Codex")
		return
	}
	printLoginSuccess(savedPath, "Codex")
}
