package claude

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const maxBypassSignatureLen = 32 * 1024 * 1024

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

func hasValidClaudeSignature(sig string) bool {
	sig = stripSignatureCachePrefix(sig)
	if sig == "" {
		return false
	}
	return sig[0] == 'E' || sig[0] == 'R'
}

func ValidateClaudeBypassSignatures(inputRawJSON []byte) error {
	messages := gjson.GetBytes(inputRawJSON, "messages")
	if !messages.IsArray() {
		return nil
	}

	for i, message := range messages.Array() {
		content := message.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			if part.Get("type").String() != "thinking" {
				continue
			}

			rawSignature := strings.TrimSpace(part.Get("signature").String())
			if rawSignature == "" {
				return fmt.Errorf("messages[%d].content[%d]: missing thinking signature", i, j)
			}

			if _, err := normalizeClaudeBypassSignature(rawSignature); err != nil {
				return fmt.Errorf("messages[%d].content[%d]: %w", i, j, err)
			}
		}
	}

	return nil
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
