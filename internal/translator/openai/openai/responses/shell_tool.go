package responses

import (
	"errors"
	"math"
	"strconv"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errInvalidShellAction = errors.New("invalid local shell action: commands must be nonempty strings and limits must be positive integers")

type responsesLocalShell struct {
	Name      string
	Available bool
}

func newResponsesLocalShell(root gjson.Result, identity *translatorcommon.ResponsesToolIdentity, toolNames map[string]string) responsesLocalShell {
	var shell responsesLocalShell
	toolLists := []gjson.Result{root.Get("tools")}
	hasHistory := false
	reserved := make(map[string]bool)
	for _, declaration := range identity.Declarations() {
		reserved[declaration.Name] = true
		reserved[declaration.QualifiedName] = true
		reserved[toolNames[declaration.QualifiedName]] = true
	}
	for _, item := range root.Get("input").Array() {
		switch item.Get("type").String() {
		case "additional_tools":
			toolLists = append(toolLists, item.Get("tools"))
		case "function_call", "custom_tool_call":
			name := identity.ResolveName(item.Get("name").String(), item.Get("namespace").String())
			reserved[name] = true
			reserved[toolNames[name]] = true
		case "shell_call":
			environment := item.Get("environment.type")
			hasHistory = hasHistory || !environment.Exists() || environment.String() == "local"
		}
	}
	for _, tools := range toolLists {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "shell" && tool.Get("environment.type").String() == "local" {
				shell.Available = true
			}
		}
	}
	if !shell.Available && !hasHistory {
		return shell
	}
	shell.Name = "__cpa_local_shell"
	for suffix := 1; reserved[shell.Name]; suffix++ {
		shell.Name = "__cpa_local_shell_" + strconv.Itoa(suffix)
	}
	return shell
}

func (shell responsesLocalShell) matches(name string) bool {
	return shell.Available && name == shell.Name
}

func (shell responsesLocalShell) declaration() []byte {
	tool := []byte(`{"type":"function","function":{"name":"","description":"请求客户端在其本地 shell 环境中执行命令，每个 commands 项是一条完整命令","parameters":{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"},"minItems":1},"timeout_ms":{"type":"integer","minimum":1},"max_output_length":{"type":"integer","minimum":1}},"required":["commands"],"additionalProperties":false}}}`)
	tool, _ = sjson.SetBytes(tool, "function.name", shell.Name)
	return tool
}

func (shell responsesLocalShell) history(items []gjson.Result) []gjson.Result {
	if shell.Name == "" {
		return items
	}
	normalized := append([]gjson.Result(nil), items...)
	for index, item := range items {
		raw := []byte(item.Raw)
		switch item.Get("type").String() {
		case "shell_call":
			if environment := item.Get("environment.type"); environment.Exists() && environment.String() != "local" {
				continue
			}
			raw, _ = sjson.SetBytes(raw, "type", "function_call")
			raw, _ = sjson.SetBytes(raw, "name", shell.Name)
			raw, _ = sjson.SetBytes(raw, "arguments", item.Get("action").Raw)
		case "shell_call_output":
			raw, _ = sjson.SetBytes(raw, "type", "function_call_output")
			raw, _ = sjson.SetBytes(raw, "output", item.Raw)
		default:
			continue
		}
		normalized[index] = gjson.ParseBytes(raw)
	}
	return normalized
}

func shellCallItem(callID, arguments, status string) ([]byte, error) {
	if !gjson.Valid(arguments) {
		return nil, errInvalidShellAction
	}
	action := gjson.Parse(arguments)
	commands := action.Get("commands")
	if !action.IsObject() || !commands.IsArray() || len(commands.Array()) == 0 {
		return nil, errInvalidShellAction
	}
	for _, command := range commands.Array() {
		if command.Type != gjson.String || strings.TrimSpace(command.String()) == "" {
			return nil, errInvalidShellAction
		}
	}
	valid := true
	action.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "commands":
		case "timeout_ms", "max_output_length":
			number := value.Float()
			valid = value.Type == gjson.Null || (value.Type == gjson.Number && number > 0 && !math.IsInf(number, 0) && math.Trunc(number) == number)
		default:
			valid = false
		}
		return valid
	})
	if !valid {
		return nil, errInvalidShellAction
	}
	item := shellCallPlaceholder(callID)
	item, _ = sjson.SetBytes(item, "status", status)
	item, _ = sjson.SetRawBytes(item, "action", []byte(arguments))
	return item, nil
}

func shellCallPlaceholder(callID string) []byte {
	item := []byte(`{"id":"","type":"shell_call","status":"in_progress","call_id":"","action":{"commands":[]}}`)
	item, _ = sjson.SetBytes(item, "id", "sh_"+callID)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	return item
}

func responsesShellFailure(responseID string, err error) []byte {
	response := []byte(`{"id":"","object":"response","status":"failed","error":{"type":"server_error","code":"invalid_tool_arguments","message":"","param":null},"output":[]}`)
	response, _ = sjson.SetBytes(response, "id", responseID)
	response, _ = sjson.SetBytes(response, "error.message", err.Error())
	return response
}

func responsesShellFailureEvent(responseID string, sequence int, err error) []byte {
	event := []byte(`{"type":"response.failed","sequence_number":0,"response":{}}`)
	event, _ = sjson.SetBytes(event, "sequence_number", sequence)
	event, _ = sjson.SetRawBytes(event, "response", responsesShellFailure(responseID, err))
	return emitRespEvent("response.failed", event)
}
