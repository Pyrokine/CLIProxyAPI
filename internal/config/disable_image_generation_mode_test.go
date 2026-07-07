package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisableImageGenerationModePassthroughYAML(t *testing.T) {
	var cfg struct {
		DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation"`
	}
	if err := yaml.Unmarshal([]byte("disable-image-generation: passthrough\n"), &cfg); err != nil {
		t.Fatalf("unmarshal passthrough YAML: %v", err)
	}
	if cfg.DisableImageGeneration != DisableImageGenerationPassthrough {
		t.Fatalf("DisableImageGeneration = %v, want passthrough", cfg.DisableImageGeneration)
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal passthrough YAML: %v", err)
	}
	if got := string(out); !strings.Contains(got, "disable-image-generation: passthrough") {
		t.Fatalf("marshaled YAML = %q, want passthrough", got)
	}
}

func TestDisableImageGenerationModePassthroughJSON(t *testing.T) {
	var mode DisableImageGenerationMode
	if err := json.Unmarshal([]byte(`"passthrough"`), &mode); err != nil {
		t.Fatalf("unmarshal passthrough JSON: %v", err)
	}
	if mode != DisableImageGenerationPassthrough {
		t.Fatalf("mode = %v, want passthrough", mode)
	}

	out, err := json.Marshal(mode)
	if err != nil {
		t.Fatalf("marshal passthrough JSON: %v", err)
	}
	if string(out) != `"passthrough"` {
		t.Fatalf("marshaled JSON = %s, want passthrough", out)
	}
}
