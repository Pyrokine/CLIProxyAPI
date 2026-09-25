package config

import "testing"

func TestParseCooldownStateConfiguration(t *testing.T) {
	cfg, errParse := ParseConfigBytes(
		[]byte(`
save-cooldown-status: true
openai-compatibility:
  - name: compat
    base-url: https://compat.example/v1
    disable-cooling: true
`),
	)
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if !cfg.SaveCooldownStatus {
		t.Fatal("SaveCooldownStatus = false, want true")
	}
	if len(cfg.OpenAICompatibility) != 1 || cfg.OpenAICompatibility[0].DisableCooling == nil || !*cfg.OpenAICompatibility[0].DisableCooling {
		t.Fatalf("OpenAICompatibility = %#v, want disable-cooling true", cfg.OpenAICompatibility)
	}
}

func TestHomeRuntimeDisablesCooldownPersistence(t *testing.T) {
	cfg := &Config{SaveCooldownStatus: true}
	ApplyHomeRuntimeConfig(cfg)
	if cfg.SaveCooldownStatus {
		t.Fatal("SaveCooldownStatus = true in Home runtime, want false")
	}
}
