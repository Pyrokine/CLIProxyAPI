package helps

import (
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var emptyGeminiUserTurnJSON = []byte(`{"role":"user","parts":[{"text":""}]}`)

// EnsureGeminiLeadingUserContent ensures that the contents array at path starts with a user turn.
func EnsureGeminiLeadingUserContent(payload []byte, path string) []byte {
	if gjson.GetBytes(payload, path+".0.role").String() != "model" {
		return payload
	}
	contents := util.GetGJSONBytesNoCopy(payload, path)
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}

	contentItems := make([][]byte, 0, len(contentArray)+1)
	contentItems = append(contentItems, emptyGeminiUserTurnJSON)
	for _, content := range contentArray {
		contentItems = append(contentItems, []byte(content.Raw))
	}

	out, errSet := sjson.SetRawBytes(payload, path, translatorcommon.JoinRawArray(contentItems))
	if errSet != nil {
		return payload
	}
	return out
}

func contentHasFunctionResponse(content gjson.Result) bool {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return false
	}
	for _, part := range parts.Array() {
		if part.Get("functionResponse").Exists() {
			return true
		}
	}
	return false
}

// EnsureGeminiTrailingUserContent ensures that the contents array at path ends with a user turn.
// A trailing functionResponse remains unchanged because Gemini should generate the following model turn.
func EnsureGeminiTrailingUserContent(payload []byte, path string) []byte {
	contents := util.GetGJSONBytesNoCopy(payload, path)
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}
	lastContent := contentArray[len(contentArray)-1]
	lastRole := lastContent.Get("role").String()
	if (lastRole != "model" && lastRole != "assistant") || contentHasFunctionResponse(lastContent) {
		return payload
	}

	contentItems := make([][]byte, 0, len(contentArray)+1)
	for _, content := range contentArray {
		contentItems = append(contentItems, []byte(content.Raw))
	}
	contentItems = append(contentItems, emptyGeminiUserTurnJSON)

	out, errSet := sjson.SetRawBytes(payload, path, translatorcommon.JoinRawArray(contentItems))
	if errSet != nil {
		return payload
	}
	return out
}

// EnsureGeminiBoundaryUserContent ensures that the contents array at path starts and ends with a user turn.
func EnsureGeminiBoundaryUserContent(payload []byte, path string) []byte {
	payload = EnsureGeminiLeadingUserContent(payload, path)
	return EnsureGeminiTrailingUserContent(payload, path)
}
