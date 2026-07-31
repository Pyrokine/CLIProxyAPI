package diff

import (
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestBuildConfigChangeDetailsIncludesTrustedProxies(t *testing.T) {
	oldCfg := &config.Config{}
	oldCfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	newCfg := &config.Config{}
	newCfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}

	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	want := "remote-management.trusted-proxies: [127.0.0.1/32] -> [127.0.0.1/32, ::1/128]"
	if !slices.Contains(changes, want) {
		t.Fatalf("changes = %v, want %q", changes, want)
	}
}
