package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/safemode"
	"gopkg.in/yaml.v3"
)

func TestSchemaDefaultsPreserveBlockScalars(t *testing.T) {
	const fixture = `port: 8317
# Heading retained
plugin-extension:
  # Nested note
  literal: |-
    # literal content
    still literal
  folded: >-
    leading text
    # folded content
    trailing text
payload:
  override:
    - models: [{name: fixture}]
      params:
        note: |+
          # payload text
          succeeds
`
	for _, name := range []string{"ordinary", "startup", "nested"} {
		t.Run(
			name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
					t.Fatal(err)
				}
				original, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				switch name {
				case "ordinary":
					original.Debug = true
					if err := SaveConfigPreserveComments(path, original); err != nil {
						t.Fatal(err)
					}
				case "startup":
					EnsureDefaultsInFile(path, original)
				case "nested":
					if err := SaveConfigPreserveCommentsUpdateNestedScalar(
						path, []string{"plugin-extension", "setting"}, "changed",
					); err != nil {
						t.Fatal(err)
					}
				}
				actual, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(actual.Payload.Override) != 1 || actual.Payload.Override[0].Params["note"] != "# payload text\nsucceeds\n" {
					t.Fatalf("payload content changed after save: %#v", actual.Payload.Override)
				}
				file, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var value struct {
					Extension struct{ Literal, Folded string } `yaml:"plugin-extension"`
				}
				if err := yaml.Unmarshal(file, &value); err != nil {
					t.Fatal(err)
				}
				if value.Extension.Literal != "# literal content\nstill literal" ||
					value.Extension.Folded != "leading text # folded content trailing text" {
					t.Fatalf("block scalar changed: %+v", value.Extension)
				}
				for _, comment := range []string{"# Heading retained", "  # Nested note"} {
					if !strings.Contains(string(file), comment) {
						t.Fatalf("missing %q after save", comment)
					}
				}
			},
		)
	}
}

func TestSchemaDefaultsSDKCommentNormalization(t *testing.T) {
	const ordinary = "  # a comment\nport: 8317\n"
	if got := string(NormalizeCommentIndentation([]byte(ordinary))); got != "# a comment\nport: 8317\n" {
		t.Fatalf("comment remained indented: %q", got)
	}
	for _, style := range []string{"|-", ">-"} {
		input := "port: 8317\n---\nblock: " + style + "\n  # text\n  more text\n"
		if got := string(NormalizeCommentIndentation([]byte(input))); got != input {
			t.Fatalf("changed document with %s scalar: %q", style, got)
		}
	}
}

func TestSchemaDefaultsPreservesModelRefreshZeroAndPolicyMapDeletion(t *testing.T) {
	const key = "synthetic-policy"
	const input = `port: 8317
api-key-rules:
  ` + key + `:
    model-mappings:
      retain: target-one # Keep this mapping description
      remove: target-two
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.ModelRefreshInterval != defaultModelRefreshInterval {
		t.Fatalf("load omitted refresh default: %v", err)
	}
	cfg.ModelRefreshInterval = 0
	delete(cfg.APIKeyRules[key].ModelMappings, "remove")
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.ModelRefreshInterval != 0 {
		t.Fatalf("zero model refresh interval did not survive reload: %v", err)
	}
	mappings := reloaded.APIKeyRules[key].ModelMappings
	if len(mappings) != 1 || mappings["retain"] != "target-one" {
		t.Fatalf("saved mappings are %v", mappings)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(stored), "# Keep this mapping description") {
		t.Fatalf("mapping comment did not survive: %v", err)
	}
}

func TestSchemaDefaultsStartupOnlyWritesApplicableOmittedValues(t *testing.T) {
	const inactive = `codex-api-key:
  # Legacy key stays available to edit in YAML
  - api-key: synthetic-inactive-codex
    future-option: retained
  - api-key: synthetic-active-codex
    base-url: https://codex.fixture.invalid/v1
    models:
      - name: fixture-upstream
        alias: fixture-alias
        use-max-completion-tokens: false
`
	for _, tc := range []struct {
		name   string
		input  string
		want   []string
		absent []string
	}{
		{
			name: "disabled", input: "port: 8317\ntls:\n  enable: true\n",
			absent: []string{
				"session-affinity-ttl:", "session-affinity-subagents:", "check-interval:", "alpha-search:",
				"support-configuration-update:",
			},
		},
		{
			name:  "enabled",
			input: "port: 8317\nrouting: {session-affinity: true}\nremote-management: {auto-check-update: true}\n" + inactive,
			want: []string{
				"session-affinity-ttl: 1h", "session-affinity-subagents: true", "check-interval: 180",
				"alpha-search: false", "support-configuration-update: false", "synthetic-inactive-codex",
				"future-option: retained", "# Legacy key stays available to edit in YAML",
			},
		},
		{
			name: "explicit", input: `port: 8317
routing:
  strategy: fill-first
  session-affinity: true
  session-affinity-ttl: 15m
  session-affinity-subagents: false
remote-management:
  auto-check-update: true
  check-interval: 45
  auto-update-cpa: true
`,
			want: []string{
				"strategy: fill-first", "session-affinity-ttl: 15m", "session-affinity-subagents: false",
				"check-interval: 45", "auto-update-cpa: true",
			},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
					t.Fatal(err)
				}
				config, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				EnsureDefaultsInFile(path, config)
				loaded, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "enabled" && (len(loaded.CodexKey) != 1 || loaded.CodexKey[0].APIKey != "synthetic-active-codex" ||
					loaded.CodexKey[0].DisableCooling != nil || loaded.CodexKey[0].DisableCodexCloaking != nil || loaded.CodexKey[0].RequestRetry != nil) {
					t.Fatal("startup changed Codex activation or credential inheritance")
				}
				if loaded.TLS.HTTPRedirectPort != 0 || loaded.NonStreamKeepAliveInterval != 0 || loaded.Streaming != (StreamingConfig{}) ||
					loaded.VideoResultAuthCacheTTL != "3h" || loaded.GPTImage2BaseModel != "gpt-5.4-mini" {
					t.Fatal("loaded startup defaults differ from effective defaults")
				}
				loaded.Debug = true
				if err := SaveConfigPreserveComments(path, loaded); err != nil {
					t.Fatal(err)
				}
				file, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				text := string(file)
				want := append(
					[]string{
						"http-redirect-port: 0", "keepalive-seconds: 0", "bootstrap-retries: 0",
						"nonstream-keepalive-interval: 0", "video-result-auth-cache-ttl: 3h",
						"gpt-image-2-base-model: gpt-5.4-mini",
					}, tc.want...,
				)
				if tc.name != "explicit" {
					want = append(want, "strategy: round-robin", "auto-update-cpa: false")
				}
				for _, snippet := range want {
					if !strings.Contains(text, snippet) {
						t.Errorf("missing %q", snippet)
					}
				}
				for _, snippet := range tc.absent {
					if strings.Contains(text, snippet) {
						t.Errorf("unexpected inapplicable value %q", snippet)
					}
				}
			},
		)
	}
}

func TestSchemaDefaultsLocalStartupPreservesAuthorities(t *testing.T) {
	const input = `port: 8317
antigravity:
  connection-pool:
    enabled: true
codex:
  live-media-relay:
    enabled: true
    allow-private-remote-ips: false # Keep the legacy spelling
model-catalog:
  models: [{name: legacy-model}]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	catalogPath := filepath.Join(dir, "models.json")
	catalog := `{"schema_version":1,"models":[]}`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	EnsureDefaultsInFile(path, cfg)
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := loaded.Antigravity.ConnectionPool
	if pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 2 || pool.IdleConnTimeout != "" {
		t.Fatalf("pool defaults changed transport inheritance: %+v", pool)
	}
	if relay := loaded.Codex.LiveMediaRelay; !relay.DisablePrivateRemoteIPs || relay.EffectiveMaxSessions() != 32 {
		t.Fatalf("live relay changed on reload: %+v", relay)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"max-idle-conns-per-host: 2", "allow-private-remote-ips: false # Keep the legacy spelling",
		"trusted-proxies: []", "cors-allowed-origins: []", "store-sources: []", "store-auth: []",
		"api-key-aliases: {}", "api-key-rules: {}",
	} {
		if !strings.Contains(string(stored), want) {
			t.Errorf("missing source default or setting %q", want)
		}
	}
	for _, absent := range []string{
		"model-catalog:", "lifecycle-config-revision:", "auth-revision:", "home:", "disable-private-remote-ips:",
	} {
		if strings.Contains(string(stored), absent) {
			t.Errorf("wrote inactive or external setting %q", absent)
		}
	}
	if data, err := os.ReadFile(catalogPath); err != nil || string(data) != catalog {
		t.Fatalf("startup modified models.json: %v", err)
	}
	loaded.Codex.LiveMediaRelay.DisablePrivateRemoteIPs = false
	if err := SaveConfigPreserveComments(path, loaded); err != nil {
		t.Fatal(err)
	}
	if updated, err := LoadConfig(path); err != nil || updated.Codex.LiveMediaRelay.DisablePrivateRemoteIPs {
		t.Fatalf("typed update could not reset the legacy flag: %v", err)
	}
}

func TestSchemaDefaultsTypedExistingRecordsAndExplicitValues(t *testing.T) {
	const input = `port: 8317
remote-management: {auto-update-cpa: true}
stream-defaults: &stream-defaults
  keepalive-seconds: 20
  bootstrap-retries: 0
streaming:
  <<: *stream-defaults
nonstream-keepalive-interval: 12
codex-api-key:
  - api-key: synthetic-key
    base-url: https://codex.fixture.invalid/v1
    alpha-search: true # Keep this comment when disabled
    models: [{name: fixture-model, alias: fixture}]
openai-compatibility:
  - name: fixture-provider
    base-url: https://provider.fixture.invalid/v1
    api-key-entries: [{api-key: synthetic-secondary}]
    models: [{name: fixture-model, alias: fixture}]
api-key-rules:
  synthetic-client:
    limits: [{window: 1h, requests: 50}]
  synthetic-blocked:
    blocked-models: [fixture]
plugins:
  store-auth:
    - match: https://first.fixture.invalid/
      allow-insecure: false # Keep the transport default comment
    - match: "" # Inactive legacy row
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Streaming = StreamingConfig{}
	cfg.NonStreamKeepAliveInterval = 0
	if err := yaml.Unmarshal(
		[]byte("auto-update-panel: false\ndisable-auto-update-panel: false\n"), &cfg.RemoteManagement,
	); err != nil {
		t.Fatal(err)
	}
	cfg.RemoteManagement.AutoUpdateCPA = false
	cfg.CredentialInFlight.MaxDetails = 0
	cfg.CodexKey[0].AlphaSearch = false
	disabled, zero := false, 0
	cfg.CodexKey[0].DisableCooling = &disabled
	cfg.OpenAICompatibility[0].APIKeyEntries[0].Weight = &zero
	cfg.Routing.SessionAffinity, cfg.Routing.SessionAffinitySubagents = true, &disabled
	cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile = &disabled
	cfg.XAIKey = []XAIKey{{APIKey: "synthetic-xai", BaseURL: "https://xai.fixture.invalid/v1"}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"priority: 0", "alpha-search: false # Keep this comment when disabled", "websockets: false",
		"support-configuration-update: false", "use-max-completion-tokens: false", "force-mapping: false",
		"is-compat: false", "support-prompt-cache-key: false", "image: false", "disabled: false", "weight: 0",
		"policy-profile: default", "reservation-ttl: 24h", "tokens: 0", "cost-micro: 0",
		"allow-insecure: false # Keep the transport default comment", "# Inactive legacy row",
		"session-affinity-ttl: 1h", "session-affinity-subagents: false", "auto-update-cpa: false",
		"disable-auto-update-panel: false", "stabilize-device-profile: false", "max-details: 0",
		"<<: *stream-defaults", "keepalive-seconds: 20", "keepalive-seconds: 0",
	} {
		if !strings.Contains(string(stored), want) {
			t.Errorf("missing source value %q", want)
		}
	}
	if strings.Count(string(stored), "bootstrap-retries:") != 1 {
		t.Fatal("save replaced the unchanged inherited retry default with a local setting")
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RemoteManagement.IsAutoUpdatePanel() || loaded.RemoteManagement.AutoUpdateCPA ||
		loaded.Streaming != (StreamingConfig{}) || loaded.NonStreamKeepAliveInterval != 0 || loaded.CredentialInFlight.MaxDetails != 0 ||
		loaded.Routing.SessionAffinitySubagents == nil || *loaded.Routing.SessionAffinitySubagents ||
		loaded.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || *loaded.ClaudeHeaderDefaults.StabilizeDeviceProfile {
		t.Fatal("a typed zero or false did not reach the config reader")
	}
	codex := loaded.CodexKey[0]
	if codex.AlphaSearch || codex.DisableCooling == nil || *codex.DisableCooling || codex.RequestRetry != nil ||
		codex.Weight != nil || codex.DisableCodexCloaking != nil || codex.Models[0].SupportConfigurationUpdate {
		t.Fatal("typed save changed Codex defaults or overrides")
	}
	provider := loaded.OpenAICompatibility[0]
	if key := provider.APIKeyEntries[0]; key.Weight == nil || *key.Weight != 0 || key.Disabled || provider.Models[0].UseMaxCompletionTokens {
		t.Fatal("typed save changed OpenAI selector defaults")
	}
	policy, ok := loaded.APIKeyRule("synthetic-client")
	if !ok || policy.PolicyProfile != "default" || policy.ReservationTTL != "24h" || len(policy.Limits) != 1 || policy.Limits[0].Requests != 50 ||
		!loaded.APIKeyBlocksModel(
			"synthetic-blocked", "fixture",
		) || loaded.APIKeyRules["synthetic-blocked"].PolicyProfile != "" {
		t.Fatal("typed save changed API-key policy or populated defaults on a rule without limits")
	}
	if len(loaded.Plugins.StoreAuth) != 1 || loaded.Plugins.StoreAuth[0].Type != "none" || loaded.Plugins.StoreAuth[0].AllowInsecure {
		t.Fatal("store auth gained an active legacy row or changed authentication defaults")
	}
}

func TestSchemaDefaultsExampleKeepsSetupModeWithoutExampleRecords(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKeys) != 3 || len(safemode.ExampleAPIKeys(cfg.APIKeys)) != len(cfg.APIKeys) || cfg.RemoteManagement.SecretKey != "" {
		t.Fatal("example must keep only the client setup markers")
	}
	if len(cfg.APIKeyRules) != 0 || len(cfg.APIKeyAliases) != 0 || len(cfg.Plugins.Configs) != 0 || len(cfg.Plugins.StoreAuth) != 0 {
		t.Fatal("example contains active policy or plugin records")
	}
	for family, rows := range cfg.ConfiguredProviderKeyRows() {
		if len(rows) != 0 {
			t.Errorf("example contains an active %s provider", family)
		}
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8317 || cfg.RequestRetry != 3 || cfg.MaxRetryInterval != 30 ||
		cfg.QuotaRefresh.Enabled || cfg.Streaming != (StreamingConfig{}) || cfg.Plugins.Enabled ||
		cfg.ImageArtifactCache.RetentionDays != defaultImageArtifactRetentionDays || cfg.ImageArtifactCache.MaxTotalSizeMB != defaultImageArtifactCacheSizeMB {
		t.Fatal("example changed local startup settings or enabled a feature")
	}
}
