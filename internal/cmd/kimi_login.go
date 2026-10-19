package cmd

import (
	"context"
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoKimiLogin starts the selected Kimi OAuth device flow and saves its credentials.
func DoKimiLogin(cfg *config.Config, options *LoginOptions, provider string) {
	displayName := "Kimi.com"
	switch provider {
	case "kimi":
	case "kimi-ai":
		displayName = "Kimi.ai"
	default:
		log.Errorf("unsupported Kimi login provider: %s", provider)
		return
	}
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), provider, cfg, authOpts)
	if err != nil {
		log.Errorf("%s authentication failed: %v", displayName, err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Printf("%s authentication successful!\n", displayName)
}
