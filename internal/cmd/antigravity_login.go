package cmd

import (
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// DoAntigravityLogin triggers the OAuth flow for the antigravity provider and saves tokens.
func DoAntigravityLogin(cfg *config.Config, options *LoginOptions) {
	record, savedPath, err := doLogin(cfg, options, "antigravity", nil)
	if err != nil {
		log.Errorf("Antigravity authentication failed: %v", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Antigravity authentication successful!")
}
