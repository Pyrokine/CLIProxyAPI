package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func pluginRepairRequest(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	engine.ServeHTTP(res, req)
	return res
}

func checkPluginRepairReceipt(
	t *testing.T,
	engine *gin.Engine,
	res *httptest.ResponseRecorder,
	status int,
	outcome string,
	effects ...string,
) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status=%d want %d; body=%s", res.Code, status, res.Body)
	}
	result := decodeMutationHeader(t, res)
	if result.Outcome != outcome || !result.AuditAvailable || result.ConfirmedEffect != (len(effects) != 0) || len(result.Effects) != len(effects) {
		t.Fatalf("mutation result=%+v; want %s %v", result, outcome, effects)
	}
	for _, kind := range effects {
		found := false
		for _, effect := range result.Effects {
			found = found || effect.Kind == kind
		}
		if !found {
			t.Fatalf("missing effect %q in %+v", kind, result)
		}
	}
	record := pluginRepairRequest(engine, http.MethodGet, "/mutations/"+result.MutationID, "")
	var stored MutationResult
	if err := json.Unmarshal(
		record.Body.Bytes(), &stored,
	); record.Code != http.StatusOK || err != nil || !reflect.DeepEqual(result, stored) {
		t.Fatalf("audit readback status=%d body=%s, want %+v", record.Code, record.Body, result)
	}
}

func TestPluginRepairStoreReceiptsAndSavedEnabledState(t *testing.T) {
	const registryURL = "https://registry.fixture.invalid/registry.json"
	const artifactURL = "https://registry.fixture.invalid/fixture.zip"
	archive := pluginStoreTestArchive(t, "fixture.so", "synthetic artifact, never loaded")
	registry := fmt.Sprintf(
		`{"schema_version":2,"plugins":[{"id":"fixture","name":"Fixture","description":"Synthetic plugin","author":"Fixture","version":"0.2.0",`+
			`"install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":%q,"sha256":"%x"}]}}]}`,
		artifactURL, sha256.Sum256(archive),
	)
	for _, test := range []struct {
		name    string
		update  bool
		failure bool
	}{
		{name: "new install"},
		{name: "disabled update", update: true},
		{name: "runtime publication fails", failure: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				pluginsDir := t.TempDir()
				platformDir := filepath.Join(pluginsDir, "linux", "amd64")
				configs := map[string]config.PluginInstanceConfig{}
				if test.update {
					if err := os.MkdirAll(platformDir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(
						filepath.Join(platformDir, "fixture-v0.1.0.so"), []byte("previous synthetic artifact"), 0600,
					); err != nil {
						t.Fatal(err)
					}
					configs["fixture"] = pluginConfigFromYAML(
						t,
						"enabled: false\npriority: 7\nmode: retained\nstore:\n  id: fixture\n  version: v0.1.0\n  source-url: "+registryURL+"\n",
					)
				}
				h := newConfigListTestHandler(
					t, &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: pluginsDir, Configs: configs}},
				)
				h.pluginStoreRegistryURL = registryURL
				h.pluginStoreHTTPClient = fakePluginStoreHTTPClient(
					func(req *http.Request) (*http.Response, error) {
						var data []byte
						switch req.URL.String() {
						case registryURL:
							data = []byte(registry)
						case artifactURL:
							data = archive
						default:
							t.Fatalf("unexpected synthetic request %s", req.URL)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Request: req,
							Header: http.Header{},
						}, nil
					},
				)
				applied := 0
				h.configCommitted = func(_ context.Context, commit ConfigCommit) error {
					applied++
					if enabled := pluginInstanceEnabled(commit.Config.Plugins.Configs["fixture"]); enabled == test.update {
						t.Errorf("runtime received enabled=%v, update=%v", enabled, test.update)
					}
					if test.failure {
						return errors.New("synthetic runtime publication failure")
					}
					return nil
				}
				engine := resourceMutationEngine(t, h)
				engine.GET("/mutations/:mutation_id", h.GetMutationResult)
				engine.POST(
					"/plugin-store/:id/install", func(c *gin.Context) { h.installPluginFromStore(c, "linux", "amd64") },
				)
				res := pluginRepairRequest(engine, http.MethodPost, "/plugin-store/fixture/install", "")
				effects := []string{"configuration.persisted", "configuration.published", "plugin.published"}
				if test.failure {
					checkPluginRepairReceipt(t, engine, res, http.StatusServiceUnavailable, "partial", effects...)
				} else {
					checkPluginRepairReceipt(
						t, engine, res, http.StatusOK, "succeeded", append(effects, "configuration.runtime_applied")...,
					)
				}
				data, err := os.ReadFile(filepath.Join(platformDir, "fixture-v0.2.0.so"))
				if err != nil || string(data) != "synthetic artifact, never loaded" || applied != 1 {
					t.Fatalf("artifact=%q err=%v runtime calls=%d", data, err, applied)
				}
				stored, err := config.LoadConfig(h.configFilePath)
				if err != nil {
					t.Fatal(err)
				}
				item := stored.Plugins.Configs["fixture"]
				if item.Enabled == nil || *item.Enabled == test.update || pluginStoreDesiredVersion(item) != "0.2.0" {
					t.Fatalf("saved config=%+v", item)
				}
				if test.update {
					mode := yamlMappingValue(pluginConfigNode(item), "mode")
					if mode == nil || mode.Value != "retained" || item.Priority != 7 {
						t.Fatalf("update changed existing settings: %+v", item)
					}
				}
			},
		)
	}
}

func TestPluginRepairDeleteReturnsCommittedEffects(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		outcome string
	}{
		{"unloaded", http.StatusOK, "succeeded"},
		{"runtime still loaded", http.StatusConflict, "partial"},
		{"runtime sync failed", http.StatusServiceUnavailable, "partial"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				dir := t.TempDir()
				h := newConfigListTestHandler(
					t, &config.Config{
						Plugins: config.PluginsConfig{
							Enabled: true, Dir: dir,
							Configs: map[string]config.PluginInstanceConfig{
								"fixture": pluginConfigFromYAML(
									t, "enabled: true\n",
								),
							},
						},
					},
				)
				for _, version := range []string{"0.1.0", "0.2.0"} {
					if err := os.WriteFile(
						filepath.Join(dir, "fixture-v"+version+testPluginExtension()), []byte("synthetic"), 0600,
					); err != nil {
						t.Fatal(err)
					}
				}
				host := &fakePluginRuntimeHost{busy: true, unloadResult: test.status != http.StatusConflict}
				h.pluginHost = host
				if test.status == http.StatusServiceUnavailable {
					h.configCommitted = func(
						context.Context,
						ConfigCommit,
					) error {
						return errors.New("synthetic runtime sync failure")
					}
				}
				engine := resourceMutationEngine(t, h)
				engine.DELETE("/plugins/:id", h.DeletePlugin)
				engine.GET("/mutations/:mutation_id", h.GetMutationResult)
				res := pluginRepairRequest(engine, http.MethodDelete, "/plugins/fixture", "")
				checkPluginRepairReceipt(
					t, engine, res, test.status, test.outcome, "configuration.persisted", "configuration.published",
					"plugin.deleted",
				)
				var body struct {
					FileDeleted       bool `json:"file_deleted"`
					ConfiguredRemoved bool `json:"configured_removed"`
					RestartRequired   bool `json:"restart_required"`
				}
				if err := json.Unmarshal(
					res.Body.Bytes(), &body,
				); err != nil || !body.FileDeleted || !body.ConfiguredRemoved || body.RestartRequired != (test.status == http.StatusConflict) {
					t.Fatalf("deletion response=%s", res.Body)
				}
				files, err := filepath.Glob(filepath.Join(dir, "fixture-*"))
				_, configured := h.configSnapshot().Plugins.Configs["fixture"]
				if err != nil || len(files) != 0 || configured {
					t.Fatalf("remaining files=%v configured=%v err=%v", files, configured, err)
				}
			},
		)
	}
}

func TestPluginRepairRateLimitUnbanDistinguishesRuntimeAndPersistence(t *testing.T) {
	const ip = "192.0.2.23"
	for _, fail := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("persistence_failure=%t", fail), func(t *testing.T) {
				h := &Handler{
					cfg: &config.Config{}, failedAttempts: map[string]*attemptInfo{
						ip: {blockedUntil: time.Now().Add(time.Hour), banCount: 1},
					},
				}
				engine := resourceMutationEngine(t, h)
				engine.DELETE("/rate-limit/ban", h.DeleteRateLimitBan)
				engine.GET("/rate-limit/status", h.GetRateLimitStatus)
				engine.GET("/mutations/:mutation_id", h.GetMutationResult)
				if fail {
					if _, err := h.stateStore.db.Exec(
						`CREATE TRIGGER test_reject_rate_limit BEFORE INSERT ON state_items
					WHEN NEW.key = 'management_rate_limit' BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`,
					); err != nil {
						t.Fatal(err)
					}
				}
				res := pluginRepairRequest(engine, http.MethodDelete, "/rate-limit/ban", `{"ip":"`+ip+`"}`)
				if fail {
					checkPluginRepairReceipt(t, engine, res, http.StatusOK, "partial", "security.runtime_applied")
					if !strings.Contains(
						res.Body.String(), `"persistence_pending":true`,
					) || !h.rateLimitPersistenceDirty {
						t.Fatalf("unconfirmed persistence not reported or queued: %s", res.Body)
					}
				} else {
					checkPluginRepairReceipt(
						t, engine, res, http.StatusOK, "succeeded", "security.runtime_applied", "security.persisted",
					)
					saved, err := h.stateStore.loadRateLimitState(context.Background())
					if err != nil || saved == nil || len(saved.ActiveBans) != 0 || len(saved.Events) != 1 || saved.Events[0].Type != rateLimitEventManualUnban {
						t.Fatalf("saved rate-limit state=%+v err=%v", saved, err)
					}
				}
				status := pluginRepairRequest(engine, http.MethodGet, "/rate-limit/status", "")
				if !strings.Contains(res.Body.String(), `"unbanned":true`) ||
					!strings.Contains(status.Body.String(), `"banned_ips":[]`) ||
					!strings.Contains(status.Body.String(), `"type":"manual_unban"`) {
					t.Fatalf("unban=%s status=%s", res.Body, status.Body)
				}
				noop := pluginRepairRequest(engine, http.MethodDelete, "/rate-limit/ban", `{"ip":"`+ip+`"}`)
				checkPluginRepairReceipt(t, engine, noop, http.StatusOK, "uncertain")
				if !strings.Contains(noop.Body.String(), `"unbanned":false`) {
					t.Fatalf("unchanged ban result=%s", noop.Body)
				}
			},
		)
	}
}

func TestPluginRepairConfigReadbackRetainsSelectedVersionPrecisionAndRedaction(t *testing.T) {
	dir := t.TempDir()
	platformDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(platformDir, 0700); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(platformDir, "fixture-v0.1.0"+testPluginExtension())
	for _, path := range []string{selected, filepath.Join(platformDir, "fixture-v0.2.0"+testPluginExtension())} {
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h := newConfigListTestHandler(
		t, &config.Config{
			Plugins: config.PluginsConfig{
				Dir: dir,
				Configs: map[string]config.PluginInstanceConfig{
					"fixture": pluginConfigFromYAML(
						t, "enabled: false\npriority: 5\nmode: old\nstore:\n  id: fixture\n  version: v0.1.0\n",
					),
				},
			},
		},
	)
	engine := resourceMutationEngine(t, h)
	engine.PATCH("/plugins/:id/config", h.PatchPluginConfig)
	engine.PATCH("/plugins/:id/enabled", h.PatchPluginEnabled)
	engine.GET("/plugins/:id/config", h.GetPluginConfig)
	engine.GET("/plugins", h.ListPlugins)
	engine.GET("/mutations/:mutation_id", h.GetMutationResult)
	patch := `{"priority":0,"api-key":"synthetic-private-value","options":{"nested":[9007199254740993,0.1234567890123456789],"label":"changed"},"mode":null}`
	res := pluginRepairRequest(engine, http.MethodPatch, "/plugins/fixture/config", patch)
	checkPluginRepairReceipt(
		t, engine, res, http.StatusOK, "succeeded", "configuration.persisted", "configuration.published",
	)
	read := pluginRepairRequest(engine, http.MethodGet, "/plugins/fixture/config", "")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(read.Body.Bytes(), &body); err != nil || read.Code != http.StatusOK ||
		string(body["priority"]) != "0" || len(body["mode"]) != 0 || len(body["store"]) != 0 ||
		!strings.Contains(string(body["options"]), `9007199254740993,0.1234567890123456789`) ||
		!isConfigYAMLPlaceholder(strings.Trim(string(body["api-key"]), `"`)) ||
		strings.Contains(read.Body.String(), "synthetic-private-value") {
		t.Fatalf("configuration readback=%s", read.Body)
	}
	res = pluginRepairRequest(engine, http.MethodPatch, "/plugins/fixture/enabled", `{"enabled":true}`)
	checkPluginRepairReceipt(
		t, engine, res, http.StatusOK, "succeeded", "configuration.persisted", "configuration.published",
	)
	listResponse := pluginRepairRequest(engine, http.MethodGet, "/plugins", "")
	var list pluginListResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil || listResponse.Code != http.StatusOK ||
		list.PluginsEnabled || len(list.Plugins) != 1 || list.Plugins[0].Path != selected ||
		!list.Plugins[0].Enabled || list.Plugins[0].EffectiveEnabled || list.Plugins[0].Registered {
		t.Fatalf("list=%s", listResponse.Body)
	}
}
