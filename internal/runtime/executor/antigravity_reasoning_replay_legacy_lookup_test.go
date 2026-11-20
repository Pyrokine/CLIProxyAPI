package executor

// 顺序扫描参考算法独立定位工具、结果和上下文，不使用生产请求索引

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyAntigravityFunctionResponseContentIndexForReplay(
	payload []byte,
	itemResult gjson.Result,
) (int, string, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	candidateIDs := []string{callID}
	if stableID := util.GeminiClaudeToolUseID(
		callID, name, args.Raw,
	); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if contentIndex, ok := legacyAntigravityFunctionResponseContentIndex(payload, candidateID); ok {
			return contentIndex, candidateID, true
		}
	}
	return -1, "", false
}

func legacyAntigravityFunctionResponseContentIndex(payload []byte, callID string) (int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, false
	}
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			fr := part.Get("functionResponse")
			if fr.Exists() && strings.TrimSpace(fr.Get("id").String()) == callID {
				return i, true
			}
		}
	}
	return -1, false
}

func legacyAntigravityPayloadHasFunctionCallID(payload []byte, callID string) bool {
	_, _, ok := legacyAntigravityFunctionCallPartLocation(payload, callID)
	return ok
}

func legacyAntigravityFunctionCallPartLocation(
	payload []byte,
	callID string,
) (contentIndex int, partIndex int, ok bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for pi, part := range parts.Array() {
			fc := part.Get("functionCall")
			if fc.Exists() && strings.TrimSpace(fc.Get("id").String()) == callID {
				return ci, pi, true
			}
		}
	}
	return -1, -1, false
}

func legacyAntigravityFunctionCallPartLocationForReplayWithSchemas(
	payload []byte,
	itemResult gjson.Result,
	toolSchemas map[string]any,
) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		return -1, -1, false
	}
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	candidateIDs := []string{callID}
	if stableID := util.GeminiClaudeToolUseID(
		callID, name, args.Raw,
	); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if candidateID == "" {
			continue
		}
		ci, pi, found := legacyAntigravityFunctionCallPartLocation(payload, candidateID)
		if !found {
			continue
		}
		if legacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
			fc := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
			if antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				return ci, pi, true
			}
			log.Debugf(
				"antigravity replay: located call %q at contents[%d].parts[%d] "+
					"but name/args did not match ledger item (opaque_id=%t)",
				name,
				ci,
				pi,
				util.IsGeminiClaudeToolUseID(candidateID),
			)
			return -1, -1, false
		}
		// The candidate ID matched exactly, so callID+name+args are already proven
		// identical. Only the surrounding context drifted, which invalidates the
		// cached signature but not the tool identity.
		log.Debugf(
			"antigravity replay: exact tool ID match for %q at "+
				"contents[%d].parts[%d] rejected by context hash "+
				"(opaque_id=%t)",
			name,
			ci,
			pi,
			util.IsGeminiClaudeToolUseID(candidateID),
		)
		return -1, -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	contentArr := contents.Array()
	cachedCI := int(itemResult.Get("contentIndex").Int())
	if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
		if cachedCI < 0 || cachedCI >= len(contentArr) ||
			!legacyAntigravityReplayItemContextMatches(payload, itemResult, cachedCI) {
			return -1, -1, false
		}
		wantedOccurrence := int(targetOccurrence.Int())
		occurrence := 0
		for pi, part := range contentArr[cachedCI].Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() ||
				(util.IsGeminiClaudeToolUseID(fc.Get("id").String()) &&
					fc.Get("id").String() !=
						util.GeminiClaudeToolUseID(
							callID, name, args.Raw,
						)) ||
				!antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				continue
			}
			if occurrence == wantedOccurrence {
				return cachedCI, pi, true
			}
			occurrence++
		}
		return -1, -1, false
	}

	matches := make([][2]int, 0, 1)
	for ci, content := range contentArr {
		if !legacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
			continue
		}
		for pi, part := range content.Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() ||
				(util.IsGeminiClaudeToolUseID(fc.Get("id").String()) &&
					fc.Get("id").String() !=
						util.GeminiClaudeToolUseID(
							callID, name, args.Raw,
						)) {
				continue
			}
			if antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				matches = append(matches, [2]int{ci, pi})
			}
		}
	}
	if len(matches) == 1 {
		return matches[0][0], matches[0][1], true
	}
	return -1, -1, false
}

func legacyAntigravityFunctionCallProvenanceLocation(
	payload []byte,
	itemResult gjson.Result,
	toolSchemas map[string]any,
) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if name == "" || !args.Exists() || callID == "" {
		return -1, -1, false
	}
	stableID := util.GeminiClaudeToolUseID(
		callID, name, args.Raw,
	)
	if stableID == "" || stableID == callID {
		return -1, -1, false
	}
	ci, pi, found := legacyAntigravityFunctionCallPartLocation(payload, stableID)
	if !found {
		return -1, -1, false
	}
	fc := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
	if !antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
		return -1, -1, false
	}
	return ci, pi, true
}

func legacyAntigravityRequestHasThoughtSignatureAt(payload []byte, itemResult gjson.Result) bool {
	partPath, ok := legacyAntigravityThoughtSignatureReplayPartPath(payload, itemResult)
	if !ok {
		return false
	}
	return antigravityHasNativeThoughtSignature(gjson.GetBytes(payload, partPath+".thoughtSignature").String())
}

//goland:noinspection DuplicatedCode
func legacyAntigravityThoughtSignatureReplayPartPath(payload []byte, itemResult gjson.Result) (string, bool) {
	ci := int(itemResult.Get("contentIndex").Int())
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return "", false
	}
	contentArr := contents.Array()
	if ci < 0 || ci >= len(contentArr) ||
		!strings.EqualFold(strings.TrimSpace(contentArr[ci].Get("role").String()), "model") {
		return "", false
	}
	parts := contentArr[ci].Get("parts")
	if !parts.IsArray() {
		return "", false
	}
	// 目标指纹定位片段，上下文指纹约束签名所属历史
	if !legacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
		return "", false
	}
	partArr := parts.Array()
	targetKind := strings.TrimSpace(itemResult.Get("targetKind").String())
	targetHash := strings.TrimSpace(itemResult.Get("targetHash").String())
	if targetHash != "" {
		if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
			wanted := int(targetOccurrence.Int())
			occurrence := 0
			for pi, part := range partArr {
				kind, fingerprint := antigravityReplayPartFingerprint(part)
				if fingerprint != targetHash || (targetKind != "" && kind != targetKind) {
					continue
				}
				if occurrence == wanted {
					return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
				}
				occurrence++
			}
			return "", false
		}
		pi := int(itemResult.Get("partIndex").Int())
		if pi >= 0 && pi < len(partArr) {
			kind, fingerprint := antigravityReplayPartFingerprint(partArr[pi])
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		for pi, part := range partArr {
			kind, fingerprint := antigravityReplayPartFingerprint(part)
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		return "", false
	}

	pi := int(itemResult.Get("partIndex").Int())
	if pi >= 0 && pi < len(partArr) && partArr[pi].Type != gjson.Null {
		if kind, _ := antigravityReplayPartFingerprint(partArr[pi]); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
		}
	}
	// Legacy cache entries may point at a streamed signature-only part after
	// multiple text chunks. Attach them to the last semantic part in the same
	// model content, never to a different turn.
	for candidate, p := range slices.Backward(partArr) {
		if kind, _ := antigravityReplayPartFingerprint(p); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, candidate), true
		}
	}
	return "", false
}

func legacyAntigravityReplayContextFingerprint(payload []byte, beforeContentIndex int) string {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() || beforeContentIndex < 0 {
		return ""
	}
	contentArr := contents.Array()
	if beforeContentIndex > len(contentArr) {
		return ""
	}
	var context strings.Builder
	for _, path := range []string{"request.systemInstruction", "request.tools", "request.toolConfig"} {
		if value := gjson.GetBytes(payload, path); value.Exists() {
			context.WriteString(path)
			context.WriteByte('\x00')
			context.Write(antigravityCanonicalReplayJSON([]byte(value.Raw)))
			context.WriteByte('\x00')
		}
	}
	for ci := range beforeContentIndex {
		content := contentArr[ci]
		context.WriteString(strings.ToLower(strings.TrimSpace(content.Get("role").String())))
		context.WriteByte('\x00')
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		parts.ForEach(
			func(_, part gjson.Result) bool {
				normalized := []byte(part.Raw)
				for _, signaturePath := range []string{
					"thoughtSignature",
					"thought_signature",
					"extra_content.google.thought_signature",
				} {
					normalized, _ = sjson.DeleteBytes(normalized, signaturePath)
				}
				context.Write(antigravityCanonicalReplayJSON(normalized))
				context.WriteByte('\x00')
				return true
			},
		)
	}
	if context.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(context.String()))
	return fmt.Sprintf("%x", sum[:])
}

func legacyAntigravityReplayItemContextMatches(payload []byte, itemResult gjson.Result, contentIndex int) bool {
	expected := strings.TrimSpace(itemResult.Get("contextHash").String())
	return expected == "" || expected == legacyAntigravityReplayContextFingerprint(payload, contentIndex)
}

func legacyAntigravitySetReplayItemContextHash(item []byte, payload []byte, contentIndex int) []byte {
	if contextHash := legacyAntigravityReplayContextFingerprint(payload, contentIndex); contextHash != "" {
		item, _ = sjson.SetBytes(item, "contextHash", contextHash)
	}
	return item
}
