package util

import (
	"reflect"
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
