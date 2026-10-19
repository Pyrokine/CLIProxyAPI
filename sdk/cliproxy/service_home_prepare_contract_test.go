package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestHomePrepareFailureKeepsCurrentConfiguration(t *testing.T) {
	root := t.TempDir()
	base := &config.Config{}
	base.Home.Enabled = true
	base.Plugins.Enabled = true
	base.Plugins.Dir = root
	service, err := NewBuilder().WithConfig(base).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	enabled := true
	remote := &config.Config{}
	remote.Plugins.Enabled = true
	remote.Plugins.Dir = root
	remote.Plugins.Configs = map[string]internalconfig.PluginInstanceConfig{
		"broken": {Enabled: &enabled, Raw: yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "store"},
			{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "id"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "broken"},
			}},
		}}},
	}
	err = service.applyHomeOverlayContext(context.Background(), remote)
	if err == nil {
		t.Errorf("invalid store manifest was acknowledged as a successful Home config")
	}
	if service.cfg != base {
		t.Errorf("failed Home plugin preparation replaced current config")
	}
}

func TestHomePluginApplyFailureKeepsCurrentConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin-root-file")
	if err := os.WriteFile(root, []byte("synthetic root file"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := &config.Config{}
	base.Home.Enabled = true
	service := &Service{cfg: base}
	remote := &config.Config{}
	remote.Home.Enabled = true
	remote.Plugins.Enabled = true
	remote.Plugins.Dir = root
	prepared, _, didSync, errPrepare := service.prepareHomePlugins(t.Context(), remote)
	if errPrepare != nil || !didSync {
		t.Fatalf("preparation must succeed before testing installation failure: sync=%v error=%v", didSync, errPrepare)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	err := service.applyHomeOverlayContext(t.Context(), remote)
	if err == nil || !strings.Contains(err.Error(), "failed to sync home plugins") {
		t.Fatalf("installation failure was not returned: %v", err)
	}
	if service.cfg != base || service.homePluginSyncKey != "" {
		t.Fatalf("failed Home installation published configuration: config=%p want=%p key=%q", service.cfg, base, service.homePluginSyncKey)
	}
	data, errRead := os.ReadFile(root)
	if errRead != nil || string(data) != "synthetic root file" {
		t.Fatalf("installation changed the existing root: data=%q error=%v", data, errRead)
	}
}
