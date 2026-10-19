package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiJSONSchemaCarrierPreservesSupportedConstraints(t *testing.T) {
	input := `{"type":"object","additionalProperties":{"type":"string","pattern":"^ok$"},"properties":{
		"code":{"type":"string","minLength":2,"maxLength":8,"pattern":"^[a-z]+$"},
		"labels":{"items":{"type":"string"}},
		"wrong":{"type":"object","items":{"type":"string"}},
		"upper":{"type":"ARRAY","items":true}
	}}`
	output := CleanJSONSchemaForGeminiJSONSchema(input)
	for path, want := range map[string]string{
		"additionalProperties.pattern": "^ok$",
		"properties.code.minLength":    "2",
		"properties.code.maxLength":    "8",
		"properties.code.pattern":      "^[a-z]+$",
		"properties.labels.type":       "array",
		"properties.upper.type":        "ARRAY",
	} {
		if got := gjson.Get(output, path).String(); got != want {
			t.Errorf("%s = %q, want %q; schema = %s", path, got, want, output)
		}
	}
	if gjson.Get(output, "properties.wrong.items").Exists() {
		t.Fatalf("non-array node retained items: %s", output)
	}
	if got := gjson.Get(output, "properties.upper.items"); !got.IsObject() {
		t.Fatalf("boolean true items were not normalized: %s", output)
	}
}

func TestGeminiJSONSchemaBooleanTrueIsEmptyObject(t *testing.T) {
	if got := CleanJSONSchemaForGeminiJSONSchema(`true`); got != `{}` {
		t.Fatalf("true schema = %s, want {}", got)
	}
}

func TestGeminiSchemaRepairsBooleanDefinitionsAndDependencies(t *testing.T) {
	input := `{"type":"object","properties":{"any":true,"blocked":false,"nested":{"oneOf":[true,false]},"array":{"type":"array","items":[true,false]}},"$defs":{"any":true},"definitions":{"any":true},"dependentSchemas":{"any":true},"dependencies":{"any":true,"blocked":false,"required":["id"]},"maximum":9007199254740993}`
	output := normalizeMalformedSchemaObjects(input, true)
	for _, path := range []string{
		"properties.any", "properties.nested.oneOf.0", "properties.array.items.0",
		"$defs.any", "definitions.any", "dependentSchemas.any", "dependencies.any",
	} {
		if got := gjson.Get(output, path); !got.IsObject() || len(got.Map()) != 0 {
			t.Errorf("%s = %s, want {}: %s", path, got.Raw, output)
		}
	}
	for _, path := range []string{"properties.blocked", "properties.nested.oneOf.1", "properties.array.items.1", "dependencies.blocked"} {
		if got := gjson.Get(output, path); got.Type != gjson.False {
			t.Errorf("%s = %s, want false: %s", path, got.Raw, output)
		}
	}
	if got := gjson.Get(output, "dependencies.required.0"); got.String() != "id" {
		t.Errorf("array dependency = %s, want id: %s", got.Raw, output)
	}
	if got := gjson.Get(output, "maximum"); got.Raw != "9007199254740993" {
		t.Errorf("maximum = %s, want exact integer: %s", got.Raw, output)
	}
}

func TestGeminiSchemaPreservesObjectPropertiesFromMixedUnions(t *testing.T) {
	cleaners := map[string]func(string) string{
		"gemini":               CleanJSONSchemaForGemini,
		"gemini-json-schema":   CleanJSONSchemaForGeminiJSONSchema,
		"antigravity-tool":     CleanJSONSchemaForAntigravity,
		"antigravity-response": CleanJSONSchemaForAntigravityResponse,
	}
	for _, test := range []struct {
		name  string
		input string
		other bool
	}{
		{
			name: "union type", input: `{"type":"object","properties":{"endpoint":{"type":["string","object"],"properties":{"traces":{"type":"string"}},"required":["traces"]}}}`,
		},
		{
			name: "mixed branches", input: `{"type":"object","properties":{"endpoint":{"properties":{"traces":{"type":"string"}},"required":["traces"],"anyOf":[{"type":"string"},{"type":"object","properties":{"other":{"type":"string"}}}]}}}`,
			other: true,
		},
		{
			name: "primitive type", input: `{"type":"object","properties":{"endpoint":{"type":"string","properties":{"traces":{"type":"string"}},"required":["traces"]}}}`,
		},
		{
			name: "conflicting array items", input: `{"type":"object","properties":{"endpoint":{"type":"array","items":{"type":"string"},"properties":{"traces":{"type":"string"}}}}}`,
		},
	} {
		for name, clean := range cleaners {
			t.Run(test.name+"/"+name, func(t *testing.T) {
				output := clean(test.input)
				if got := gjson.Get(output, "properties.endpoint.type").String(); got != "object" {
					t.Fatalf("endpoint type = %q, want object: %s", got, output)
				}
				if !gjson.Get(output, "properties.endpoint.properties.traces").Exists() {
					t.Fatalf("endpoint traces missing: %s", output)
				}
				if test.other && !gjson.Get(output, "properties.endpoint.properties.other").Exists() {
					t.Fatalf("merged branch property missing: %s", output)
				}
				if test.name == "conflicting array items" && gjson.Get(output, "properties.endpoint.items").Exists() {
					t.Fatalf("non-array node retained items: %s", output)
				}
			})
		}
	}
}

func TestGeminiSchemaRemovesUnsupportedKeywords(t *testing.T) {
	input := `{"type":"object","additionalItems":{"type":"string"},"unevaluatedProperties":true,"unevaluatedItems":false,"contentSchema":{"type":"object"},"properties":{"item":{"type":"object","additionalItems":true,"unevaluatedProperties":true,"unevaluatedItems":false,"contentSchema":true}}}`
	for _, cleaner := range []func(string) string{CleanJSONSchemaForGemini, CleanJSONSchemaForAntigravity} {
		output := cleaner(input)
		for _, key := range []string{"additionalItems", "unevaluatedProperties", "unevaluatedItems", "contentSchema"} {
			if gjson.Get(output, key).Exists() || gjson.Get(output, "properties.item."+key).Exists() {
				t.Fatalf("unsupported %s remained: %s", key, output)
			}
		}
	}
}
