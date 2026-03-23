package common

import (
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeCLIRequest applies common transformations to a Gemini CLI request template:
//   - Fixes CLI tool response format
//   - Renames system_instruction → systemInstruction
//   - Normalizes content roles to valid "user"/"model" values
//   - Renames function_declarations.parameters → parametersJsonSchema
func NormalizeCLIRequest(template string) ([]byte, error) {
	var err error
	template, err = FixCLIToolResponse(template)
	if err != nil {
		return nil, err
	}

	if si := gjson.Get(template, "request.system_instruction"); si.Exists() {
		template, _ = sjson.SetRaw(template, "request.systemInstruction", si.Raw)
		template, _ = sjson.Delete(template, "request.system_instruction")
	}
	rawJSON := []byte(template)

	// Normalize roles in request.contents
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if contents.Exists() {
		prevRole := ""
		idx := 0
		contents.ForEach(func(_ gjson.Result, value gjson.Result) bool {
			role := value.Get("role").String()
			if role != "user" && role != "model" {
				var newRole string
				if prevRole == "" || prevRole == "model" {
					newRole = "user"
				} else {
					newRole = "model"
				}
				rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("request.contents.%d.role", idx), newRole)
				role = newRole
			}
			prevRole = role
			idx++
			return true
		})
	}

	// Rename function_declarations.parameters → parametersJsonSchema
	toolsResult := gjson.GetBytes(rawJSON, "request.tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		for i := range toolsResult.Array() {
			fdResult := gjson.GetBytes(rawJSON, fmt.Sprintf("request.tools.%d.function_declarations", i))
			if fdResult.Exists() && fdResult.IsArray() {
				for j := range fdResult.Array() {
					paramPath := fmt.Sprintf("request.tools.%d.function_declarations.%d.parameters", i, j)
					if gjson.GetBytes(rawJSON, paramPath).Exists() {
						str, _ := util.RenameKey(string(rawJSON), paramPath,
							fmt.Sprintf("request.tools.%d.function_declarations.%d.parametersJsonSchema", i, j))
						rawJSON = []byte(str)
					}
				}
			}
		}
	}

	return rawJSON, nil
}
