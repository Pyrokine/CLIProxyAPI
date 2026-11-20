package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeAntigravityRequestSchemasProjectsUnionsAndPreservesEnumTypes(t *testing.T) {
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
	action := responseSchema.Get("properties.action")
	if action.Get("anyOf").Exists() || action.Get("type").String() != "object" || !action.Get("nullable").Bool() {
		t.Errorf("response anyOf was not projected to nullable object: %s", responseSchema.Raw)
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
	if gotType := toolSchema.Get("properties.level.type").String(); gotType != "number" {
		t.Errorf("tool enum type = %q, want number: %s", gotType, toolSchema.Raw)
	}
}

func TestSanitizeAntigravityToolSchemasKeepNativeTypeAndNullableOnBothPaths(t *testing.T) {
	payload := `{"request":{"tools":[{"functionDeclarations":[{"name":"tool","parameters":{
		"type":"object",
		"properties":{
			"level":{"type":"number","enum":[1,2]},
			"note":{"type":["string","null"]}
		},
		"required":["level","note"]
	}}]}]}}`

	for _, requirePlaceholder := range []bool{false, true} {
		got := sanitizeAntigravityRequestSchemas(payload, requirePlaceholder)
		schema := gjson.Get(got, "request.tools.0.functionDeclarations.0.parameters")
		if schema.Get("properties.level.type").String() != "number" {
			t.Fatalf("placeholder=%v changed numeric tool argument type: %s", requirePlaceholder, schema.Raw)
		}
		if values := schema.Get("properties.level.enum").Array(); len(values) != 2 {
			t.Fatalf("placeholder=%v lost numeric enum values: %s", requirePlaceholder, schema.Raw)
		}
		for _, member := range schema.Get("properties.level.enum").Array() {
			if member.Type != gjson.String {
				t.Fatalf("placeholder=%v left non-string proto enum: %s", requirePlaceholder, schema.Raw)
			}
		}
		if !schema.Get("properties.note.nullable").Bool() || schema.Get("required.1").String() != "note" {
			t.Fatalf("placeholder=%v lost native nullable/required semantics: %s", requirePlaceholder, schema.Raw)
		}
	}
}

func TestSanitizeAntigravityRequestSchemasStripsEncryptedMetadata(t *testing.T) {
	encryptedSchema := `{"type":"object","properties":{"key":{"type":"string","encrypted":true},` +
		`"timeout":{"type":"integer","encrypted":false}},"required":["key"]}`

	for _, declContainer := range []string{"functionDeclarations", "function_declarations"} {
		payload := `{"request":{"tools":[{"` + declContainer +
			`":[{"name":"test_tool","parameters":` + encryptedSchema + `}]}]}}`

		for _, useAntigravitySchema := range []bool{false, true} {
			got := sanitizeAntigravityRequestSchemas(payload, useAntigravitySchema)
			if strings.Contains(got, `"encrypted"`) {
				t.Errorf(
					"declContainer=%s antigravity=%v: encrypted marker survived: %s",
					declContainer, useAntigravitySchema, got,
				)
			}
			schema := gjson.Get(got, "request.tools.0."+declContainer+".0.parameters")
			if !schema.Get("properties.key.type").Exists() || schema.Get("properties.key.type").String() != "string" {
				t.Errorf(
					"declContainer=%s antigravity=%v: key property was corrupted: %s",
					declContainer, useAntigravitySchema, schema.Raw,
				)
			}
		}
	}
}
