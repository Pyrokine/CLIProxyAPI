package synthesizer

import (
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestOpenAICompatibilityKeyDisablePreservesIdentityAndSelection(t *testing.T) {
	const source = `openai-compatibility:
  - name: fixture-compatible
    base-url: https://provider.test/v1
    api-key-entries:
      - api-key: fixture-first-key
        disabled: true
        weight: 7
        proxy-url: http://proxy.test:8080
      - api-key: fixture-second-key
        weight: 3
`
	synthesize := func(raw string) []*coreauth.Auth {
		t.Helper()
		var cfg config.Config
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		auths, err := NewConfigSynthesizer().Synthesize(
			&SynthesisContext{
				Config: &cfg, IDGenerator: NewStableIDGenerator(),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(auths) != 2 {
			t.Fatalf("credential count = %d, disabled credentials must remain addressable", len(auths))
		}
		return auths
	}
	disabled := synthesize(source)
	if !disabled[0].Disabled || disabled[0].Status != coreauth.StatusDisabled || disabled[1].Disabled ||
		disabled[1].Status != coreauth.StatusActive {
		t.Fatalf(
			"per-key disabled/active states = %t/%s, %t/%s", disabled[0].Disabled, disabled[0].Status,
			disabled[1].Disabled, disabled[1].Status,
		)
	}
	enabled := synthesize(strings.Replace(source, "disabled: true", "disabled: false", 1))
	if enabled[0].Disabled || enabled[0].Status != coreauth.StatusActive {
		t.Fatalf("re-enabled state = %t/%s", enabled[0].Disabled, enabled[0].Status)
	}
	for index, before := range disabled {
		after := enabled[index]
		if before.ID != after.ID || before.Attributes["source"] != after.Attributes["source"] ||
			before.Attributes["config_index"] != after.Attributes["config_index"] {
			t.Errorf("key %d changed identity after status update", index)
		}
		if before.ProxyURL != after.ProxyURL || before.Attributes["weight"] != after.Attributes["weight"] {
			t.Errorf("key %d changed proxy/weight after status update", index)
		}
	}
	if enabled[0].Attributes["weight"] != "7" || enabled[1].Attributes["weight"] != "3" ||
		enabled[0].ProxyURL != "http://proxy.test:8080" {
		t.Fatal("per-key selection settings were lost")
	}
}
