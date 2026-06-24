package signature

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

type Provider string

const (
	ProviderUnknown      Provider = "unknown"
	ProviderClaude       Provider = "claude"
	ProviderGemini       Provider = "gemini"
	ProviderGeminiBypass Provider = "gemini_bypass"
	ProviderGPT          Provider = "gpt"
)

type BlockKind string

const (
	BlockKindUnknown            BlockKind = "unknown"
	BlockKindClaudeThinking     BlockKind = "claude_thinking"
	BlockKindGeminiModelPart    BlockKind = "gemini_model_part"
	BlockKindGeminiFunctionCall BlockKind = "gemini_function_call"
	BlockKindGPTReasoning       BlockKind = "gpt_reasoning"
)

type CompatibilityAction string

const (
	ActionPreserve                CompatibilityAction = "preserve"
	ActionDropBlock               CompatibilityAction = "drop_block"
	ActionDropSignature           CompatibilityAction = "drop_signature"
	ActionReplaceWithGeminiBypass CompatibilityAction = "replace_with_gemini_bypass"
	ActionNoCompatibleReplacement CompatibilityAction = "no_compatible_replacement"
)

const (
	GeminiSkipThoughtSignatureValidator = "skip_thought_signature_validator"
	GeminiContextEngineeringBypass      = "context_engineering_is_the_way_to_go"
	maxSignatureLen                     = 32 * 1024 * 1024
)

type CompatibilityDecision struct {
	TargetProvider       Provider
	DetectedProvider     Provider
	BlockKind            BlockKind
	Compatible           bool
	Action               CompatibilityAction
	ReplacementSignature string
	NormalizedSignature  string
	Reason               string
}

type ClaudeMessagesSanitizeOptions struct {
	TargetProvider                Provider
	TargetModel                   string
	DropEmptyMessages             bool
	DropToolSignatures            bool
	DropEmptyThinkingPlaceholders bool
}

type SanitizeReport struct {
	TargetProvider     Provider
	Preserved          int
	DroppedBlocks      int
	DroppedSignatures  int
	ReplacedSignatures int
	Decisions          []CompatibilityDecision
}

func ProviderFromModelName(modelName string) Provider {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	switch {
	case strings.Contains(lower, "claude"):
		return ProviderClaude
	case strings.Contains(lower, "gemini"):
		return ProviderGemini
	case strings.Contains(lower, "gpt"), strings.Contains(lower, "openai"), strings.Contains(
		lower, "codex",
	), strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"):
		return ProviderGPT
	default:
		return ProviderUnknown
	}
}

func DetectProvider(rawSignature string) Provider {
	return DetectProviderForBlock(rawSignature, BlockKindUnknown)
}

func DetectProviderForBlock(rawSignature string, blockKind BlockKind) Provider {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return ProviderUnknown
	}
	if prefixedProvider, unprefixed, ok := SplitProviderPrefix(sig); ok {
		switch prefixedProvider {
		case ProviderGemini:
			if IsGeminiBypass(unprefixed) {
				return ProviderGeminiBypass
			}
			if isRecognizedGeminiSignature(unprefixed) {
				return ProviderGemini
			}
		case ProviderClaude:
			if IsValidClaudeSignature(unprefixed, true) {
				return ProviderClaude
			}
		case ProviderGPT:
			if IsValidGPTReasoningSignature(unprefixed) {
				return ProviderGPT
			}
		}
		return ProviderUnknown
	}
	if strings.Contains(sig, "#") {
		return ProviderUnknown
	}
	if IsGeminiBypass(sig) {
		return ProviderGeminiBypass
	}
	if IsValidGPTReasoningSignature(sig) {
		return ProviderGPT
	}
	if IsValidClaudeSignature(sig, true) {
		return ProviderClaude
	}
	if isRecognizedGeminiSignature(sig) {
		return ProviderGemini
	}
	return ProviderUnknown
}

func DecideCompatibility(targetProvider Provider, rawSignature string, blockKind BlockKind) CompatibilityDecision {
	targetProvider = normalizeTargetProvider(targetProvider)
	if blockKind == "" {
		blockKind = BlockKindUnknown
	}
	detected := DetectProviderForBlock(rawSignature, blockKind)
	decision := CompatibilityDecision{TargetProvider: targetProvider, DetectedProvider: detected, BlockKind: blockKind}
	if providerMatchesTarget(targetProvider, detected) {
		decision.Compatible = true
		decision.Action = ActionPreserve
		decision.NormalizedSignature = normalizeCompatibleSignature(targetProvider, rawSignature)
		decision.Reason = "signature provider matches target provider"
		return decision
	}
	decision.Compatible = false
	switch targetProvider {
	case ProviderGemini:
		if blockKind == BlockKindGeminiFunctionCall || blockKind == BlockKindGeminiModelPart || blockKind == BlockKindUnknown {
			decision.Action = ActionReplaceWithGeminiBypass
			decision.ReplacementSignature = GeminiSkipThoughtSignatureValidator
			decision.Reason = "Gemini can bypass incompatible model-part signatures with the documented sentinel"
			return decision
		}
		decision.Action = ActionDropBlock
		decision.Reason = "signature is not compatible with Gemini"
	case ProviderClaude:
		decision.Action = ActionDropBlock
		decision.Reason = "Claude has no cross-provider bypass sentinel for thinking blocks"
	case ProviderGPT:
		decision.Action = ActionDropBlock
		decision.Reason = "GPT reasoning encrypted_content cannot be synthesized from another provider signature"
	default:
		decision.Action = ActionNoCompatibleReplacement
		decision.Reason = "unknown target provider"
	}
	return decision
}

func CompatibleSignatureForProvider(targetProvider Provider, rawSignature string, blockKind BlockKind) (string, bool) {
	decision := DecideCompatibility(targetProvider, rawSignature, blockKind)
	if !decision.Compatible || decision.NormalizedSignature == "" {
		return "", false
	}
	return decision.NormalizedSignature, true
}

func CompatibleAntigravityClaudeThinkingSignature(rawSignature string) (string, bool) {
	if DetectProviderForBlock(rawSignature, BlockKindClaudeThinking) != ProviderClaude {
		return "", false
	}
	normalized, err := NormalizeClaudeSignature(PayloadWithoutProviderPrefix(rawSignature), true)
	if err != nil {
		return "", false
	}
	return normalized, true
}

func SplitProviderPrefix(rawSignature string) (Provider, string, bool) {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(rawSignature), "#")
	if !ok {
		return ProviderUnknown, rawSignature, false
	}
	provider := ProviderFromCachePrefix(prefix)
	if provider == ProviderUnknown {
		return ProviderUnknown, rawSignature, false
	}
	return provider, strings.TrimSpace(rest), true
}

func ProviderFromCachePrefix(prefix string) Provider {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude", "anthropic":
		return ProviderClaude
	case "gemini", "google":
		return ProviderGemini
	case "openai", "gpt", "codex":
		return ProviderGPT
	default:
		return ProviderUnknown
	}
}

func PayloadWithoutProviderPrefix(rawSignature string) string {
	if _, unprefixed, ok := SplitProviderPrefix(rawSignature); ok {
		return unprefixed
	}
	return strings.TrimSpace(rawSignature)
}

func normalizeTargetProvider(provider Provider) Provider {
	if provider == ProviderGeminiBypass {
		return ProviderGemini
	}
	return provider
}

func providerMatchesTarget(target, detected Provider) bool {
	switch target {
	case ProviderGemini:
		return detected == ProviderGemini || detected == ProviderGeminiBypass
	case ProviderClaude:
		return detected == ProviderClaude
	case ProviderGPT:
		return detected == ProviderGPT
	default:
		return false
	}
}

func normalizeCompatibleSignature(targetProvider Provider, rawSignature string) string {
	payload := PayloadWithoutProviderPrefix(rawSignature)
	switch normalizeTargetProvider(targetProvider) {
	case ProviderClaude:
		normalized, err := NormalizeClaudeProviderNativeSignature(payload, true)
		if err != nil {
			return ""
		}
		return normalized
	case ProviderGemini:
		if IsGeminiBypass(payload) || isRecognizedGeminiSignature(payload) {
			return payload
		}
	case ProviderGPT:
		if IsValidGPTReasoningSignature(payload) {
			return payload
		}
	}
	return ""
}

func IsGeminiBypass(rawSignature string) bool {
	switch strings.TrimSpace(rawSignature) {
	case GeminiSkipThoughtSignatureValidator, GeminiContextEngineeringBypass:
		return true
	default:
		return false
	}
}

func IsValidClaudeSignature(rawSignature string, strict bool) bool {
	_, err := NormalizeClaudeSignature(rawSignature, strict)
	return err == nil
}

func NormalizeClaudeSignature(rawSignature string, strict bool) (string, error) {
	sig := PayloadWithoutProviderPrefix(rawSignature)
	if sig == "" {
		return "", fmt.Errorf("empty signature")
	}
	if len(sig) > maxSignatureLen {
		return "", fmt.Errorf("signature exceeds maximum length (%d bytes)", maxSignatureLen)
	}
	switch sig[0] {
	case 'R':
		if err := validateClaudeDoubleLayerSignature(sig, strict); err != nil {
			return "", err
		}
		return sig, nil
	case 'E':
		if err := validateClaudeSingleLayerSignature(sig, strict); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString([]byte(sig)), nil
	default:
		return "", fmt.Errorf("invalid signature: expected 'E' or 'R' prefix, got %q", string(sig[0]))
	}
}

func NormalizeClaudeProviderNativeSignature(rawSignature string, strict bool) (string, error) {
	sig := PayloadWithoutProviderPrefix(rawSignature)
	if sig == "" {
		return "", fmt.Errorf("empty signature")
	}
	if len(sig) > maxSignatureLen {
		return "", fmt.Errorf("signature exceeds maximum length (%d bytes)", maxSignatureLen)
	}
	switch sig[0] {
	case 'E':
		if err := validateClaudeSingleLayerSignature(sig, strict); err != nil {
			return "", err
		}
		return sig, nil
	case 'R':
		if err := validateClaudeDoubleLayerSignature(sig, strict); err != nil {
			return "", err
		}
		decoded, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			return "", fmt.Errorf("invalid double-layer signature: base64 decode failed: %w", err)
		}
		return string(decoded), nil
	default:
		return "", fmt.Errorf("invalid signature: expected 'E' or 'R' prefix, got %q", string(sig[0]))
	}
}

func validateClaudeDoubleLayerSignature(sig string, strict bool) error {
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
	return validateClaudeSingleLayerSignatureContent(string(decoded), 2, strict)
}

func validateClaudeSingleLayerSignature(sig string, strict bool) error {
	return validateClaudeSingleLayerSignatureContent(sig, 1, strict)
}

func validateClaudeSingleLayerSignatureContent(sig string, encodingLayers int, strict bool) error {
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
	if !strict {
		return nil
	}
	return inspectClaudeSignaturePayload(decoded, encodingLayers)
}

func inspectClaudeSignaturePayload(payload []byte, _ int) error {
	if len(payload) == 0 {
		return fmt.Errorf("invalid Claude signature: empty payload")
	}
	if payload[0] != 0x12 {
		return fmt.Errorf("invalid Claude signature: expected first byte 0x12, got 0x%02x", payload[0])
	}
	container, err := extractClaudeBytesField(payload, 2, "top-level protobuf")
	if err != nil {
		return err
	}
	channelBlock, err := extractClaudeBytesField(container, 1, "Claude Field 2 container")
	if err != nil {
		return err
	}
	return inspectClaudeChannelBlock(channelBlock)
}

func inspectClaudeChannelBlock(channelBlock []byte) error {
	haveChannelID := false
	err := walkClaudeProtobufFields(
		channelBlock, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			switch num {
			case 1:
				if typ != protowire.VarintType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.1 channel_id must be varint")
				}
				if _, err := decodeClaudeVarintField(raw, "Field 2.1.1 channel_id"); err != nil {
					return err
				}
				haveChannelID = true
			case 2, 7:
				if typ != protowire.VarintType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.%d must be varint", num)
				}
				if _, err := decodeClaudeVarintField(raw, fmt.Sprintf("Field 2.1.%d", num)); err != nil {
					return err
				}
			case 6:
				if typ != protowire.BytesType {
					return fmt.Errorf("invalid Claude signature: Field 2.1.6 model_text must be bytes")
				}
				modelBytes, err := decodeClaudeBytesField(raw, "Field 2.1.6 model_text")
				if err != nil {
					return err
				}
				if !utf8.Valid(modelBytes) {
					return fmt.Errorf("invalid Claude signature: Field 2.1.6 model_text is not valid UTF-8")
				}
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	if !haveChannelID {
		return fmt.Errorf("invalid Claude signature: missing Field 2.1.1 channel_id")
	}
	return nil
}

func extractClaudeBytesField(msg []byte, fieldNum protowire.Number, scope string) ([]byte, error) {
	var value []byte
	err := walkClaudeProtobufFields(
		msg, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			if num != fieldNum {
				return nil
			}
			if typ != protowire.BytesType {
				return fmt.Errorf("invalid Claude signature: %s field %d must be bytes", scope, fieldNum)
			}
			bytesValue, err := decodeClaudeBytesField(raw, fmt.Sprintf("%s field %d", scope, fieldNum))
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

func walkClaudeProtobufFields(
	msg []byte,
	visit func(num protowire.Number, typ protowire.Type, raw []byte) error,
) error {
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

func decodeClaudeVarintField(raw []byte, label string) (uint64, error) {
	value, n := protowire.ConsumeVarint(raw)
	if n < 0 {
		return 0, fmt.Errorf("invalid Claude signature: failed to decode %s: %w", label, protowire.ParseError(n))
	}
	return value, nil
}

func decodeClaudeBytesField(raw []byte, label string) ([]byte, error) {
	value, n := protowire.ConsumeBytes(raw)
	if n < 0 {
		return nil, fmt.Errorf("invalid Claude signature: failed to decode %s: %w", label, protowire.ParseError(n))
	}
	return value, nil
}

func IsValidGPTReasoningSignature(rawSignature string) bool {
	_, err := InspectGPTReasoningSignature(rawSignature)
	return err == nil
}

func InspectGPTReasoningSignature(rawSignature string) (int, error) {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return 0, fmt.Errorf("empty GPT reasoning signature")
	}
	if len(sig) > maxSignatureLen {
		return 0, fmt.Errorf("GPT reasoning signature exceeds maximum length (%d bytes)", maxSignatureLen)
	}
	if index, r, ok := firstInvalidGPTReasoningSignatureChar(sig); ok {
		return 0, fmt.Errorf(
			"invalid GPT reasoning signature: contains non-base64url character U+%04X at byte %d", r, index,
		)
	}
	if !strings.HasPrefix(sig, "gAAAA") {
		return 0, fmt.Errorf("invalid GPT reasoning signature: expected gAAAA prefix")
	}
	decoded, err := decodeGPTReasoningSignature(sig)
	if err != nil {
		return 0, err
	}
	if len(decoded) < 73 {
		return 0, fmt.Errorf("invalid GPT reasoning signature: decoded payload too short")
	}
	if decoded[0] != 0x80 {
		return 0, fmt.Errorf("invalid GPT reasoning signature: expected version 0x80, got 0x%02x", decoded[0])
	}
	ciphertextLen := len(decoded) - 1 - 8 - 16 - 32
	if ciphertextLen <= 0 || ciphertextLen%16 != 0 {
		return 0, fmt.Errorf(
			"invalid GPT reasoning signature: ciphertext length %d is not a positive AES block multiple", ciphertextLen,
		)
	}
	return ciphertextLen, nil
}

func decodeGPTReasoningSignature(sig string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(sig); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(sig); err == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("invalid GPT reasoning signature: base64url decode failed")
}

func firstInvalidGPTReasoningSignatureChar(sig string) (int, rune, bool) {
	for index, r := range sig {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '=':
		default:
			return index, r, true
		}
	}
	return 0, 0, false
}

func isRecognizedGeminiSignature(rawSignature string) bool {
	if IsGeminiBypass(rawSignature) {
		return true
	}
	decoded, err := decodeGeminiThoughtSignature(strings.TrimSpace(rawSignature))
	if err != nil || len(decoded) == 0 {
		return false
	}
	return isGeminiField1Envelope(decoded) || isGeminiField2Envelope(decoded)
}

func decodeGeminiThoughtSignature(sig string) ([]byte, error) {
	if sig == "" {
		return nil, fmt.Errorf("empty Gemini thought signature")
	}
	if len(sig) > maxSignatureLen {
		return nil, fmt.Errorf("gemini thought signature exceeds maximum length (%d bytes)", maxSignatureLen)
	}
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err == nil {
		return decoded, nil
	}
	if decoded, rawErr := base64.RawStdEncoding.DecodeString(sig); rawErr == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("invalid Gemini thought signature: base64 decode failed: %w", err)
}

func isGeminiField1Envelope(decoded []byte) bool {
	offset := 0
	recordCount := 0
	for offset < len(decoded) {
		num, typ, n := protowire.ConsumeTag(decoded[offset:])
		if n < 0 || num != 1 || typ != protowire.BytesType {
			return false
		}
		offset += n
		value, n := protowire.ConsumeBytes(decoded[offset:])
		if n < 0 || !isLikelyGeminiOpaquePayload(value) {
			return false
		}
		recordCount++
		offset += n
	}
	return offset == len(decoded) && recordCount > 0
}

func isGeminiField2Envelope(decoded []byte) bool {
	num, typ, n := protowire.ConsumeTag(decoded)
	if n < 0 || num != 2 || typ != protowire.BytesType {
		return false
	}
	offset := n
	container, n := protowire.ConsumeBytes(decoded[offset:])
	if n < 0 {
		return false
	}
	offset += n
	if offset != len(decoded) {
		return false
	}
	num, typ, n = protowire.ConsumeTag(container)
	if n < 0 || num != 1 || typ != protowire.BytesType {
		return false
	}
	containerOffset := n
	value, n := protowire.ConsumeBytes(container[containerOffset:])
	if n < 0 || !isLikelyGeminiOpaquePayload(value) {
		return false
	}
	containerOffset += n
	return containerOffset == len(container)
}

func isLikelyGeminiOpaquePayload(value []byte) bool {
	return len(value) > 0 && value[0] == 0x01
}

func GeminiReplaySignatureOrBypass(rawSignature string, blockKind BlockKind) string {
	if signature, ok := CompatibleSignatureForProvider(ProviderGemini, rawSignature, blockKind); ok {
		return signature
	}
	decision := DecideCompatibility(ProviderGemini, rawSignature, blockKind)
	if decision.Action == ActionReplaceWithGeminiBypass && decision.ReplacementSignature != "" {
		return decision.ReplacementSignature
	}
	return GeminiSkipThoughtSignatureValidator
}

func SanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}
	contents := gjson.GetBytes(payload, contentsPath)
	if !contents.IsArray() {
		return payload
	}
	contents.ForEach(
		func(contentIdx, content gjson.Result) bool {
			isModelTurn := content.Get("role").String() == "model"
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			parts.ForEach(
				func(partIdx, part gjson.Result) bool {
					partPath := fmt.Sprintf("%s.%d.parts.%d", contentsPath, contentIdx.Int(), partIdx.Int())
					if part.Get("functionResponse").Exists() {
						payload = deleteGeminiPartSignatureFields(payload, partPath)
						return true
					}
					if !isModelTurn {
						return true
					}
					hasFunctionCall := part.Get("functionCall").Exists()
					hasThought := part.Get("thought").Exists()
					rawSignature, hasSignature := geminiPartThoughtSignature(part)
					if !hasFunctionCall && !hasThought && !hasSignature {
						return true
					}
					blockKind := BlockKindGeminiModelPart
					if hasFunctionCall {
						blockKind = BlockKindGeminiFunctionCall
					}
					payload = deleteGeminiPartSignatureFields(payload, partPath)
					payload, _ = sjson.SetBytes(
						payload, partPath+".thoughtSignature", GeminiReplaySignatureOrBypass(rawSignature, blockKind),
					)
					return true
				},
			)
			return true
		},
	)
	return payload
}

func geminiPartThoughtSignature(part gjson.Result) (string, bool) {
	for _, path := range []string{
		"thoughtSignature", "thought_signature", "functionCall.thoughtSignature", "functionCall.thought_signature",
		"functionResponse.thoughtSignature", "functionResponse.thought_signature",
		"extra_content.google.thought_signature",
	} {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

func deleteGeminiPartSignatureFields(payload []byte, partPath string) []byte {
	for _, path := range []string{
		"thoughtSignature", "thought_signature", "functionCall.thoughtSignature", "functionCall.thought_signature",
		"functionResponse.thoughtSignature", "functionResponse.thought_signature",
		"extra_content.google.thought_signature",
	} {
		payload, _ = sjson.DeleteBytes(payload, partPath+"."+path)
	}
	return payload
}
