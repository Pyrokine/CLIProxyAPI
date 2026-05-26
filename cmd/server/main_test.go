package main

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestCloudDeployHomeConfigIsAvailableWithoutLocalFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Home.Enabled = true
	cfg.Port = 8317

	if !cloudConfigAvailable("/path/that/does/not/exist/config.yaml", cfg) {
		t.Fatal("expected Home config to be available without local config file")
	}
}
