package helps

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeCodexToolSchemasComplexUnionPreservesLiteralPropertyNames(t *testing.T) {
	input := []byte(`{
		"tools":[{
			"type":"function",
			"name":"dispatch",
			"parameters":{
				"type":"object",
				"properties":{
					"my.action:kind\\name":{
						"type":"string",
						"description":"keep me",
						"oneOf":[
							{"const":"a","description":"A"},
							{"const":"b"},{"const":"c"},{"const":"d"},
							{"const":"e"},{"const":"f"},{"const":"g"},{"const":"h"}
						]
					},
					"untouched":{"type":"integer","minimum":1}
				},
				"required":["my.action:kind\\name"]
			}
		}]
	}`)

	output := NormalizeCodexToolSchemas(input)
	var decoded struct {
		Tools []struct {
			Parameters struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"parameters"`
		} `json:"tools"`
	}
	if errUnmarshal := json.Unmarshal(output, &decoded); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v", errUnmarshal)
	}
	property, exists := decoded.Tools[0].Parameters.Properties[`my.action:kind\name`]
	if !exists {
		t.Fatalf("literal property name was changed: %s", output)
	}
	if gjson.GetBytes(property, "oneOf").Exists() {
		t.Fatalf("oneOf was not removed: %s", property)
	}
	if got := len(gjson.GetBytes(property, "enum").Array()); got != 8 {
		t.Fatalf("enum length = %d, want 8: %s", got, property)
	}
	if got := gjson.GetBytes(property, "description").String(); got != "keep me" {
		t.Fatalf("description = %q, want keep me", got)
	}
	if got := gjson.GetBytes(decoded.Tools[0].Parameters.Properties["untouched"], "minimum").Int(); got != 1 {
		t.Fatalf("untouched minimum = %d, want 1", got)
	}
}

func TestNormalizeCodexToolSchemasPreservesAmbiguousUnions(t *testing.T) {
	tests := []struct {
		name     string
		property string
	}{
		{
			name: "numeric semantic duplicate",
			property: `{"oneOf":[{"const":1},{"const":1.0},{"const":2},{"const":3},` +
				`{"const":4},{"const":5},{"const":6},{"const":7}]}`,
		},
		{
			name: "oneOf and anyOf together",
			property: `{"oneOf":[{"const":"a"},{"const":"b"},{"const":"c"},{"const":"d"},` +
				`{"const":"e"},{"const":"f"},{"const":"g"},{"const":"h"}],` +
				`"anyOf":[{"type":"string"}]}`,
		},
		{
			name: "nonmatching existing enum",
			property: `{"enum":["a","b"],"oneOf":[{"const":"a"},{"const":"b"},{"const":"c"},` +
				`{"const":"d"},{"const":"e"},{"const":"f"},{"const":"g"},{"const":"h"}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"type":"object",` +
				`"properties":{"value":` + test.property + `}}}]}`)
			output := NormalizeCodexToolSchemas(input)
			if !gjson.GetBytes(output, "tools.0.parameters.properties.value.oneOf").Exists() {
				t.Fatalf("ambiguous oneOf was changed: %s", output)
			}
		})
	}
}

func TestNormalizeCodexToolSchemasPreservesLargeIntegerPrecision(t *testing.T) {
	input := []byte(`{"tools":[{"type":"custom","name":"f","parameters":{"type":"object","properties":{` +
		`"value":{"oneOf":[{"const":90071992547409931234567890},{"const":2},{"const":3},{"const":4},` +
		`{"const":5},{"const":6},{"const":7},{"const":8}]}}}}]}`)
	output := NormalizeCodexToolSchemas(input)
	if !bytes.Contains(output, []byte("90071992547409931234567890")) {
		t.Fatalf("large integer precision was lost: %s", output)
	}
	if gjson.GetBytes(output, "tools.0.parameters.properties.value.oneOf").Exists() {
		t.Fatalf("oneOf was not removed: %s", output)
	}
	if got := len(gjson.GetBytes(output, "tools.0.parameters.properties.value.enum").Array()); got != 8 {
		t.Fatalf("enum length = %d, want 8", got)
	}
}

func TestNormalizeCodexToolSchemasStripsOnlySchemaUnicodePatterns(t *testing.T) {
	input := []byte(`{
		"tools":[{
			"type":"function",
			"name":"patterns",
			"parameters":{
				"type":"object",
				"description":{"pattern":"\\p{KeepDescription}"},
				"default":{"pattern":"\\p{KeepDefault}"},
				"enum":[{"pattern":"\\p{KeepEnum}"}],
				"properties":{
					"name":{"type":"string","pattern":"^[\\p{L}]+$"},
					"nested":{"type":"array","items":{"type":"string","pattern":"\\P{ASCII}"}}
				},
				"$defs":{"valid":{"type":"string","pattern":"^[a-z]+$"}},
				"patternProperties":{
					"^ok$":{"type":"string","pattern":"\\p{L}"},
					"\\p{BadKey}":{"type":"string"}
				}
			}
		}]
	}`)

	output := NormalizeCodexToolSchemas(input)
	params := gjson.GetBytes(output, "tools.0.parameters")
	if params.Get("properties.name.pattern").Exists() || params.Get("properties.nested.items.pattern").Exists() {
		t.Fatalf("unsupported schema patterns remain: %s", output)
	}
	if got := params.Get("$defs.valid.pattern").String(); got != "^[a-z]+$" {
		t.Fatalf("valid pattern = %q, want ^[a-z]+$", got)
	}
	if got := params.Get("description.pattern").String(); got != `\p{KeepDescription}` {
		t.Fatalf("description user data changed: %q", got)
	}
	if got := params.Get("default.pattern").String(); got != `\p{KeepDefault}` {
		t.Fatalf("default user data changed: %q", got)
	}
	if got := params.Get("enum.0.pattern").String(); got != `\p{KeepEnum}` {
		t.Fatalf("enum user data changed: %q", got)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(params.Raw), &decoded); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v", errUnmarshal)
	}
	patternProperties := decoded["patternProperties"].(map[string]any)
	if _, exists := patternProperties[`\p{BadKey}`]; exists {
		t.Fatalf("unsupported patternProperties key remains: %s", output)
	}
	if _, exists := patternProperties["^ok$"]; !exists {
		t.Fatalf("valid patternProperties key removed: %s", output)
	}
	if !bytes.Equal(output, NormalizeCodexToolSchemas(output)) {
		t.Fatalf("normalization is not idempotent:\nfirst:  %s\nsecond: %s", output, NormalizeCodexToolSchemas(output))
	}
}

func TestNormalizeCodexToolSchemasHandlesNamespaceAndMalformedParameters(t *testing.T) {
	input := []byte(`{"tools":[{"type":"namespace","name":"agents","tools":[` +
		`{"type":"function","name":"spawn","parameters":{"type":"object","properties":{` +
		`"model":{"anyOf":[{"const":"a"},{"const":"b"},{"const":"c"},{"const":"d"},` +
		`{"const":"e"},{"const":"f"},{"const":"g"},{"const":"h"}]}}}},` +
		`{"type":"function","name":"broken","parameters":null}]}]}`)
	output := NormalizeCodexToolSchemas(input)
	if gjson.GetBytes(output, "tools.0.tools.0.parameters.properties.model.anyOf").Exists() {
		t.Fatalf("nested anyOf was not removed: %s", output)
	}
	if got := len(gjson.GetBytes(output, "tools.0.tools.0.parameters.properties.model.enum").Array()); got != 8 {
		t.Fatalf("nested enum length = %d, want 8", got)
	}
	if got := gjson.GetBytes(output, "tools.0.tools.1.parameters"); !got.Exists() || got.Type != gjson.Null {
		t.Fatalf("malformed parameters changed: %s", output)
	}

	invalid := []byte(`{"tools":[`)
	if got := NormalizeCodexToolSchemas(invalid); !bytes.Equal(got, invalid) {
		t.Fatalf("invalid payload changed: %s", got)
	}
}
