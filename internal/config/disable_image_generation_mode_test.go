package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisableImageGenerationMode_UnmarshalYAML(t *testing.T) {
	for _, test := range []struct {
		value string
		want  DisableImageGenerationMode
	}{
		{value: "false", want: DisableImageGenerationOff},
		{value: "true", want: DisableImageGenerationAll},
		{value: "chat", want: DisableImageGenerationChat},
		{value: "passthrough", want: DisableImageGenerationPassthrough},
	} {
		t.Run(
			test.value, func(t *testing.T) {
				var wrapper struct {
					V DisableImageGenerationMode `yaml:"disable-image-generation"`
				}
				if err := yaml.Unmarshal([]byte("disable-image-generation: "+test.value+"\n"), &wrapper); err != nil {
					t.Fatalf("unmarshal %s: %v", test.value, err)
				}
				if wrapper.V != test.want {
					t.Fatalf("%s => %v, want %v", test.value, wrapper.V, test.want)
				}
			},
		)
	}
}

func TestDisableImageGenerationMode_UnmarshalJSON(t *testing.T) {
	for _, test := range []struct {
		value string
		want  DisableImageGenerationMode
	}{
		{value: "false", want: DisableImageGenerationOff},
		{value: "true", want: DisableImageGenerationAll},
		{value: `"chat"`, want: DisableImageGenerationChat},
		{value: `"passthrough"`, want: DisableImageGenerationPassthrough},
	} {
		t.Run(
			test.value, func(t *testing.T) {
				var value DisableImageGenerationMode
				if err := json.Unmarshal([]byte(test.value), &value); err != nil {
					t.Fatalf("unmarshal %s: %v", test.value, err)
				}
				if value != test.want {
					t.Fatalf("%s => %v, want %v", test.value, value, test.want)
				}
			},
		)
	}
}

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
