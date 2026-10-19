package claude

import (
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeRegisteredOpenAIBooleanSubschemas(t *testing.T) {
	input := []byte(`{"tools":[{"name":"fixture","input_schema":{
		"type":"object",
		"properties":{
			"anything":true,
			"disabled":false,
			"patch":{"type":"array","items":true,"prefixItems":[true,false]},
			"either":{"anyOf":[true,{"type":"string"}]},
			"nested":{"type":"object","properties":{"value":true},"additionalProperties":true},
			"flag":{"type":"boolean","default":true,"enum":[true,false],"examples":[{"properties":{"data":true},"items":true}]},
			"large":{"type":"integer","default":9007199254740993,"enum":[9007199254740993]},
			"properties":{"type":"object","properties":{"items":true,"default":true,"additionalProperties":true}},
			"map":{"type":"object","additionalProperties":{"type":"array","items":true}}
		},
		"$defs":{"wildcard":true,"reject":false},
		"definitions":{"wildcard":true},
		"patternProperties":{"^x$":true},
		"dependentSchemas":{"flag":true},
		"dependencies":{"flag":["anything"],"patch":true},
		"additionalProperties":false,
		"default":{"properties":{"data":true},"items":true},
		"x-extra":{"properties":{"data":true},"items":true}
	}}],"messages":[{"role":"user","content":"fixture"}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.RequestEnvelope{
				Format: sdktranslator.FormatClaude, Model: "fixture-openai", Body: input, Stream: stream,
			})
			if envelope.Err != nil {
				t.Fatal(envelope.Err)
			}
			schema := gjson.GetBytes(envelope.Body, "tools.0.function.parameters")
			for _, path := range []string{
				"properties.anything", "properties.patch.items", "properties.patch.prefixItems.0", "properties.either.anyOf.0",
				"properties.nested.properties.value", "$defs.wildcard", "definitions.wildcard", "patternProperties.^x$",
				"dependentSchemas.flag", "dependencies.patch", "properties.properties.properties.items",
				"properties.properties.properties.default", "properties.properties.properties.additionalProperties", "properties.map.additionalProperties.items",
			} {
				if got := schema.Get(path); !got.IsObject() || got.Raw != "{}" {
					t.Errorf("schema position %s=%s, want {}; body=%s", path, got.Raw, envelope.Body)
				}
			}
			for _, path := range []string{"properties.disabled", "$defs.reject", "properties.patch.prefixItems.1", "additionalProperties", "properties.flag.enum.1"} {
				if schema.Get(path).Type != gjson.False {
					t.Errorf("false constraint/data %s changed: %s", path, schema.Raw)
				}
			}
			for _, path := range []string{
				"properties.nested.additionalProperties", "properties.flag.default", "properties.flag.enum.0",
				"properties.flag.examples.0.properties.data", "properties.flag.examples.0.items",
				"default.properties.data", "default.items", "x-extra.properties.data", "x-extra.items",
			} {
				if schema.Get(path).Type != gjson.True {
					t.Errorf("boolean data %s changed: %s", path, schema.Raw)
				}
			}
			for _, path := range []string{"properties.large.default", "properties.large.enum.0"} {
				if schema.Get(path).Raw != "9007199254740993" {
					t.Errorf("integer precision %s changed: %s", path, schema.Raw)
				}
			}
			if got := schema.Get("dependencies.flag").Raw; got != `["anything"]` {
				t.Errorf("property dependency changed: %s", got)
			}
		})
	}
}
