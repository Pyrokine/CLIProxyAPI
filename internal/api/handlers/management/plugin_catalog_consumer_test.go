package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
)

func TestPluginSparsePatchPreservesAnotherClientsUpdatedFields(t *testing.T) {
	h := newConfigListTestHandler(
		t, &config.Config{
			Plugins: config.PluginsConfig{
				Dir: t.TempDir(), Configs: map[string]config.PluginInstanceConfig{
					"alpha": pluginConfigFromYAML(t, "first: original\nsecond: original\n"),
				},
			},
		},
	)
	engine := gin.New()
	engine.GET("/plugins/:id/config", h.GetPluginConfig)
	engine.PATCH("/plugins/:id/config", h.PatchPluginConfig)
	call := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/plugins/alpha/config", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s status=%d body=%s", method, response.Code, response.Body.String())
		}
		return response
	}
	var opened map[string]any
	if err := json.Unmarshal(call(http.MethodGet, "").Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	call(http.MethodPatch, `{"second":"other-client"}`)
	call(http.MethodPatch, `{"first":"edited"}`)
	var current map[string]any
	if err := json.Unmarshal(call(http.MethodGet, "").Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current["first"] != "edited" || current["second"] != "other-client" || opened["second"] != "original" {
		t.Fatalf("sparse PATCH current=%v opened=%v", current, opened)
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Plugins.Configs["alpha"]
	if first, second := yamlMappingValue(&item.Raw, "first"), yamlMappingValue(
		&item.Raw, "second",
	); first == nil || first.Value != "edited" || second == nil || second.Value != "other-client" {
		t.Fatalf("persisted sparse PATCH first=%v second=%v", first, second)
	}
}

func TestModelsCatalogMetaSeparatesInitialEmbeddedSourceFromRefreshError(t *testing.T) {
	before := registry.GetCatalogMeta()
	if before.Source != "embed" || before.LastError != "" {
		t.Fatalf("initial embedded metadata=%+v", before)
	}
	h := &Handler{}
	c, response := newArtifactCacheTestContext(http.MethodGet, "/models-catalog-meta", "")
	h.GetModelsCatalogMeta(c)
	if response.Code != 200 {
		t.Fatalf("metadata status=%d body=%s", response.Code, response.Body.String())
	}
	var meta registry.CatalogMeta
	if err := json.Unmarshal(response.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Source != "embed" || meta.LastError != "" || len(meta.Sources) == 0 {
		t.Fatalf("initial source advertised a failed refresh: %+v", meta)
	}
	if after := registry.GetCatalogMeta(); after.LastRefreshAt != before.LastRefreshAt ||
		after.LastError != before.LastError ||
		after.Source != before.Source {
		t.Fatalf("metadata GET changed state: before=%+v after=%+v", before, after)
	}
}
