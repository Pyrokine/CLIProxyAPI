package cliproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

func homeBatchArchive(t *testing.T, name, content string) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func homeBatchConfig(t *testing.T, root string, archives map[string][]byte) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true, Dir: root, Configs: map[string]config.PluginInstanceConfig{},
		},
	}
	for id, archive := range archives {
		digest := sha256.Sum256(archive)
		raw := fmt.Sprintf(
			"enabled: true\nstore:\n  schema-version: 2\n  id: %s\n  version: 0.2.0"+
				"\n  install:\n    type: direct\n    artifacts:\n      - goos: %s\n    "+
				"    goarch: %s\n        url: https://1.1.1.1/%s.zip\n        sha256: %"+
				"s\n",
			id, runtime.GOOS, runtime.GOARCH, id, hex.EncodeToString(digest[:]),
		)
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(raw), &node); err != nil {
			t.Fatal(err)
		}
		var item config.PluginInstanceConfig
		if err := node.Content[0].Decode(&item); err != nil {
			t.Fatal(err)
		}
		cfg.Plugins.Configs[id] = item
	}
	return cfg
}

func homeBatchExtension() string {
	switch runtime.GOOS {
	case "windows":
		return ".dll"
	case "darwin":
		return ".dylib"
	default:
		return ".so"
	}
}

func homeBatchTransport(t *testing.T, archives map[string][]byte, before func(string)) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = homeArtifactTransport(
		func(request *http.Request) (*http.Response, error) {
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/"), ".zip")
			if before != nil {
				before(id)
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(archives[id])),
				Request: request,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func TestHomePluginFailedBatchRetriesWithoutOtherConfigApplication(t *testing.T) {
	root := t.TempDir()
	extension := homeBatchExtension()
	archives := map[string][]byte{
		"alpha": homeBatchArchive(t, "alpha"+extension, "new-alpha"),
		"beta":  homeBatchArchive(t, "unrelated.txt", "invalid-second-plugin"),
	}
	homeBatchTransport(t, archives, nil)
	base := &config.Config{
		Home:    config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"},
		Plugins: config.PluginsConfig{Enabled: true, Dir: root},
	}
	service := &Service{cfg: base}
	remote := homeBatchConfig(t, root, archives)
	if err := service.applyHomeOverlayWithClient(t.Context(), remote, nil); err == nil || !strings.Contains(
		err.Error(), "failed to sync home plugins",
	) {
		t.Fatalf("second install failure was lost: %v", err)
	}
	if service.cfg != base {
		t.Fatal("failed batch published a new runtime configuration")
	}
	if _, err := os.Stat(
		filepath.Join(
			root, runtime.GOOS, runtime.GOARCH, "alpha-v0.2.0"+extension,
		),
	); !os.IsNotExist(err) {
		t.Fatalf("failed batch retained its successful first artifact: %v", err)
	}
	pending, err := sdkpluginstore.PendingInstallPluginIDs(root)
	if err != nil || len(pending) != 0 {
		t.Fatalf("failed batch left a pending transaction: %v, %v", pending, err)
	}
	archives["beta"] = homeBatchArchive(t, "beta"+extension, "new-beta")
	remote = homeBatchConfig(t, root, archives)
	err = service.applyHomeOverlayWithClient(t.Context(), remote, nil)
	if err == nil || !strings.Contains(err.Error(), "home client is unavailable") {
		t.Fatalf("retry did not reach post-publication reporting: %v", err)
	}
	if service.cfg == base {
		t.Fatal("retry did not publish the corrected Home configuration")
	}
	for _, id := range []string{"alpha", "beta"} {
		data, err := os.ReadFile(filepath.Join(root, runtime.GOOS, runtime.GOARCH, id+"-v0.2.0"+extension))
		if err != nil || string(data) != "new-"+id {
			t.Fatalf("retry did not install %s: %q, %v", id, data, err)
		}
	}
}

func TestHomePluginPendingRollbackUsesPublishedConfigAndRetries(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		sameRoot       bool
		committedAlias bool
	}{
		{"same-root", true, false},
		{"different-root", false, false},
		{"committed-root-alias", true, true},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				root := t.TempDir()
				extension := homeBatchExtension()
				archives := map[string][]byte{
					"alpha": homeBatchArchive(t, "alpha"+extension, "new-alpha"),
					"beta":  homeBatchArchive(t, "beta"+extension, "new-beta"),
				}
				remote := homeBatchConfig(t, root, archives)
				base := &config.Config{
					Home:    config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"},
					Plugins: config.PluginsConfig{Enabled: true, Dir: root},
				}
				if !testCase.sameRoot {
					base.Plugins.Dir = t.TempDir()
					base.Plugins.Configs = remote.Plugins.Configs
				}
				expectedBeforePreparation := "old-alpha"
				if testCase.committedAlias {
					alias := filepath.Join(t.TempDir(), "plugin-root")
					if err := os.Symlink(root, alias); err != nil {
						t.Skipf("directory aliases unavailable: %v", err)
					}
					base.Plugins.Dir = alias
					base.Plugins.Configs = remote.Plugins.Configs
					expectedBeforePreparation = "new-alpha"
				}
				service := &Service{cfg: base}
				directory := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(directory, "alpha-v0.2.0"+extension)
				if err := os.WriteFile(target, []byte("old-alpha"), 0o600); err != nil {
					t.Fatal(err)
				}
				rootLock, err := sdkpluginstore.LockRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte("new-alpha"))
				transaction, err := sdkpluginstore.PrepareInstallTransactionLocked(
					root, "alpha", target, "0.2.0", hex.EncodeToString(digest[:]), true, rootLock,
				)
				if err != nil {
					rootLock.Unlock()
					t.Fatal(err)
				}
				fingerprint, err := config.PluginInstanceConfigFingerprint(remote.Plugins.Configs["alpha"])
				if err != nil {
					rootLock.Unlock()
					t.Fatal(err)
				}
				if err := transaction.SetExpectedConfigFingerprint(fingerprint); err != nil {
					rootLock.Unlock()
					t.Fatal(err)
				}
				staged := filepath.Join(directory, "replacement.tmp")
				if err := os.WriteFile(staged, []byte("external-change"), 0o600); err != nil {
					rootLock.Unlock()
					t.Fatal(err)
				}
				if err := os.Rename(staged, target); err != nil {
					rootLock.Unlock()
					t.Fatal(err)
				}
				rootLock.Unlock()
				requests := 0
				sawPreviousArtifact := false
				homeBatchTransport(
					t, archives, func(id string) {
						requests++
						if id == "alpha" {
							data, err := os.ReadFile(target)
							sawPreviousArtifact = err == nil && string(data) == expectedBeforePreparation
						}
					},
				)
				err = service.applyHomeOverlayWithClient(t.Context(), remote, nil)
				if err == nil || !strings.Contains(err.Error(), "recover published state") {
					t.Fatalf("pending rollback conflict was acknowledged: %v", err)
				}
				if requests != 0 || service.cfg != base {
					t.Fatal("failed recovery proceeded to download or runtime publication")
				}
				if err := os.WriteFile(target, []byte("new-alpha"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = service.applyHomeOverlayWithClient(t.Context(), remote, nil)
				if err == nil || !strings.Contains(err.Error(), "home client is unavailable") {
					t.Fatalf("retry did not reach post-publication reporting: %v", err)
				}
				if service.cfg == base || requests != 2 || !sawPreviousArtifact {
					t.Fatalf(
						"retry did not recover against published state: requests=%d previous=%t", requests,
						sawPreviousArtifact,
					)
				}
			},
		)
	}
}

func TestHomePluginFailedBatchRestoresPublishedRuntime(t *testing.T) {
	for _, restoreFailure := range []bool{false, true} {
		t.Run(
			fmt.Sprint(restoreFailure), func(t *testing.T) {
				oldRoot := filepath.Join(t.TempDir(), "published-plugins")
				if err := os.MkdirAll(oldRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				base := &config.Config{
					Home:    config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"},
					Plugins: config.PluginsConfig{Dir: oldRoot},
				}
				service, err := NewBuilder().WithConfig(base).WithConfigPath(
					filepath.Join(
						t.TempDir(), "config.yaml",
					),
				).Build()
				if err != nil {
					t.Fatal(err)
				}
				if service.pluginHost == nil {
					t.Fatal("builder did not create the plugin runtime")
				}
				if err := service.pluginHost.ApplyConfig(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(
					func() {
						if err := service.Shutdown(context.Background()); err != nil {
							t.Error(err)
						}
					},
				)
				if restoreFailure {
					preserved := oldRoot + ".preserved"
					if err := os.Rename(oldRoot, preserved); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(oldRoot, []byte("blocking file"), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(
						func() {
							if err := os.Remove(oldRoot); err != nil {
								t.Error(err)
							}
							if err := os.Rename(preserved, oldRoot); err != nil {
								t.Error(err)
							}
						},
					)
				}
				root := t.TempDir()
				extension := homeBatchExtension()
				archives := map[string][]byte{
					"alpha": homeBatchArchive(t, "alpha"+extension, "new-alpha"),
					"beta":  homeBatchArchive(t, "unrelated.txt", "invalid-second-plugin"),
				}
				homeBatchTransport(t, archives, nil)
				err = service.applyHomeOverlayWithClient(t.Context(), homeBatchConfig(t, root, archives), nil)
				if err == nil || !strings.Contains(err.Error(), "failed to sync home plugins") {
					t.Fatalf("batch failure was lost: %v", err)
				}
				if strings.Contains(err.Error(), "restore published plugin runtime") != restoreFailure {
					t.Fatalf("runtime restoration result was lost: %v", err)
				}
				if service.cfg != base || service.pluginHost.PluginBusy("alpha") {
					t.Fatal("failed batch changed the published plugin runtime")
				}
				if _, err := os.Stat(
					filepath.Join(
						root, runtime.GOOS, runtime.GOARCH, "alpha-v0.2.0"+extension,
					),
				); !os.IsNotExist(err) {
					t.Fatalf("failed batch retained an artifact: %v", err)
				}
			},
		)
	}
}

func TestHomePluginSameConfigurationRetriesAfterSecondInstallFailure(t *testing.T) {
	root := t.TempDir()
	extension := homeBatchExtension()
	archives := map[string][]byte{
		"alpha": homeBatchArchive(t, "alpha"+extension, "new-alpha"),
		"beta":  homeBatchArchive(t, "beta"+extension, "new-beta"),
	}
	homeBatchTransport(t, archives, nil)
	base := &config.Config{
		Home:    config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"},
		Plugins: config.PluginsConfig{Enabled: true, Dir: root},
	}
	service := &Service{cfg: base}
	remote := homeBatchConfig(t, root, archives)
	blockedTarget := filepath.Join(root, runtime.GOOS, runtime.GOARCH, "beta-v0.2.0"+extension)
	if err := os.MkdirAll(blockedTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := service.applyHomeOverlayWithClient(t.Context(), remote, nil); err == nil || !strings.Contains(
		err.Error(), "target plugin is not a regular file",
	) {
		t.Fatalf("second install did not reject its blocked target: %v", err)
	}
	if service.cfg != base {
		t.Fatal("failed batch replaced the published config")
	}
	pending, err := sdkpluginstore.PendingInstallPluginIDs(root)
	if err != nil || len(pending) != 0 {
		t.Fatalf("failed batch retained its first transaction: %v, %v", pending, err)
	}
	if err := os.Remove(blockedTarget); err != nil {
		t.Fatal(err)
	}
	err = service.applyHomeOverlayWithClient(t.Context(), remote, nil)
	if err == nil || !strings.Contains(err.Error(), "home client is unavailable") {
		t.Fatalf("identical config retry did not reach post-publication reporting: %v", err)
	}
	if service.cfg == base {
		t.Fatal("identical Home config could not publish after the filesystem obstruction was removed")
	}
	for _, id := range []string{"alpha", "beta"} {
		data, err := os.ReadFile(filepath.Join(root, runtime.GOOS, runtime.GOARCH, id+"-v0.2.0"+extension))
		if err != nil || string(data) != "new-"+id {
			t.Fatalf("identical retry did not install %s: %q, %v", id, data, err)
		}
	}
}
