package util

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCleanJSONSchemaForAntigravityResponsePreservesUnions(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{
			"action":{"anyOf":[
				{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},
				{"type":"null"}
			]},
			"label":{"oneOf":[{"type":"string"},{"type":"null"}]}
		}
	}`

	result := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	for _, testCase := range []struct {
		path      string
		wantTypes []string
	}{
		{path: "properties.action.anyOf", wantTypes: []string{"object", "null"}},
		{path: "properties.label.oneOf", wantTypes: []string{"string", "null"}},
	} {
		union := result.Get(testCase.path)
		if !union.IsArray() {
			t.Errorf("response union %s was flattened: %s", testCase.path, result.Raw)
			continue
		}
		gotTypes := make([]string, 0, len(union.Array()))
		for _, branch := range union.Array() {
			gotTypes = append(gotTypes, branch.Get("type").String())
		}
		if !reflect.DeepEqual(gotTypes, testCase.wantTypes) {
			t.Errorf(
				"response union %s types = %v, want %v: %s", testCase.path, gotTypes, testCase.wantTypes, result.Raw,
			)
		}
	}
}

func TestCleanJSONSchemaForAntigravityResponsePreservesEnumType(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{
			"conviction":{"type":"number","enum":[0.25,0.5,1]},
			"count":{"type":"integer","enum":[1,2]}
		}
	}`

	result := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	for _, testCase := range []struct {
		path       string
		wantType   string
		wantValues []string
	}{
		{path: "properties.conviction", wantType: "number", wantValues: []string{"0.25", "0.5", "1"}},
		{path: "properties.count", wantType: "integer", wantValues: []string{"1", "2"}},
	} {
		schema := result.Get(testCase.path)
		if gotType := schema.Get("type").String(); gotType != testCase.wantType {
			t.Errorf("%s type = %q, want %q: %s", testCase.path, gotType, testCase.wantType, result.Raw)
		}
		gotValues := make([]string, 0, len(schema.Get("enum").Array()))
		for _, enumValue := range schema.Get("enum").Array() {
			if enumValue.Type != gjson.String {
				t.Errorf("%s enum value is not a string: %s", testCase.path, enumValue.Raw)
			}
			gotValues = append(gotValues, enumValue.String())
		}
		if !reflect.DeepEqual(gotValues, testCase.wantValues) {
			t.Errorf("%s enum values = %v, want %v: %s", testCase.path, gotValues, testCase.wantValues, result.Raw)
		}
	}
}

func TestJSONSchemaCleanOptionsKeepToolRewritesScoped(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{
			"choice":{"anyOf":[{"type":"string"},{"type":"null"}]},
			"level":{"type":"number","enum":[1,2]}
		}
	}`

	toolSchema := gjson.Parse(CleanJSONSchemaForAntigravity(input))
	if toolSchema.Get("properties.choice.anyOf").Exists() {
		t.Errorf("tool union was not flattened: %s", toolSchema.Raw)
	}
	if gotType := toolSchema.Get("properties.level.type").String(); gotType != "string" {
		t.Errorf("tool enum type = %q, want string: %s", gotType, toolSchema.Raw)
	}

	responseSchema := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	if !responseSchema.Get("properties.choice.anyOf").IsArray() {
		t.Errorf("response union was flattened: %s", responseSchema.Raw)
	}
	if gotType := responseSchema.Get("properties.level.type").String(); gotType != "number" {
		t.Errorf("response enum type = %q, want number: %s", gotType, responseSchema.Raw)
	}
}

func TestCleanJSONSchemaForAntigravityResponsePreservesAdditionalPropertiesFalse(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{
			"nested":{
				"type":"object",
				"properties":{"value":{"type":"string"}},
				"additionalProperties":false
			}
		},
		"additionalProperties":false
	}`
	result := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	for _, path := range []string{"additionalProperties", "properties.nested.additionalProperties"} {
		value := result.Get(path)
		if !value.Exists() || value.Type != gjson.False {
			t.Errorf("%s = %s, want false; schema=%s", path, value.Raw, result.Raw)
		}
	}
	if strings.Contains(result.Raw, "No extra properties allowed") {
		t.Fatalf("response schema gained tool-only hint: %s", result.Raw)
	}

	nonFalse := CleanJSONSchemaForAntigravityResponse(`{
		"type":"object",
		"properties":{"map":{"type":"object","additionalProperties":{"type":"string"}}},
		"additionalProperties":true
	}`)
	if strings.Contains(nonFalse, `"additionalProperties"`) {
		t.Fatalf("non-false additionalProperties survived: %s", nonFalse)
	}
}

func TestCleanJSONSchemaMergesConditionalPropertiesWithoutPromotingRequired(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{"kind":{"type":"string"}},
		"required":["kind"],
		"if":{"properties":{"kind":{"const":"sell"}}},
		"then":{"properties":{"sell_reason":{"type":"string"}},"required":["sell_reason"]},
		"else":{"properties":{"buy_reason":{"type":"string"}},"required":["buy_reason"]}
	}`
	for name, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
		"response":    CleanJSONSchemaForAntigravityResponse,
	} {
		t.Run(
			name, func(t *testing.T) {
				result := gjson.Parse(clean(input))
				for _, path := range []string{"if", "then", "else"} {
					if result.Get(path).Exists() {
						t.Errorf("conditional %s survived: %s", path, result.Raw)
					}
				}
				for _, path := range []string{"properties.sell_reason", "properties.buy_reason"} {
					if !result.Get(path).Exists() {
						t.Errorf("conditional property %s was lost: %s", path, result.Raw)
					}
				}
				if required := result.Get("required").Array(); len(required) != 1 || required[0].String() != "kind" {
					t.Errorf("required = %v, want only kind; schema=%s", required, result.Raw)
				}
			},
		)
	}
}

func TestCleanJSONSchemaForAntigravityResponseHandlesRequiredOnlyUnion(t *testing.T) {
	input := `{
		"type":"object",
		"anyOf":[{"required":["left"]},{"required":["right"]}],
		"properties":{"left":{"type":"integer"},"right":{"type":"integer"}},
		"additionalProperties":false
	}`
	result := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	if result.Get("type").String() != "object" || !result.Get("properties.left").Exists() ||
		!result.Get("properties.right").Exists() {
		t.Fatalf("parent object schema was replaced: %s", result.Raw)
	}
	if result.Get("anyOf").Exists() {
		t.Fatalf("required-only union survived: %s", result.Raw)
	}
	if result.Get("required").Exists() {
		t.Fatalf("alternative required fields were promoted: %s", result.Raw)
	}
}

func TestCleanJSONSchemaRemovesContainsWithHint(t *testing.T) {
	input := `{"type":"array","items":{"type":"string"},"contains":{"enum":["x"]}}`
	for name, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
		"response":    CleanJSONSchemaForAntigravityResponse,
	} {
		t.Run(
			name, func(t *testing.T) {
				result := gjson.Parse(clean(input))
				if result.Get("contains").Exists() {
					t.Fatalf("contains survived: %s", result.Raw)
				}
				if !strings.Contains(result.Get("description").String(), "contains") {
					t.Fatalf("contains hint missing: %s", result.Raw)
				}
			},
		)
	}
}

func TestCleanJSONSchemaRemovesEncryptedMetadataButKeepsNamedProperty(t *testing.T) {
	input := `{
		"type":"object",
		"properties":{
			"encrypted":{"type":"boolean","encrypted":true},
			"nested":{"type":"object","properties":{"secret":{"type":"string","encrypted":false}}}
		},
		"required":["encrypted"]
	}`
	for name, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
		"response":    CleanJSONSchemaForAntigravityResponse,
	} {
		t.Run(
			name, func(t *testing.T) {
				result := gjson.Parse(clean(input))
				if result.Get("properties.encrypted.type").String() != "boolean" {
					t.Fatalf("property named encrypted was removed: %s", result.Raw)
				}
				for _, path := range []string{
					"properties.encrypted.encrypted",
					"properties.nested.properties.secret.encrypted",
				} {
					if result.Get(path).Exists() {
						t.Errorf("metadata %s survived: %s", path, result.Raw)
					}
				}
			},
		)
	}
}

func TestCleanJSONSchemaRepairsMalformedAndStringifiedSchemas(t *testing.T) {
	malformed := `{
		"type":"object",
		"properties":{
			"data":{
				"parent":{"type":"string","required":true},
				"optional":{"type":"integer","required":false}
			}
		}
	}`
	for name, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
		"response":    CleanJSONSchemaForAntigravityResponse,
	} {
		t.Run(
			name, func(t *testing.T) {
				result := gjson.Parse(clean(malformed))
				if result.Get("properties.data.type").String() != "object" ||
					result.Get("properties.data.properties.parent.type").String() != "string" {
					t.Fatalf("bare property map was not repaired: %s", result.Raw)
				}
				required := result.Get("properties.data.required").Array()
				if len(required) != 1 || required[0].String() != "parent" {
					t.Fatalf("promoted required = %v, want parent; schema=%s", required, result.Raw)
				}
				if result.Get("properties.data.properties.optional.required").Exists() {
					t.Fatalf("boolean required metadata survived: %s", result.Raw)
				}
			},
		)
	}

	stringified := `"{\"type\":\"object\",\"properties\":{\"name\":{\"type\":\"string\"}}}"`
	result := gjson.Parse(CleanJSONSchemaForGemini(stringified))
	if result.Get("properties.name.type").String() != "string" {
		t.Fatalf("stringified schema was not decoded: %s", result.Raw)
	}
	result = gjson.Parse(CleanJSONSchemaForGemini(""))
	if result.Get("type").String() != "object" || !result.Get("properties").IsObject() {
		t.Fatalf("empty schema did not receive object fallback: %s", result.Raw)
	}
}

func TestCleanJSONSchemaAddsArrayItemsOnlyForTools(t *testing.T) {
	input := `{"type":"object","properties":{"values":{"type":"array"},"existing":{"type":"array","items":{"type":"number"}}}}`
	for name, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		t.Run(
			name, func(t *testing.T) {
				result := gjson.Parse(clean(input))
				if result.Get("properties.values.items.type").String() != "string" {
					t.Fatalf("missing items default was not added: %s", result.Raw)
				}
				if result.Get("properties.existing.items.type").String() != "number" {
					t.Fatalf("existing items changed: %s", result.Raw)
				}
			},
		)
	}
	response := gjson.Parse(CleanJSONSchemaForAntigravityResponse(input))
	if response.Get("properties.values.items").Exists() {
		t.Fatalf("response schema gained tool-only items default: %s", response.Raw)
	}
}
