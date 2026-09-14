package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePluginsDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "", want: "plugins"},
		{name: "relative", input: "./state/../plugins", want: "plugins"},
		{name: "home", input: "~/.cli-proxy-api/plugins", want: filepath.Join(homeDir, ".cli-proxy-api", "plugins")},
		{name: "home root", input: "~", want: homeDir},
		{name: "other user", input: "~other/plugins", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got, errResolve := ResolvePluginsDir(tt.input)
				if tt.wantErr {
					if errResolve == nil {
						t.Fatalf("ResolvePluginsDir(%q) error = nil", tt.input)
					}
					return
				}
				if errResolve != nil {
					t.Fatalf("ResolvePluginsDir(%q) error = %v", tt.input, errResolve)
				}
				if got != filepath.Clean(tt.want) {
					t.Fatalf("ResolvePluginsDir(%q) = %q, want %q", tt.input, got, filepath.Clean(tt.want))
				}
			},
		)
	}
}

func TestParseConfigBytesResolvesPluginsDirAndAuthRevision(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	cfg, errParse := ParseConfigBytes([]byte("plugins:\n  enabled: true\n  dir: ~/.cli-proxy-api/plugins\n  auth-revision: 42\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	wantDir := filepath.Join(homeDir, ".cli-proxy-api", "plugins")
	if cfg.Plugins.Dir != wantDir {
		t.Fatalf("Plugins.Dir = %q, want %q", cfg.Plugins.Dir, wantDir)
	}
	if cfg.Plugins.AuthRevision != 42 {
		t.Fatalf("Plugins.AuthRevision = %d, want 42", cfg.Plugins.AuthRevision)
	}
}

func TestParseConfigBytesRejectsUnsupportedUserPluginsDir(t *testing.T) {
	_, errParse := ParseConfigBytes([]byte("plugins:\n  enabled: true\n  dir: ~other/.cli-proxy-api/plugins\n"))
	if errParse == nil {
		t.Fatal("ParseConfigBytes() error = nil")
	}
}

func TestLoadConfigResolvesRelativePluginsDirFromProcessWorkingDirectory(t *testing.T) {
	workspace := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: ./plugin-data\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	t.Chdir(workspace)

	cfg, errLoad := LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if cfg.Plugins.Dir != "plugin-data" {
		t.Fatalf("Plugins.Dir = %q, want plugin-data", cfg.Plugins.Dir)
	}
}
