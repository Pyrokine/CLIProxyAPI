package main

import (
	"io"
	"os"
	"strings"
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

func TestNormalizeHomePort(t *testing.T) {
	tests := []struct {
		name string
		port int
		want int
	}{
		{name: "negative defaults", port: -1, want: 8317},
		{name: "zero defaults", port: 0, want: 8317},
		{name: "default preserved", port: 8317, want: 8317},
		{name: "custom preserved", port: 9090, want: 9090},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeHomePort(test.port); got != test.want {
				t.Fatalf("normalizeHomePort(%d) = %d, want %d", test.port, got, test.want)
			}
		})
	}
}

func TestModelCatalogUpdaterPlan(t *testing.T) {
	tests := []struct {
		name            string
		localModel      bool
		homeEnabled     bool
		wantModels      bool
		wantCodexClient bool
	}{
		{name: "normal CPA refreshes both catalogs", wantModels: true, wantCodexClient: true},
		{
			name:            "home mode keeps models local and refreshes Codex templates",
			homeEnabled:     true,
			wantCodexClient: true,
		},
		{name: "local model disables remote catalogs", localModel: true},
		{name: "local model disables remote catalogs under home", localModel: true, homeEnabled: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				gotModels, gotCodex := modelCatalogUpdaterPlan(tt.localModel, tt.homeEnabled)
				if gotModels != tt.wantModels || gotCodex != tt.wantCodexClient {
					t.Fatalf(
						"modelCatalogUpdaterPlan(%v, %v) = (%v, %v), want (%v, %v)",
						tt.localModel,
						tt.homeEnabled,
						gotModels,
						gotCodex,
						tt.wantModels,
						tt.wantCodexClient,
					)
				}
			},
		)
	}
}

func TestShouldCreateExampleAPIKeyLocalManagementPassword(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	if !shouldCreateExampleAPIKeyLocalManagementPassword(cfg, "") {
		t.Fatal("expected local management password when no management credential is configured")
	}
	if shouldCreateExampleAPIKeyLocalManagementPassword(cfg, "existing") {
		t.Fatal("expected existing local password to be preserved")
	}
	cfg.RemoteManagement.SecretKey = "hashed-secret"
	if shouldCreateExampleAPIKeyLocalManagementPassword(cfg, "") {
		t.Fatal("expected remote-management secret-key to disable local password generation")
	}
	cfg.RemoteManagement.SecretKey = ""
	t.Setenv("MANAGEMENT_PASSWORD", "env-secret")
	if shouldCreateExampleAPIKeyLocalManagementPassword(cfg, "") {
		t.Fatal("expected MANAGEMENT_PASSWORD to disable local password generation")
	}
}

func TestWriteExampleAPIKeyLocalManagementPasswordRejectsNonTerminal(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	password := "safe-mode-secret-value"
	if writeExampleAPIKeyLocalManagementPassword(writer, password) {
		t.Fatal("writeExampleAPIKeyLocalManagementPassword accepted a non-terminal output")
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	if strings.Contains(string(output), password) {
		t.Fatalf("non-terminal output contained management password: %q", output)
	}
}

func TestWriteExampleAPIKeyLocalManagementPasswordRejectsNullDevice(t *testing.T) {
	nullDevice, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open null device: %v", err)
	}
	defer func() { _ = nullDevice.Close() }()

	if writeExampleAPIKeyLocalManagementPassword(nullDevice, "safe-mode-secret-value") {
		t.Fatal("writeExampleAPIKeyLocalManagementPassword accepted the null device")
	}
}

func TestShouldEnableExampleAPIKeySafeMode(t *testing.T) {
	cfg := &config.Config{APIKeys: []string{"your-api-key-1"}}
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
			name: "real key", cfg: &config.Config{APIKeys: []string{"real-key"}},
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
				got := shouldEnableExampleAPIKeySafeMode(
					tt.cfg, tt.commandMode, tt.tuiMode, tt.standalone, tt.cloudConfigMissing, tt.homeMode,
				)
				if got != tt.want {
					t.Fatalf("shouldEnableExampleAPIKeySafeMode() = %v, want %v", got, tt.want)
				}
			},
		)
	}
}
