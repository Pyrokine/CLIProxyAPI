package signature

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

type Provider string

const (
	providerUnknown      Provider = "unknown"
	ProviderClaude       Provider = "claude"
	ProviderGemini       Provider = "gemini"
	providerGeminiBypass Provider = "gemini_bypass"
	ProviderGPT          Provider = "gpt"
	providerKimi         Provider = "kimi"
	// providerGrok is target-only because xAI encrypted_content has no reliable provider marker.
	providerGrok Provider = "grok"
)

type BlockKind string

const (
	blockKindUnknown            BlockKind = "unknown"
	BlockKindClaudeThinking     BlockKind = "claude_thinking"
	BlockKindGeminiModelPart    BlockKind = "gemini_model_part"
	BlockKindGeminiFunctionCall BlockKind = "gemini_function_call"
	BlockKindGPTReasoning       BlockKind = "gpt_reasoning"
)

type compatibilityAction string

const (
	actionPreserve                compatibilityAction = "preserve"
	actionDropBlock               compatibilityAction = "drop_block"
	actionDropSignature           compatibilityAction = "drop_signature"
	actionReplaceWithGeminiBypass compatibilityAction = "replace_with_gemini_bypass"
	actionNoCompatibleReplacement compatibilityAction = "no_compatible_replacement"
)

const (
	GeminiSkipThoughtSignatureValidator = "skip_thought_signature_validator"
	geminiContextEngineeringBypass      = "context_engineering_is_the_way_to_go"
	maxSignatureLen                     = 32 * 1024 * 1024
	MaxGeminiThoughtSignatureLen        = maxSignatureLen
)

type CompatibilityDecision struct {
	TargetProvider       Provider
	DetectedProvider     Provider
	BlockKind            BlockKind
	Compatible           bool
	Action               compatibilityAction
	ReplacementSignature string
	NormalizedSignature  string
	Reason               string
}

// SignatureCompatibilityOptions controls compatibility-only signature handling.
// Strict provider targets should use the zero value.
type SignatureCompatibilityOptions struct {
	// PreserveUnknownNonEmpty retains an opaque signature only when no known
	// provider claims it. Known foreign signatures remain incompatible.
	PreserveUnknownNonEmpty bool
}

type ClaudeMessagesSanitizeOptions struct {
	TargetProvider                Provider
	TargetModel                   string
	DropEmptyMessages             bool
	DropToolSignatures            bool
	DropEmptyThinkingPlaceholders bool
	// PreserveThinkingBlocks keeps compatibility-mode thinking blocks and their
	// original signatures, including empty and opaque values.
	PreserveThinkingBlocks bool
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
	case strings.Contains(lower, "kimi"), strings.Contains(lower, "moonshot"),
		strings.HasPrefix(lower, "k2"), strings.HasPrefix(lower, "k3"):
		return providerKimi
	case strings.Contains(lower, "grok"):
		return providerGrok
	default:
		return providerUnknown
	}
}

func DetectProvider(rawSignature string) Provider {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return providerUnknown
	}
	if prefixedProvider, unprefixed, ok := splitProviderPrefix(sig); ok {
		if strings.Contains(unprefixed, "#") {
			return providerUnknown
		}
		switch prefixedProvider {
		case ProviderGemini:
			if IsGeminiBypass(unprefixed) {
				return providerGeminiBypass
			}
			if isRecognizedGeminiSignature(unprefixed) {
				return ProviderGemini
			}
		case ProviderClaude:
			if isValidClaudeCAISSignature(unprefixed) || isValidAntigravityClaudeCAQSSignature(unprefixed) ||
				isValidClaudeSignature(unprefixed, true) {
				return ProviderClaude
			}
		case ProviderGPT:
			if isValidGPTReasoningSignature(unprefixed) {
				return ProviderGPT
			}
		}
		return providerUnknown
	}
	if strings.Contains(sig, "#") {
		return providerUnknown
	}
	if IsGeminiBypass(sig) {
		return providerGeminiBypass
	}
	if isValidGPTReasoningSignature(sig) {
		return ProviderGPT
	}
	if isValidClaudeCAISSignature(sig) || isValidAntigravityClaudeCAQSSignature(sig) ||
		isValidClaudeSignature(sig, true) {
		return ProviderClaude
	}
	if isRecognizedGeminiSignature(sig) {
		return ProviderGemini
	}
	if isValidKimiThinkingSignature(sig) {
		return providerKimi
	}
	return providerUnknown
}

func decideCompatibility(targetProvider Provider, rawSignature string, blockKind BlockKind) CompatibilityDecision {
	return decideCompatibilityWithOptions(targetProvider, rawSignature, blockKind, SignatureCompatibilityOptions{})
}

func decideCompatibilityWithOptions(
	targetProvider Provider,
	rawSignature string,
	blockKind BlockKind,
	options SignatureCompatibilityOptions,
) CompatibilityDecision {
	targetProvider = normalizeTargetProvider(targetProvider)
	if blockKind == "" {
		blockKind = blockKindUnknown
	}
	detected := DetectProvider(rawSignature)
	decision := CompatibilityDecision{TargetProvider: targetProvider, DetectedProvider: detected, BlockKind: blockKind}
	if targetProvider == ProviderClaude && strings.HasPrefix(PayloadWithoutProviderPrefix(rawSignature), "Q") {
		decision.Action = actionDropBlock
		decision.Reason = "Antigravity CAQS wrapper requires Antigravity replay"
		return decision
	}
	if providerMatchesTarget(targetProvider, detected) {
		decision.Compatible = true
		decision.Action = actionPreserve
		decision.NormalizedSignature = normalizeCompatibleSignature(targetProvider, rawSignature)
		decision.Reason = "signature provider matches target provider"
		return decision
	}
	_, _, hasKnownProviderPrefix := splitProviderPrefix(rawSignature)
	if options.PreserveUnknownNonEmpty && detected == providerUnknown && !hasKnownProviderPrefix &&
		strings.TrimSpace(rawSignature) != "" {
		decision.Compatible = true
		decision.Action = actionPreserve
		decision.NormalizedSignature = rawSignature
		decision.Reason = "compatibility mode preserves a non-empty signature with unknown provider"
		return decision
	}
	decision.Compatible = false
	switch targetProvider {
	case ProviderGemini:
		if blockKind == BlockKindGeminiFunctionCall || blockKind == BlockKindGeminiModelPart || blockKind == blockKindUnknown {
			decision.Action = actionReplaceWithGeminiBypass
			decision.ReplacementSignature = GeminiSkipThoughtSignatureValidator
			decision.Reason = "Gemini can bypass incompatible model-part signatures with the documented sentinel"
			return decision
		}
		decision.Action = actionDropBlock
		decision.Reason = "signature is not compatible with Gemini"
	case ProviderClaude:
		decision.Action = actionDropBlock
		decision.Reason = "Claude has no cross-provider bypass sentinel for thinking blocks"
	case ProviderGPT:
		decision.Action = actionDropBlock
		decision.Reason = "GPT reasoning encrypted_content cannot be synthesized from another provider signature"
	case providerKimi:
		decision.Action = actionDropSignature
		decision.Reason = "Kimi does not validate replayed thinking signatures, so the block survives without one"
	case providerGrok:
		decision.Action = actionDropBlock
		decision.Reason = "xAI verifies encrypted_content on replay and rejects foreign or mutated blobs"
	default:
		decision.Action = actionNoCompatibleReplacement
		decision.Reason = "unknown target provider"
	}
	return decision
}

func CompatibleSignatureForProvider(targetProvider Provider, rawSignature string, blockKind BlockKind) (string, bool) {
	return CompatibleSignatureForProviderWithOptions(
		targetProvider,
		rawSignature,
		blockKind,
		SignatureCompatibilityOptions{},
	)
}

// CompatibleSignatureForProviderWithOptions applies provider validation while
// allowing an explicit compatibility endpoint to retain an unknown opaque value.
func CompatibleSignatureForProviderWithOptions(
	targetProvider Provider,
	rawSignature string,
	blockKind BlockKind,
	options SignatureCompatibilityOptions,
) (string, bool) {
	decision := decideCompatibilityWithOptions(targetProvider, rawSignature, blockKind, options)
	if !decision.Compatible || decision.NormalizedSignature == "" {
		return "", false
	}
	return decision.NormalizedSignature, true
}

func CompatibleAntigravityClaudeThinkingSignature(rawSignature string) (string, bool) {
	if DetectProvider(rawSignature) != ProviderClaude {
		return "", false
	}
	payload := PayloadWithoutProviderPrefix(rawSignature)
	if strings.HasPrefix(payload, "Q") {
		if !isValidAntigravityClaudeCAQSSignature(payload) {
			return "", false
		}
		return payload, true
	}
	normalized, err := normalizeClaudeSignature(payload, true)
	if err != nil {
		return "", false
	}
	return normalized, true
}

func splitProviderPrefix(rawSignature string) (Provider, string, bool) {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(rawSignature), "#")
	if !ok {
		return providerUnknown, rawSignature, false
	}
	provider := providerFromCachePrefix(prefix)
	if provider == providerUnknown {
		return providerUnknown, rawSignature, false
	}
	return provider, strings.TrimSpace(rest), true
}

func providerFromCachePrefix(prefix string) Provider {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude", "anthropic", "cais", "claude-cais", "claude_cais", "ccmax", "claude-code-max", "claude_code_max":
		return ProviderClaude
	case "gemini", "google":
		return ProviderGemini
	case "openai", "gpt", "codex":
		return ProviderGPT
	default:
		return providerUnknown
	}
}

func PayloadWithoutProviderPrefix(rawSignature string) string {
	if _, unprefixed, ok := splitProviderPrefix(rawSignature); ok {
		return unprefixed
	}
	return strings.TrimSpace(rawSignature)
}

func normalizeTargetProvider(provider Provider) Provider {
	if provider == providerGeminiBypass {
		return ProviderGemini
	}
	return provider
}

func providerMatchesTarget(target, detected Provider) bool {
	switch target {
	case ProviderGemini:
		return detected == ProviderGemini || detected == providerGeminiBypass
	case ProviderClaude:
		return detected == ProviderClaude
	case ProviderGPT:
		return detected == ProviderGPT
	case providerKimi:
		return detected == providerKimi
	default:
		return false
	}
}

func normalizeCompatibleSignature(targetProvider Provider, rawSignature string) string {
	payload := PayloadWithoutProviderPrefix(rawSignature)
	switch normalizeTargetProvider(targetProvider) {
	case ProviderClaude:
		if isValidClaudeCAISSignature(payload) {
			return payload
		}
		normalized, err := normalizeClaudeProviderNativeSignature(payload, true)
		if err != nil {
			return ""
		}
		return normalized
	case ProviderGemini:
		if IsGeminiBypass(payload) || isRecognizedGeminiSignature(payload) {
			return payload
		}
	case ProviderGPT:
		if isValidGPTReasoningSignature(payload) {
			return payload
		}
	case providerKimi:
		if isValidKimiThinkingSignature(payload) {
			return payload
		}
	}
	return ""
}

func IsGeminiBypass(rawSignature string) bool {
	switch strings.TrimSpace(rawSignature) {
	case GeminiSkipThoughtSignatureValidator, geminiContextEngineeringBypass:
		return true
	default:
		return false
	}
}

func isValidClaudeSignature(rawSignature string, strict bool) bool {
	_, err := normalizeClaudeSignature(rawSignature, strict)
	return err == nil
}

func normalizeClaudeSignature(rawSignature string, strict bool) (string, error) {
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

func normalizeClaudeProviderNativeSignature(rawSignature string, strict bool) (string, error) {
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

func isValidGPTReasoningSignature(rawSignature string) bool {
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
	if isValidClaudeCAISSignature(rawSignature) {
		return false
	}
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
	value, ok := consumeGeminiField2Field1Value(decoded)
	return ok && (isLikelyGeminiOpaquePayload(value) || isASCIIUUIDBytes(value) ||
		isLikelyGeminiToolInvocationPayload(value))
}

func consumeGeminiField2Field1Value(decoded []byte) ([]byte, bool) {
	num, typ, n := protowire.ConsumeTag(decoded)
	if n < 0 || num != 2 || typ != protowire.BytesType {
		return nil, false
	}
	offset := n
	container, n := protowire.ConsumeBytes(decoded[offset:])
	if n < 0 {
		return nil, false
	}
	offset += n
	if offset != len(decoded) {
		return nil, false
	}
	num, typ, n = protowire.ConsumeTag(container)
	if n < 0 || num != 1 || typ != protowire.BytesType {
		return nil, false
	}
	containerOffset := n
	value, n := protowire.ConsumeBytes(container[containerOffset:])
	if n < 0 {
		return nil, false
	}
	containerOffset += n
	if containerOffset != len(container) {
		return nil, false
	}
	return value, true
}

func isLikelyGeminiOpaquePayload(value []byte) bool {
	return len(value) > 0 && value[0] == 0x01
}

func isLikelyGeminiToolInvocationPayload(value []byte) bool {
	if len(value) == 0 {
		return false
	}

	hasTinkField := false
	for offset := 0; offset < len(value); {
		_, fieldType, tagLen := protowire.ConsumeTag(value[offset:])
		if tagLen < 0 {
			return false
		}
		offset += tagLen

		var fieldLen int
		switch fieldType {
		case protowire.VarintType:
			_, fieldLen = protowire.ConsumeVarint(value[offset:])
		case protowire.BytesType:
			var bytesValue []byte
			bytesValue, fieldLen = protowire.ConsumeBytes(value[offset:])
			if fieldLen >= 0 && isLikelyGeminiOpaquePayload(bytesValue) {
				hasTinkField = true
			}
		case protowire.Fixed32Type:
			_, fieldLen = protowire.ConsumeFixed32(value[offset:])
		case protowire.Fixed64Type:
			_, fieldLen = protowire.ConsumeFixed64(value[offset:])
		default:
			return false
		}
		if fieldLen < 0 {
			return false
		}
		offset += fieldLen
	}
	return hasTinkField
}

func isASCIIUUIDBytes(value []byte) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return false
			}
		}
	}
	return true
}

func GeminiReplaySignatureOrBypass(rawSignature string, blockKind BlockKind) string {
	if signature, ok := CompatibleSignatureForProvider(ProviderGemini, rawSignature, blockKind); ok {
		return signature
	}
	decision := decideCompatibility(ProviderGemini, rawSignature, blockKind)
	if decision.Action == actionReplaceWithGeminiBypass && decision.ReplacementSignature != "" {
		return decision.ReplacementSignature
	}
	return GeminiSkipThoughtSignatureValidator
}

func SanitizeGeminiRequestThoughtSignatures(
	payload []byte,
	contentsPath string,
	preserveEmptyThoughtSignatures ...bool,
) []byte {
	preserveEmpty := len(preserveEmptyThoughtSignatures) > 0 && preserveEmptyThoughtSignatures[0]
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}
	contents := util.GetGJSONBytesNoCopy(payload, contentsPath)
	if !contents.IsArray() || !geminiContentsThoughtSignaturesNeedSanitize(contents) {
		return payload
	}

	contentsChanged := false
	contentItems := make([][]byte, 0, int(contents.Get("#").Int()))
	contents.ForEach(
		func(contentIndex, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				contentItems = append(contentItems, []byte(content.Raw))
				return true
			}

			isModelTurn := content.Get("role").String() == "model"
			firstFunctionCallSeen := false
			partsChanged := false
			partItems := make([][]byte, 0, int(parts.Get("#").Int()))
			parts.ForEach(
				func(partIndex, part gjson.Result) bool {
					partJSON := []byte(part.Raw)
					rawSignature, hasSignature := geminiPartThoughtSignature(part)
					if preserveEmpty &&
						part.Get("thought").Bool() &&
						hasSignature &&
						strings.TrimSpace(rawSignature) == "" {
						partItems = append(partItems, partJSON)
						return true
					}
					if part.Get("functionResponse").Exists() {
						if hasSignature {
							partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
							partsChanged = true
							logGeminiThoughtSignatureSanitize(
								contentsPath, int(contentIndex.Int()), int(partIndex.Int()), CompatibilityDecision{
									TargetProvider: ProviderGemini,
									BlockKind:      BlockKindGeminiModelPart,
									Action:         actionDropSignature,
									Reason:         "functionResponse parts cannot replay thought signatures",
								}, rawSignature, true,
							)
						}
						partItems = append(partItems, partJSON)
						return true
					}
					if !isModelTurn {
						partItems = append(partItems, partJSON)
						return true
					}
					if isGeminiServerToolPart(part) {
						partItems = append(partItems, partJSON)
						return true
					}

					hasFunctionCall := part.Get("functionCall").Exists()
					isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
					if hasFunctionCall {
						firstFunctionCallSeen = true
					}
					if !hasFunctionCall && !hasSignature {
						partItems = append(partItems, partJSON)
						return true
					}

					blockKind := BlockKindGeminiModelPart
					if hasFunctionCall {
						blockKind = BlockKindGeminiFunctionCall
					}
					decision := decideCompatibility(ProviderGemini, rawSignature, blockKind)
					replaySignature := ""
					switch {
					case isFirstFunctionCall:
						replaySignature = GeminiReplaySignatureOrBypass(rawSignature, blockKind)
					case hasSignature && decision.Action == actionPreserve && !IsGeminiBypass(PayloadWithoutProviderPrefix(rawSignature)):
						replaySignature = decision.NormalizedSignature
					case hasSignature:
						decision.Action = actionDropSignature
						decision.ReplacementSignature = ""
						if hasFunctionCall {
							decision.Reason = "unsigned sibling functionCalls preserve native parallel-call shape"
						} else {
							decision.Reason = "non-function model parts do not synthesize Gemini bypass signatures"
						}
					}

					partChanged := false
					if replaySignature != "" {
						if !hasNormalizedGeminiPartThoughtSignature(part, replaySignature) {
							partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
							partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", replaySignature)
							partChanged = true
						}
					} else if hasSignature {
						partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
						partChanged = true
					}
					if partChanged {
						partsChanged = true
						if decision.Action != actionPreserve {
							logGeminiThoughtSignatureSanitize(
								contentsPath, int(contentIndex.Int()), int(partIndex.Int()), decision, rawSignature,
								hasSignature,
							)
						}
					}
					partItems = append(partItems, partJSON)
					return true
				},
			)

			contentJSON := []byte(content.Raw)
			if partsChanged {
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", joinGeminiSignatureRawArray(partItems))
				contentsChanged = true
			}
			contentItems = append(contentItems, contentJSON)
			return true
		},
	)

	if !contentsChanged {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, contentsPath, joinGeminiSignatureRawArray(contentItems))
	if err != nil {
		return payload
	}
	return updated
}

func geminiContentsThoughtSignaturesNeedSanitize(contents gjson.Result) bool {
	needsSanitize := false
	contents.ForEach(
		func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			isModelTurn := content.Get("role").String() == "model"
			firstFunctionCallSeen := false
			parts.ForEach(
				func(_, part gjson.Result) bool {
					rawSignature, hasSignature := geminiPartThoughtSignature(part)
					if part.Get("functionResponse").Exists() {
						needsSanitize = hasSignature
						return !needsSanitize
					}
					if !isModelTurn {
						return true
					}
					if isGeminiServerToolPart(part) {
						return true
					}
					hasFunctionCall := part.Get("functionCall").Exists()
					isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
					if hasFunctionCall {
						firstFunctionCallSeen = true
					}
					if isFirstFunctionCall {
						replaySignature := GeminiReplaySignatureOrBypass(rawSignature, BlockKindGeminiFunctionCall)
						needsSanitize = !hasNormalizedGeminiPartThoughtSignature(part, replaySignature)
						return !needsSanitize
					}
					if !hasSignature {
						return true
					}
					blockKind := BlockKindGeminiModelPart
					if hasFunctionCall {
						blockKind = BlockKindGeminiFunctionCall
					}
					decision := decideCompatibility(ProviderGemini, rawSignature, blockKind)
					if decision.Action != actionPreserve || IsGeminiBypass(PayloadWithoutProviderPrefix(rawSignature)) {
						needsSanitize = true
						return false
					}
					needsSanitize = !hasNormalizedGeminiPartThoughtSignature(part, decision.NormalizedSignature)
					return !needsSanitize
				},
			)
			return !needsSanitize
		},
	)
	return needsSanitize
}

func validateGeminiThoughtSignatures(payload []byte, allowBypass bool) error {
	contents, contentsPath := geminiContents(payload)
	if !contents.IsArray() {
		return nil
	}
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		isModelTurn := strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "model")
		firstFunctionCallSeen := false
		for partIndex, part := range parts.Array() {
			hasFunctionCall := part.Get("functionCall").Exists()
			isFirstFunctionCall := isModelTurn && hasFunctionCall && !firstFunctionCallSeen
			if isModelTurn && hasFunctionCall {
				firstFunctionCallSeen = true
			}
			rawSignature, hasSignature := geminiPartThoughtSignature(part)
			if !hasFunctionCall && !hasSignature {
				continue
			}
			partPath := fmt.Sprintf("%s[%d].parts[%d]", contentsPath, contentIndex, partIndex)
			rawSignature = strings.TrimSpace(rawSignature)
			if part.Get("functionResponse").Exists() && hasSignature {
				return fmt.Errorf("%s: functionResponse must not carry thoughtSignature", partPath)
			}
			if rawSignature == "" {
				if isFirstFunctionCall {
					return fmt.Errorf("%s: missing thoughtSignature on first functionCall", partPath)
				}
				if hasSignature {
					return fmt.Errorf("%s: empty thoughtSignature", partPath)
				}
				continue
			}
			if IsGeminiBypass(rawSignature) {
				if !allowBypass {
					return fmt.Errorf("%s: Gemini bypass sentinel is not allowed", partPath)
				}
				if !isFirstFunctionCall {
					return fmt.Errorf(
						"%s: Gemini bypass sentinel is allowed only on the first model functionCall", partPath,
					)
				}
			} else if !isRecognizedGeminiSignature(rawSignature) {
				return fmt.Errorf("%s: invalid Gemini thoughtSignature", partPath)
			}
			if !hasNormalizedGeminiPartThoughtSignature(part, rawSignature) {
				return fmt.Errorf("%s: thoughtSignature must use one canonical top-level field", partPath)
			}
		}
	}
	return nil
}

type geminiFunctionCallRef struct {
	id   string
	name string
	path string
}

type geminiFunctionResponseRef struct {
	part gjson.Result
	path string
}

func ValidateGeminiFunctionCallPairing(payload []byte) error {
	contents, contentsPath := geminiContents(payload)
	if !contents.IsArray() {
		return nil
	}
	var pending []geminiFunctionCallRef
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			if len(pending) > 0 {
				return fmt.Errorf(
					"%s[%d]: content appears before %d pending functionResponse part(s)", contentsPath, contentIndex,
					len(pending),
				)
			}
			continue
		}
		var calls []geminiFunctionCallRef
		var responses []geminiFunctionResponseRef
		for partIndex, part := range parts.Array() {
			partPath := fmt.Sprintf("%s[%d].parts[%d]", contentsPath, contentIndex, partIndex)
			if call := part.Get("functionCall"); call.Exists() {
				if call.Get("name").String() == "" {
					return fmt.Errorf("%s: missing functionCall.name", partPath)
				}
				calls = append(
					calls,
					geminiFunctionCallRef{id: call.Get("id").String(), name: call.Get("name").String(), path: partPath},
				)
			}
			if part.Get("functionResponse").Exists() {
				responses = append(responses, geminiFunctionResponseRef{part: part, path: partPath})
			}
		}
		if len(calls) > 0 && len(responses) > 0 {
			return fmt.Errorf(
				"%s[%d]: functionCall and functionResponse parts must not be interleaved in the same content",
				contentsPath, contentIndex,
			)
		}
		if len(calls) > 0 {
			if len(pending) > 0 {
				return fmt.Errorf(
					"%s[%d]: functionCall appears before %d pending functionResponse part(s)", contentsPath,
					contentIndex, len(pending),
				)
			}
			pending = calls
			continue
		}
		if len(responses) == 0 {
			if len(pending) > 0 {
				return fmt.Errorf(
					"%s[%d]: content appears before %d pending functionResponse part(s)", contentsPath, contentIndex,
					len(pending),
				)
			}
			continue
		}
		if len(pending) == 0 {
			return fmt.Errorf("%s[%d]: functionResponse without preceding functionCall", contentsPath, contentIndex)
		}
		if len(responses) != len(pending) {
			return fmt.Errorf(
				"%s[%d]: functionResponse count %d does not match pending functionCall count %d", contentsPath,
				contentIndex, len(responses), len(pending),
			)
		}
		for responseIndex, responseRef := range responses {
			response := responseRef.part.Get("functionResponse")
			call := pending[responseIndex]
			responseID := response.Get("id").String()
			responseName := response.Get("name").String()
			if call.id != "" && responseID == "" {
				return fmt.Errorf("%s: missing functionResponse.id for %s", responseRef.path, call.path)
			}
			if call.id != "" && responseID != call.id {
				return fmt.Errorf(
					"%s: functionResponse.id %q does not match functionCall.id %q at %s", responseRef.path, responseID,
					call.id, call.path,
				)
			}
			if responseName == "" {
				return fmt.Errorf("%s: missing functionResponse.name", responseRef.path)
			}
			if call.name != "" && responseName != call.name {
				return fmt.Errorf(
					"%s: functionResponse.name %q does not match functionCall.name %q at %s", responseRef.path,
					responseName, call.name, call.path,
				)
			}
		}
		pending = nil
	}
	return nil
}

func geminiContents(payload []byte) (gjson.Result, string) {
	if contents := util.GetGJSONBytesNoCopy(payload, "contents"); contents.Exists() {
		return contents, "contents"
	}
	return util.GetGJSONBytesNoCopy(payload, "request.contents"), "request.contents"
}

func logGeminiThoughtSignatureSanitize(
	contentsPath string,
	contentIndex, partIndex int,
	decision CompatibilityDecision,
	rawSignature string,
	hasSignature bool,
) {
	log.WithFields(
		log.Fields{
			"component":         "signature_sanitizer",
			"target_provider":   string(ProviderGemini),
			"action":            string(decision.Action),
			"reason":            decision.Reason,
			"contents_path":     contentsPath,
			"content_index":     contentIndex,
			"part_index":        partIndex,
			"block_kind":        string(decision.BlockKind),
			"detected_provider": string(decision.DetectedProvider),
			"has_signature":     hasSignature,
			"signature_length":  len(strings.TrimSpace(rawSignature)),
		},
	).Debug("gemini request: sanitized thoughtSignature before upstream")
}

var geminiPartThoughtSignaturePaths = []string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

func isGeminiServerToolPart(part gjson.Result) bool {
	for _, path := range []string{"toolCall", "tool_call", "toolResponse", "tool_response"} {
		if part.Get(path).Exists() {
			return true
		}
	}
	return false
}

func geminiPartThoughtSignature(part gjson.Result) (string, bool) {
	for _, path := range geminiPartThoughtSignaturePaths {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

func hasNormalizedGeminiPartThoughtSignature(part gjson.Result, replaySignature string) bool {
	canonicalCount := 0
	part.ForEach(
		func(key, _ gjson.Result) bool {
			if key.String() == "thoughtSignature" {
				canonicalCount++
			}
			return true
		},
	)
	canonical := part.Get("thoughtSignature")
	if canonicalCount != 1 || canonical.Type != gjson.String || canonical.String() != replaySignature {
		return false
	}
	for _, path := range geminiPartThoughtSignaturePaths[1:] {
		if part.Get(path).Exists() {
			return false
		}
	}
	return true
}

func deleteGeminiPartThoughtSignatureFields(payload []byte) []byte {
	for _, path := range geminiPartThoughtSignaturePaths {
		for gjson.GetBytes(payload, path).Exists() {
			updated, err := sjson.DeleteBytes(payload, path)
			if err != nil || len(updated) >= len(payload) {
				break
			}
			payload = updated
		}
	}
	return payload
}

func joinGeminiSignatureRawArray(items [][]byte) []byte {
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for index, item := range items {
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}
