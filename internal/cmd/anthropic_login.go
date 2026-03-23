package cmd

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// DoClaudeLogin triggers the Claude OAuth flow through the shared authentication manager.
func DoClaudeLogin(cfg *config.Config, options *LoginOptions) {
	_, savedPath, err := doLogin(cfg, options, "claude", nil)
	if err != nil {
		handleOAuthError(err, "Claude")
		return
	}
	printLoginSuccess(savedPath, "Claude")
}
