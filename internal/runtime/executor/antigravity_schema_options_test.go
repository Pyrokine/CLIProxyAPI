package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeAntigravityRequestSchemasPreservesResponseUnionAndEnumType(t *testing.T) {
	payload := `{"request":{
		"tools":[{"functionDeclarations":[{"name":"tool","parameters":{"type":"object","properties":{
			"choice":{"anyOf":[{"type":"string"},{"type":"null"}]},
			"level":{"type":"number","enum":[1,2]}
		}}}]}],
		"generationConfig":{"responseSchema":{"type":"object","properties":{
			"action":{"anyOf":[
				{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},
				{"type":"null"}
			]},
			"conviction":{"type":"number","enum":[0.25,0.5,1]}
		}}}
	}}`

	got := sanitizeAntigravityRequestSchemas(payload, true)
	responseSchema := gjson.Get(got, "request.generationConfig.responseSchema")
	union := responseSchema.Get("properties.action.anyOf")
	if !union.IsArray() || len(union.Array()) != 2 || union.Get("1.type").String() != "null" {
		t.Errorf("response anyOf union was flattened: %s", responseSchema.Raw)
	}
	conviction := responseSchema.Get("properties.conviction")
	if gotType := conviction.Get("type").String(); gotType != "number" {
		t.Errorf("response enum type = %q, want number: %s", gotType, responseSchema.Raw)
	}
	for _, enumValue := range conviction.Get("enum").Array() {
		if enumValue.Type != gjson.String {
			t.Errorf("response enum value is not a string: %s", conviction.Raw)
		}
	}

	toolSchema := gjson.Get(got, "request.tools.0.functionDeclarations.0.parameters")
	if toolSchema.Get("properties.choice.anyOf").Exists() {
		t.Errorf("tool anyOf union was not flattened: %s", toolSchema.Raw)
	}
	if gotType := toolSchema.Get("properties.level.type").String(); gotType != "string" {
		t.Errorf("tool enum type = %q, want string: %s", gotType, toolSchema.Raw)
	}
}
