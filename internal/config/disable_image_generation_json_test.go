package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisableImageGenerationJSONRetainsLegacyBooleans(t *testing.T) {
	for _, fixture := range []struct {
		mode DisableImageGenerationMode
		wire string
	}{
		{DisableImageGenerationOff, `false`},
		{DisableImageGenerationAll, `true`},
		{DisableImageGenerationChat, `"chat"`},
		{DisableImageGenerationPassthrough, `"passthrough"`},
	} {
		t.Run(
			fixture.mode.String(), func(t *testing.T) {
				cfg := &Config{DisableImageGeneration: fixture.mode}
				encoded, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				if got := string(fields["disable-image-generation"]); got != fixture.wire {
					t.Errorf("configuration JSON mode=%s, want %s", got, fixture.wire)
				}
				var decoded Config
				if err := json.Unmarshal(
					encoded, &decoded,
				); err != nil || decoded.DisableImageGeneration != fixture.mode {
					t.Fatalf("configuration round trip: mode=%v error=%v", decoded.DisableImageGeneration, err)
				}
				yamlBytes, err := yaml.Marshal(fixture.mode)
				if err != nil {
					t.Fatal(err)
				}
				var yamlValue string
				if err := yaml.Unmarshal(yamlBytes, &yamlValue); err != nil || yamlValue != fixture.mode.String() {
					t.Fatalf("YAML mode changed: value=%q error=%v", yamlValue, err)
				}
			},
		)
	}
}
