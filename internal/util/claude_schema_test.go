package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeToolInputSchema(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "root anyOf without type",
			input: `{
				"anyOf": [
					{"type":"object","properties":{"a":{"type":"string"}}},
					{"type":"object","properties":{"b":{"type":"integer"}}}
				]
			}`,
			expected: `{
				"type":"object",
				"properties":{
					"a":{"type":"string"},
					"b":{"type":"integer"}
				}
			}`,
		},
		{
			name: "root oneOf keeps nested union",
			input: `{
				"type":"object",
				"properties":{
					"nested":{"oneOf":[{"type":"string"},{"type":"number"}]}
				},
				"oneOf":[
					{"properties":{"a":{"type":"string"}},"required":["a"]},
					{"properties":{"b":{"type":"string"}},"required":["b"]}
				]
			}`,
			expected: `{
				"type":"object",
				"properties":{
					"nested":{"oneOf":[{"type":"string"},{"type":"number"}]},
					"a":{"type":"string"},
					"b":{"type":"string"}
				}
			}`,
		},
		{
			name: "root anyOf drops alternative required fields",
			input: `{
				"type":"object",
				"properties":{"a":{"type":"string"},"b":{"type":"string"}},
				"anyOf":[{"required":["a"]},{"required":["b"]}]
			}`,
			expected: `{
				"type":"object",
				"properties":{"a":{"type":"string"},"b":{"type":"string"}}
			}`,
		},
		{
			name: "root allOf merges properties and required fields",
			input: `{
				"type":"object",
				"properties":{"base":{"type":"boolean"}},
				"required":["base"],
				"allOf":[
					{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},
					{"properties":{"b":{"type":"integer"}},"required":["a","b"]}
				]
			}`,
			expected: `{
				"type":"object",
				"properties":{
					"base":{"type":"boolean"},
					"a":{"type":"string"},
					"b":{"type":"integer"}
				},
				"required":["base","a","b"]
			}`,
		},
		{
			name: "ordinary object schema",
			input: `{
				"type":"object",
				"properties":{"query":{"type":"string"}},
				"required":["query"],
				"additionalProperties":false
			}`,
			expected: `{
				"type":"object",
				"properties":{"query":{"type":"string"}},
				"required":["query"],
				"additionalProperties":false
			}`,
		},
		{
			name:     "invalid schema",
			input:    `{"type":`,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "boolean schema",
			input:    `true`,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "null schema",
			input:    `null`,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "array schema",
			input:    `[]`,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "string schema",
			input:    `"invalid"`,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "empty schema",
			input:    ``,
			expected: `{"type":"object","properties":{}}`,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				actual := NormalizeClaudeToolInputSchema([]byte(test.input))
				compareJSON(t, test.expected, string(actual))
			},
		)
	}
}

func TestNormalizeClaudeSchemaDialect(t *testing.T) {
	input := []byte(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{
			"$schema":{"type":"string"},
			"nested":{
				"type":"array",
				"items":{
					"$id":"nested",
					"type":"object",
					"properties":{
						"bad":{"type":"string","pattern":"^\\p{L}+$"},
						"safe":{"type":"string","pattern":"^[a-z]+$"}
					}
				}
			}
		},
		"patternProperties":{
			"^\\P{N}+$":{"type":"string"},
			"^[a-z]+$":{"$schema":"nested","type":"number"}
		}
	}`)

	output := NormalizeClaudeSchemaDialect(input)
	result := gjson.ParseBytes(output)
	if result.Get("$schema").Exists() || result.Get("properties.nested.items.$id").Exists() {
		t.Fatalf("dialect metadata survived: %s", output)
	}
	if _, exists := result.Get("properties").Map()["$schema"]; !exists {
		t.Fatalf("property named $schema was removed: %s", output)
	}
	if result.Get("properties.nested.items.properties.bad.pattern").Exists() {
		t.Fatalf("unsupported Unicode pattern survived: %s", output)
	}
	if got := result.Get("properties.nested.items.properties.safe.pattern").String(); got != "^[a-z]+$" {
		t.Fatalf("safe pattern = %q, want preserved", got)
	}
	patternProperties := result.Get("patternProperties").Map()
	if _, exists := patternProperties[`^\P{N}+$`]; exists {
		t.Fatalf("unsupported patternProperties key survived: %s", output)
	}
	if safe, exists := patternProperties[`^[a-z]+$`]; !exists || safe.Get("$schema").Exists() {
		t.Fatalf("safe patternProperties schema was lost or not normalized: %s", output)
	}
}

func TestNormalizeClaudeSchemaDialectDecodesJSONUnicodeEscapes(t *testing.T) {
	escapedBackslashP := string([]byte{'\\', 'u', '0', '0', '5', 'c', '\\', 'u', '0', '0', '7', '0'})
	input := []byte(`{"type":"string","pattern":"^` + escapedBackslashP + `{L}+$"}`)
	output := NormalizeClaudeSchemaDialect(input)
	if gjson.GetBytes(output, "pattern").Exists() {
		t.Fatalf("JSON Unicode-escaped property pattern survived: %s", output)
	}
}

func TestNormalizeClaudeToolInputSchemaAppliesNestedDialectCleanup(t *testing.T) {
	input := []byte(`{
		"type":"object",
		"properties":{
			"query":{"$schema":"draft-07","type":"string","pattern":"^\\p{L}+$"}
		},
		"required":["query"]
	}`)
	result := gjson.ParseBytes(NormalizeClaudeToolInputSchema(input))
	if result.Get("properties.query.$schema").Exists() || result.Get("properties.query.pattern").Exists() {
		t.Fatalf("nested dialect cleanup was not applied: %s", result.Raw)
	}
}

func TestClaudeSchemaMissesRequired(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   bool
	}{
		{
			name:   "missing required",
			schema: `{"type":"object","properties":{"a":{"type":"string"}}}`,
			want:   true,
		},
		{
			name:   "partial required",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`,
			want:   true,
		},
		{
			name:   "complete required",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`,
			want:   false,
		},
		{
			name: "nested incomplete",
			schema: `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"string"}}}},` +
				`"required":["outer"]}`,
			want: true,
		},
		{
			name: "union branch incomplete",
			schema: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},` +
				`{"type":"object","properties":{"b":{"type":"string"}}}]}`,
			want: true,
		},
		{name: "invalid schema", schema: `{"type":`, want: false},
		{name: "null schema", schema: `null`, want: false},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				if got := ClaudeSchemaMissesRequired([]byte(testCase.schema)); got != testCase.want {
					t.Fatalf("ClaudeSchemaMissesRequired() = %v, want %v", got, testCase.want)
				}
			},
		)
	}
}

func TestHasUnsupportedUnicodePropertyEscape(t *testing.T) {
	tests := map[string]bool{
		`^\p{L}+$`:   true,
		`^\P{N}+$`:   true,
		`^\\p{L}+$`:  false,
		`^\\\p{L}+$`: true,
		`^[a-z]+$`:   false,
		`\p`:         false,
		`^[^\0]*$`:   true,
		`^[^\x00]*$`: false,
		`^\\0$`:      false,
	}
	for pattern, want := range tests {
		if got := HasUnsupportedUnicodePropertyEscape(pattern); got != want {
			t.Errorf("HasUnsupportedUnicodePropertyEscape(%q) = %v, want %v", pattern, got, want)
		}
	}
}
