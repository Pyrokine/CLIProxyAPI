package claude

import (
	"encoding/base64"
	"fmt"
	"strings"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxBypassSignatureLen = 32 * 1024 * 1024

	geminiClaudeCarrierPrefix     = "cpa-gemini-carrier-v1:"
	geminiClaudeCarrierNext       = "next"
	geminiClaudeCarrierPrevious   = "previous"
	geminiClaudeCarrierStandalone = "standalone"
	geminiClaudeCarrierText       = "text"
	geminiClaudeCarrierFunction   = "function"
	geminiClaudeCarrierAny        = "any"
)

func encodeGeminiClaudeCarrierSignature(rawSignature, direction, targetKind string) string {
	rawSignature = strings.TrimSpace(rawSignature)
	if rawSignature == "" {
		return ""
	}
	return geminiClaudeCarrierPrefix + direction + ":" + targetKind + ":" +
		base64.RawStdEncoding.EncodeToString([]byte(rawSignature))
}

func decodeGeminiClaudeCarrierSignature(
	rawSignature string,
) (signatureValue, direction, targetKind string, marked, ok bool) {
	rawSignature = strings.TrimSpace(rawSignature)
	if !strings.HasPrefix(rawSignature, geminiClaudeCarrierPrefix) {
		return rawSignature, "", "", false, true
	}
	marked = true
	if len(rawSignature) > (maxBypassSignatureLen*4/3)+1024 {
		return "", "", "", true, false
	}
	fields := strings.SplitN(strings.TrimPrefix(rawSignature, geminiClaudeCarrierPrefix), ":", 3)
	if len(fields) != 3 {
		return "", "", "", true, false
	}
	direction, targetKind = fields[0], fields[1]
	switch direction {
	case geminiClaudeCarrierNext, geminiClaudeCarrierPrevious, geminiClaudeCarrierStandalone:
	default:
		return "", "", "", true, false
	}
	switch targetKind {
	case geminiClaudeCarrierText, geminiClaudeCarrierFunction, geminiClaudeCarrierAny:
	default:
		return "", "", "", true, false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(fields[2])
	if err != nil || len(decoded) == 0 || strings.HasPrefix(string(decoded), geminiClaudeCarrierPrefix) {
		return "", "", "", true, false
	}
	blockKind := sigcompat.BlockKindGeminiModelPart
	if targetKind == geminiClaudeCarrierFunction {
		blockKind = sigcompat.BlockKindGeminiFunctionCall
	}
	normalized, compatible := sigcompat.CompatibleSignatureForProvider(
		sigcompat.ProviderGemini,
		string(decoded),
		blockKind,
	)
	if !compatible || sigcompat.IsGeminiBypass(sigcompat.PayloadWithoutProviderPrefix(normalized)) {
		return "", "", "", true, false
	}
	return normalized, direction, targetKind, true, true
}

func geminiClaudeSemanticTargetKind(block gjson.Result) string {
	switch block.Get("type").String() {
	case "text":
		return geminiClaudeCarrierText
	case "tool_use":
		return geminiClaudeCarrierFunction
	case "thinking":
		if strings.TrimSpace(block.Get("thinking").String()) != "" {
			return geminiClaudeCarrierText
		}
	}
	return ""
}

func geminiClaudeCarrierMatchesAdjacent(
	blocks []gjson.Result,
	index int,
	direction string,
	targetKind string,
) bool {
	step := 1
	if direction == geminiClaudeCarrierPrevious {
		step = -1
	}
	for adjacent := index + step; adjacent >= 0 && adjacent < len(blocks); adjacent += step {
		if kind := geminiClaudeSemanticTargetKind(blocks[adjacent]); kind != "" {
			return targetKind == geminiClaudeCarrierAny || targetKind == kind
		}
		if blocks[adjacent].Get("type").String() != "thinking" ||
			strings.TrimSpace(blocks[adjacent].Get("thinking").String()) != "" {
			return false
		}
	}
	return false
}

// StripEmptySignatureThinkingBlocks removes thinking blocks whose signatures are
// empty or not in Claude format after stripping an optional cache prefix.
func StripEmptySignatureThinkingBlocks(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	modified := false
	for i, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}

		kept := make([]string, 0, len(content.Array()))
		stripped := false
		for _, part := range content.Array() {
			if part.Get("type").String() == "thinking" && !hasValidClaudeSignature(part.Get("signature").String()) {
				stripped = true
				continue
			}
			kept = append(kept, part.Raw)
		}
		if !stripped {
			continue
		}

		modified = true
		if len(kept) == 0 {
			payload, _ = sjson.SetRawBytes(payload, fmt.Sprintf("messages.%d.content", i), []byte("[]"))
			continue
		}
		payload, _ = sjson.SetRawBytes(
			payload,
			fmt.Sprintf("messages.%d.content", i),
			[]byte("["+strings.Join(kept, ",")+"]"),
		)
	}
	if !modified {
		return payload
	}
	return payload
}

// StripInvalidGeminiSignatureThinkingBlocks preserves only thinking carriers
// whose signatures can be replayed to Gemini.
func StripInvalidGeminiSignatureThinkingBlocks(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	changed := false
	messageItems := make([][]byte, 0, len(messages.Array()))
	for _, message := range messages.Array() {
		messageJSON := []byte(message.Raw)
		content := message.Get("content")
		if !content.IsArray() {
			messageItems = append(messageItems, messageJSON)
			continue
		}
		contentChanged := false
		assistantMessage := strings.EqualFold(message.Get("role").String(), "assistant")
		contentBlocks := content.Array()
		contentItems := make([][]byte, 0, len(contentBlocks))
		pendingCarrierTargetKind := ""
		for blockIndex, block := range contentBlocks {
			if block.Get("type").String() == "thinking" {
				rawSignature := strings.TrimSpace(block.Get("signature").String())
				thinkingText := strings.TrimSpace(block.Get("thinking").String())
				if rawSignature == "" && thinkingText != "" &&
					(pendingCarrierTargetKind == geminiClaudeCarrierAny || pendingCarrierTargetKind == geminiClaudeCarrierText) {
					pendingCarrierTargetKind = ""
					contentItems = append(contentItems, []byte(block.Raw))
					continue
				}
				innerSignature, direction, targetKind, marked, okCarrier :=
					decodeGeminiClaudeCarrierSignature(rawSignature)
				blockKind := sigcompat.BlockKindGeminiModelPart
				if marked && targetKind == geminiClaudeCarrierFunction {
					blockKind = sigcompat.BlockKindGeminiFunctionCall
				}
				invalidMarkedPlacement := false
				if marked {
					switch direction {
					case geminiClaudeCarrierNext, geminiClaudeCarrierPrevious:
						invalidMarkedPlacement = !geminiClaudeCarrierMatchesAdjacent(
							contentBlocks,
							blockIndex,
							direction,
							targetKind,
						)
					case geminiClaudeCarrierStandalone:
						invalidMarkedPlacement = thinkingText != "" && targetKind == geminiClaudeCarrierFunction
					}
					if thinkingText != "" && direction == geminiClaudeCarrierPrevious {
						invalidMarkedPlacement = true
					}
				}
				if !okCarrier || !assistantMessage || invalidMarkedPlacement {
					pendingCarrierTargetKind = ""
					contentChanged = true
					continue
				}
				if !marked {
					innerSignature = rawSignature
				}
				if _, ok := sigcompat.CompatibleSignatureForProvider(
					sigcompat.ProviderGemini,
					innerSignature,
					blockKind,
				); !ok {
					pendingCarrierTargetKind = ""
					contentChanged = true
					continue
				}
				if marked && direction == geminiClaudeCarrierNext {
					pendingCarrierTargetKind = targetKind
				} else {
					pendingCarrierTargetKind = ""
				}
			} else {
				pendingCarrierTargetKind = ""
			}
			contentItems = append(contentItems, []byte(block.Raw))
		}
		if contentChanged {
			messageJSON, _ = sjson.SetRawBytes(
				messageJSON,
				"content",
				translatorcommon.JoinRawArray(contentItems),
			)
			changed = true
		}
		messageItems = append(messageItems, messageJSON)
	}
	if !changed {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, "messages", translatorcommon.JoinRawArray(messageItems))
	if err != nil {
		return payload
	}
	return updated
}

func hasValidClaudeSignature(sig string) bool {
	rawSignature := sig
	sig = stripSignatureCachePrefix(sig)
	if sig == "" {
		return false
	}
	if sig[0] == 'Q' {
		_, compatible := sigcompat.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
		return compatible
	}
	return sig[0] == 'E' || sig[0] == 'R'
}

func stripSignatureCachePrefix(sig string) string {
	sig = strings.TrimSpace(sig)
	if idx := strings.IndexByte(sig, '#'); idx >= 0 {
		sig = strings.TrimSpace(sig[idx+1:])
	}
	return sig
}
