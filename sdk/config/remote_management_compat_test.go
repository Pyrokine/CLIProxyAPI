package config_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestPublishedDisableAutoUpdatePanelBoolField(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	var disabled = cfg.RemoteManagement.DisableAutoUpdatePanel
	if !disabled || cfg.RemoteManagement.IsAutoUpdatePanel() {
		t.Fatal("published bool field did not disable automatic panel installation")
	}
	cfg.RemoteManagement.DisableAutoUpdatePanel = false
	if !cfg.RemoteManagement.IsAutoUpdatePanel() {
		t.Fatal("published false bool field did not enable automatic panel installation")
	}
}
