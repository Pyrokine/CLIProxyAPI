package executor

// 顺序合并参考算法逐项修改正文，不调用生产批量或顺序回放入口

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(
	payload []byte,
	items [][]byte,
	toolSchemas map[string]any,
) [][]byte {
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "function_call_part":
			signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if contentIndex, partIndex, foundCall := legacyAntigravityFunctionCallPartLocationForReplayWithSchemas(
				payload, itemResult, toolSchemas,
			); foundCall {
				part := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d", contentIndex, partIndex))
				currentID := strings.TrimSpace(part.Get("functionCall.id").String())
				nativeID := strings.TrimSpace(itemResult.Get("call_id").String())
				needsNativeRestore := currentID != nativeID || !bytes.Equal(
					antigravityCanonicalReplayJSON([]byte(part.Get("functionCall.args").Raw)),
					antigravityCanonicalReplayJSON([]byte(itemResult.Get("args").Raw)),
				)
				if !needsNativeRestore &&
					(signature == "" || antigravityHasNativeThoughtSignature(part.Get("thoughtSignature").String())) {
					continue
				}
				break
			}
			if _, _, foundProvenance := legacyAntigravityFunctionCallProvenanceLocation(
				payload, itemResult, toolSchemas,
			); foundProvenance {
				break
			}
			callID := strings.TrimSpace(itemResult.Get("call_id").String())
			if callID == "" {
				continue
			}
			responseIndex, _, foundResponse := legacyAntigravityFunctionResponseContentIndexForReplay(
				payload, itemResult,
			)
			if !foundResponse {
				continue
			}
			contextMatches := legacyAntigravityReplayItemContextMatches(payload, itemResult, responseIndex)
			if !contextMatches && responseIndex > 0 {
				previousRole := gjson.GetBytes(
					payload, fmt.Sprintf("request.contents.%d.role", responseIndex-1),
				).String()
				contextMatches = strings.EqualFold(strings.TrimSpace(previousRole), "model") &&
					legacyAntigravityReplayItemContextMatches(payload, itemResult, responseIndex-1)
			}
			if !contextMatches {
				continue
			}
		case "thought_signature":
			if legacyAntigravityRequestHasThoughtSignatureAt(payload, itemResult) {
				continue
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func legacyInsertAntigravityReasoningReplayItemsWithSchemas(
	payload []byte,
	items [][]byte,
	toolSchemas map[string]any,
) ([]byte, bool) {
	out := payload
	changed := false
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "thought_signature":
			sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if sig == "" {
				continue
			}
			partPath, exists := legacyAntigravityThoughtSignatureReplayPartPath(out, itemResult)
			if !exists {
				continue
			}
			path := partPath + ".thoughtSignature"
			if antigravityHasNativeThoughtSignature(gjson.GetBytes(out, path).String()) {
				continue
			}
			ci := int(itemResult.Get("contentIndex").Int())
			out = antigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
			updated, err := sjson.SetBytes(out, path, sig)
			if err != nil {
				continue
			}
			out = updated
			changed = true
		case "function_call_part":
			updated, ok := legacyMergeAntigravityFunctionCallPartReplayWithSchemas(out, itemResult, toolSchemas)
			if ok {
				out = updated
				changed = true
			}
		}
	}
	return out, changed
}

//goland:noinspection DuplicatedCode
func legacyMergeAntigravityFunctionCallPartReplayWithSchemas(
	payload []byte,
	itemResult gjson.Result,
	toolSchemas map[string]any,
) ([]byte, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if name == "" || !args.Exists() {
		return payload, false
	}
	if ci, pi, exists := legacyAntigravityFunctionCallPartLocationForReplayWithSchemas(
		payload, itemResult, toolSchemas,
	); exists {
		_, allowLegacyIDRestore := toolSchemas[name]
		return restoreAntigravityNativeFunctionCallReplay(payload, ci, pi, itemResult, allowLegacyIDRestore, true)
	}
	if ci, pi, exists := legacyAntigravityFunctionCallProvenanceLocation(
		payload, itemResult, toolSchemas,
	); exists {
		return restoreAntigravityNativeFunctionCallReplay(
			payload, ci, pi, itemResult, false, legacyAntigravityReplayItemContextMatches(payload, itemResult, ci),
		)
	}
	if callID != "" {
		stableID := util.GeminiClaudeToolUseID(
			callID, name, args.Raw,
		)
		if legacyAntigravityPayloadHasFunctionCallID(payload, callID) ||
			(stableID != "" && legacyAntigravityPayloadHasFunctionCallID(payload, stableID)) {
			// The call is already in the history under its native or Claude-facing
			// ID, and neither lookup above accepted it, so the client changed it.
			// Never replay an opaque signature onto that changed call, and never
			// insert a second copy of it further down.
			return payload, false
		}
		if frIndex, currentResponseID, ok := legacyAntigravityFunctionResponseContentIndexForReplay(
			payload, itemResult,
		); ok {
			parallelModelIndex := frIndex - 1
			if parallelModelIndex >= 0 &&
				strings.EqualFold(
					strings.TrimSpace(
						gjson.GetBytes(
							payload, fmt.Sprintf("request.contents.%d.role", parallelModelIndex),
						).String(),
					),
					"model",
				) &&
				legacyAntigravityReplayItemContextMatches(payload, itemResult, parallelModelIndex) {
				if updated, appended := appendAntigravityFunctionCallToModelContent(
					payload, parallelModelIndex, name, callID, sig, args,
				); appended {
					return restoreAntigravityFunctionResponseReplayIdentity(
						updated, currentResponseID, callID, name,
					), true
				}
			}
			if legacyAntigravityReplayItemContextMatches(payload, itemResult, frIndex) {
				if updated, inserted := insertAntigravityModelFunctionCallBeforeContent(
					payload, frIndex, name, callID, sig, args,
				); inserted {
					return restoreAntigravityFunctionResponseReplayIdentity(
						updated, currentResponseID, callID, name,
					), true
				}
			}
		}
	} else {
		// Without a native call ID, only an exact semantic match is safe. Never
		// put an opaque signature on a different call at the old numeric slot.
		return payload, false
	}

	ci := antigravityReasoningReplayResolveContentIndex(payload, int(itemResult.Get("contentIndex").Int()))
	if ci < 0 || !legacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
		return payload, false
	}
	pi := int(itemResult.Get("partIndex").Int())
	out := payload
	changed := false

	partPath, exists := antigravityExistingReplayPartPath(out, ci, pi)
	if !exists {
		fc := map[string]any{"name": name, "id": callID}
		if args.Type == gjson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		part := map[string]any{"functionCall": fc}
		if sig != "" {
			part["thoughtSignature"] = sig
		}
		if updated, err := sjson.SetBytes(out, antigravityReplayPartWritePath(out, ci, pi), part); err == nil {
			return updated, true
		}
		return payload, false
	}

	pathSig := partPath + ".thoughtSignature"
	if sig != "" && !antigravityHasNativeThoughtSignature(gjson.GetBytes(out, pathSig).String()) {
		out = antigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
		if updated, err := sjson.SetBytes(out, pathSig, sig); err == nil {
			out = updated
			changed = true
		}
	}
	pathFC := partPath + ".functionCall"
	if !gjson.GetBytes(out, pathFC).Exists() {
		fc := map[string]any{"name": name, "id": callID}
		if args.Type == gjson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		if updated, err := sjson.SetBytes(out, pathFC, fc); err == nil {
			out = updated
			changed = true
		}
	}
	return out, changed
}
