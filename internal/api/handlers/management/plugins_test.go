package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestPluginConfigMutationsSyncRuntimeBeforeResponse(t *testing.T) {
	testCases := []struct {
		name        string
		method      string
		body        string
		invoke      func(*Handler, *gin.Context)
		wantEnabled bool
		wantMode    string
	}{
		{
			name:        "patch enabled",
			method:      http.MethodPatch,
			body:        `{"enabled":false}`,
			invoke:      (*Handler).PatchPluginEnabled,
			wantEnabled: false,
			wantMode:    "safe",
		},
		{
			name:        "put config",
			method:      http.MethodPut,
			body:        `{"enabled":false,"priority":3,"mode":"strict"}`,
			invoke:      (*Handler).PutPluginConfig,
			wantEnabled: false,
			wantMode:    "strict",
		},
		{
			name:        "patch config",
			method:      http.MethodPatch,
			body:        `{"mode":"strict"}`,
			invoke:      (*Handler).PatchPluginConfig,
			wantEnabled: true,
			wantMode:    "strict",
		},
	}

	for _, tc := range testCases {
		t.Run(
			tc.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				dir := t.TempDir()
				configPath := filepath.Join(dir, "config.yaml")
				if err := os.WriteFile(
					configPath, []byte(`plugins:
  enabled: true
  dir: plugins
  configs:
    alpha:
      enabled: true
      priority: 9
      mode: safe
`), 0o600,
				); err != nil {
					t.Fatalf("write config: %v", err)
				}
				cfg, errLoad := config.LoadConfig(configPath)
				if errLoad != nil {
					t.Fatalf("LoadConfig: %v", errLoad)
				}

				syncCalled := false
				h := NewHandler(
					cfg, configPath, nil, WithPluginRuntimeSync(
						func(_ context.Context, next *config.Config) {
							syncCalled = true
							item, ok := next.Plugins.Configs["alpha"]
							if !ok {
								t.Fatal("synced config missing alpha plugin config")
							}
							if item.Enabled == nil || *item.Enabled != tc.wantEnabled {
								t.Fatalf("synced enabled = %#v, want %v", item.Enabled, tc.wantEnabled)
							}
							modeNode := yamlMappingValue(&item.Raw, "mode")
							if modeNode == nil || modeNode.Value != tc.wantMode {
								t.Fatalf("synced raw mode = %#v, want %s", modeNode, tc.wantMode)
							}
						},
					),
				)

				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
				c.Request = httptest.NewRequest(tc.method, "/plugins/alpha/config", strings.NewReader(tc.body))
				c.Request.Header.Set("Content-Type", "application/json")

				tc.invoke(h, c)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
				}
				if !syncCalled {
					t.Fatal("plugin runtime sync was not called")
				}
			},
		)
	}
}

func TestPluginConfigObjectReturnsOnlyDeclaredPluginFields(t *testing.T) {
	var cfg struct {
		Plugin config.PluginInstanceConfig `yaml:"plugin"`
	}
	if err := yaml.Unmarshal(
		[]byte(`plugin:
  enabled: false
  priority: 7
  mode: safe
  secret-key: hidden
  nested:
    label: visible
`), &cfg,
	); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	got := pluginConfigObject(
		cfg.Plugin, []pluginapi.ConfigField{
			{Name: "mode", Type: pluginapi.ConfigFieldTypeEnum},
			{Name: "nested", Type: pluginapi.ConfigFieldTypeObject},
		},
	)

	if got["enabled"] != false {
		t.Fatalf("enabled = %#v, want false", got["enabled"])
	}
	if got["priority"] != 7 {
		t.Fatalf("priority = %#v, want 7", got["priority"])
	}
	if got["mode"] != "safe" {
		t.Fatalf("mode = %#v, want safe", got["mode"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["label"] != "visible" {
		t.Fatalf("nested = %#v, want label=visible", got["nested"])
	}
	if _, ok := got["secret-key"]; ok {
		t.Fatalf("secret-key was returned: %#v", got)
	}
}
