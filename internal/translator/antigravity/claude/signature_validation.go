package claude

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
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

type claudeSignatureTree struct {
	EncodingLayers      int
	ChannelID           uint64
	Field2              *uint64
	RoutingClass        string
	InfrastructureClass string
	SchemaFeatures      string
	ModelText           string
	LegacyRouteHint     string
	HasField7           bool
}

var strictClaudeBypassSignatureValidation atomic.Bool

func claudeBypassSignatureStrictMode() bool {
	return strictClaudeBypassSignatureValidation.Load()
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
	sig = stripSignatureCachePrefix(sig)
	if sig == "" {
		return false
	}
	return sig[0] == 'E' || sig[0] == 'R'
}

func normalizeClaudeBypassSignature(rawSignature string) (string, error) {
	sig := stripSignatureCachePrefix(rawSignature)
	if sig == "" {
		return "", fmt.Errorf("empty signature")
	}

	if len(sig) > maxBypassSignatureLen {
		return "", fmt.Errorf("signature exceeds maximum length (%d bytes)", maxBypassSignatureLen)
	}

	switch sig[0] {
	case 'R':
		if err := validateDoubleLayerSignature(sig); err != nil {
			return "", err
		}
		return sig, nil
	case 'E':
		if err := validateSingleLayerSignature(sig); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString([]byte(sig)), nil
	default:
		return "", fmt.Errorf("invalid signature: expected 'E' or 'R' prefix, got %q", string(sig[0]))
	}
}

func stripSignatureCachePrefix(sig string) string {
	sig = strings.TrimSpace(sig)
	if idx := strings.IndexByte(sig, '#'); idx >= 0 {
		sig = strings.TrimSpace(sig[idx+1:])
	}
	return sig
}

func validateDoubleLayerSignature(sig string) error {
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("invalid double-layer signature: base64 decode failed: %w", err)
	}
	if len(decoded) == 0 {
		return fmt.Errorf("invalid double-layer signature: empty after decode")
	}
	if decoded[0] != 'E' {
		return fmt.Errorf("invalid double-layer signature: inner does not start with 'E', got 0x%02x", decoded[0])
	}
	return validateSingleLayerSignatureContent(string(decoded), 2)
}

func validateSingleLayerSignature(sig string) error {
	return validateSingleLayerSignatureContent(sig, 1)
}

func validateSingleLayerSignatureContent(sig string, encodingLayers int) error {
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("invalid single-layer signature: base64 decode failed: %w", err)
	}
	if len(decoded) == 0 {
		return fmt.Errorf("invalid single-layer signature: empty after decode")
	}
	if decoded[0] != 0x12 {
		return fmt.Errorf("invalid Claude signature: expected first byte 0x12, got 0x%02x", decoded[0])
	}
	if !claudeBypassSignatureStrictMode() {
		return nil
	}
	_, err = inspectClaudeSignaturePayload(decoded, encodingLayers)
	return err
}

func inspectClaudeSignaturePayload(payload []byte, encodingLayers int) (*claudeSignatureTree, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("invalid Claude signature: empty payload")
	}
	if payload[0] != 0x12 {
		return nil, fmt.Errorf("invalid Claude signature: expected first byte 0x12, got 0x%02x", payload[0])
	}
	container, err := extractBytesField(payload, 2, "top-level protobuf")
	if err != nil {
		return nil, err
	}
	channelBlock, err := extractBytesField(container, 1, "Claude Field 2 container")
	if err != nil {
		return nil, err
	}
	return inspectClaudeChannelBlock(channelBlock, encodingLayers)
}

func inspectClaudeChannelBlock(channelBlock []byte, encodingLayers int) (*claudeSignatureTree, error) {
	tree := &claudeSignatureTree{
		EncodingLayers:      encodingLayers,
		RoutingClass:        "unknown",
		InfrastructureClass: "infra_unknown",
		SchemaFeatures:      "unknown_schema_features",
	}
	haveChannelID := false
	hasField6 := false
	hasField7 := false

	err := walkProtobufFields(
		channelBlock, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			switch num {
			case 1:
				if typ != protowire.VarintType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.1 channel_id must be varint")
				}
				channelID, err := decodeVarintField(raw, "Field 2.1.1 channel_id")
				if err != nil {
					return err
				}
				tree.ChannelID = channelID
				haveChannelID = true
			case 2:
				if typ != protowire.VarintType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.2 field2 must be varint")
				}
				field2, err := decodeVarintField(raw, "Field 2.1.2 field2")
				if err != nil {
					return err
				}
				tree.Field2 = &field2
			case 6:
				if typ != protowire.BytesType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.6 model_text must be bytes")
				}
				modelBytes, err := decodeBytesField(raw, "Field 2.1.6 model_text")
				if err != nil {
					return err
				}
				if !utf8.Valid(modelBytes) {
					return fmt.Errorf("invalid Claude signature: Field 2.1.6 model_text is not valid UTF-8")
				}
				tree.ModelText = string(modelBytes)
				hasField6 = true
			case 7:
				if typ != protowire.VarintType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.7 must be varint")
				}
				if _, err := decodeVarintField(raw, "Field 2.1.7"); err != nil {
					return err
				}
				hasField7 = true
				tree.HasField7 = true
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !haveChannelID {
		return nil, fmt.Errorf("invalid Claude signature: missing Field 2.1.1 channel_id")
	}

	switch tree.ChannelID {
	case 11:
		tree.RoutingClass = "routing_class_11"
	case 12:
		tree.RoutingClass = "routing_class_12"
	}

	if tree.Field2 == nil {
		tree.InfrastructureClass = "infra_default"
	} else {
		switch *tree.Field2 {
		case 1:
			tree.InfrastructureClass = "infra_aws"
		case 2:
			tree.InfrastructureClass = "infra_google"
		default:
			tree.InfrastructureClass = "infra_unknown"
		}
	}

	switch {
	case hasField6:
		tree.SchemaFeatures = "extended_model_tagged_schema"
	case !hasField6 && !hasField7 && len(channelBlock) >= 70 && len(channelBlock) <= 72:
		tree.SchemaFeatures = "compact_schema"
	}

	if tree.ChannelID == 11 {
		switch {
		case tree.Field2 == nil:
			tree.LegacyRouteHint = "legacy_default_group"
		case *tree.Field2 == 1:
			tree.LegacyRouteHint = "legacy_aws_group"
		case *tree.Field2 == 2 && tree.EncodingLayers == 2:
			tree.LegacyRouteHint = "legacy_vertex_direct"
		case *tree.Field2 == 2 && tree.EncodingLayers == 1:
			tree.LegacyRouteHint = "legacy_vertex_proxy"
		}
	}

	return tree, nil
}

func extractBytesField(msg []byte, fieldNum protowire.Number, scope string) ([]byte, error) {
	var value []byte
	err := walkProtobufFields(
		msg, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			if num != fieldNum {
				return nil
			}
			if typ != protowire.BytesType {
				return fmt.Errorf("invalid Claude signature: %s field %d must be bytes", scope, fieldNum)
			}
			bytesValue, err := decodeBytesField(raw, fmt.Sprintf("%s field %d", scope, fieldNum))
			if err != nil {
				return err
			}
			value = bytesValue
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("invalid Claude signature: missing %s field %d", scope, fieldNum)
	}
	return value, nil
}

func walkProtobufFields(msg []byte, visit func(num protowire.Number, typ protowire.Type, raw []byte) error) error {
	for offset := 0; offset < len(msg); {
		num, typ, n := protowire.ConsumeTag(msg[offset:])
		if n < 0 {
			return fmt.Errorf("invalid Claude signature: malformed protobuf tag: %w", protowire.ParseError(n))
		}
		offset += n
		valueLen := protowire.ConsumeFieldValue(num, typ, msg[offset:])
		if valueLen < 0 {
			return fmt.Errorf(
				"invalid Claude signature: malformed protobuf field %d: %w", num, protowire.ParseError(valueLen),
			)
		}
		fieldRaw := msg[offset : offset+valueLen]
		if err := visit(num, typ, fieldRaw); err != nil {
			return err
		}
		offset += valueLen
	}
	return nil
}

func decodeVarintField(raw []byte, label string) (uint64, error) {
	value, n := protowire.ConsumeVarint(raw)
	if n < 0 {
		return 0, fmt.Errorf("invalid Claude signature: failed to decode %s: %w", label, protowire.ParseError(n))
	}
	return value, nil
}

func decodeBytesField(raw []byte, label string) ([]byte, error) {
	value, n := protowire.ConsumeBytes(raw)
	if n < 0 {
		return nil, fmt.Errorf("invalid Claude signature: failed to decode %s: %w", label, protowire.ParseError(n))
	}
	return value, nil
}
