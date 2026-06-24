package common

import (
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DefaultSafetySettings returns the default Gemini safety configuration we attach to requests.
func DefaultSafetySettings() []map[string]string {
	return []map[string]string{
		{
			"category":  "HARM_CATEGORY_HARASSMENT",
			"threshold": "OFF",
		},
		{
			"category":  "HARM_CATEGORY_HATE_SPEECH",
			"threshold": "OFF",
		},
		{
			"category":  "HARM_CATEGORY_SEXUALLY_EXPLICIT",
			"threshold": "OFF",
		},
		{
			"category":  "HARM_CATEGORY_DANGEROUS_CONTENT",
			"threshold": "OFF",
		},
		{
			"category":  "HARM_CATEGORY_CIVIC_INTEGRITY",
			"threshold": "BLOCK_NONE",
		},
	}
}

// AttachDefaultSafetySettings ensures the default safety settings are present when absent.
// The caller must provide the target JSON path (e.g. "safetySettings" or "request.safetySettings").
func AttachDefaultSafetySettings(rawJSON []byte, path string) []byte {
	if gjson.GetBytes(rawJSON, path).Exists() {
		return rawJSON
	}

	out, err := sjson.SetBytes(rawJSON, path, DefaultSafetySettings())
	if err != nil {
		return rawJSON
	}

	return out
}

func CleanToolSchemas(rawJSON []byte) []byte {
	out := rawJSON
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		return out
	}

	for toolIndex := range tools.Array() {
		for _, declarationsKey := range []string{"functionDeclarations", "function_declarations"} {
			declarationsPath := fmt.Sprintf("tools.%d.%s", toolIndex, declarationsKey)
			declarations := gjson.GetBytes(out, declarationsPath)
			if !declarations.IsArray() {
				continue
			}
			for declarationIndex := range declarations.Array() {
				declarationPath := fmt.Sprintf("%s.%d", declarationsPath, declarationIndex)
				out, _ = sjson.DeleteBytes(out, declarationPath+".eager_input_streaming")
				for _, schemaKey := range []string{"parametersJsonSchema", "parameters"} {
					schemaPath := declarationPath + "." + schemaKey
					schema := gjson.GetBytes(out, schemaPath)
					if !schema.Exists() {
						continue
					}
					cleaned := util.CleanJSONSchemaForGemini(schema.Raw)
					out, _ = sjson.SetRawBytes(out, schemaPath, []byte(cleaned))
				}
			}
		}
	}
	return out
}
