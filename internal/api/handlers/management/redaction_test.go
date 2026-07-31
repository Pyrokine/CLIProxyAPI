package management

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"gopkg.in/yaml.v3"
)

func TestMaskUsageEventAPIKeysPreservesAliasIdentityOnMaskCollision(t *testing.T) {
	rawA := "abcd-first-wxyz"
	rawB := "abcd-other-wxyz"
	if util.HideAPIKey(rawA) != util.HideAPIKey(rawB) {
		t.Fatalf("test API keys do not collide")
	}

	masked := maskUsageEventAPIKeys(
		[]usage.FlatDetail{{APIKey: rawA}, {APIKey: rawB}},
		map[string]string{rawA: "first", rawB: "second"},
	)
	if masked[0].APIKey != util.HideAPIKey(rawA) || masked[1].APIKey != util.HideAPIKey(rawB) {
		t.Fatalf("maskUsageEventAPIKeys API keys = %#v", masked)
	}
	if masked[0].APIKeyAlias != "first" || masked[1].APIKeyAlias != "second" {
		t.Fatalf("maskUsageEventAPIKeys aliases = %q/%q, want first/second", masked[0].APIKeyAlias, masked[1].APIKeyAlias)
	}
}

func TestMaskUsageEventAPIKeysSerializesExplicitEmptyAlias(t *testing.T) {
	masked := maskUsageEventAPIKeys([]usage.FlatDetail{{APIKey: "abcd-other-wxyz"}}, nil)
	encoded, err := json.Marshal(masked[0])
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"api_key_alias":""`) {
		t.Fatalf("usage event omitted explicit empty alias: %s", encoded)
	}
}

func TestConfigYAMLRedactsPluginRawSecretsAndAliases(t *testing.T) {
	directSecret := "plugin-direct-secret-value"
	aliasedSecret := "plugin-aliased-secret-value"
	source := []byte(`plugins:
  enabled: true
  configs:
    sample:
      access-token: ` + directSecret + `
      endpoint: &shared-secret ` + aliasedSecret + `
      refresh-token: *shared-secret
`)

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	for _, secret := range []string{directSecret, aliasedSecret} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redactConfigYAML kept plugin secret %q: %s", secret, redacted)
		}
	}
	if count := strings.Count(string(redacted), configYAMLPlaceholderPrefix); count != 2 {
		t.Fatalf("redactConfigYAML placeholder count = %d, want 2: %s", count, redacted)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	for _, secret := range []string{directSecret, aliasedSecret} {
		if !strings.Contains(string(restored), secret) {
			t.Fatalf("restoreConfigYAMLPlaceholders omitted plugin secret %q: %s", secret, restored)
		}
	}
}

func TestConfigYAMLRejectsSensitiveAnchorAliasInNonSensitiveField(t *testing.T) {
	source := []byte("api-keys:\n  - &sensitive-anchor sk-anchor-alias-secret-value\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	var decoded struct {
		APIKeys []string `yaml:"api-keys"`
	}
	if err = yaml.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("decode redacted YAML: %v", err)
	}
	if len(decoded.APIKeys) != 1 {
		t.Fatalf("api-keys = %#v, want one placeholder", decoded.APIKeys)
	}
	anchorPlaceholder := ""
	document, err := decodeConfigYAML(redacted)
	if err != nil {
		t.Fatalf("decodeConfigYAML: %v", err)
	}
	if err = visitConfigYAMLNodes(
		document,
		"$",
		make(map[*yaml.Node]struct{}),
		func(node *yaml.Node, _ string) error {
			if isConfigYAMLAnchorPlaceholder(node.Anchor) {
				anchorPlaceholder = node.Anchor
			}
			return nil
		},
	); err != nil {
		t.Fatalf("visitConfigYAMLNodes: %v", err)
	}
	if anchorPlaceholder == "" {
		t.Fatalf("redacted YAML missing opaque anchor: %s", redacted)
	}
	submitted := append(append([]byte(nil), redacted...), []byte("usage-data-dir: *"+anchorPlaceholder+"\n")...)

	if _, err = restoreConfigYAMLPlaceholders(submitted, source); err == nil {
		t.Fatal("restoreConfigYAMLPlaceholders accepted sensitive anchor alias in non-sensitive field")
	}
}

func TestConfigYAMLRedactsAndRestoresAnchorNames(t *testing.T) {
	const sensitiveAnchor = "anchor-sensitive-value"
	source := []byte("api-keys:\n  - &" + sensitiveAnchor + " " + sensitiveAnchor + "\n  - *" + sensitiveAnchor + "\n")

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	if strings.Contains(string(redacted), sensitiveAnchor) {
		t.Fatalf("redactConfigYAML kept secret anchor name: %s", redacted)
	}
	if !strings.Contains(string(redacted), configYAMLAnchorPlaceholderPrefix) {
		t.Fatalf("redactConfigYAML omitted opaque anchor placeholder: %s", redacted)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	for _, expected := range []string{"&" + sensitiveAnchor, "*" + sensitiveAnchor} {
		if !strings.Contains(string(restored), expected) {
			t.Fatalf("restored YAML missing %q: %s", expected, restored)
		}
	}
}

func TestConfigYAMLRejectsMovedAnchorPlaceholder(t *testing.T) {
	source := []byte("api-keys:\n  - &first first-secret\n  - &second second-secret\n  - *first\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	document, err := decodeConfigYAML(redacted)
	if err != nil {
		t.Fatalf("decodeConfigYAML: %v", err)
	}
	root := document.Content[0]
	var apiKeys *yaml.Node
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "api-keys" {
			apiKeys = root.Content[index+1]
			break
		}
	}
	if apiKeys == nil || apiKeys.Kind != yaml.SequenceNode || len(apiKeys.Content) != 3 {
		t.Fatalf("api-keys node = %#v", apiKeys)
	}
	first := apiKeys.Content[0]
	second := apiKeys.Content[1]
	alias := apiKeys.Content[2]
	first.Anchor, second.Anchor = second.Anchor, first.Anchor
	alias.Value = second.Anchor
	alias.Alias = second
	submitted, err := encodeConfigYAML(document)
	if err != nil {
		t.Fatalf("encodeConfigYAML: %v", err)
	}

	if _, err = restoreConfigYAMLPlaceholders(submitted, source); err == nil {
		t.Fatal("restoreConfigYAMLPlaceholders accepted moved anchor placeholder")
	}
}

func TestConfigYAMLRejectsPlaceholderAfterSensitiveKeyRename(t *testing.T) {
	source := []byte("plugins:\n  configs:\n    sample:\n      access-token: plugin-secret-value\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	submitted := strings.Replace(string(redacted), "access-token:", "description:", 1)

	if _, err = restoreConfigYAMLPlaceholders([]byte(submitted), source); err == nil {
		t.Fatal("restoreConfigYAMLPlaceholders accepted placeholder after sensitive key rename")
	}
}

func TestConfigYAMLRedactionRestoresExplicitScalarTag(t *testing.T) {
	source := []byte("plugins:\n  configs:\n    sample:\n      access-token: !!bool true\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	var redactedValue map[string]any
	if err = yaml.Unmarshal(redacted, &redactedValue); err != nil {
		t.Fatalf("decode redacted YAML: %v", err)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	var restoredValue struct {
		Plugins struct {
			Configs map[string]struct {
				AccessToken bool `yaml:"access-token"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err = yaml.Unmarshal(restored, &restoredValue); err != nil {
		t.Fatalf("decode restored YAML: %v", err)
	}
	if !restoredValue.Plugins.Configs["sample"].AccessToken {
		t.Fatalf("restored access-token = false, want true: %s", restored)
	}
}

func TestConfigYAMLRedactsStructuredPluginRawSecrets(t *testing.T) {
	secrets := []string{"plugin-primary-secret", "plugin-nested-secret"}
	source := []byte(`plugins:
  enabled: true
  configs:
    sample:
      access-token:
        primary: plugin-primary-secret
        nested:
          - plugin-nested-secret
      endpoint: https://example.com
`)

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redactConfigYAML kept structured plugin secret %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(string(redacted), "https://example.com") {
		t.Fatalf("redactConfigYAML masked non-sensitive plugin value: %s", redacted)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	for _, secret := range secrets {
		if !strings.Contains(string(restored), secret) {
			t.Fatalf("restoreConfigYAMLPlaceholders omitted structured secret %q: %s", secret, restored)
		}
	}
}

func TestConfigYAMLRedactionPreservesExactPluginRawSecretWhitespace(t *testing.T) {
	secret := "  plugin-spaced-secret  "
	source := []byte("plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    sample:\n" +
		"      access-token: \"" + secret + "\"\n")

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redactConfigYAML kept spaced plugin secret: %s", redacted)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	if !strings.Contains(string(restored), secret) {
		t.Fatalf("restoreConfigYAMLPlaceholders changed spaced plugin secret: %s", restored)
	}
}

func TestConfigYAMLRedactsAliasMappingKeys(t *testing.T) {
	secret := "plugin-aliased-map-key-secret"
	source := []byte(`plugins:
  enabled: true
  configs:
    sample:
      field-name: &aliases-field api-key-aliases
      raw-key: &raw-key ` + secret + `
      *aliases-field:
        *raw-key: sample-alias
`)

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redactConfigYAML kept aliased map key: %s", redacted)
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	if !strings.Contains(string(restored), secret) {
		t.Fatalf("restoreConfigYAMLPlaceholders omitted aliased map key: %s", restored)
	}
}

func TestConfigYAMLOpaquePlaceholdersDoNotCollideWithMasks(t *testing.T) {
	rawA := "abcd-first-wxyz"
	rawB := "abcd-other-wxyz"
	masked := util.HideAPIKey(rawA)
	if masked != util.HideAPIKey(rawB) {
		t.Fatal("test secrets do not share a display mask")
	}
	source := []byte("proxy-url: \"" + masked + "\"\napi-keys:\n  - " + rawA + "\n  - " + rawB + "\n")

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	var decoded struct {
		ProxyURL string   `yaml:"proxy-url"`
		APIKeys  []string `yaml:"api-keys"`
	}
	if err = yaml.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("decode redacted YAML: %v", err)
	}
	if decoded.ProxyURL != masked {
		t.Fatalf("proxy-url = %q, want unchanged display-like value %q", decoded.ProxyURL, masked)
	}
	if len(decoded.APIKeys) != 2 || decoded.APIKeys[0] == decoded.APIKeys[1] {
		t.Fatalf("api-keys = %#v, want distinct placeholders", decoded.APIKeys)
	}
	for _, placeholder := range decoded.APIKeys {
		if !isConfigYAMLPlaceholder(placeholder) || placeholder == masked {
			t.Fatalf("api-key placeholder = %q, want opaque placeholder", placeholder)
		}
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	decoded = struct {
		ProxyURL string   `yaml:"proxy-url"`
		APIKeys  []string `yaml:"api-keys"`
	}{}
	if err = yaml.Unmarshal(restored, &decoded); err != nil {
		t.Fatalf("decode restored YAML: %v", err)
	}
	if decoded.ProxyURL != masked || len(decoded.APIKeys) != 2 || decoded.APIKeys[0] != rawA || decoded.APIKeys[1] != rawB {
		t.Fatalf("restored config = %#v", decoded)
	}
}

func TestConfigYAMLPlaceholderAvoidsSourceScalarCollision(t *testing.T) {
	secret := "sk-placeholder-collision-secret"
	baseSource := []byte("api-keys:\n  - " + secret + "\n")
	document, err := decodeConfigYAML(baseSource)
	if err != nil {
		t.Fatalf("decodeConfigYAML: %v", err)
	}
	plan, err := newConfigYAMLRedactionPlan(document)
	if err != nil {
		t.Fatalf("newConfigYAMLRedactionPlan: %v", err)
	}
	collidingScalar := plan.secrets[0].placeholder
	source := []byte("proxy-url: \"" + collidingScalar + "\"\napi-keys:\n  - " + secret + "\n")

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	var decoded struct {
		ProxyURL string   `yaml:"proxy-url"`
		APIKeys  []string `yaml:"api-keys"`
	}
	if err = yaml.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("decode redacted YAML: %v", err)
	}
	if decoded.ProxyURL != collidingScalar {
		t.Fatalf("proxy-url = %q, want source scalar preserved", decoded.ProxyURL)
	}
	if len(decoded.APIKeys) != 1 || decoded.APIKeys[0] == collidingScalar || !isConfigYAMLPlaceholder(decoded.APIKeys[0]) {
		t.Fatalf("api-keys = %#v, want non-colliding placeholder", decoded.APIKeys)
	}
}

func TestConfigYAMLPreservesBlockScalarValueOnRestore(t *testing.T) {
	expected := "first line\n  indented line\nlast line\n"
	source := []byte("plugins:\n  configs:\n    sample:\n      access-token: |\n        first line\n          indented line\n        last line\n")

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	if strings.Contains(string(redacted), "first line") {
		t.Fatalf("redactConfigYAML kept block scalar secret: %s", redacted)
	}
	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	var decoded struct {
		Plugins struct {
			Configs map[string]struct {
				AccessToken string `yaml:"access-token"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err = yaml.Unmarshal(restored, &decoded); err != nil {
		t.Fatalf("decode restored YAML: %v", err)
	}
	if got := decoded.Plugins.Configs["sample"].AccessToken; got != expected {
		t.Fatalf("restored block scalar = %q, want %q", got, expected)
	}
}

func TestConfigYAMLSeparatesValueAndMapKeySensitiveContainers(t *testing.T) {
	source := []byte(`plugins:
  configs:
    sample:
      access-token:
        account-name: user@example.com
        credential-value: nested-secret-value
      api-key-aliases:
        sk-alias-secret-value: visible-alias
      api-key-rules:
        sk-rule-secret-value:
          models:
            - gpt-5
`)

	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	text := string(redacted)
	for _, secret := range []string{"account-name", "user@example.com", "credential-value", "nested-secret-value", "sk-alias-secret-value", "sk-rule-secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("redactConfigYAML kept sensitive value %q: %s", secret, text)
		}
	}
	for _, visible := range []string{"visible-alias", "models", "gpt-5"} {
		if !strings.Contains(text, visible) {
			t.Fatalf("redactConfigYAML removed structured value %q: %s", visible, text)
		}
	}

	restored, err := restoreConfigYAMLPlaceholders(redacted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	for _, value := range []string{"account-name", "user@example.com", "credential-value", "nested-secret-value", "sk-alias-secret-value", "sk-rule-secret-value"} {
		if !strings.Contains(string(restored), value) {
			t.Fatalf("restoreConfigYAMLPlaceholders omitted %q: %s", value, restored)
		}
	}
}

func TestConfigYAMLRestoresPlaceholdersAfterMappingAndSequenceReorder(t *testing.T) {
	source := []byte("port: 8317\ndebug: false\ngemini-api-key:\n  - api-key: sk-first-secret-value\n    base-url: https://first.example\n  - api-key: sk-second-secret-value\n    base-url: https://second.example\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}
	document, err := decodeConfigYAML(redacted)
	if err != nil {
		t.Fatalf("decodeConfigYAML: %v", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		t.Fatalf("unexpected YAML document: %#v", document)
	}
	root := document.Content[0]
	root.Content = append(
		[]*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "new-setting"},
			{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
		},
		root.Content...,
	)
	var providers *yaml.Node
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "gemini-api-key" {
			providers = root.Content[index+1]
			break
		}
	}
	if providers == nil || providers.Kind != yaml.SequenceNode || len(providers.Content) != 2 {
		t.Fatalf("gemini-api-key node = %#v", providers)
	}
	providers.Content[0], providers.Content[1] = providers.Content[1], providers.Content[0]

	submitted, err := encodeConfigYAML(document)
	if err != nil {
		t.Fatalf("encodeConfigYAML: %v", err)
	}
	restored, err := restoreConfigYAMLPlaceholders(submitted, source)
	if err != nil {
		t.Fatalf("restoreConfigYAMLPlaceholders: %v", err)
	}
	var decoded struct {
		GeminiKeys []struct {
			APIKey  string `yaml:"api-key"`
			BaseURL string `yaml:"base-url"`
		} `yaml:"gemini-api-key"`
	}
	if err = yaml.Unmarshal(restored, &decoded); err != nil {
		t.Fatalf("decode restored YAML: %v", err)
	}
	if len(decoded.GeminiKeys) != 2 ||
		decoded.GeminiKeys[0].BaseURL != "https://second.example" ||
		decoded.GeminiKeys[0].APIKey != "sk-second-secret-value" ||
		decoded.GeminiKeys[1].BaseURL != "https://first.example" ||
		decoded.GeminiKeys[1].APIKey != "sk-first-secret-value" {
		t.Fatalf("restored providers = %#v", decoded.GeminiKeys)
	}
}

func TestConfigYAMLRejectsStaleAndMisplacedPlaceholders(t *testing.T) {
	source := []byte("api-keys:\n  - sk-first-secret-value\n  - sk-second-secret-value\n")
	redacted, err := redactConfigYAML(source)
	if err != nil {
		t.Fatalf("redactConfigYAML: %v", err)
	}

	t.Run("scalar sequence reorder", func(t *testing.T) {
		document, errDecode := decodeConfigYAML(redacted)
		if errDecode != nil {
			t.Fatalf("decodeConfigYAML: %v", errDecode)
		}
		var placeholders []*yaml.Node
		if errVisit := visitConfigYAMLSecrets(document, func(node *yaml.Node, _ string) error {
			placeholders = append(placeholders, node)
			return nil
		}); errVisit != nil {
			t.Fatalf("visitConfigYAMLSecrets: %v", errVisit)
		}
		if len(placeholders) != 2 {
			t.Fatalf("placeholder nodes = %d, want 2", len(placeholders))
		}
		placeholders[0].Value, placeholders[1].Value = placeholders[1].Value, placeholders[0].Value
		submitted, errEncode := encodeConfigYAML(document)
		if errEncode != nil {
			t.Fatalf("encodeConfigYAML: %v", errEncode)
		}
		if _, errRestore := restoreConfigYAMLPlaceholders(submitted, source); errRestore != nil {
			t.Fatalf("restoreConfigYAMLPlaceholders rejected scalar sequence reorder: %v", errRestore)
		}
	})

	t.Run("structured sequence misplacement", func(t *testing.T) {
		structuredSource := []byte("gemini-api-key:\n  - api-key: sk-first-secret-value\n    base-url: https://first.example\n  - api-key: sk-second-secret-value\n    base-url: https://second.example\n")
		structuredRedacted, errRedact := redactConfigYAML(structuredSource)
		if errRedact != nil {
			t.Fatalf("redactConfigYAML: %v", errRedact)
		}
		document, errDecode := decodeConfigYAML(structuredRedacted)
		if errDecode != nil {
			t.Fatalf("decodeConfigYAML: %v", errDecode)
		}
		var placeholders []*yaml.Node
		if errVisit := visitConfigYAMLSecrets(document, func(node *yaml.Node, _ string) error {
			if isConfigYAMLPlaceholder(node.Value) {
				placeholders = append(placeholders, node)
			}
			return nil
		}); errVisit != nil {
			t.Fatalf("visitConfigYAMLSecrets: %v", errVisit)
		}
		if len(placeholders) != 2 {
			t.Fatalf("placeholder nodes = %d, want 2", len(placeholders))
		}
		placeholders[0].Value, placeholders[1].Value = placeholders[1].Value, placeholders[0].Value
		submitted, errEncode := encodeConfigYAML(document)
		if errEncode != nil {
			t.Fatalf("encodeConfigYAML: %v", errEncode)
		}
		if _, errRestore := restoreConfigYAMLPlaceholders(submitted, structuredSource); errRestore == nil {
			t.Fatal("restoreConfigYAMLPlaceholders accepted structured placeholder misplacement")
		}
	})

	t.Run("nonsensitive location", func(t *testing.T) {
		var decoded struct {
			APIKeys []string `yaml:"api-keys"`
		}
		if errDecode := yaml.Unmarshal(redacted, &decoded); errDecode != nil {
			t.Fatalf("decode redacted YAML: %v", errDecode)
		}
		submitted := []byte("proxy-url: \"" + decoded.APIKeys[0] + "\"\n" + string(redacted))
		if _, errRestore := restoreConfigYAMLPlaceholders(submitted, source); errRestore == nil {
			t.Fatal("restoreConfigYAMLPlaceholders accepted placeholder in non-sensitive field")
		}
	})

	t.Run("stale", func(t *testing.T) {
		otherSource := []byte("api-keys:\n  - sk-replaced-secret-value\n  - sk-second-secret-value\n")
		if _, errRestore := restoreConfigYAMLPlaceholders(redacted, otherSource); errRestore == nil {
			t.Fatal("restoreConfigYAMLPlaceholders accepted stale placeholder")
		}
	})
}
