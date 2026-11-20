package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestConfigYAMLClientKeyReferencesThroughConditionalSave(t *testing.T) {
	const first = "sk-client-reference-first-abcdef"
	const second = "sk-client-reference-second-abcdef"
	const original = "port: 8317\napi-keys:\n  - " +
		first +
		"\n  - " +
		second +
		"\napi-key-aliases:\n  " +
		first +
		(": initial\napi" +
			"-key-rules:\n " +
			" ") +
		first +
		(":\n    blocked" +
			"-models: " +
			"[model-a]\ngem" +
			"ini-api-key:" +
			"\n  - api-key:" +
			" ") +
		first +
		"\n"
	for _, name := range []string{
		"keep", "rename_alias", "new_alias", "new_rule", "reorder_and_share",
		"client_to_upstream", "upstream_to_client", "client_to_management", "ordinary_value", "ordinary_map_key",
		"nested_alias_key", "duplicate_upstream", "stale_etag",
	} {
		t.Run(
			name, func(t *testing.T) {
				t.Setenv("WRITABLE_PATH", "")
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				h := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
				t.Cleanup(func() { _ = h.Close() })
				request := func(handler gin.HandlerFunc, method, body, etag string) *httptest.ResponseRecorder {
					t.Helper()
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(method, "/config.yaml", strings.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/yaml")
					if etag != "" {
						c.Request.Header.Set("If-Match", etag)
					}
					handler(c)
					for _, raw := range []string{first, second} {
						if strings.Contains(recorder.Body.String(), raw) {
							t.Fatal("management response disclosed a client key")
						}
					}
					return recorder
				}
				get := request(h.GetConfigYAML, http.MethodGet, "", "")
				if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
					t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
				}
				var payload map[string]any
				if err := yaml.Unmarshal(get.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				keys := payload["api-keys"].([]any)
				firstRef, secondRef := keys[0].(string), keys[1].(string)
				aliases := payload["api-key-aliases"].(map[string]any)
				rules := payload["api-key-rules"].(map[string]any)
				upstream := payload["gemini-api-key"].([]any)[0].(map[string]any)
				upstreamRef := upstream["api-key"].(string)
				if !isConfigYAMLPlaceholder(firstRef) || !isConfigYAMLPlaceholder(secondRef) ||
					firstRef == secondRef ||
					upstreamRef == firstRef ||
					aliases[firstRef] != "initial" ||
					rules[firstRef] == nil {
					t.Fatal(
						"configured client key references do not share the required identity or cross secret types",
					)
				}
				wantStatus := http.StatusOK
				wantOriginal := []byte(original)
				wantKeys := []string{first, second}
				switch name {
				case "keep":
				case "rename_alias":
					aliases[firstRef] = "renamed"
				case "new_alias":
					aliases[secondRef] = "second"
				case "new_rule":
					rules[secondRef] = map[string]any{"blocked-models": []any{"model-b"}}
				case "reorder_and_share":
					payload["api-keys"] = []any{secondRef, firstRef}
					wantKeys = []string{second, first}
					aliases[firstRef] = "renamed"
					aliases[secondRef] = "second"
					rules[secondRef] = map[string]any{"blocked-models": []any{"model-b"}}
				case "client_to_upstream":
					upstream["api-key"] = firstRef
					wantStatus = http.StatusUnprocessableEntity
				case "upstream_to_client":
					aliases[upstreamRef] = "invalid"
					wantStatus = http.StatusUnprocessableEntity
				case "client_to_management":
					payload["remote-management"] = map[string]any{"secret-key": firstRef}
					wantStatus = http.StatusUnprocessableEntity
				case "ordinary_value":
					payload["proxy-url"] = firstRef
					wantStatus = http.StatusUnprocessableEntity
				case "ordinary_map_key":
					payload[firstRef] = "invalid"
					wantStatus = http.StatusUnprocessableEntity
				case "nested_alias_key":
					payload["plugin"] = map[string]any{"api-key-aliases": map[string]any{firstRef: "invalid"}}
					wantStatus = http.StatusUnprocessableEntity
				case "duplicate_upstream":
					payload["gemini-api-key"] = []any{upstream, map[string]any{"api-key": upstreamRef}}
					wantStatus = http.StatusUnprocessableEntity
				case "stale_etag":
					aliases[secondRef] = "second"
					wantOriginal = []byte(original + "debug: true\n")
					if err := os.WriteFile(path, wantOriginal, 0600); err != nil {
						t.Fatal(err)
					}
					wantStatus = http.StatusPreconditionFailed
				}
				body, err := yaml.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if wantStatus == http.StatusOK {
					validated := request(h.ValidateConfigYAML, http.MethodPost, string(body), "")
					var result struct {
						Valid bool `json:"valid"`
					}
					if err = json.Unmarshal(
						validated.Body.Bytes(), &result,
					); err != nil || validated.Code != http.StatusOK || !result.Valid {
						t.Fatalf(
							"prepared validation status=%d body=%s error=%v",
							validated.Code,
							validated.Body.String(),
							err,
						)
					}
				}
				saved := request(h.PutConfigYAML, http.MethodPut, string(body), get.Header().Get("ETag"))
				if saved.Code != wantStatus {
					t.Fatalf("save status=%d want=%d body=%s", saved.Code, wantStatus, saved.Body.String())
				}
				stored, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if wantStatus != http.StatusOK {
					if !bytes.Equal(stored, wantOriginal) {
						t.Fatal("rejected save changed the config")
					}
					return
				}
				var cfg config.Config
				if err = yaml.Unmarshal(stored, &cfg); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(
					cfg.APIKeys, wantKeys,
				) ||
					len(cfg.GeminiKey) !=
						1 ||
					cfg.GeminiKey[0].APIKey !=
						first ||
					cfg.APIKeyAliases[first] ==
						"" ||
					!reflect.DeepEqual(
						cfg.APIKeyRules[first].BlockedModels, []string{"model-a"},
					) || strings.Contains(string(stored), configYAMLPlaceholderPrefix) {
					t.Fatal(
						"conditional save did not preserve the existing keys, aliases, rules and upstream credential",
					)
				}
				if (name == "rename_alias" || name == "reorder_and_share") &&
					cfg.APIKeyAliases[first] != "renamed" {
					t.Fatal("alias rename was not saved")
				}
				if (name == "new_alias" || name == "reorder_and_share") &&
					cfg.APIKeyAliases[second] != "second" {
					t.Fatal("new alias was not saved")
				}
				if (name == "new_rule" || name == "reorder_and_share") && !reflect.DeepEqual(
					cfg.APIKeyRules[second].BlockedModels, []string{"model-b"},
				) {
					t.Fatal("new rule was not saved")
				}
			},
		)
	}
}
