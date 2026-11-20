package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/gin-gonic/gin"
)

func TestListPluginsUnconfiguredEntriesRemainDisabled(t *testing.T) {
	for _, source := range []struct {
		name       string
		discovered bool
		registered bool
	}{
		{name: "discovered", discovered: true},
		{name: "registered", registered: true},
		{name: "discovered and registered", discovered: true, registered: true},
	} {
		for _, globallyEnabled := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/global=%t", source.name, globallyEnabled), func(t *testing.T) {
					dir := t.TempDir()
					if source.discovered {
						for _, extension := range []string{".so", ".dylib", ".dll"} {
							if err := os.WriteFile(filepath.Join(dir, "alpha"+extension), nil, 0o600); err != nil {
								t.Fatal(err)
							}
						}
					}
					host := &fakePluginRuntimeHost{}
					if source.registered {
						host.registered = []pluginhost.RegisteredPluginInfo{{ID: "alpha"}}
					}
					cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: globallyEnabled, Dir: dir}}
					h := &Handler{cfg: cfg, pluginHost: host}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugins", nil)
					h.ListPlugins(c)
					if recorder.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					var response pluginListResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.PluginsEnabled != globallyEnabled || len(response.Plugins) != 1 {
						t.Fatalf("unexpected list response: %+v", response)
					}
					entry := response.Plugins[0]
					if entry.ID != "alpha" || entry.Configured || entry.Enabled || entry.EffectiveEnabled {
						t.Fatalf("unconfigured plugin must remain disabled: %+v", entry)
					}
					if entry.Registered != source.registered || (entry.Path != "") != source.discovered {
						t.Fatalf("plugin source metadata changed: %+v", entry)
					}
					if len(cfg.Plugins.Configs) != 0 {
						t.Fatal("listing created a plugin configuration")
					}
				},
			)
		}
	}
}
