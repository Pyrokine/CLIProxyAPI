package responses

import (
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertResponsesDeclarationToOpenAIChat(declaration translatorcommon.ResponsesToolDeclaration) ([]byte, bool) {
	tool := gjson.Parse(declaration.Raw)
	if declaration.QualifiedName == "" {
		return nil, false
	}
	if declaration.Kind == translatorcommon.ResponsesCustomTool {
		chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}}`)
		chatTool, _ = sjson.SetBytes(chatTool, "function.name", declaration.QualifiedName)
		if description := responsesToolDescription(tool); description != "" {
			chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
		}
		return chatTool, true
	}

	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", declaration.QualifiedName)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	if parameters := responsesToolParameters(tool); parameters.Exists() {
		chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", []byte(parameters.Raw))
	}
	return chatTool, true
}

func responsesToolDescription(tool gjson.Result) string {
	if description := strings.TrimSpace(tool.Get("description").String()); description != "" {
		return description
	}
	return strings.TrimSpace(tool.Get("function.description").String())
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}
