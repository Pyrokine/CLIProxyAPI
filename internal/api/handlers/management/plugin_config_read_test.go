package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestPluginConfigGETPreservesRawPrecisionSecretsAndSnapshot(t *testing.T) {
	const secret = "synthetic-private-plugin-value"
	item := pluginConfigFromYAML(
		t, `priority: 3
mode: public
api-key: `+secret+`
opaque:
  large: 9007199254740993
  decimal: 0.12345678901234567890123456789
  mixed: [true, null, {name: retained}]
nested:
  credentials:
    token: synthetic-nested-token
endpoint: &secret_alias synthetic-alias-token
refresh-token: *secret_alias
shared: &shared {label: original}
copy: *shared
store:
  source_id: internal-source
  version: internal-version
`,
	)
	dir := t.TempDir()
	h := newConfigListTestHandler(
		t, &config.Config{
			SDKConfig: config.SDKConfig{
				APIKeys: []string{"synthetic-client"},
				APIKeyRules: map[string]config.APIKeyRule{
					"synthetic-client": {Limits: []config.APIKeyPolicyLimit{{Window: "1h", Tokens: 100}}},
				},
			},
			Plugins: config.PluginsConfig{
				Dir: dir, Configs: map[string]config.PluginInstanceConfig{
					"alpha": item,
					"beta":  pluginConfigFromYAML(t, "api-key: "+secret+"\n"),
				},
			},
		},
	)
	h.pluginHost = &fakePluginRuntimeHost{
		registered: []pluginhost.RegisteredPluginInfo{
			{
				ID: "alpha", Metadata: pluginapi.Metadata{
					ConfigFields: []pluginapi.ConfigField{
						{Name: "api-key", Type: "string"}, {Name: "mode", Type: "string"},
					},
				},
			},
			{ID: "registered"},
		},
	}
	engine := gin.New()
	engine.GET("/plugins", h.ListPlugins)
	engine.GET("/plugins/:id/config", h.GetPluginConfig)
	engine.PUT("/plugins/:id/config", h.PutPluginConfig)
	engine.PATCH("/plugins/:id/config", h.PatchPluginConfig)
	call := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s status=%d body=%s", method, path, response.Code, response.Body.String())
		}
		return response
	}
	decode := func(response *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var body map[string]any
		decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	before, err := yaml.Marshal(h.configSnapshot().Plugins.Configs)
	if err != nil {
		t.Fatal(err)
	}
	response := call(http.MethodGet, "/plugins/alpha/config", "", 200)
	body := decode(response)
	if _, wrapper := body["config"]; wrapper {
		t.Fatal("legacy direct object response was wrapped")
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(
		response.Body.String(), "synthetic-nested-token",
	) || strings.Contains(response.Body.String(), "synthetic-alias-token") {
		t.Fatalf("secret exposed: %s", response.Body.String())
	}
	marker, ok := body["api-key"].(string)
	if !ok || !isConfigYAMLPlaceholder(marker) {
		t.Fatalf("secret reference=%v", body["api-key"])
	}
	if _, found := body["store"]; found {
		t.Fatal("internal store provenance became editable")
	}
	opaque := body["opaque"].(map[string]any)
	if opaque["large"].(json.Number).String() != "9007199254740993" ||
		opaque["decimal"].(json.Number).String() != "0.12345678901234567890123456789" {
		t.Fatalf("numeric precision=%+v", opaque)
	}
	if len(opaque["mixed"].([]any)) != 3 || body["copy"].(map[string]any)["label"] != "original" {
		t.Fatalf("opaque structure=%+v", body)
	}
	after, err := yaml.Marshal(h.configSnapshot().Plugins.Configs)
	if err != nil || !bytes.Equal(before, after) || pluginInstanceEnabled(h.configSnapshot().Plugins.Configs["alpha"]) {
		t.Fatal("GET changed the preserved snapshot or enabled default")
	}
	listed := call(http.MethodGet, "/plugins", "", 200)
	var list pluginListResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, entry := range list.Plugins {
		if entry.ID == "alpha" {
			matched = true
			if entry.Enabled || entry.Config["api-key"] != marker {
				t.Fatalf("list/get secret strategy differs: %+v", entry)
			}
		}
	}
	if !matched {
		t.Fatal("configured plugin absent from list")
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		body["mode"] = "edited-" + method
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		call(method, "/plugins/alpha/config", string(encoded), 200)
		loaded, err := config.LoadConfig(h.configFilePath)
		if err != nil {
			t.Fatal(err)
		}
		node := pluginConfigNode(loaded.Plugins.Configs["alpha"])
		apiKey := yamlMappingValue(node, "api-key")
		if apiKey == nil || apiKey.Value != secret || yamlMappingValue(node, "store") == nil {
			t.Fatal("save replaced a credential or lost internal provenance")
		}
		content, err := os.ReadFile(h.configFilePath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), configYAMLPlaceholderPrefix) {
			t.Fatal("a display reference was persisted as a credential")
		}
		freshResponse := call(http.MethodGet, "/plugins/alpha/config", "", 200)
		if strings.Contains(freshResponse.Body.String(), secret) || strings.Contains(
			freshResponse.Body.String(), "synthetic-alias-token",
		) {
			t.Fatal("JSON roundtrip lost the secret alias redaction relation")
		}
		fresh := decode(freshResponse)
		if fresh["opaque"].(map[string]any)["decimal"].(json.Number).String() != "0.12345678901234567890123456789" {
			t.Fatal("read/write lost opaque precision")
		}
	}
	beta := decode(call(http.MethodGet, "/plugins/beta/config", "", 200))
	if beta["api-key"] == marker {
		t.Fatal("different plugin IDs shared a secret capability")
	}
	encoded, _ := json.Marshal(map[string]any{"api-key": marker})
	beforeFile, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	call(http.MethodPatch, "/plugins/beta/config", string(encoded), 400)
	afterFile, err := os.ReadFile(h.configFilePath)
	if err != nil || !bytes.Equal(beforeFile, afterFile) {
		t.Fatal("cross-plugin reference changed the file")
	}
	call(http.MethodGet, "/plugins/registered/config", "", 200)
	if err := os.WriteFile(filepath.Join(dir, "discovered.so"), []byte("fixture; never loaded"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := call(http.MethodGet, "/plugins/discovered/config", "", 200).Body.String(); got != "{}" {
		t.Fatalf("discovered unconfigured body=%s", got)
	}
	call(http.MethodGet, "/plugins/missing/config", "", 404)
	call(http.MethodGet, "/plugins/bad%20id/config", "", 400)
	var group sync.WaitGroup
	for range 16 {
		group.Go(
			func() {
				for range 8 {
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/plugins/alpha/config", nil))
					if response.Code != 200 {
						t.Errorf("concurrent GET status=%d", response.Code)
						return
					}
				}
			},
		)
	}
	group.Wait()
}

func TestPluginConfigReadsReportInvalidPriority(t *testing.T) {
	var raw yaml.Node
	if err := yaml.Unmarshal([]byte("priority: invalid\napi-key: synthetic-private-value\n"), &raw); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Dir: t.TempDir(), Configs: map[string]config.PluginInstanceConfig{"alpha": {Raw: *raw.Content[0]}},
			},
		},
	}
	engine := gin.New()
	engine.GET("/plugins", h.ListPlugins)
	engine.GET("/plugins/:id/config", h.GetPluginConfig)
	for _, path := range []string{"/plugins", "/plugins/alpha/config"} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		const wantBody = `{"error":"plugin_config_encode_failed"}`
		if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != wantBody {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}
