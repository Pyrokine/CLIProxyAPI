// Package gemini provides in-provider request normalization for Gemini API.
// It ensures incoming v1beta requests meet minimal schema requirements
// expected by Google's Generative Language API.
package gemini

import (
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertGeminiRequestToGemini normalizes Gemini v1beta requests.
//   - Adds a default role for each content if missing or invalid.
//     The first message defaults to "user", then alternates user/model when needed.
//
// It keeps the payload otherwise unchanged.
func convertGeminiRequestToGemini(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Fast path: if no contents field, only attach safety settings
	contents := gjson.GetBytes(rawJSON, "contents")
	if !contents.Exists() {
		return common.AttachDefaultSafetySettings(rawJSON, "safetySettings")
	}

	// Rename camelCase functionDeclarations -> snake_case function_declarations
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		for i := range toolsResult.Array() {
			if gjson.GetBytes(rawJSON, fmt.Sprintf("tools.%d.functionDeclarations", i)).Exists() {
				strJson, _ := util.RenameKey(
					string(rawJSON), fmt.Sprintf("tools.%d.functionDeclarations", i),
					fmt.Sprintf("tools.%d.function_declarations", i),
				)
				rawJSON = []byte(strJson)
			}
		}
	}

	rawJSON = common.RenameToolParameters(rawJSON)

	// Walk contents and fix roles
	out := rawJSON
	prevRole := ""
	idx := 0
	contents.ForEach(
		func(_ gjson.Result, value gjson.Result) bool {
			role := value.Get("role").String()

			// Only user/model are valid for Gemini v1beta requests
			valid := role == "user" || role == "model"
			if role == "" || !valid {
				var newRole string
				if prevRole == "" {
					newRole = "user"
				} else if prevRole == "user" {
					newRole = "model"
				} else {
					newRole = "user"
				}
				path := fmt.Sprintf("contents.%d.role", idx)
				out, _ = sjson.SetBytes(out, path, newRole)
				role = newRole
			}

			prevRole = role
			idx++
			return true
		},
	)

	out = common.InjectThoughtSignatures(out, "contents")

	if gjson.GetBytes(rawJSON, "generationConfig.responseSchema").Exists() {
		strJson, _ := util.RenameKey(
			string(out), "generationConfig.responseSchema", "generationConfig.responseJsonSchema",
		)
		out = []byte(strJson)
	}

	out = common.AttachDefaultSafetySettings(out, "safetySettings")
	return out
}
