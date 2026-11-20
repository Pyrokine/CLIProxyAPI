package util

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNativeJSONSchemaCleanersPreserveOriginalTypesAndConstraints(t *testing.T) {
	input := `{"type":"object","encrypted":true,"$id":"drop","$defs":{
		"Payload":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}
	},"properties":{
		"action":{"anyOf":[
			{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},
			{"type":"null"}
		]},
		"label":{"oneOf":[{"type":"string"},{"type":"null"}]},
		"conviction":{"type":"number","enum":[0.25,0.5,1],"minimum":0.25,"maximum":1},
		"count":{"type":"integer","enum":[1,2],"maximum":9007199254740993},
		"enabled":{"type":"boolean","enum":[true,false]},
		"value":{"type":["number","null"]},
		"payload":{"$ref":"#/$defs/Payload"},
		"literal":{"const":{"type":"object","encrypted":true,"x-note":"keep"}},
		"nullable_value":{"type":"string","nullable":true},
		"values":{"type":"array"},
		"encrypted":{"type":"boolean","encrypted":false}
	},"required":["payload","value","action","not_declared"],"additionalProperties":true,
	"propertyOrdering":["enabled","count","conviction"],
	"anyOf":[{"required":["count"]},{"required":["enabled"]}]}`
	for name, clean := range map[string]func(string) string{
		"gemini":      CleanJSONSchemaForGeminiJSONSchema,
		"antigravity": CleanJSONSchemaForAntigravityJSONSchema,
	} {
		t.Run(
			name, func(t *testing.T) {
				output := clean(input)
				for _, path := range []string{
					"$defs", "properties.action", "properties.label", "properties.conviction",
					"properties.count", "properties.enabled", "properties.value", "properties.payload",
					"properties.literal", "properties.values", "required", "anyOf",
					"additionalProperties", "propertyOrdering",
				} {
					want, wantOK := decodeSchemaJSON(gjson.Get(input, path).Raw)
					got, gotOK := decodeSchemaJSON(gjson.Get(output, path).Raw)
					if !wantOK || !gotOK || !reflect.DeepEqual(got, want) {
						t.Errorf("%s changed: got=%v want=%v; schema=%s", path, got, want, output)
					}
				}
				if gjson.Get(output, "encrypted").Exists() || gjson.Get(output, `\$id`).Exists() ||
					gjson.Get(output, "properties.encrypted.encrypted").Exists() {
					t.Errorf("unsupported metadata survived: %s", output)
				}
				if gjson.Get(output, "properties.encrypted.type").String() != "boolean" {
					t.Errorf("property named encrypted was removed: %s", output)
				}
				node := gjson.Get(output, "properties.nullable_value")
				if node.Get("type").Raw != `["string","null"]` || node.Get("nullable").Exists() {
					t.Errorf("nullable value lost its null alternative: %s", node.Raw)
				}
				if got := clean(output); got != output {
					t.Errorf("native cleaner is not idempotent: %s", got)
				}
				for range 3 {
					if got := clean(input); got != output {
						t.Fatalf("native cleaner changed ordering: %s", got)
					}
				}
			},
		)
	}
}
