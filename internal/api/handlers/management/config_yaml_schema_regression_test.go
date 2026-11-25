package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func newSchemaYAMLTest(t *testing.T, source string) (*Handler, *gin.Engine) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil, withEnvSecret(""))
	t.Cleanup(
		func() {
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		},
	)
	router := gin.New()
	router.GET("/config.yaml", h.GetConfigYAML)
	router.PUT("/config.yaml", h.PutConfigYAML)
	router.POST("/config.yaml/validate", h.ValidateConfigYAML)
	return h, router
}

func schemaYAMLRequest(router *gin.Engine, method, path, body, etag string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if strings.HasPrefix(path, "/config.yaml") {
		request.Header.Set("Content-Type", "application/yaml")
	} else {
		request.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestConfigYAMLSchemaProviderAndSDKPreserveBlockScalar(t *testing.T) {
	const source = `port: 8317
payload:
  override:
    - models: [{name: fixture}]
      params:
        note: |-
          # first line in the value
          second line
`
	h, router := newSchemaYAMLTest(t, source)
	router.POST("/codex-api-key", h.PostCodexKey)
	post := schemaYAMLRequest(router, http.MethodPost, "/codex-api-key", `{"api-key":"synthetic-codex"}`, "")
	if post.Code != http.StatusBadRequest {
		t.Fatalf("missing Codex endpoint returned %d: %s", post.Code, post.Body)
	}
	post = schemaYAMLRequest(
		router, http.MethodPost, "/codex-api-key",
		`{"api-key":"synthetic-codex","base-url":"https://codex.fixture.invalid/v1"}`, "",
	)
	if post.Code != http.StatusCreated {
		t.Fatalf("valid Codex endpoint returned %d: %s", post.Code, post.Body)
	}
	checkValue := func(path string) {
		t.Helper()
		got, err := config.LoadConfig(path)
		if err != nil || len(got.Payload.Override) != 1 || got.Payload.Override[0].Params["note"] != "# first line in the value\nsecond line" {
			t.Fatalf("file load did not preserve the scalar: %v", err)
		}
	}
	checkValue(h.configFilePath)
	sdkPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteConfig(sdkPath, []byte(source)); err != nil {
		t.Fatal(err)
	}
	checkValue(sdkPath)
}

func TestConfigYAMLSchemaPreservesStructuredZeros(t *testing.T) {
	for _, tc := range []struct {
		path, body, readback string
		put, get             func(*Handler, *gin.Context)
		matches              func(*config.Config) bool
	}{
		{
			"/auto-refresh-interval", `{"value":0}`, `"auto-refresh-interval":0`,
			(*Handler).PutAutoRefreshInterval, (*Handler).GetAutoRefreshInterval,
			func(cfg *config.Config) bool { return cfg.AutoRefreshInterval == 0 },
		},
		{
			"/error-logs-max-files", `{"value":0}`, `"error-logs-max-files":0`,
			(*Handler).PutErrorLogsMaxFiles, (*Handler).GetErrorLogsMaxFiles,
			func(cfg *config.Config) bool { return cfg.ErrorLogsMaxFiles == 0 },
		},
		{
			"/ws-auth", `{"value":false}`, `"ws-auth":false`,
			(*Handler).PutWebsocketAuth, (*Handler).GetWebsocketAuth,
			func(cfg *config.Config) bool { return !cfg.WebsocketAuth },
		},
		{
			"/image-artifact-cache", `{"retention_days":0}`, `"retention_days":0`,
			(*Handler).PutImageArtifactCache, (*Handler).GetImageArtifactCache,
			func(cfg *config.Config) bool {
				return cfg.ImageArtifactCache.RetentionDays == 0 && cfg.ImageArtifactCache.MaxTotalSizeMB == 10240
			},
		},
		{
			"/image-artifact-cache", `{"max_total_size_mb":0}`, `"max_total_size_mb":0`,
			(*Handler).PutImageArtifactCache, (*Handler).GetImageArtifactCache,
			func(cfg *config.Config) bool {
				return cfg.ImageArtifactCache.RetentionDays == 7 && cfg.ImageArtifactCache.MaxTotalSizeMB == 0
			},
		},
		{
			"/image-artifact-cache", `{"retention_days":0,"max_total_size_mb":0}`, `"max_total_size_mb":0`,
			(*Handler).PutImageArtifactCache, (*Handler).GetImageArtifactCache,
			func(cfg *config.Config) bool { return cfg.ImageArtifactCache == (config.ImageArtifactCache{}) },
		},
	} {
		t.Run(
			tc.path+tc.body, func(t *testing.T) {
				h, router := newSchemaYAMLTest(t, "port: 8317\n")
				if tc.matches(h.configSnapshot()) {
					t.Fatal("fixture already defaults to the value being written")
				}
				router.PATCH(tc.path, func(c *gin.Context) { tc.put(h, c) })
				router.GET(tc.path, func(c *gin.Context) { tc.get(h, c) })
				changed := schemaYAMLRequest(router, http.MethodPatch, tc.path, tc.body, "")
				if changed.Code != http.StatusOK {
					t.Fatalf("structured write returned %d: %s", changed.Code, changed.Body)
				}
				read := schemaYAMLRequest(router, http.MethodGet, tc.path, "", "")
				if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), tc.readback) {
					t.Fatalf("structured readback returned %d: %s", read.Code, read.Body)
				}
				loaded, err := config.LoadConfig(h.configFilePath)
				if err != nil || !tc.matches(loaded) {
					t.Fatalf("fresh file load did not preserve this setting: %v", err)
				}
			},
		)
	}
}

func TestConfigYAMLSchemaPrunesPersistedRuleMembers(t *testing.T) {
	const key = "synthetic-client-policy"
	const original = `port: 8317
api-keys: [` + key + `]
api-key-rules:
  ` + key + `:
    blocked-models: [fixture-model]
    providers: [codex] # Existing provider note
    future-field: untouched
`
	h, router := newSchemaYAMLTest(t, original)
	router.PUT("/oauth-excluded-models", h.PutOAuthExcludedModels)
	for _, update := range []string{`{"codex":["fixture-model"]}`, `{}`} {
		saved := schemaYAMLRequest(router, http.MethodPut, "/oauth-excluded-models", update, "")
		if saved.Code != http.StatusOK {
			t.Fatalf("set exclusions returned %d: %s", saved.Code, saved.Body)
		}
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := loaded.APIKeyRule(key)
	if !ok || len(rule.Providers) != 1 || rule.Providers[0] != "codex" || loaded.APIKeyBlocksModel(
		key, "fixture-model",
	) ||
		len(loaded.OAuthExcludedModels) != 0 || h.configSnapshot().APIKeyBlocksModel(key, "fixture-model") {
		t.Fatal("file reload reinstated a removed restriction or lost the provider restriction")
	}
	stored, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "blocked-models:") || !strings.Contains(
		string(stored), "future-field: untouched",
	) ||
		!strings.Contains(string(stored), "# Existing provider note") {
		t.Fatal("writeback did not preserve retained rule data and comment")
	}
}

func TestConfigYAMLSchemaRedactsOnlySensitiveLeavesAndPreservesScalarContent(t *testing.T) {
	const original = `port: 8317
api-keys: [synthetic-client-secret]
credential-concurrency:
  cpa-heartbeat-timeout: 3s # Heartbeat comment
  max-limit: 500
  observation-barrier-revision: 0
  private-token: synthetic-concurrency-secret
plugins:
  store-auth:
    - match: https://store.fixture.invalid
      type: bearer
      token-env: FIXTURE_STORE_TOKEN
      token: synthetic-store-secret
payload:
  override:
    - models: [{name: fixture}]
      params:
        max_tokens: 0
        max_completion_tokens: 280
        access_token: synthetic-payload-secret
        note: |-
          # literal payload line
          remains part of the value
  default-raw:
    - models: [{name: fixture}]
      params:
        max_output_tokens: '512'
        access_token: '"synthetic-raw-secret"'
  default:
    - models: [{name: fixture}]
      params:
        max_tokens: synthetic-invalid-budget
extension:
  store-auth:
    - token-env: SYNTHETIC_PRIVATE_VALUE
  credential-concurrency:
    max-limit: 793
  notes:
    # Keep this standalone comment
    folded: >-
      first line
      # folded content
      last line
`
	h, router := newSchemaYAMLTest(t, original)
	get := schemaYAMLRequest(router, http.MethodGet, "/config.yaml", "", "")
	redacted := get.Body.String()
	if get.Code != http.StatusOK || get.Header().Get("ETag") == "" || get.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("YAML source read returned %d", get.Code)
	}
	for _, want := range []string{
		"cpa-heartbeat-timeout: 3s # Heartbeat comment", "max-limit: 500", "observation-barrier-revision: 0",
		"token-env: FIXTURE_STORE_TOKEN", "max_tokens: 0", "max_completion_tokens: 280", "max_output_tokens: '512'",
		"# Keep this standalone comment",
	} {
		if !strings.Contains(redacted, want) {
			t.Errorf("declared value missing: %q", want)
		}
	}
	for _, secret := range []string{
		"synthetic-client-secret", "synthetic-concurrency-secret", "synthetic-store-secret", "synthetic-payload-secret",
		"synthetic-raw-secret", "synthetic-invalid-budget", "SYNTHETIC_PRIVATE_VALUE", "max-limit: 793",
	} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redaction exposed a private value: %q", secret)
		}
	}
	changed := strings.NewReplacer(
		"max-limit: 500", "max-limit: 600", "FIXTURE_STORE_TOKEN", "FIXTURE_NEW_STORE_TOKEN",
		"max_completion_tokens: 280", "max_completion_tokens: 300",
	).Replace(redacted)
	if valid := validateSchemaYAMLSnapshot(t, router, changed); !valid.Valid {
		t.Fatalf("redacted source preflight rejected compatible values: %+v", valid.Errors)
	}
	put := schemaYAMLRequest(router, http.MethodPut, "/config.yaml", changed, get.Header().Get("ETag"))
	if put.Code != http.StatusOK {
		t.Fatalf("conditional YAML save returned %d: %s", put.Code, put.Body)
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CredentialConcurrency.MaxLimit != 600 || loaded.Plugins.StoreAuth[0].TokenEnv != "FIXTURE_NEW_STORE_TOKEN" ||
		loaded.Payload.Override[0].Params["max_tokens"] != 0 || loaded.Payload.Override[0].Params["max_completion_tokens"] != 300 ||
		loaded.Payload.Override[0].Params["access_token"] != "synthetic-payload-secret" ||
		loaded.Payload.Override[0].Params["note"] != "# literal payload line\nremains part of the value" ||
		loaded.Payload.DefaultRaw[0].Params["access_token"] != `"synthetic-raw-secret"` || loaded.APIKeys[0] != "synthetic-client-secret" {
		t.Fatal("readback differs from the edited typed/configured values")
	}
	stored, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	var readback struct {
		Extension struct {
			Notes struct{ Folded string }
		} `yaml:"extension"`
	}
	if err := yaml.Unmarshal(stored, &readback); err != nil ||
		readback.Extension.Notes.Folded != "first line # folded content last line" ||
		!strings.Contains(string(stored), "# Keep this standalone comment") ||
		!strings.Contains(string(stored), "private-token: synthetic-concurrency-secret") ||
		!strings.Contains(string(stored), "token: synthetic-store-secret") ||
		!strings.Contains(string(stored), "max-limit: 793") {
		t.Fatalf("file did not preserve comments, literal values, or unknown fields: %v", err)
	}
}

type schemaYAMLValidationResponse struct {
	Valid    bool                    `json:"valid"`
	Errors   []configValidationError `json:"errors"`
	Warnings []string                `json:"warnings"`
}

func validateSchemaYAMLSnapshot(t *testing.T, router *gin.Engine, source string) schemaYAMLValidationResponse {
	t.Helper()
	response := schemaYAMLRequest(router, http.MethodPost, "/config.yaml/validate", source, "")
	var status schemaYAMLValidationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || response.Code != http.StatusOK {
		t.Fatalf("preflight returned %d: %s", response.Code, response.Body)
	}
	return status
}

func TestConfigYAMLSchemaValidatesChangedRulesWithoutBlockingLegacyRows(t *testing.T) {
	const empty = "port: 8317\n"
	for _, tc := range []struct {
		name, field, bad, changed, fixed string
		active                           func(*config.Config) bool
	}{
		{
			"default-raw", "payload.default-raw[0].params.sample",
			"payload:\n  default-raw:\n    - models: [{name: fixture}]\n      params: {sample: 'not JSON', temperature: 0.8}\n",
			"not JSON", "'false'",
			func(cfg *config.Config) bool {
				return len(cfg.Payload.DefaultRaw) == 1 && cfg.Payload.DefaultRaw[0].Params["sample"] == "false"
			},
		},
		{
			"override-raw", "payload.override-raw[0].params.sample",
			"payload:\n  override-raw:\n    - models: [{name: fixture}]\n      params: {sample: 'not JSON', temperature: 0.8}\n",
			"not JSON", "'0'",
			func(cfg *config.Config) bool {
				return len(cfg.Payload.OverrideRaw) == 1 && cfg.Payload.OverrideRaw[0].Params["sample"] == "0"
			},
		},
		{
			"oauth-regex", "oauth-request-scoped-errors.codex[0].match-regexr[0]",
			"oauth-request-scoped-errors:\n  codex:\n    - status: 429\n      match-regexr: ['[']\n      action: continue\n",
			"status: 429", "[rate]",
			func(cfg *config.Config) bool {
				return len(cfg.OAuthRequestScopedErrors["codex"]) == 1 && cfg.OAuthRequestScopedErrors["codex"][0].MatchRegexr[0] == "[rate]"
			},
		},
		{
			"plugin-match", "plugins.store-auth[0].match",
			"plugins:\n  store-auth:\n    - match: ''\n      type: bearer\n      token-env: FIXTURE_TOKEN_ENV\n    - match: https://other.fixture.invalid\n      type: none\n",
			"FIXTURE_TOKEN_ENV", "https://store.fixture.invalid",
			func(cfg *config.Config) bool {
				return len(cfg.Plugins.StoreAuth) == 2 && cfg.Plugins.StoreAuth[0].Match == "https://store.fixture.invalid" &&
					cfg.Plugins.StoreAuth[1].Match == "https://other.fixture.invalid"
			},
		},
		{
			"codex-endpoint", "codex-api-key[0].base-url",
			"codex-api-key:\n  - api-key: synthetic-codex-secret\n    prefix: initially\n",
			"prefix: initially", "https://codex.fixture.invalid/v1",
			func(cfg *config.Config) bool {
				return len(cfg.CodexKey) == 1 && cfg.CodexKey[0].BaseURL == "https://codex.fixture.invalid/v1"
			},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				h, router := newSchemaYAMLTest(t, empty)
				bad := empty + tc.bad
				assertRejected := func(source, etag string) {
					t.Helper()
					result := validateSchemaYAMLSnapshot(t, router, source)
					if result.Valid || len(result.Errors) != 1 || result.Errors[0].Field != tc.field {
						t.Fatalf("wanted error at %q: %+v", tc.field, result)
					}
					before, err := os.ReadFile(h.configFilePath)
					if err != nil {
						t.Fatal(err)
					}
					put := schemaYAMLRequest(router, http.MethodPut, "/config.yaml", source, etag)
					var issue configValidationError
					if err := json.Unmarshal(
						put.Body.Bytes(), &issue,
					); err != nil || put.Code != http.StatusUnprocessableEntity || issue.Field != tc.field {
						t.Fatalf("invalid PUT returned %d: %s", put.Code, put.Body)
					}
					after, err := os.ReadFile(h.configFilePath)
					if err != nil || string(after) != string(before) {
						t.Fatal("a rejected edit changed the config file")
					}
				}
				assertRejected(bad, "")
				if err := os.WriteFile(h.configFilePath, []byte(bad), 0o600); err != nil {
					t.Fatal(err)
				}
				get := schemaYAMLRequest(router, http.MethodGet, "/config.yaml", "", "")
				if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
					t.Fatalf("could not read legacy YAML: %d", get.Code)
				}
				legacy := get.Body.String() + "debug: true\n"
				preview := validateSchemaYAMLSnapshot(t, router, legacy)
				if !preview.Valid || !strings.Contains(strings.Join(preview.Warnings, "\n"), tc.field) {
					t.Fatalf("unchanged invalid rule must be shown as a warning: %+v", preview)
				}
				etag := get.Header().Get("ETag")
				assertRejected(strings.Replace(legacy, tc.changed, tc.changed+"1", 1), etag)
				put := schemaYAMLRequest(router, http.MethodPut, "/config.yaml", legacy, etag)
				if put.Code != http.StatusOK || !h.configSnapshot().Debug {
					t.Fatalf("unrelated edit returned %d: %s", put.Code, put.Body)
				}
				router.PATCH("/debug", h.PutDebug)
				patch := schemaYAMLRequest(router, http.MethodPatch, "/debug", `{"value":false}`, "")
				if patch.Code != http.StatusOK || h.configSnapshot().Debug {
					t.Fatalf("unrelated structured edit returned %d: %s", patch.Code, patch.Body)
				}
				get = schemaYAMLRequest(router, http.MethodGet, "/config.yaml", "", "")
				fixed := get.Body.String()
				switch tc.name {
				case "default-raw", "override-raw":
					fixed = strings.Replace(fixed, "'not JSON'", tc.fixed, 1)
				case "oauth-regex":
					fixed = strings.Replace(fixed, "'['", "'"+tc.fixed+"'", 1)
				case "plugin-match":
					fixed = strings.Replace(fixed, "match: ''", "match: "+tc.fixed, 1)
				case "codex-endpoint":
					fixed = strings.Replace(fixed, "prefix: initially", "prefix: initially\n    base-url: "+tc.fixed, 1)
				}
				if fixed == get.Body.String() {
					t.Fatal("the source lost the legacy row")
				}
				put = schemaYAMLRequest(router, http.MethodPut, "/config.yaml", fixed, get.Header().Get("ETag"))
				if put.Code != http.StatusOK {
					t.Fatalf("corrected configuration returned %d: %s", put.Code, put.Body)
				}
				loaded, err := config.LoadConfig(h.configFilePath)
				if err != nil || !tc.active(loaded) {
					t.Fatalf("correcting the rule did not activate the saved row: %v", err)
				}
			},
		)
	}
}
