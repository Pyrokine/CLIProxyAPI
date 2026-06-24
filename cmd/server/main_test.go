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

func TestShouldStartExampleAPIKeyWarningServer(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"your-api-key-1"}}}
	tests := []struct {
		name               string
		cfg                *config.Config
		commandMode        bool
		tuiMode            bool
		standalone         bool
		cloudConfigMissing bool
		homeMode           bool
		want               bool
	}{
		{name: "normal server with example key", cfg: cfg, want: true},
		{
			name: "real key", cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"real-key"}}},
			want: false,
		},
		{name: "command mode", cfg: cfg, commandMode: true, want: false},
		{name: "home mode", cfg: cfg, homeMode: true, want: false},
		{name: "cloud missing config", cfg: cfg, cloudConfigMissing: true, want: false},
		{name: "tui client mode", cfg: cfg, tuiMode: true, want: false},
		{name: "tui standalone mode", cfg: cfg, tuiMode: true, standalone: true, want: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := shouldStartExampleAPIKeyWarningServer(
					tt.cfg, tt.commandMode, tt.tuiMode, tt.standalone, tt.cloudConfigMissing, tt.homeMode,
				)
				if got != tt.want {
					t.Fatalf("shouldStartExampleAPIKeyWarningServer() = %v, want %v", got, tt.want)
				}
			},
		)
	}
}
