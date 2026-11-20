package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestMutationYAMLCatalogFieldsRemainUncertainUntilCatalogTransaction(t *testing.T) {
	for _, field := range []string{"oauth-model-alias: {}\n", "oauth-model-alias: null\n", "model-catalog: {}\n"} {
		t.Run(
			strings.TrimSpace(field), func(t *testing.T) {
				t.Setenv("WRITABLE_PATH", "")
				root := t.TempDir()
				path := filepath.Join(root, "config.yaml")
				if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
					t.Fatal(err)
				}
				store, err := openManagementStateStorePath(filepath.Join(root, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				h := &Handler{cfg: &config.Config{Port: 8317}, configFilePath: path, stateStore: store}
				engine := gin.New()
				engine.Use(fakeManagementAuditIdentity(), h.MutationAuditMiddleware())
				engine.PUT("/v0/management/config.yaml", h.PutConfigYAML)
				request := httptest.NewRequest(
					http.MethodPut, "/v0/management/config.yaml", strings.NewReader("port: 8318\n"+field),
				)
				request.RemoteAddr = "127.0.0.1:12345"
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				result := decodeMutationHeader(t, response)
				wantOutcome := "uncertain"
				if strings.HasPrefix(field, "oauth-model-alias:") {
					wantOutcome = "succeeded"
					t.Cleanup(func() { config.SetRuntimeModelCatalog(nil) })
				}
				if response.Code != 200 || result.Outcome != wantOutcome || !result.ConfirmedEffect {
					t.Fatalf("catalog transaction response=%d result=%+v", response.Code, result)
				}
			},
		)
	}
}

func TestMutationEffectsFollowActualConfigurationAndPluginCommits(t *testing.T) {
	for _, resource := range []string{"configuration", "plugin"} {
		for _, failRuntime := range []bool{false, true} {
			name := resource + "/success"
			if failRuntime {
				name = resource + "/runtime-failure"
			}
			t.Run(
				name, func(t *testing.T) {
					t.Setenv("WRITABLE_PATH", "")
					root := t.TempDir()
					path := filepath.Join(root, "config.yaml")
					if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
						t.Fatal(err)
					}
					store, err := openManagementStateStorePath(filepath.Join(root, "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					cfg := &config.Config{
						Port: 8317, Plugins: config.PluginsConfig{
							Dir: t.TempDir(), Configs: map[string]config.PluginInstanceConfig{
								"alpha": pluginConfigFromYAML(
									t, "enabled: true\nmode: safe\n",
								),
							},
						},
					}
					h := &Handler{cfg: cfg, configFilePath: path, stateStore: store}
					calls := 0
					if resource == "configuration" {
						h.configCommitted = func(context.Context, ConfigCommit) error {
							calls++
							if failRuntime {
								return errors.New("synthetic runtime publication failure")
							}
							return nil
						}
					} else {
						h.pluginRuntimeSync = func(context.Context, *config.Config) error {
							calls++
							if failRuntime {
								return errors.New("synthetic plugin publication failure")
							}
							return nil
						}
					}
					engine := gin.New()
					engine.Use(fakeManagementAuditIdentity(), h.MutationAuditMiddleware())
					engine.PUT("/v0/management/config.yaml", h.PutConfigYAML)
					engine.PATCH("/v0/management/plugins/:id/config", h.PatchPluginConfig)
					method, route, body := http.MethodPut, "/v0/management/config.yaml", "port: 8318\n"
					if resource == "plugin" {
						method, route, body = http.MethodPatch, "/v0/management/plugins/alpha/"+
							"config", `{"mode":`+
							`"strict"}`
					}
					request := httptest.NewRequest(method, route, strings.NewReader(body))
					request.RemoteAddr = "127.0.0.1:12345"
					if resource == "plugin" {
						request.Header.Set("Content-Type", "application/json")
					}
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, request)
					result := decodeMutationHeader(t, response)
					wantStatus, wantOutcome := 200, "succeeded"
					if failRuntime {
						wantStatus, wantOutcome = 503, "partial"
					}
					if response.Code != wantStatus || result.Outcome != wantOutcome ||
						!result.ConfirmedEffect ||
						!result.AuditAvailable ||
						calls != 1 {
						t.Fatalf("response=%d result=%+v calls=%d", response.Code, result, calls)
					}
					persisted, published, runtimeApplied := false, false, false
					for _, effect := range result.Effects {
						if effect.Kind == "configuration.persisted" {
							persisted = true
						}
						if effect.Kind == "configuration.published" {
							published = true
						}
						if strings.HasSuffix(effect.Kind, ".runtime_applied") {
							runtimeApplied = true
						}
					}
					if !persisted || !published || runtimeApplied == failRuntime {
						t.Fatalf("effects do not match actual stages: %+v", result.Effects)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if resource == "configuration" && !strings.Contains(string(data), "port: 8318") {
						t.Fatal("reported persistence did not reach the config file")
					}
					if resource == "plugin" && !strings.Contains(string(data), "mode: strict") {
						t.Fatal("reported plugin persistence did not reach the config file")
					}
				},
			)
		}
	}
}
