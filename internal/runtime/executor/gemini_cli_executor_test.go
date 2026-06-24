package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCleanGeminiCLIRequestSchemas(t *testing.T) {
	input := []byte(`{
		"request": {
			"tools": [{
				"functionDeclarations": [{
					"name": "lookup",
					"parameters": {
						"$schema": "http://json-schema.org/draft-07/schema#",
						"type": "object",
						"properties": {
							"mode": {
								"type": "string",
								"enum": ["fast", "deep"],
								"enumTitles": ["Fast", "Deep"]
							}
						}
					}
				}]
			}],
			"generationConfig": {
				"responseSchema": {
					"type": "object",
					"properties": {
						"payload": {
							"type": "object",
							"prefill": "remove me",
							"properties": {"value": {"type": "string"}}
						}
					}
				}
			}
		}
	}`)

	output := cleanGeminiCLIRequestSchemas(input)

	if gjson.GetBytes(output, "request.tools.0.functionDeclarations.0.parameters.$schema").Exists() {
		t.Fatalf("tool parameters still contain $schema: %s", output)
	}
	if gjson.GetBytes(output, "request.tools.0.functionDeclarations.0.parameters.properties.mode.enumTitles").Exists() {
		t.Fatalf("tool parameters still contain enumTitles: %s", output)
	}
	if gjson.GetBytes(output, "request.generationConfig.responseSchema.properties.payload.prefill").Exists() {
		t.Fatalf("response schema still contains prefill: %s", output)
	}
	if !gjson.GetBytes(
		output, "request.tools.0.functionDeclarations.0.parameters.properties.mode.description",
	).Exists() {
		t.Fatalf("enum description was not preserved after cleanup: %s", output)
	}
}
