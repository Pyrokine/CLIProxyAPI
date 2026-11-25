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

func TestNativeReleaseArchiveNamesMatchUpdaterContract(t *testing.T) {
	var cfg struct {
		ProjectName string `yaml:"project_name"`
		Archives    []struct {
			NameTemplate    string   `yaml:"name_template"`
			Formats         []string `yaml:"formats"`
			FormatOverrides []struct {
				Goos    string   `yaml:"goos"`
				Formats []string `yaml:"formats"`
			} `yaml:"format_overrides"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(readReleaseSource(t, ".goreleaser.yml"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectName != "CLIProxyAPI" {
		t.Errorf("release project_name = %q, want CLIProxyAPI", cfg.ProjectName)
	}
	if len(cfg.Archives) != 1 {
		t.Fatalf("release defines %d archive sets, want 1", len(cfg.Archives))
	}
	archive := cfg.Archives[0]
	if archive.NameTemplate != "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}" {
		t.Errorf("archive name_template = %q, want the updater's versioned archive name", archive.NameTemplate)
	}
	if strings.Join(archive.Formats, ",") != "tar.gz" || len(archive.FormatOverrides) != 1 ||
		archive.FormatOverrides[0].Goos != "windows" || strings.Join(archive.FormatOverrides[0].Formats, ",") != "zip" {
		t.Fatalf("archive formats do not match the updater: %+v", archive)
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

func TestReleaseWorkflowsKeepPreflightAndPublicationGates(t *testing.T) {
	toolVersions := make(map[string]string)
	for _, tc := range []struct {
		file, event, tag string
		needs            map[string][]string
	}{
		{
			"release.yaml", "push", "*", map[string][]string{
				"deep-quality": {"preflight"}, "native-artifacts": {"preflight"},
				"publish": {"deep-quality", "native-artifacts"},
			},
		},
		{
			"docker-image.yml", "push", "v*", map[string][]string{
				"deep-quality": {"preflight"}, "docker_amd64": {"deep-quality"}, "docker_arm64": {"deep-quality"},
				"docker_manifest": {"docker_amd64", "docker_arm64"},
			},
		},
		{"pr-test-build.yml", "pull_request", "", map[string][]string{"build": {"preflight"}}},
		{"release-preflight.yml", "workflow_call", "", nil},
		{"deep-quality.yml", "workflow_call", "", nil},
	} {
		t.Run(
			tc.file, func(t *testing.T) {
				var workflow struct {
					On   map[string]yaml.Node `yaml:"on"`
					Jobs map[string]struct {
						Needs yaml.Node `yaml:"needs"`
						Uses  string    `yaml:"uses"`
						Steps []struct {
							Run  string            `yaml:"run"`
							Uses string            `yaml:"uses"`
							With map[string]string `yaml:"with"`
						} `yaml:"steps"`
					} `yaml:"jobs"`
				}
				if err := yaml.Unmarshal(readReleaseSource(t, ".github/workflows/"+tc.file), &workflow); err != nil {
					t.Fatal(err)
				}
				trigger, ok := workflow.On[tc.event]
				if !ok {
					t.Fatalf("workflow lost its %s entry point", tc.event)
				}
				if tc.tag != "" {
					var push struct {
						Tags []string `yaml:"tags"`
					}
					if err := trigger.Decode(&push); err != nil || strings.Join(push.Tags, ",") != tc.tag {
						t.Fatalf("tag trigger = %v, error %v; want %s", push.Tags, err, tc.tag)
					}
				}
				for id, want := range tc.needs {
					job, ok := workflow.Jobs[id]
					if !ok {
						t.Fatalf("missing job %s", id)
					}
					var got []string
					if job.Needs.Kind == yaml.ScalarNode {
						got = []string{job.Needs.Value}
					} else if err := job.Needs.Decode(&got); err != nil {
						t.Fatal(err)
					}
					sort.Strings(got)
					sort.Strings(want)
					if strings.Join(got, ",") != strings.Join(want, ",") {
						t.Errorf("%s needs %v, want %v", id, got, want)
					}
				}
				if len(tc.needs) != 0 && workflow.Jobs["preflight"].Uses != "./.github/workflows/release-preflight.yml" {
					t.Error("workflow bypasses the shared release preflight")
				}
				var contractCheck, configCheck bool
				for _, job := range workflow.Jobs {
					for _, step := range job.Steps {
						contractCheck = contractCheck || step.Run == "go -C cmd/server test -count=1 release_contract_test.go"
						if strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
							toolVersions[tc.file] = step.With["version"]
							configCheck = configCheck || step.With["args"] == "check"
						}
					}
				}
				if tc.file == "release-preflight.yml" && (!contractCheck || !configCheck) {
					t.Error("preflight must check standalone release contracts and the actual GoReleaser configuration")
				}
				if tc.file == "deep-quality.yml" {
					for _, event := range []string{"workflow_dispatch", "schedule"} {
						if _, ok := workflow.On[event]; !ok {
							t.Errorf("quality workflow lost its %s entry point", event)
						}
					}
				}
			},
		)
	}
	if version := toolVersions["release-preflight.yml"]; version == "" || version != toolVersions["release.yaml"] {
		t.Fatalf(
			"GoReleaser preflight version %q differs from native release version %q",
			version, toolVersions["release.yaml"],
		)
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
	for _, scenario := range []struct {
		name, wantError string
	}{
		{"complete", ""},
		{"prerelease", ""},
		{"unprefixed tag", ""},
		{"wrong prefix", "Incomplete release targets"},
		{"wrong version", "Incomplete release targets"},
		{"missing target", "Incomplete release targets"},
		{"wrong format", "Incomplete release targets"},
		{"extra archive", "Incomplete release targets"},
		{"duplicate checksum", "Invalid or duplicate checksum entry"},
		{"invalid checksum", "Invalid or duplicate checksum entry"},
		{"checksum path", "Invalid or duplicate checksum entry"},
		{"missing checksum", "Release archives and checksums differ"},
		{"extra checksum", "Release archives and checksums differ"},
		{"damaged archive", "FAILED"},
		{"missing notes", "CHANGELOG.md has no entry"},
		{"notes for different prerelease", "CHANGELOG.md has no entry"},
		{"notes stop at next release", ""},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				root := t.TempDir()
				dist := filepath.Join(root, "dist")
				if err := os.Mkdir(dist, 0o700); err != nil {
					t.Fatal(err)
				}
				tag, version, prefix := "v1.2.3", "1.2.3", "CLIProxyAPI"
				switch scenario.name {
				case "prerelease":
					tag, version = "v1.2.3-aug.6", "1.2.3-aug.6"
				case "unprefixed tag":
					tag = "1.2.3"
				case "wrong prefix":
					prefix = "v6"
				case "wrong version":
					version = "1.2.4"
				}
				var manifest strings.Builder
				var first string
				for _, system := range []string{"linux", "darwin", "windows"} {
					for _, arch := range []string{"amd64", "arm64"} {
						ext := ".tar.gz"
						if system == "windows" || scenario.name == "wrong format" && system == "linux" {
							ext = ".zip"
						}
						name := prefix + "_" + version + "_" + system + "_" + arch + ext
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
				checksums := manifest.String()
				switch scenario.name {
				case "duplicate checksum":
					checksums += checksums
				case "invalid checksum":
					checksums = "invalid" + checksums[64:]
				case "checksum path":
					checksums = strings.Replace(checksums, filepath.Base(first), "../"+filepath.Base(first), 1)
				case "missing checksum":
					_, checksums, _ = strings.Cut(checksums, "\n")
				case "extra checksum":
					checksums += fmt.Sprintf("%x  unexpected.tar.gz\n", sha256.Sum256(nil))
				}
				if err := os.WriteFile(
					filepath.Join(dist, "checksums_fixture.txt"), []byte(checksums), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				notes := "## " + tag + "\n\nFixture release\n"
				changelog := notes
				switch scenario.name {
				case "missing notes":
					changelog = "## v1.2.4\n\nOther release\n"
				case "notes for different prerelease":
					changelog = "## v1.2.3-aug.6\n\nOther release\n"
				case "notes stop at next release":
					changelog += "\n## 1.2.2\n\nPrevious release\n"
				}
				if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o600); err != nil {
					t.Fatal(err)
				}
				switch scenario.name {
				case "missing target":
					if err := os.Remove(first); err != nil {
						t.Fatal(err)
					}
				case "damaged archive":
					if err := os.WriteFile(first, []byte("changed fixture"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "extra archive":
					if err := os.WriteFile(filepath.Join(dist, "extra_linux_amd64.tar.gz"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				command := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", recipe)
				command.Dir = root
				command.Env = []string{"PATH=/usr/bin:/bin", "RELEASE_TAG=" + tag, "HOME=" + root}
				output, err := command.CombinedOutput()
				if scenario.wantError == "" {
					if err != nil {
						t.Fatalf("complete release inventory rejected: %v\n%s", err, output)
					}
					lines := strings.Split(strings.TrimSpace(checksums), "\n")
					sort.Slice(
						lines, func(i, j int) bool { return strings.Fields(lines[i])[1] < strings.Fields(lines[j])[1] },
					)
					for name, want := range map[string]string{
						"dist/checksums.txt": strings.Join(lines, "\n") + "\n", "release-notes.md": notes,
					} {
						got, err := os.ReadFile(filepath.Join(root, name))
						if err != nil || string(got) != want {
							t.Fatalf("%s = %q, error %v; want %q", name, got, err, want)
						}
					}
					return
				}
				if err == nil || !strings.Contains(string(output), scenario.wantError) {
					t.Fatalf("release inventory rejection = %v\n%s; want %q", err, output, scenario.wantError)
				}
			},
		)
	}
}
