package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func readReleaseSource(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeReleaseTargetsKeepUnixPluginLoader(t *testing.T) {
	var cfg struct {
		Builds []struct {
			ID     string   `yaml:"id"`
			Env    []string `yaml:"env"`
			Goos   []string `yaml:"goos"`
			Goarch []string `yaml:"goarch"`
			Skip   string   `yaml:"skip"`
		} `yaml:"builds"`
		Release struct {
			Disable bool `yaml:"disable"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(readReleaseSource(t, ".goreleaser.yml"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Release.Disable {
		t.Fatal("native artifact jobs must leave complete release publication to the publisher")
	}
	for _, tc := range []struct {
		os, arch string
		want     []string
	}{
		{"linux", "amd64", []string{"linux/amd64"}},
		{"linux", "arm64", []string{"linux/arm64"}},
		{"darwin", "amd64", []string{"darwin/amd64", "darwin/arm64"}},
		{"darwin", "arm64", []string{"darwin/amd64", "darwin/arm64"}},
		{"windows", "amd64", []string{"windows/amd64", "windows/arm64"}},
	} {
		t.Run(
			tc.os+"/"+tc.arch, func(t *testing.T) {
				var targets []string
				for _, build := range cfg.Builds {
					condition, err := template.New(build.ID).Parse(build.Skip)
					if err != nil {
						t.Fatal(err)
					}
					var result bytes.Buffer
					if err := condition.Execute(
						&result, map[string]any{
							"Runtime": map[string]string{"Goos": tc.os, "Goarch": tc.arch},
						},
					); err != nil {
						t.Fatal(err)
					}
					skip, err := strconv.ParseBool(result.String())
					if err != nil {
						t.Fatal(err)
					}
					if skip {
						continue
					}
					var cgo string
					for _, value := range build.Env {
						if selected, ok := strings.CutPrefix(value, "CGO_ENABLED="); ok {
							cgo = selected
						}
					}
					for _, system := range build.Goos {
						wantCGO := "1"
						if system == "windows" {
							wantCGO = "0"
						}
						if cgo != wantCGO {
							t.Fatalf("%s uses CGO_ENABLED=%s, want %s", build.ID, cgo, wantCGO)
						}
						for _, arch := range build.Goarch {
							targets = append(targets, system+"/"+arch)
						}
					}
				}
				sort.Strings(targets)
				if strings.Join(targets, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("native runner selected targets %v, want %v", targets, tc.want)
				}
			},
		)
	}
	dockerfile := string(readReleaseSource(t, "Dockerfile"))
	if !strings.Contains(dockerfile, "apk add --no-cache gcc musl-dev") ||
		!strings.Contains(dockerfile, "CGO_ENABLED=1 GOOS=linux go build") {
		t.Fatal("Docker build does not provide the native Unix loader compiler")
	}
}

func TestContainerDefaultsMatchPublishedImageAndLocalPorts(t *testing.T) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.cluster.yml"} {
		var cfg struct {
			Services map[string]struct {
				Image string   `yaml:"image"`
				Ports []string `yaml:"ports"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(readReleaseSource(t, name), &cfg); err != nil {
			t.Fatal(err)
		}
		service := cfg.Services["cli-proxy-api"]
		if service.Image != "${CLI_PROXY_IMAGE:-pyrokine/cli-proxy-api:latest}" {
			t.Fatalf("%s uses an unpublished default image: %s", name, service.Image)
		}
		if name == "docker-compose.yml" {
			if len(service.Ports) != 6 {
				t.Fatalf("local Compose ports changed: %v", service.Ports)
			}
			for _, port := range service.Ports {
				if !strings.HasPrefix(port, "127.0.0.1:") {
					t.Fatalf("local Compose exposes a port beyond loopback: %s", port)
				}
			}
		}
	}
	for _, name := range []string{"scripts/prepare-management-panel.sh", "scripts/prepare-management-panel.ps1"} {
		data := string(readReleaseSource(t, name))
		if !strings.Contains(data, "v1.26.0-aug.6") || strings.Contains(data, "v1.23.1-aug.1") {
			t.Fatalf("%s does not select the current management panel by default", name)
		}
	}
	build := string(readReleaseSource(t, "docker-build.sh"))
	if !strings.Contains(build, "./scripts/prepare-management-panel.sh") || strings.Contains(build, "v1.23.1-aug.1") {
		t.Fatal("Docker build bypasses the management panel default")
	}
}

func TestReleaseWorkflowValidatesCompleteArchiveInventory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the release publisher runs on Linux")
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readReleaseSource(t, ".github/workflows/release.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	var recipe string
	for _, step := range workflow.Jobs["publish"].Steps {
		if step.Name == "Validate release inventory and prepare notes" {
			recipe = step.Run
		}
	}
	if recipe == "" {
		t.Fatal("release publisher does not validate its archive inventory")
	}
	for _, scenario := range []string{"complete", "missing target", "damaged archive"} {
		t.Run(
			scenario, func(t *testing.T) {
				root := t.TempDir()
				dist := filepath.Join(root, "dist")
				if err := os.Mkdir(dist, 0o700); err != nil {
					t.Fatal(err)
				}
				var manifest strings.Builder
				var first string
				for _, system := range []string{"linux", "darwin", "windows"} {
					for _, arch := range []string{"amd64", "arm64"} {
						ext := ".tar.gz"
						if system == "windows" {
							ext = ".zip"
						}
						name := "CLIProxyAPI_1.2.3_" + system + "_" + arch + ext
						path := filepath.Join(dist, name)
						data := []byte("synthetic archive " + system + "/" + arch)
						if err := os.WriteFile(path, data, 0o600); err != nil {
							t.Fatal(err)
						}
						if first == "" {
							first = path
						}
						// strings.Builder.Write always returns the full length and a nil error.
						_, _ = fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), name)
					}
				}
				if err := os.WriteFile(
					filepath.Join(dist, "checksums_fixture.txt"), []byte(manifest.String()), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(root, "CHANGELOG.md"), []byte("## v1.2.3\n\nFixture release\n"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "missing target":
					if err := os.Remove(first); err != nil {
						t.Fatal(err)
					}
				case "damaged archive":
					if err := os.WriteFile(first, []byte("changed fixture"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				command := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", recipe)
				command.Dir = root
				command.Env = []string{"PATH=/usr/bin:/bin", "RELEASE_TAG=v1.2.3", "HOME=" + root}
				output, err := command.CombinedOutput()
				if scenario == "complete" {
					if err != nil {
						t.Fatalf("complete release inventory rejected: %v\n%s", err, output)
					}
					return
				}
				if err == nil {
					t.Fatalf("invalid release inventory accepted for %s: %s", scenario, output)
				}
				if scenario == "missing target" && !strings.Contains(string(output), "Incomplete release targets") ||
					scenario == "damaged archive" && !strings.Contains(string(output), "FAILED") {
					t.Fatalf("unexpected rejection for %s: %v\n%s", scenario, err, output)
				}
			},
		)
	}
}
