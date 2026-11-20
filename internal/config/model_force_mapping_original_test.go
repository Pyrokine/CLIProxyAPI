package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOriginalModelForceMappingSurvivesYAMLAndJSON(t *testing.T) {
	for _, modelType := range []struct {
		name string
		make func() any
	}{
		{"claude", func() any { return &ClaudeModel{} }},
		{"gemini", func() any { return &GeminiModel{} }},
		{"codex", func() any { return &CodexModel{} }},
		{"vertex", func() any { return &VertexCompatModel{} }},
		{"openai-compatibility", func() any { return &OpenAICompatibilityModel{} }},
	} {
		for _, force := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/force=%t", modelType.name, force), func(t *testing.T) {
					model := modelType.make()
					input := fmt.Sprintf("name: upstream\nalias: public\nforce-mapping: %t\n", force)
					if err := yaml.Unmarshal([]byte(input), model); err != nil {
						t.Fatal(err)
					}
					mapping, ok := model.(interface{ GetForceMapping() bool })
					if !ok || mapping.GetForceMapping() != force {
						t.Fatalf("YAML force-mapping=%+v, want %t", model, force)
					}
					encodedYAML, err := yaml.Marshal(model)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(encodedYAML), "force-mapping: true") != force {
						t.Fatalf("YAML lost the original force-mapping field: %s", encodedYAML)
					}
					encodedJSON, err := json.Marshal(model)
					if err != nil {
						t.Fatal(err)
					}
					restored := modelType.make()
					if err := json.Unmarshal(encodedJSON, restored); err != nil {
						t.Fatal(err)
					}
					if restored.(interface{ GetForceMapping() bool }).GetForceMapping() != force {
						t.Fatalf("JSON lost force-mapping: %s", encodedJSON)
					}
				},
			)
		}
	}
}
