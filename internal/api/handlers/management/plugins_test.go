package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
						func(_ context.Context, next *config.Config) error {
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
							return nil
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

func TestPluginConfigRuntimeSyncKeepsCommitSequence(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("plugins:\n  configs:\n    alpha:\n      enabled: true\n"),
		0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	runtimeStarted := make(chan struct{})
	releaseRuntime := make(chan struct{})
	h := NewHandler(
		cfg,
		configPath,
		nil,
		WithPluginRuntimeSync(
			func(context.Context, *config.Config) error {
				close(runtimeStarted)
				<-releaseRuntime
				return nil
			},
		),
	)
	t.Cleanup(func() { _ = h.Close() })

	mutationDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
		c.Request = httptest.NewRequest(
			http.MethodPatch,
			"/plugins/alpha",
			strings.NewReader(`{"enabled":false}`),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		h.PatchPluginEnabled(c)
		mutationDone <- rec
	}()
	select {
	case <-runtimeStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin runtime sync did not start")
	}

	externalEntered := make(chan struct{})
	externalDone := make(chan error, 1)
	go func() {
		externalDone <- h.ApplyExternalConfigUpdate(
			func() error {
				close(externalEntered)
				return nil
			},
		)
	}()
	select {
	case <-externalEntered:
		t.Fatal("external config update entered while plugin runtime sync was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRuntime)
	if rec := <-mutationDone; rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if errExternal := <-externalDone; errExternal != nil {
		t.Fatalf("ApplyExternalConfigUpdate() error = %v", errExternal)
	}
}

func TestPluginConfigCommitCallbackAvoidsSecondRuntimePublication(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("plugins:\n  configs:\n    alpha:\n      enabled: true\n"),
		0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var committedCalls int
	var runtimeCalls int
	h := NewHandler(
		cfg,
		configPath,
		nil,
		WithConfigCommitted(
			func(context.Context, ConfigCommit) error {
				committedCalls++
				return nil
			},
		),
		WithPluginRuntimeSync(
			func(context.Context, *config.Config) error {
				runtimeCalls++
				return nil
			},
		),
	)
	t.Cleanup(func() { _ = h.Close() })
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/plugins/alpha",
		strings.NewReader(`{"enabled":false}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchPluginEnabled(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if committedCalls != 1 {
		t.Fatalf("config committed calls = %d, want 1", committedCalls)
	}
	if runtimeCalls != 0 {
		t.Fatalf("secondary plugin runtime calls = %d, want 0", runtimeCalls)
	}
}

func TestPluginConfigMutationFailureKeepsPublishedConfigAndRuntime(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		body   string
		invoke func(*Handler, *gin.Context)
	}{
		{
			name: "patch enabled", method: http.MethodPatch, body: `{"enabled":false}`,
			invoke: (*Handler).PatchPluginEnabled,
		},
		{
			name: "put config", method: http.MethodPut, body: `{"enabled":false,"mode":"strict"}`,
			invoke: (*Handler).PutPluginConfig,
		},
		{
			name: "patch config", method: http.MethodPatch, body: `{"mode":"strict"}`,
			invoke: (*Handler).PatchPluginConfig,
		},
	}

	for _, tc := range testCases {
		t.Run(
			tc.name, func(t *testing.T) {
				root := t.TempDir()
				blockedParent := filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
					t.Fatalf("write blocking file: %v", err)
				}
				cfg := &config.Config{
					Plugins: config.PluginsConfig{
						Configs: map[string]config.PluginInstanceConfig{
							"alpha": pluginConfigFromYAML(t, "enabled: true\nmode: safe\n"),
						},
					},
				}
				syncCalled := false
				h := &Handler{
					cfg:            cfg,
					configFilePath: filepath.Join(blockedParent, "config.yaml"),
					pluginRuntimeSync: func(context.Context, *config.Config) error {
						syncCalled = true
						return nil
					},
				}

				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
				c.Request = httptest.NewRequest(tc.method, "/plugins/alpha/config", strings.NewReader(tc.body))
				c.Request.Header.Set("Content-Type", "application/json")

				tc.invoke(h, c)

				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
				}
				if syncCalled {
					t.Fatal("plugin runtime sync was called after persistence failed")
				}
				published := h.configSnapshot()
				item := published.Plugins.Configs["alpha"]
				if item.Enabled == nil || !*item.Enabled {
					t.Fatalf("published enabled = %#v, want true", item.Enabled)
				}
				mode := yamlMappingValue(&item.Raw, "mode")
				if mode == nil || mode.Value != "safe" {
					t.Fatalf("published mode = %#v, want safe", mode)
				}
			},
		)
	}
}

func TestPluginRuntimePanicReturnsUnavailableAfterCommit(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath,
		[]byte("plugins:\n  configs:\n    alpha:\n      enabled: true\n"),
		0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	h := NewHandler(
		cfg,
		configPath,
		nil,
		WithPluginRuntimeSync(func(context.Context, *config.Config) error { panic("runtime failed") }),
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/plugins/alpha", strings.NewReader(`{"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchPluginEnabled(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	item := h.configSnapshot().Plugins.Configs["alpha"]
	if item.Enabled == nil || *item.Enabled {
		t.Fatalf("published enabled = %#v, want false", item.Enabled)
	}
	saved, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	savedItem := saved.Plugins.Configs["alpha"]
	if savedItem.Enabled == nil || *savedItem.Enabled {
		t.Fatalf("saved enabled = %#v, want false", savedItem.Enabled)
	}
}

func TestPluginConfigMutationsPreserveStoreIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Configs: map[string]config.PluginInstanceConfig{
				"alpha": pluginConfigFromYAML(
					t,
					"enabled: true\nmode: safe\nstore:\n  id: alpha\n  source-id: official\n  source-url: https://official.example/registry.json\n",
				),
			},
		},
	}
	h := NewHandler(cfg, configPath, nil)
	t.Cleanup(func() { _ = h.Close() })

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "alpha"}}
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/plugins/alpha/config",
		strings.NewReader(`{"enabled":false,"mode":"strict"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutPluginConfig(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	item := h.configSnapshot().Plugins.Configs["alpha"]
	sourceID, sourceURL, managed := pluginStoreConfiguredSource(item)
	if !managed || sourceID != "official" || sourceURL != "https://official.example/registry.json" {
		t.Fatalf("store identity = (%q, %q, %v)", sourceID, sourceURL, managed)
	}
	if got := yamlMappingValue(&item.Raw, "mode"); got == nil || got.Value != "strict" {
		t.Fatalf("mode = %#v, want strict", got)
	}

	for _, mutation := range []struct {
		name   string
		method string
		body   string
		invoke func(*Handler, *gin.Context)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			body:   `{"store":{"source-id":"other"}}`,
			invoke: (*Handler).PutPluginConfig,
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			body:   `{"store":null}`,
			invoke: (*Handler).PatchPluginConfig,
		},
	} {
		t.Run(
			mutation.name, func(t *testing.T) {
				recMutation := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recMutation)
				ctx.Params = []gin.Param{{Key: "id", Value: "alpha"}}
				ctx.Request = httptest.NewRequest(
					mutation.method,
					"/plugins/alpha/config",
					strings.NewReader(mutation.body),
				)
				ctx.Request.Header.Set("Content-Type", "application/json")
				mutation.invoke(h, ctx)
				if recMutation.Code != http.StatusBadRequest ||
					!strings.Contains(recMutation.Body.String(), "reserved_plugin_config_field") {
					t.Fatalf("status = %d, body=%s", recMutation.Code, recMutation.Body.String())
				}
			},
		)
	}

	item = h.configSnapshot().Plugins.Configs["alpha"]
	sourceID, sourceURL, managed = pluginStoreConfiguredSource(item)
	if !managed || sourceID != "official" || sourceURL != "https://official.example/registry.json" {
		t.Fatalf("store identity after rejected mutation = (%q, %q, %v)", sourceID, sourceURL, managed)
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
