package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func sanitizeAntigravityGeminiRequestSignatures(modelName string, payload []byte) []byte {
	if !antigravityUsesReasoningReplayCache(modelName) {
		return payload
	}
	payload = internalsignature.SanitizeGeminiRequestThoughtSignatures(payload, "request.contents")
	return normalizeAntigravityGeminiFunctionResponseRoles(payload)
}

var antigravityEmptyGeminiUserTurnJSON = []byte(`{"role":"user","parts":[{"text":""}]}`)

func ensureAntigravityGeminiLeadingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	if gjson.GetBytes(payload, "request.contents.0.role").String() != "model" {
		return payload
	}
	return insertAntigravityGeminiBoundaryTurn(payload, true)
}

func ensureAntigravityGeminiTrailingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}
	last := contentArray[len(contentArray)-1]
	role := strings.ToLower(strings.TrimSpace(last.Get("role").String()))
	if (role != "model" && role != "assistant") || antigravityContentHasFunctionResponse(last) {
		return payload
	}
	return insertAntigravityGeminiBoundaryTurn(payload, false)
}

func ensureAntigravityGeminiBoundaryUserContent(modelName string, payload []byte) []byte {
	payload = ensureAntigravityGeminiLeadingUserContent(modelName, payload)
	return ensureAntigravityGeminiTrailingUserContent(modelName, payload)
}

func insertAntigravityGeminiBoundaryTurn(payload []byte, leading bool) []byte {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}
	items := make([][]byte, 0, len(contentArray)+1)
	if leading {
		items = append(items, antigravityEmptyGeminiUserTurnJSON)
	}
	for _, content := range contentArray {
		items = append(items, []byte(content.Raw))
	}
	if !leading {
		items = append(items, antigravityEmptyGeminiUserTurnJSON)
	}
	updated, errSet := sjson.SetRawBytes(payload, "request.contents", translatorcommon.JoinRawArray(items))
	if errSet != nil {
		return payload
	}
	return updated
}

func antigravityContentHasFunctionResponse(content gjson.Result) bool {
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

type antigravityContentEdit struct {
	index       int64
	path        string
	start       int
	end         int
	replacement []byte
}

// normalizeAntigravityGeminiFunctionResponseRoles edits each response turn in
// isolation, then splices all changed turns into the request with one body copy.
func normalizeAntigravityGeminiFunctionResponseRoles(rawJSON []byte) []byte {
	rawJSON = repairAntigravityGeminiFunctionResponseNames(rawJSON)
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	type functionRef struct {
		id   string
		name string
	}

	edits := make([]antigravityContentEdit, 0)
	var pending []functionRef
	validOffsets := true
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			pending = nil
			return true
		}

		var calls, responses []functionRef
		var responseParts, otherParts []json.RawMessage
		partCount := 0
		hasOtherPart := false
		parts.ForEach(func(_, part gjson.Result) bool {
			partCount++
			switch {
			case part.Get("functionCall").Exists():
				calls = append(calls, functionRef{
					id:   part.Get("functionCall.id").String(),
					name: part.Get("functionCall.name").String(),
				})
			case part.Get("functionResponse").Exists():
				responses = append(responses, functionRef{
					id:   part.Get("functionResponse.id").String(),
					name: part.Get("functionResponse.name").String(),
				})
				responseParts = append(responseParts, json.RawMessage(part.Raw))
			default:
				hasOtherPart = true
				otherParts = append(otherParts, json.RawMessage(part.Raw))
			}
			return true
		})
		if partCount == 0 {
			pending = nil
			return true
		}
		if len(calls) > 0 && len(responses) == 0 {
			pending = calls
			return true
		}
		if len(responses) == 0 {
			if hasOtherPart {
				pending = nil
			}
			return true
		}
		if len(calls) > 0 {
			pending = nil
			return true
		}

		var contentJSON []byte
		contentChanged := false
		if len(pending) > 0 {
			ordered := make([]json.RawMessage, 0, partCount)
			used := make([]bool, len(responses))
			for _, call := range pending {
				for responseIndex, response := range responses {
					if used[responseIndex] {
						continue
					}
					if (call.id != "" && response.id == call.id) ||
						(call.id == "" && call.name != "" && response.name == call.name) {
						used[responseIndex] = true
						ordered = append(ordered, responseParts[responseIndex])
						break
					}
				}
			}
			for responseIndex := range responses {
				if !used[responseIndex] {
					ordered = append(ordered, responseParts[responseIndex])
				}
			}
			ordered = append(ordered, otherParts...)
			encoded, errMarshal := json.Marshal(ordered)
			if errMarshal == nil && !bytes.Equal(encoded, []byte(parts.Raw)) {
				contentJSON = []byte(content.Raw)
				if updated, errSet := sjson.SetRawBytes(contentJSON, "parts", encoded); errSet == nil {
					contentJSON = updated
					contentChanged = true
				}
			}
		}
		pending = nil
		if !hasOtherPart && content.Get("role").String() != "model" {
			if contentJSON == nil {
				contentJSON = []byte(content.Raw)
			}
			if updated, errSet := sjson.SetBytes(contentJSON, "role", "model"); errSet == nil {
				contentJSON = updated
				contentChanged = true
			}
		}
		if !contentChanged {
			return true
		}

		start := content.Index
		end := start + len(content.Raw)
		if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(content.Raw)) {
			validOffsets = false
		}
		edits = append(edits, antigravityContentEdit{
			index:       contentIndex.Int(),
			start:       start,
			end:         end,
			replacement: contentJSON,
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

// applyAntigravityIndexedEdits splices collected JSON fragments into the original
// request with one body copy.
func applyAntigravityIndexedEdits(rawJSON []byte, edits []antigravityContentEdit, validOffsets bool) []byte {
	if len(edits) == 0 {
		return rawJSON
	}
	if !validOffsets {
		return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
	}

	finalSize := len(rawJSON)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		cursor = edit.end
	}
	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, rawJSON[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	return append(out, rawJSON[cursor:]...)
}

// applyAntigravityContentEditsWithSJSON preserves path semantics if a GJSON
// result cannot be proven to point into the original request bytes.
func applyAntigravityContentEditsWithSJSON(rawJSON []byte, edits []antigravityContentEdit) []byte {
	out := rawJSON
	for _, edit := range edits {
		path := edit.path
		if path == "" {
			path = fmt.Sprintf("request.contents.%d", edit.index)
		}
		if updated, errSet := sjson.SetRawBytes(out, path, edit.replacement); errSet == nil {
			out = updated
		}
	}
	return out
}

// repairAntigravityGeminiFunctionResponseNames copies missing or placeholder
// functionResponse names from the matching functionCall.
func repairAntigravityGeminiFunctionResponseNames(rawJSON []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	callIDToName := make(map[string]string)
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			call := part.Get("functionCall")
			if call.Exists() {
				id := strings.TrimSpace(call.Get("id").String())
				name := strings.TrimSpace(call.Get("name").String())
				if id != "" && name != "" && name != "unknown" {
					callIDToName[id] = name
				}
			}
			return true
		})
		return true
	})
	if len(callIDToName) == 0 {
		return rawJSON
	}

	edits := make([]antigravityContentEdit, 0)
	validOffsets := true
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(partIndex, part gjson.Result) bool {
			response := part.Get("functionResponse")
			if !response.Exists() {
				return true
			}
			id := strings.TrimSpace(response.Get("id").String())
			name := strings.TrimSpace(response.Get("name").String())
			if id == "" || (name != "" && name != "unknown") {
				return true
			}
			realName, ok := callIDToName[id]
			if !ok {
				return true
			}
			updatedPart, errSet := sjson.SetBytes([]byte(part.Raw), "functionResponse.name", realName)
			if errSet != nil {
				return true
			}
			start := part.Index
			end := start + len(part.Raw)
			if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(part.Raw)) {
				validOffsets = false
			}
			edits = append(edits, antigravityContentEdit{
				index:       contentIndex.Int(),
				path:        fmt.Sprintf("request.contents.%d.parts.%d", contentIndex.Int(), partIndex.Int()),
				start:       start,
				end:         end,
				replacement: updatedPart,
			})
			return true
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}
