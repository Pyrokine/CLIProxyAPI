package main

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cmd"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// loginFlags groups all login-related command-line flags.
type loginFlags struct {
	vertexImport      string
	projectID         string
	login             bool
	codexLogin        bool
	codexDeviceLogin  bool
	claudeLogin       bool
	qwenLogin         bool
	iflowLogin        bool
	iflowCookie       bool
	antigravityLogin  bool
	kimiLogin         bool
	noBrowser         bool
	oauthCallbackPort int
}

// isLoginMode returns true when any login-related flag is set.
func (f *loginFlags) isLoginMode() bool {
	return f.vertexImport != "" ||
		f.login || f.codexLogin || f.codexDeviceLogin || f.claudeLogin ||
		f.qwenLogin || f.iflowLogin || f.iflowCookie || f.antigravityLogin || f.kimiLogin
}

// runLoginMode dispatches to the appropriate login command.
// Returns true if a login mode was handled, false if none matched.
func runLoginMode(cfg *config.Config, flags *loginFlags) bool {
	if !flags.isLoginMode() {
		return false
	}

	options := &cmd.LoginOptions{
		NoBrowser:    flags.noBrowser,
		CallbackPort: flags.oauthCallbackPort,
	}

	switch {
	case flags.vertexImport != "":
		cmd.DoVertexImport(cfg, flags.vertexImport)
	case flags.login:
		cmd.DoLogin(cfg, flags.projectID, options)
	case flags.antigravityLogin:
		cmd.DoAntigravityLogin(cfg, options)
	case flags.codexLogin:
		cmd.DoCodexLogin(cfg, options)
	case flags.codexDeviceLogin:
		cmd.DoCodexDeviceLogin(cfg, options)
	case flags.claudeLogin:
		cmd.DoClaudeLogin(cfg, options)
	case flags.qwenLogin:
		cmd.DoQwenLogin(cfg, options)
	case flags.iflowLogin:
		cmd.DoIFlowLogin(cfg, options)
	case flags.iflowCookie:
		cmd.DoIFlowCookieAuth(cfg, options)
	case flags.kimiLogin:
		cmd.DoKimiLogin(cfg, options)
	}

	return true
}
