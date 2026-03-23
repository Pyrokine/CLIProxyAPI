package cmd

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

const (
	codexLoginModeMetadataKey = "codex_login_mode"
	codexLoginModeDevice      = "device"
)

// DoCodexDeviceLogin triggers the Codex device-code flow while keeping the
// existing codex-login OAuth callback flow intact.
func DoCodexDeviceLogin(cfg *config.Config, options *LoginOptions) {
	_, savedPath, err := doLogin(cfg, options, "codex", map[string]string{
		codexLoginModeMetadataKey: codexLoginModeDevice,
	})
	if err != nil {
		handleOAuthError(err, "Codex device")
		return
	}
	printLoginSuccess(savedPath, "Codex device")
}
