package responses

import (
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func responsesChatToolNameMap(identity *translatorcommon.ResponsesToolIdentity, request []byte) map[string]string {
	declarations := identity.Declarations()
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		names = append(names, declaration.QualifiedName)
	}
	root := gjson.ParseBytes(request)
	var historyNames []string
	for _, item := range root.Get("input").Array() {
		if kind := item.Get("type").String(); kind == "function_call" || kind == "custom_tool_call" {
			historyNames = append(historyNames, identity.ResolveName(item.Get("name").String(), item.Get("namespace").String()))
		}
	}
	name, namespace := responsesToolChoiceName(root.Get("tool_choice"))
	historyNames = append(historyNames, identity.ResolveName(name, namespace))
	return util.ChatFunctionNameMap(names, historyNames...)
}

func responsesToolChoiceName(choice gjson.Result) (string, string) {
	var name, namespace string
	for _, prefix := range []string{"", "function.", "custom."} {
		if name == "" {
			name = choice.Get(prefix + "name").String()
		}
		if namespace == "" {
			namespace = choice.Get(prefix + "namespace").String()
		}
	}
	return name, namespace
}

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
	for _, path := range []string{"description", "function.description"} {
		if description := tool.Get(path).String(); strings.TrimSpace(description) != "" {
			return description
		}
	}
	return ""
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
