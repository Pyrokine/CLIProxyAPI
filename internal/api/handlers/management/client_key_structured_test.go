package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clientaccess "github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestConfigYAMLStructuredClientReferencesThroughAccessProvider(t *testing.T) {
	const first = "synthetic-client-first-key"
	const second = "synthetic-client-second-key"
	const rootOnly = "synthetic-client-root-only"
	var originalProvider sdkaccess.Provider
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == sdkaccess.DefaultAccessProviderName {
			originalProvider = provider
		}
	}
	t.Cleanup(
		func() {
			if originalProvider != nil {
				sdkaccess.RegisterProvider(sdkaccess.ProviderTypeConfigAPIKey, originalProvider)
			} else {
				sdkaccess.UnregisterProvider(sdkaccess.ProviderTypeConfigAPIKey)
			}
		},
	)
	for _, schema := range []struct {
		name, field, entries string
	}{
		{"scalar", "api-key-entries", "        - " + first + " # retain scalar\n        - " + second + "\n"},
		{
			"object", "api-key-entries",
			"        - api-key: " +
				first +
				("\n          label: first\n          priority: 7\n          " +
					"metadata: {region: test}\n        - api-key: ") +
				second +
				("\n          " +
					"label: " +
					"second\n"),
		},
		{
			"mixed", "api-key-entries",
			"        - " +
				first +
				" # retain scalar\n        - api-key: " +
				second +
				("\n          label: " +
					"second\n          " +
					"metadata: {region: " +
					"test}\n"),
		},
		{"legacy", "api-keys", "        - " + first + "\n        - " + second + "\n"},
	} {
		for _, withRoot := range []bool{false, true} {
			name := schema.name
			if withRoot {
				name += "/with-root"
			}
			t.Run(
				name, func(t *testing.T) {
					t.Setenv("WRITABLE_PATH", "")
					source := "port: 8317\n"
					if withRoot {
						source += "api-keys: [" + first + ", " + rootOnly + "]\n"
					}
					source += "auth:\n  providers:\n    config-api-key:\n      " + schema.field + ":\n" + schema.entries
					source += "api-key-aliases:\n  " +
						first +
						": initial\napi-key-rules:\n  " +
						first +
						(":\n    blocked" +
							"-models: " +
							"[model-a]\n")
					source += "gemini-api-key:\n  - api-key: " + first + "\n    base-url: https://synthetic.invalid\n"
					path := filepath.Join(t.TempDir(), "config.yaml")
					if err := os.WriteFile(path, []byte(source), 0600); err != nil {
						t.Fatal(err)
					}
					cfg, err := config.LoadConfig(path)
					if err != nil {
						t.Fatal(err)
					}
					h := NewHandler(cfg, path, nil, withEnvSecret(""))
					t.Cleanup(func() { _ = h.Close() })
					request := func(handler gin.HandlerFunc, method, body, etag string) *httptest.ResponseRecorder {
						t.Helper()
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(method, "/config.yaml", strings.NewReader(body))
						if etag != "" {
							c.Request.Header.Set("If-Match", etag)
						}
						handler(c)
						for _, key := range []string{first, second, rootOnly} {
							if strings.Contains(recorder.Body.String(), key) {
								t.Fatal("management response disclosed a client key")
							}
						}
						return recorder
					}
					get := request(h.GetConfigYAML, http.MethodGet, "", "")
					if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
						t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
					}
					document, err := decodeConfigYAML(get.Body.Bytes())
					if err != nil {
						t.Fatal(err)
					}
					root := document.Content[0]
					provider := yamlMappingValue(
						yamlMappingValue(yamlMappingValue(root, "auth"), "providers"), "config-api-key",
					)
					entries := yamlMappingValue(provider, schema.field)
					reference := func(entry *yaml.Node) string {
						t.Helper()
						if entry == nil {
							t.Fatal("client key entry is missing")
						}
						if entry.Kind == yaml.ScalarNode {
							return entry.Value
						}
						key := yamlMappingValue(entry, "api-key")
						if key == nil {
							t.Fatal("client key value is missing")
						}
						return key.Value
					}
					firstRef, secondRef := reference(entries.Content[0]), reference(entries.Content[1])
					if !isConfigYAMLPlaceholder(firstRef) || !isConfigYAMLPlaceholder(secondRef) ||
						firstRef == secondRef {
						t.Fatal("structured client keys lack distinct opaque references")
					}
					aliases, rules := yamlMappingValue(
						root,
						"api-key-aliases",
					), yamlMappingValue(
						root,
						"api-key-rules",
					)
					if aliases.Content[0].Value != firstRef || rules.Content[0].Value != firstRef {
						t.Fatal("nested client key does not share its alias and rule identity")
					}
					if withRoot && yamlMappingValue(root, "api-keys").Content[0].Value != firstRef {
						t.Fatal("root and nested occurrences of the same client key differ")
					}
					upstream := yamlMappingValue(root, "gemini-api-key").Content[0]
					upstreamRef := reference(upstream)
					if upstreamRef == firstRef {
						t.Fatal("upstream and client keys share a secret type")
					}
					aliases.Content[1].Value = "renamed"
					setYAMLMappingValue(
						aliases, secondRef, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "second"},
					)
					newRule, err := yamlNodeFromJSONObject(map[string]any{"blocked-models": []any{"model-b"}})
					if err != nil {
						t.Fatal(err)
					}
					setYAMLMappingValue(rules, secondRef, newRule)
					if entries.Content[1].Kind == yaml.MappingNode {
						label := yamlMappingValue(entries.Content[1], "label")
						if label == nil {
							t.Fatal("second client key label is missing")
						}
						label.Value = "edited second"
					}
					entries.Content[0], entries.Content[1] = entries.Content[1], entries.Content[0]
					body, err := encodeConfigYAML(document)
					if err != nil {
						t.Fatal(err)
					}
					validated := request(h.ValidateConfigYAML, http.MethodPost, string(body), "")
					var validation struct {
						Valid bool `json:"valid"`
					}
					if err = json.Unmarshal(validated.Body.Bytes(), &validation); err != nil ||
						!validation.Valid {
						t.Fatalf(
							"validation status=%d body=%s error=%v",
							validated.Code,
							validated.Body.String(),
							err,
						)
					}
					saved := request(h.PutConfigYAML, http.MethodPut, string(body), get.Header().Get("ETag"))
					if saved.Code != http.StatusOK {
						t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
					}
					stored, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					storedDocument, err := decodeConfigYAML(stored)
					if err != nil {
						t.Fatal(err)
					}
					storedRoot := storedDocument.Content[0]
					storedProvider := yamlMappingValue(
						yamlMappingValue(
							yamlMappingValue(storedRoot, "auth"), "providers",
						), "config-api-key",
					)
					storedEntries := yamlMappingValue(storedProvider, schema.field)
					if storedEntries == nil || storedEntries.Kind != yaml.SequenceNode ||
						reference(storedEntries.Content[0]) != second ||
						reference(storedEntries.Content[1]) != first ||
						storedEntries.Content[0].Kind != entries.Content[0].Kind ||
						storedEntries.Content[1].Kind != entries.Content[1].Kind {
						t.Fatal("save changed the client key schema, order or scalar/object types")
					}
					if (yamlMappingValue(storedRoot, "api-keys") != nil) != withRoot || strings.Contains(
						string(stored), configYAMLPlaceholderPrefix,
					) {
						t.Fatal("save migrated the original root/nested format or retained a placeholder")
					}
					if schema.name == "scalar" || schema.name == "mixed" {
						if !bytes.Contains(stored, []byte("# retain scalar")) {
							t.Fatal("scalar comment was lost")
						}
					}
					if entries.Content[0].Kind == yaml.MappingNode {
						if label := yamlMappingValue(storedEntries.Content[0], "label"); label == nil ||
							label.Value != "edited second" {
							t.Fatal("object metadata edit was lost")
						}
					}
					metadataEntry := storedEntries.Content[0]
					if schema.name == "object" {
						metadataEntry = storedEntries.Content[1]
						if priority := yamlMappingValue(metadataEntry, "priority"); priority == nil ||
							priority.Value != "7" {
							t.Fatal("object priority was lost")
						}
					}
					if schema.name == "object" || schema.name == "mixed" {
						metadata := yamlMappingValue(metadataEntry, "metadata")
						if region := yamlMappingValue(metadata, "region"); region == nil ||
							region.Value != "test" {
							t.Fatal("structured metadata was lost")
						}
					}
					loaded, err := config.LoadConfig(path)
					if err != nil || loaded.APIKeyAliases[first] != "renamed" ||
						loaded.APIKeyAliases[second] != "second" ||
						!reflect.DeepEqual(
							loaded.APIKeyRules[second].BlockedModels, []string{"model-b"},
						) {
						t.Fatalf("saved alias/rule load error=%v", err)
					}
					manager := sdkaccess.NewManager()
					if _, err = clientaccess.ApplyAccessProviders(
						manager, nil, loaded,
					); err != nil || len(manager.Providers()) != 1 {
						t.Fatalf(
							"access provider registration error=%v count=%d",
							err,
							len(manager.Providers()),
						)
					}
					wantKeys := []string{first, second}
					if withRoot {
						wantKeys = append(wantKeys, rootOnly)
					}
					for _, key := range wantKeys {
						req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
						req.Header.Set("Authorization", "Bearer "+key)
						result, authErr := manager.Authenticate(context.Background(), req)
						if authErr != nil || result == nil || result.Principal != key {
							t.Fatalf("saved key was not authenticated: result=%v error=%v", result, authErr)
						}
					}
					invalidRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					invalidRequest.Header.Set("Authorization", "Bearer synthetic-not-configured")
					if result, authErr := manager.Authenticate(
						context.Background(), invalidRequest,
					); authErr == nil || result != nil {
						t.Fatal("unconfigured key was authenticated")
					}
					for _, invalid := range []string{"upstream", "metadata", "other-provider", "upstream-to-client"} {
						t.Run(
							invalid, func(t *testing.T) {
								current := request(h.GetConfigYAML, http.MethodGet, "", "")
								rejected, errDecode := decodeConfigYAML(current.Body.Bytes())
								if errDecode != nil {
									t.Fatal(errDecode)
								}
								badRoot := rejected.Content[0]
								badProviders := yamlMappingValue(yamlMappingValue(badRoot, "auth"), "providers")
								badProvider := yamlMappingValue(badProviders, "config-api-key")
								badEntries := yamlMappingValue(badProvider, schema.field)
								badUpstream := yamlMappingValue(
									yamlMappingValue(badRoot, "gemini-api-key").Content[0], "api-key",
								)
								switch invalid {
								case "upstream":
									badUpstream.Value = reference(badEntries.Content[0])
								case "metadata":
									badEntries.Content = append(
										badEntries.Content, &yaml.Node{
											Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
												{Kind: yaml.ScalarNode, Value: "label"},
												{
													Kind:  yaml.ScalarNode,
													Value: reference(badEntries.Content[0]),
												},
											},
										},
									)
								case "other-provider":
									other, errNode := yamlNodeFromJSONObject(
										map[string]any{
											"api-key-entries": []any{reference(badEntries.Content[0])},
										},
									)
									if errNode != nil {
										t.Fatal(errNode)
									}
									setYAMLMappingValue(badProviders, "other-provider", other)
								case "upstream-to-client":
									if badEntries.Content[0].Kind == yaml.ScalarNode {
										badEntries.Content[0].Value = badUpstream.Value
									} else {
										key := yamlMappingValue(badEntries.Content[0], "api-key")
										if key == nil {
											t.Fatal("client key value is missing")
										}
										key.Value = badUpstream.Value
									}
								}
								badBody, errEncode := encodeConfigYAML(rejected)
								if errEncode != nil {
									t.Fatal(errEncode)
								}
								failed := request(
									h.PutConfigYAML,
									http.MethodPut,
									string(badBody),
									current.Header().Get("ETag"),
								)
								if failed.Code != http.StatusUnprocessableEntity {
									t.Fatalf(
										"invalid %s status=%d body=%s",
										invalid,
										failed.Code,
										failed.Body.String(),
									)
								}
								unchanged, errRead := os.ReadFile(path)
								if errRead != nil || !bytes.Equal(unchanged, stored) {
									t.Fatal("rejected reference changed the saved configuration")
								}
							},
						)
					}
				},
			)
		}
	}
}
