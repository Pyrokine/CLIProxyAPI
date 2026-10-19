package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigurationCheckCommand(t *testing.T) {
	if os.Getenv("CPA_CONFIG_CHECK_HELPER") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		os.Exit(0)
	}

	const valid = "host: 127.0.0.1\nport: 48317\napi-keys: [acceptance-placeholder-key]\nplugins: {enabled: false, dir: plugins}\n"
	tests := []struct {
		name        string
		data        string
		lastGood    bool
		pluginFile  bool
		missing     bool
		cloud       bool
		wantCode    int
		wantMessage string
	}{
		{name: "valid", data: valid, wantMessage: "configuration is valid"},
		{
			name: "plain management key", data: valid + "remote-management: {secret-key: acceptance-placeholder-secret}\n",
			wantMessage: "configuration is valid",
		},
		{
			name: "invalid port", data: strings.ReplaceAll(valid, "48317", "70000"),
			wantCode: 1, wantMessage: "port 70000 is out of range",
		},
		{name: "invalid YAML", data: "port: [\n", wantCode: 1, wantMessage: "failed to load config"},
		{
			name: "invalid YAML with last-good", data: "port: [\n", lastGood: true,
			wantCode: 1, wantMessage: "failed to load config",
		},
		{
			name: "invalid Meta entry", data: valid + "meta-api-key: [{api-key: 'dca:acceptance-placeholder'}]\n",
			wantCode: 1, wantMessage: "meta-api-key[0].api-key",
		},
		{name: "missing file", missing: true, wantCode: 1, wantMessage: "failed to load config"},
		{
			name: "plugin bootstrap failure", data: valid, pluginFile: true,
			wantCode: 1, wantMessage: "failed to apply bootstrap plugin config",
		},
		{name: "cloud mode", data: valid, cloud: true, wantMessage: "configuration is valid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			backup := config.LastGoodPath(path)
			if tt.lastGood {
				if err := os.WriteFile(backup, []byte(valid), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.pluginFile {
				if err := os.WriteFile(filepath.Join(dir, "plugins"), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("HOME_JWT=invalid-placeholder\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(
				ctx, os.Args[0], "-test.run=^TestConfigurationCheckCommand$", "--",
				"-config", path, "-local-model", "-t",
			)
			command.Dir = dir
			command.Env = []string{"CPA_CONFIG_CHECK_HELPER=1", "HOME=" + dir, "PATH=/usr/bin:/bin", "GOMAXPROCS=2"}
			if tt.cloud {
				command.Env = append(command.Env, "DEPLOY=cloud")
			}
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("configuration check timed out: %s", output)
			}
			code := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("execute configuration check: %v", err)
				}
				code = exitError.ExitCode()
			}
			if code != tt.wantCode || !strings.Contains(string(output), tt.wantMessage) {
				t.Fatalf("configuration check exit=%d, want=%d, output=%s", code, tt.wantCode, output)
			}
			data, err := os.ReadFile(path)
			if tt.missing {
				if !os.IsNotExist(err) {
					t.Fatalf("missing configuration was created: %v", err)
				}
			} else if err != nil || string(data) != tt.data {
				t.Fatalf("configuration check changed source file: %v", err)
			}
			data, err = os.ReadFile(backup)
			if tt.lastGood {
				if err != nil || string(data) != valid {
					t.Fatalf("configuration check changed last-good backup: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("configuration check created last-good backup: %v", err)
			}
		})
	}
}

func TestKimiDomainsEnterLoginMode(t *testing.T) {
	for _, flags := range []loginFlags{{kimiLogin: true}, {kimiAILogin: true}} {
		if !flags.isLoginMode() {
			t.Fatalf("Kimi login flags did not enable command mode: %+v", flags)
		}
	}
}

func TestMetaEntersLoginMode(t *testing.T) {
	if !(&loginFlags{metaLogin: true}).isLoginMode() {
		t.Fatal("Meta login flag did not enable command mode")
	}
}

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
		wantDevin       bool
	}{
		{name: "normal CPA refreshes all catalogs", wantModels: true, wantCodexClient: true, wantDevin: true},
		{
			name:            "home mode keeps models local and refreshes edge catalogs",
			homeEnabled:     true,
			wantCodexClient: true,
			wantDevin:       true,
		},
		{name: "local model disables remote catalogs", localModel: true},
		{name: "local model disables remote catalogs under home", localModel: true, homeEnabled: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				gotModels, gotCodex, gotDevin := modelCatalogUpdaterPlan(tt.localModel, tt.homeEnabled)
				if gotModels != tt.wantModels || gotCodex != tt.wantCodexClient || gotDevin != tt.wantDevin {
					t.Fatalf(
						"modelCatalogUpdaterPlan(%v, %v) = (%v, %v, %v), want (%v, %v, %v)",
						tt.localModel,
						tt.homeEnabled,
						gotModels,
						gotCodex,
						gotDevin,
						tt.wantModels,
						tt.wantCodexClient,
						tt.wantDevin,
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
