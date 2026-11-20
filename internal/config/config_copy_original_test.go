package config

import (
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestCloneNil(t *testing.T) {
	cloned, err := Clone(nil)
	if err != nil || cloned != nil {
		t.Fatalf("Clone(nil) = %#v, %v, want nil, nil", cloned, err)
	}
}

func TestParseConfigBytes_AntigravitySensitiveWords(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`antigravity:
  sensitive-words:
    - "API"
    - "proxy"
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	want := []string{"API", "proxy"}
	if !reflect.DeepEqual(cfg.Antigravity.SensitiveWords, want) {
		t.Fatalf("Antigravity.SensitiveWords = %#v, want %#v", cfg.Antigravity.SensitiveWords, want)
	}
}

func TestParseConfigBytes_AntigravityConnectionPool(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`antigravity:
  connection-pool:
    enabled: false
    idle-conn-timeout: "15s"
    max-idle-conns-per-host: 4
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	pool := cfg.Antigravity.ConnectionPool
	if pool.Enabled == nil || *pool.Enabled {
		t.Fatalf("Enabled = %v, want false", pool.Enabled)
	}
	if pool.IdleConnTimeout != "15s" {
		t.Fatalf("IdleConnTimeout = %q, want 15s", pool.IdleConnTimeout)
	}
	if pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 4 {
		t.Fatalf("MaxIdleConnsPerHost = %v, want 4", pool.MaxIdleConnsPerHost)
	}
}

func TestCloneDeepCopiesOriginalConfig(t *testing.T) {
	cfg := sampleCloneConfig()
	cloned, err := Clone(cfg)
	if err != nil || cloned == nil {
		t.Fatalf("Clone() = %#v, %v", cloned, err)
	}
	if cloned == cfg {
		t.Fatal("Clone() returned original pointer")
	}
	mutateOriginalConfig(t, cfg)
	if cloned.Home.Host != "home.local" {
		t.Fatalf("clone.Home.Host = %q, want home.local", cloned.Home.Host)
	}
	if cloned.APIKeys[0] != "client-key" {
		t.Fatalf("clone.APIKeys[0] = %q, want client-key", cloned.APIKeys[0])
	}
	if cloned.OAuthExcludedModels["codex"][0] != "hidden-model" {
		t.Fatalf("clone.OAuthExcludedModels[codex][0] = %q, want hidden-model", cloned.OAuthExcludedModels["codex"][0])
	}
	if cloned.OAuthModelAlias["codex"][0].Alias != "client-model" {
		t.Fatalf(
			"clone.OAuthModelAlias[codex][0].Alias = %q, want client-model", cloned.OAuthModelAlias["codex"][0].Alias,
		)
	}
	if got := pluginRawScalar(t, cloned.Plugins.Configs["sample"].Raw, "mode"); got != "first" {
		t.Fatalf("clone plugin raw mode = %q, want first", got)
	}
	if cloned.OpenAICompatibility[0].Models[0].Thinking.Levels[0] != "low" {
		t.Fatalf("clone thinking level = %q, want low", cloned.OpenAICompatibility[0].Models[0].Thinking.Levels[0])
	}
	if got := cloned.Payload.Default[0].Params["object"].(map[string]any)["key"]; got != "value" {
		t.Fatalf("clone payload object key = %#v, want value", got)
	}

	cloned.APIKeys[0] = "clone-client-key"
	cloned.OAuthExcludedModels["codex"][0] = "clone-hidden-model"
	cloned.OAuthModelAlias["codex"][0].Alias = "clone-client-model"
	cloned.OpenAICompatibility[0].Models[0].Thinking.Levels[0] = "clone-low"
	cloned.Payload.Default[0].Params["object"].(map[string]any)["key"] = "clone-value"
	plugin := cloned.Plugins.Configs["sample"]
	setPluginRawScalar(t, &plugin.Raw, "mode", "third")
	cloned.Plugins.Configs["sample"] = plugin
	var rawFields map[string]any
	if err := plugin.Raw.Decode(&rawFields); err != nil {
		t.Fatal(err)
	}
	if rawFields["mode"] != "third" || rawFields["mode-alias"] != "third" {
		t.Fatalf("clone raw anchor relation changed: %#v", rawFields)
	}
	if cfg.APIKeys[0] != "mutated-client-key" {
		t.Fatalf("cfg.APIKeys[0] = %q, want mutated-client-key", cfg.APIKeys[0])
	}
	if cfg.OAuthExcludedModels["codex"][0] != "mutated-hidden-model" {
		t.Fatalf(
			"cfg.OAuthExcludedModels[codex][0] = %q, want mutated-hidden-model", cfg.OAuthExcludedModels["codex"][0],
		)
	}
	if cfg.OAuthModelAlias["codex"][0].Alias != "mutated-client-model" {
		t.Fatalf(
			"cfg.OAuthModelAlias[codex][0].Alias = %q, want mutated-client-model",
			cfg.OAuthModelAlias["codex"][0].Alias,
		)
	}
	if got := pluginRawScalar(t, cfg.Plugins.Configs["sample"].Raw, "mode"); got != "second" {
		t.Fatalf("cfg plugin raw mode = %q, want second", got)
	}
	if cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0] != "mutated-low" {
		t.Fatalf("cfg thinking level = %q, want mutated-low", cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0])
	}
	if got := cfg.Payload.Default[0].Params["object"].(map[string]any)["key"]; got != "mutated-value" {
		t.Fatalf("cfg payload object key = %#v, want mutated-value", got)
	}
}

func TestCloneDoesNotShareOriginalReferenceFields(t *testing.T) {
	cfg := sampleCloneConfig()
	cloned, err := Clone(cfg)
	if err != nil || cloned == nil {
		t.Fatalf("Clone() = %#v, %v", cloned, err)
	}
	assertNoSharedRuntimeReferences(t, reflect.ValueOf(cfg), reflect.ValueOf(cloned), "Config")
}

func TestCopyNodeShallowPreservesRawAliasGraph(t *testing.T) {
	source := samplePluginRawNode("first")
	destination := &yaml.Node{Kind: yaml.ScalarNode, Value: "old", HeadComment: "retained"}
	copyNodeShallow(destination, &source)
	setPluginRawScalar(t, destination, "mode", "changed")
	var values map[string]any
	if err := destination.Decode(&values); err != nil {
		t.Fatal(err)
	}
	if values["mode"] != "changed" || values["mode-alias"] != "changed" || destination.HeadComment != "retained" {
		t.Fatalf("copied node lost alias relation or comment: %#v, %q", values, destination.HeadComment)
	}
	if got := pluginRawScalar(t, source, "mode"); got != "first" {
		t.Fatalf("source node changed: %q", got)
	}
	assertNoSharedRuntimeReferences(t, reflect.ValueOf(&source), reflect.ValueOf(destination), "Raw")
}

func sampleCloneConfig() *Config {
	pluginEnabled := false
	cacheUserID := true
	return &Config{
		APIKeys: []string{"client-key"},
		Streaming: StreamingConfig{
			KeepAliveSeconds: 3,
			BootstrapRetries: 2,
		},
		Home: HomeConfig{
			Enabled: true, Host: "home.local", Port: 8081,
			TLS: HomeTLSConfig{
				Enable: true, ServerName: "home.local", CACert: "ca", ClientCert: "cert", ClientKey: "key",
				UseTargetServerName: true,
			},
		},
		Plugins: PluginsConfig{
			Enabled: true, Dir: "plugins", StoreSources: []string{"https://plugins.example/store.json"},
			Configs: map[string]PluginInstanceConfig{
				"sample": {Enabled: &pluginEnabled, Priority: 10, Raw: samplePluginRawNode("first")},
			},
		},
		GeminiKey: []GeminiKey{
			{
				APIKey: "gemini-key", Models: []GeminiModel{{Name: "gemini-upstream", Alias: "gemini-upstream-alias"}},
				Headers: map[string]string{"X-Gemini": "one"}, ExcludedModels: []string{"gemini-hidden"},
			},
		},
		CodexKey: []CodexKey{
			{
				APIKey: "codex-key", Models: []CodexModel{{Name: "codex-upstream", Alias: "codex-client"}},
				Headers: map[string]string{"X-Codex": "one"}, ExcludedModels: []string{"codex-hidden-key"},
			},
		},
		ClaudeKey: []ClaudeKey{
			{
				APIKey: "claude-key", Models: []ClaudeModel{{Name: "claude-upstream", Alias: "claude-client"}},
				Headers: map[string]string{"X-Claude": "one"}, ExcludedModels: []string{"claude-hidden"},
				Cloak: &CloakConfig{SensitiveWords: []string{"secret"}, CacheUserID: &cacheUserID},
			},
		},
		OpenAICompatibility: []OpenAICompatibility{
			{
				Name:          "compat",
				APIKeyEntries: []OpenAICompatibilityAPIKey{{APIKey: "compat-key", ProxyURL: "http://proxy.local"}},
				Models: []OpenAICompatibilityModel{
					{
						Name: "compat-upstream", Alias: "compat-client",
						Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
					},
				},
				Headers: map[string]string{"X-Compat": "one"},
			},
		},
		VertexCompatAPIKey: []VertexCompatKey{
			{
				APIKey: "vertex-key", Headers: map[string]string{"X-Vertex": "one"},
				Models:         []VertexCompatModel{{Name: "vertex-upstream", Alias: "vertex-client"}},
				ExcludedModels: []string{"vertex-hidden"},
			},
		},
		OAuthExcludedModels: map[string][]string{"codex": {"hidden-model"}},
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"codex": {{Name: "upstream-model", Alias: "client-model", Fork: true}},
		},
		Payload: PayloadConfig{
			Default: []PayloadRule{
				{
					Models: []PayloadModelRule{
						{
							Name: "model-*", Headers: map[string]string{"X-Tier": "gold"},
							Match: []map[string]any{{"tier": "gold"}}, Exist: []string{"$.messages"},
						},
					},
					Params: map[string]any{
						"object": map[string]any{"key": "value"},
						"array":  []any{"first", map[string]any{"nested": "value"}},
					},
				},
			},
			Filter: []PayloadFilterRule{
				{
					Models: []PayloadModelRule{{Name: "model-*"}}, Params: []string{"$.secret"},
				},
			},
		},
	}
}

func mutateOriginalConfig(t *testing.T, cfg *Config) {
	t.Helper()
	cfg.Home.Host = "mutated-home.local"
	cfg.APIKeys[0] = "mutated-client-key"
	cfg.OAuthExcludedModels["codex"][0] = "mutated-hidden-model"
	cfg.OAuthModelAlias["codex"][0].Alias = "mutated-client-model"
	cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0] = "mutated-low"
	cfg.Payload.Default[0].Params["object"].(map[string]any)["key"] = "mutated-value"
	plugin := cfg.Plugins.Configs["sample"]
	setPluginRawScalar(t, &plugin.Raw, "mode", "second")
	cfg.Plugins.Configs["sample"] = plugin
}

func samplePluginRawNode(mode string) yaml.Node {
	modeValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mode, Anchor: "modeAnchor"}
	return yaml.Node{
		Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"},
			{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode"},
			modeValue,
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode-alias"},
			{Kind: yaml.AliasNode, Value: "modeAnchor", Alias: modeValue},
		},
	}
}

func pluginRawScalar(t *testing.T, node yaml.Node, key string) string {
	t.Helper()
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key && node.Content[i+1] != nil {
			return node.Content[i+1].Value
		}
	}
	t.Fatalf("raw plugin node missing key %q", key)
	return ""
}

func setPluginRawScalar(t *testing.T, node *yaml.Node, key, value string) {
	t.Helper()
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key && node.Content[i+1] != nil {
			node.Content[i+1].Value = value
			return
		}
	}
	t.Fatalf("raw plugin node missing key %q", key)
}

func assertNoSharedRuntimeReferences(t *testing.T, original, cloned reflect.Value, path string) {
	t.Helper()
	if !original.IsValid() || !cloned.IsValid() {
		return
	}
	if original.Kind() == reflect.Interface {
		if original.IsNil() || cloned.IsNil() {
			return
		}
		assertNoSharedRuntimeReferences(t, original.Elem(), cloned.Elem(), path)
		return
	}
	if original.Kind() != cloned.Kind() {
		t.Fatalf("%s kind mismatch: %s != %s", path, original.Kind(), cloned.Kind())
	}
	// Opaque and value-only kinds are outside the config clone-reference check.
	//goland:noinspection GoSwitchMissingCasesForIotaConsts
	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() || cloned.IsNil() {
			return
		}
		if original.Pointer() == cloned.Pointer() {
			t.Fatalf("%s shares pointer %x", path, original.Pointer())
		}
		assertNoSharedRuntimeReferences(t, original.Elem(), cloned.Elem(), path+"->"+original.Type().Elem().String())
	case reflect.Map:
		if original.IsNil() || cloned.IsNil() {
			return
		}
		if original.Pointer() == cloned.Pointer() {
			t.Fatalf("%s shares map pointer %x", path, original.Pointer())
		}
		iter := original.MapRange()
		for iter.Next() {
			key := iter.Key()
			assertNoSharedRuntimeReferences(t, iter.Value(), cloned.MapIndex(key), path+"["+keyForPath(key)+"]")
		}
	case reflect.Slice:
		if original.IsNil() || cloned.IsNil() {
			return
		}
		if original.Pointer() == cloned.Pointer() {
			t.Fatalf("%s shares slice pointer %x", path, original.Pointer())
		}
		for i := 0; i < original.Len(); i++ {
			assertNoSharedRuntimeReferences(t, original.Index(i), cloned.Index(i), path+"[]")
		}
	case reflect.Struct:
		for i := 0; i < original.NumField(); i++ {
			field := original.Type().Field(i)
			assertNoSharedRuntimeReferences(t, original.Field(i), cloned.Field(i), path+"."+field.Name)
		}
	}
}

func keyForPath(key reflect.Value) string {
	if key.Kind() == reflect.String {
		return key.String()
	}
	return key.Type().String()
}
