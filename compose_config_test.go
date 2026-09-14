package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type composeConfig struct {
	Services map[string]struct {
		Volumes []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"volumes"`
	} `json:"services"`
}

func TestDockerComposePluginStorage(t *testing.T) {
	if _, errLookPath := exec.LookPath("docker"); errLookPath != nil {
		t.Skip("docker is unavailable")
	}
	root, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("Getwd() error = %v", errGetwd)
	}
	tests := []struct {
		name            string
		file            string
		customPluginDir string
		wantTargets     []string
	}{
		{
			name: "standalone defaults",
			file: "docker-compose.yml",
			wantTargets: []string{
				"/CLIProxyAPI/config", "/root/.cli-proxy-api", "/CLIProxyAPI/logs", "/CLIProxyAPI/plugins",
			},
		},
		{
			name:            "standalone custom plugin source",
			file:            "docker-compose.yml",
			customPluginDir: filepath.Join(t.TempDir(), "standalone-plugins"),
			wantTargets: []string{
				"/CLIProxyAPI/config", "/root/.cli-proxy-api", "/CLIProxyAPI/logs", "/CLIProxyAPI/plugins",
			},
		},
		{
			name:        "cluster defaults",
			file:        "docker-compose.cluster.yml",
			wantTargets: []string{"/root/.cli-proxy-api", "/CLIProxyAPI/logs", "/CLIProxyAPI/plugins"},
		},
		{
			name:            "cluster custom plugin source",
			file:            "docker-compose.cluster.yml",
			customPluginDir: filepath.Join(t.TempDir(), "cluster-plugins"),
			wantTargets:     []string{"/root/.cli-proxy-api", "/CLIProxyAPI/logs", "/CLIProxyAPI/plugins"},
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				cmd := exec.Command("docker", "compose", "-f", tt.file, "config", "--format", "json")
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "HOME_JWT=test-token")
				if tt.customPluginDir != "" {
					cmd.Env = append(cmd.Env, "CLI_PROXY_PLUGIN_PATH="+tt.customPluginDir)
				}
				raw, errRun := cmd.CombinedOutput()
				if errRun != nil {
					t.Fatalf("docker compose config error = %v\n%s", errRun, raw)
				}
				var cfg composeConfig
				if errDecode := json.Unmarshal(raw, &cfg); errDecode != nil {
					t.Fatalf("Unmarshal() error = %v\n%s", errDecode, raw)
				}
				service, okService := cfg.Services["cli-proxy-api"]
				if !okService {
					t.Fatalf("cli-proxy-api service missing: %#v", cfg.Services)
				}
				mounts := make(map[string]string, len(service.Volumes))
				for _, volume := range service.Volumes {
					mounts[volume.Target] = volume.Source
				}
				for _, target := range tt.wantTargets {
					if mounts[target] == "" {
						t.Fatalf("mount target %s missing: %#v", target, mounts)
					}
				}
				if tt.customPluginDir != "" && mounts["/CLIProxyAPI/plugins"] != tt.customPluginDir {
					t.Fatalf("plugin source = %q, want %q", mounts["/CLIProxyAPI/plugins"], tt.customPluginDir)
				}
			},
		)
	}
}
