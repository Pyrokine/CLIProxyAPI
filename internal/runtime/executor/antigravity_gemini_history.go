package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
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

func normalizeAntigravityGeminiFunctionResponseRoles(payload []byte) []byte {
	payload = repairAntigravityGeminiFunctionResponseNames(payload)
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	type functionRef struct {
		id   string
		name string
	}
	out := payload
	var pending []functionRef
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() || len(parts.Array()) == 0 {
			pending = nil
			continue
		}
		var calls []functionRef
		var responses []functionRef
		var responseParts []json.RawMessage
		hasOtherPart := false
		parts.ForEach(
			func(_, part gjson.Result) bool {
				switch {
				case part.Get("functionCall").Exists():
					calls = append(
						calls, functionRef{
							id:   part.Get("functionCall.id").String(),
							name: part.Get("functionCall.name").String(),
						},
					)
				case part.Get("functionResponse").Exists():
					responses = append(
						responses, functionRef{
							id:   part.Get("functionResponse.id").String(),
							name: part.Get("functionResponse.name").String(),
						},
					)
					responseParts = append(responseParts, json.RawMessage(part.Raw))
				default:
					hasOtherPart = true
				}
				return true
			},
		)
		if len(calls) > 0 && len(responses) == 0 {
			pending = calls
			continue
		}
		if len(responses) == 0 {
			if hasOtherPart {
				pending = nil
			}
			continue
		}
		if hasOtherPart || len(calls) > 0 {
			pending = nil
			continue
		}
		if len(pending) == len(responses) {
			ordered := make([]json.RawMessage, 0, len(responseParts))
			used := make([]bool, len(responses))
			for _, call := range pending {
				matched := -1
				for responseIndex, response := range responses {
					if used[responseIndex] {
						continue
					}
					if (call.id != "" && response.id == call.id) ||
						(call.id == "" && call.name != "" && response.name == call.name) {
						matched = responseIndex
						break
					}
				}
				if matched < 0 {
					ordered = nil
					break
				}
				used[matched] = true
				ordered = append(ordered, responseParts[matched])
			}
			if len(ordered) == len(responseParts) {
				if encoded, err := json.Marshal(ordered); err == nil {
					if updated, err := sjson.SetRawBytes(
						out,
						fmt.Sprintf("request.contents.%d.parts", contentIndex),
						encoded,
					); err == nil {
						out = updated
					}
				}
			}
		}
		pending = nil
		if content.Get("role").String() != "model" {
			if updated, err := sjson.SetBytes(
				out,
				fmt.Sprintf("request.contents.%d.role", contentIndex),
				"model",
			); err == nil {
				out = updated
			}
		}
	}
	return out
}

func repairAntigravityGeminiFunctionResponseNames(payload []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	callIDToName := make(map[string]string)
	contents.ForEach(
		func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			parts.ForEach(
				func(_, part gjson.Result) bool {
					call := part.Get("functionCall")
					if !call.Exists() {
						return true
					}
					id := strings.TrimSpace(call.Get("id").String())
					name := strings.TrimSpace(call.Get("name").String())
					if id != "" && name != "" && name != "unknown" {
						callIDToName[id] = name
					}
					return true
				},
			)
			return true
		},
	)
	if len(callIDToName) == 0 {
		return payload
	}

	out := payload
	contents.ForEach(
		func(contentIndex, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			parts.ForEach(
				func(partIndex, part gjson.Result) bool {
					response := part.Get("functionResponse")
					if !response.Exists() {
						return true
					}
					id := strings.TrimSpace(response.Get("id").String())
					name := strings.TrimSpace(response.Get("name").String())
					if id == "" || (name != "" && name != "unknown") {
						return true
					}
					if realName := callIDToName[id]; realName != "" {
						path := fmt.Sprintf(
							"request.contents.%d.parts.%d.functionResponse.name",
							contentIndex.Int(),
							partIndex.Int(),
						)
						if updated, err := sjson.SetBytes(out, path, realName); err == nil {
							out = updated
						}
					}
					return true
				},
			)
			return true
		},
	)
	return out
}
