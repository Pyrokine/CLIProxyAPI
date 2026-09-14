package common

import "testing"

func TestCleanToolSchemasReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"tools":[{"function_declarations":[{"name":"lookup","parametersJsonSchema":{"type":"object","properties":{"query":{"type":"string"}}}}]}]}`)

	output := CleanToolSchemas(input)

	if &output[0] != &input[0] {
		t.Fatal("normalized tool schemas caused a payload copy")
	}
}
