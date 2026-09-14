package common

import (
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// defaultSafetySettings returns the default Gemini safety configuration we attach to requests.
func defaultSafetySettings() []map[string]string {
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
	if util.GetGJSONBytesNoCopy(rawJSON, path).Exists() {
		return rawJSON
	}

	out, err := sjson.SetBytes(rawJSON, path, defaultSafetySettings())
	if err != nil {
		return rawJSON
	}

	return out
}

func CleanToolSchemas(rawJSON []byte) []byte {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "tools")
	if !tools.IsArray() {
		return rawJSON
	}

	toolsChanged := false
	toolItems := translatorcommon.NewRawArrayItems(tools.Get("#").Int())
	tools.ForEach(
		func(_, tool gjson.Result) bool {
			toolRaw := []byte(tool.Raw)
			toolChanged := false
			for _, declarationsKey := range []string{"functionDeclarations", "function_declarations"} {
				declarations := util.GetGJSONBytesNoCopy(toolRaw, declarationsKey)
				if !declarations.IsArray() {
					continue
				}
				declarationsChanged := false
				declarationItems := translatorcommon.NewRawArrayItems(declarations.Get("#").Int())
				declarations.ForEach(
					func(_, declaration gjson.Result) bool {
						declarationRaw := []byte(declaration.Raw)
						declarationChanged := false
						if declaration.Get("eager_input_streaming").Exists() {
							declarationRaw, _ = sjson.DeleteBytes(declarationRaw, "eager_input_streaming")
							declarationChanged = true
						}
						for _, schemaKey := range []string{"parametersJsonSchema", "parameters"} {
							schema := util.GetGJSONBytesNoCopy(declarationRaw, schemaKey)
							if !schema.Exists() {
								continue
							}
							cleaned := util.CleanJSONSchemaForGemini(schema.Raw)
							if cleaned == schema.Raw {
								continue
							}
							declarationRaw, _ = sjson.SetRawBytes(
								declarationRaw,
								schemaKey,
								[]byte(cleaned),
							)
							declarationChanged = true
						}
						declarationsChanged = declarationsChanged || declarationChanged
						declarationItems = append(declarationItems, declarationRaw)
						return true
					},
				)
				if declarationsChanged {
					toolRaw, _ = sjson.SetRawBytes(
						toolRaw,
						declarationsKey,
						translatorcommon.JoinRawArray(declarationItems),
					)
					toolChanged = true
				}
			}
			toolsChanged = toolsChanged || toolChanged
			toolItems = append(toolItems, toolRaw)
			return true
		},
	)
	if !toolsChanged {
		return rawJSON
	}
	out, err := sjson.SetRawBytes(rawJSON, "tools", translatorcommon.JoinRawArray(toolItems))
	if err != nil {
		return rawJSON
	}
	return out
}
