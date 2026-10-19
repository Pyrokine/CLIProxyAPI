package signature

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const observedClaudeCAISFable5Sample = "CAISqwIKiAEIEBgCKkBHRlRBsNiptQUWfPoOhuQKwi5LnncZVO9bB5jqOs76D7uBtgktML0zqJtNmLHXHHcgD6lk4MQu4QBXzFd1lbC3Mg5jbGF1ZGUtZmFibGUtNTgBQgh0aGlua2luZ1okZDk3NDM5NzUtNGJiMC00OTM2LTllMjgtZDViMGQyMWJkYzQ4EgxCGh+XVFFFeySAjtAaDL/A1LltGu6MMJ+eXSIwsN0oBpDrqLv22UBfkMnTotnIbkvkOyb9xZHgigG6OZVHaI3gThm+maLKmgO5PrFLKlDFYp+YZksy/wKwszJlnLTPzAK+NUlfzagOE1ymtZTXhAYK260XyFYmg/te/C231+Fr/hoX+EJoUBnrn0gD7hqMISOT+TaFEuOXYsN517GfaxgB"

const observedClaudeCAISContextID = "d9743975-4bb0-4936-9e28-d5b0d21bdc48"

type claudeCAISTestParts struct {
	includeEnvelopeVersion    bool
	envelopeVersion           uint64
	includeTrailer            bool
	includeContainer          bool
	includeChannelBlock       bool
	includeContainerSignature bool
	containerSignatureLen     int
	includeChannelID          bool
	channelID                 uint64
	channelIDAsBytes          bool
	includeInfrastructure     bool
	infrastructure            uint64
	includeChannelVersion     bool
	includeSignature          bool
	signatureLen              int
	includeModelText          bool
	modelText                 []byte
	includeField7             bool
	blockKind                 string
	contextID                 string
}

func defaultClaudeCAISTestParts(model string) claudeCAISTestParts {
	return claudeCAISTestParts{
		includeEnvelopeVersion: true,
		envelopeVersion:        2,
		includeTrailer:         true,
		includeContainer:       true,
		includeChannelBlock:    true,
		includeChannelID:       true,
		channelID:              16,
		includeChannelVersion:  true,
		includeSignature:       true,
		signatureLen:           64,
		includeModelText:       true,
		modelText:              []byte(model),
		includeField7:          true,
		blockKind:              "thinking",
		contextID:              observedClaudeCAISContextID,
	}
}

func (p claudeCAISTestParts) encode() string {
	var channelBlock []byte
	if p.includeChannelID {
		if p.channelIDAsBytes {
			channelBlock = protowire.AppendTag(channelBlock, 1, protowire.BytesType)
			channelBlock = protowire.AppendBytes(channelBlock, []byte{0x10})
		} else {
			channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
			channelBlock = protowire.AppendVarint(channelBlock, p.channelID)
		}
	}
	if p.includeInfrastructure {
		channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
		channelBlock = protowire.AppendVarint(channelBlock, p.infrastructure)
	}
	if p.includeChannelVersion {
		channelBlock = protowire.AppendTag(channelBlock, 3, protowire.VarintType)
		channelBlock = protowire.AppendVarint(channelBlock, 2)
	}
	if p.includeSignature {
		channelBlock = protowire.AppendTag(channelBlock, 5, protowire.BytesType)
		channelBlock = protowire.AppendBytes(channelBlock, make([]byte, p.signatureLen))
	}
	if p.includeModelText {
		channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
		channelBlock = protowire.AppendBytes(channelBlock, p.modelText)
	}
	if p.includeField7 {
		channelBlock = protowire.AppendTag(channelBlock, 7, protowire.VarintType)
		channelBlock = protowire.AppendVarint(channelBlock, 1)
	}
	if p.blockKind != "" {
		channelBlock = protowire.AppendTag(channelBlock, 8, protowire.BytesType)
		channelBlock = protowire.AppendString(channelBlock, p.blockKind)
	}
	if p.contextID != "" {
		channelBlock = protowire.AppendTag(channelBlock, 11, protowire.BytesType)
		channelBlock = protowire.AppendString(channelBlock, p.contextID)
	}

	var container []byte
	if p.includeChannelBlock {
		container = protowire.AppendTag(container, 1, protowire.BytesType)
		container = protowire.AppendBytes(container, channelBlock)
	}
	if p.includeContainerSignature {
		container = protowire.AppendTag(container, 5, protowire.BytesType)
		container = protowire.AppendBytes(container, make([]byte, p.containerSignatureLen))
	}

	var payload []byte
	if p.includeEnvelopeVersion {
		payload = protowire.AppendTag(payload, 1, protowire.VarintType)
		payload = protowire.AppendVarint(payload, p.envelopeVersion)
	}
	if p.includeContainer {
		payload = protowire.AppendTag(payload, 2, protowire.BytesType)
		payload = protowire.AppendBytes(payload, container)
	}
	if p.includeTrailer {
		payload = protowire.AppendTag(payload, 3, protowire.VarintType)
		payload = protowire.AppendVarint(payload, 1)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func defaultClaudeCAQSTestParts(blockKind string) claudeCAISTestParts {
	parts := defaultClaudeCAISTestParts("")
	parts.envelopeVersion = 4
	parts.channelID = 17
	parts.includeSignature = false
	parts.includeModelText = false
	parts.includeContainerSignature = true
	parts.containerSignatureLen = 128
	parts.blockKind = blockKind
	return parts
}

func TestClaudeCAISSignatureObservedSample(t *testing.T) {
	if !isValidClaudeCAISSignature(observedClaudeCAISFable5Sample) {
		t.Fatal("observed Claude CAIS sample should be valid")
	}
	info, err := inspectClaudeCAISSignature(observedClaudeCAISFable5Sample)
	if err != nil {
		t.Fatalf("InspectClaudeCAISSignature failed: %v", err)
	}
	if info.FirstByte != claudeCAISSignatureMarker || info.EnvelopeVersion != 2 || info.ChannelID != 16 {
		t.Fatalf("unexpected CAIS envelope metadata: %+v", info)
	}
	if info.ModelText != "claude-fable-5" || info.BlockKind != "thinking" ||
		info.ContextID != observedClaudeCAISContextID || info.SignatureLen != 64 {
		t.Fatalf("unexpected CAIS channel metadata: %+v", info)
	}
	if isRecognizedGeminiSignature(observedClaudeCAISFable5Sample) {
		t.Fatal("Gemini signature validation must not claim Claude CAIS")
	}
	if isValidClaudeCAISSignature(validClaudeThinkingSignatureForTest()) {
		t.Fatal("CAIS signature validation must not claim classic Claude signatures")
	}

	for _, prefix := range []string{
		"",
		"claude#",
		"anthropic#",
		"cais#",
		"claude-cais#",
		"claude_cais#",
		"ccmax#",
		"claude-code-max#",
		"claude_code_max#",
	} {
		signature := prefix + observedClaudeCAISFable5Sample
		if got := DetectProvider(signature); got != ProviderClaude {
			t.Fatalf("DetectProvider(%q) = %q, want %q", prefix, got, ProviderClaude)
		}
		decision := decideCompatibility(ProviderClaude, signature, BlockKindClaudeThinking)
		if !decision.Compatible || decision.Action != actionPreserve ||
			decision.NormalizedSignature != observedClaudeCAISFable5Sample {
			t.Fatalf("Claude compatibility decision for %q = %+v", prefix, decision)
		}
	}

	if decision := decideCompatibility(
		ProviderGemini,
		observedClaudeCAISFable5Sample,
		BlockKindClaudeThinking,
	); decision.Compatible || decision.Action != actionDropBlock {
		t.Fatalf("Gemini compatibility decision = %+v, want drop block", decision)
	}
	if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(observedClaudeCAISFable5Sample); ok || normalized != "" {
		t.Fatalf("Antigravity compatibility = %q, %v, want empty and false", normalized, ok)
	}
	if isValidGrokEncryptedContent(observedClaudeCAISFable5Sample) {
		t.Fatal("Claude CAIS signature must not be accepted as Grok encrypted_content")
	}
}

func TestClaudeCAQSV4ContainerSignature(t *testing.T) {
	for _, blockKind := range []string{"thinking", "narration"} {
		t.Run(
			blockKind, func(t *testing.T) {
				signature := defaultClaudeCAQSTestParts(blockKind).encode()
				info, err := inspectClaudeCAISSignature(signature)
				if err != nil {
					t.Fatalf("inspectClaudeCAISSignature failed: %v", err)
				}
				if info.EnvelopeVersion != 4 || info.ChannelID != 17 || info.ModelText != "" ||
					info.BlockKind != blockKind || info.SignatureLen != 128 {
					t.Fatalf("unexpected CAQS metadata: %+v", info)
				}
				if got := DetectProvider(signature); got != ProviderClaude {
					t.Fatalf("DetectProvider = %q, want %q", got, ProviderClaude)
				}
			},
		)
	}
}

func TestClaudeCAQSV4RejectsMalformedPayloads(t *testing.T) {
	tests := []struct {
		name  string
		parts claudeCAISTestParts
	}{
		{
			name: "missing container signature",
			parts: func() claudeCAISTestParts {
				parts := defaultClaudeCAQSTestParts("thinking")
				parts.includeContainerSignature = false
				return parts
			}(),
		},
		{
			name: "empty container signature",
			parts: func() claudeCAISTestParts {
				parts := defaultClaudeCAQSTestParts("thinking")
				parts.containerSignatureLen = 0
				return parts
			}(),
		},
		{name: "invalid block kind", parts: defaultClaudeCAQSTestParts("redacted_thinking")},
		{
			name: "v3 still requires model text",
			parts: func() claudeCAISTestParts {
				parts := defaultClaudeCAQSTestParts("thinking")
				parts.envelopeVersion = 3
				return parts
			}(),
		},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				if _, err := inspectClaudeCAISSignature(testCase.parts.encode()); err == nil {
					t.Fatal("malformed CAQS payload was accepted")
				}
			},
		)
	}
}

func TestClaudeCAISSignatureSurvivesClaudeSanitize(t *testing.T) {
	for _, prefix := range []string{"claude#", "cais#", "ccmax#"} {
		t.Run(
			prefix, func(t *testing.T) {
				input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"keep","signature":"` + prefix + observedClaudeCAISFable5Sample + `"},{"type":"text","text":"answer"}]}]}`)
				output, report := SanitizeClaudeMessagesForClaudeUpstream(input, "claude-opus-5")
				if report.Preserved != 1 || report.DroppedBlocks != 0 {
					t.Fatalf("sanitize report = %+v, want one preserved block", report)
				}
				if got := gjson.GetBytes(
					output, "messages.0.content.0.signature",
				).String(); got != observedClaudeCAISFable5Sample {
					t.Fatalf("sanitized signature = %q, want unprefixed CAIS sample", got)
				}
			},
		)
	}
}

func TestClaudeCAISSignatureToleratesFieldDrift(t *testing.T) {
	tests := []struct {
		name  string
		parts claudeCAISTestParts
	}{
		{name: "observed layout", parts: defaultClaudeCAISTestParts("claude-opus-5")},
		{
			name: "new channel id", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.channelID = 17
				return p
			}(),
		},
		{
			name: "new envelope version", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.envelopeVersion = 3
				return p
			}(),
		},
		{
			name: "no trailer", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeTrailer = false
				return p
			}(),
		},
		{
			name: "no channel version", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeChannelVersion = false
				return p
			}(),
		},
		{
			name: "longer signature bytes", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.signatureLen = 96
				return p
			}(),
		},
		{
			name: "no field 7", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeField7 = false
				return p
			}(),
		},
		{
			name: "different block kind", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.blockKind = "redacted_thinking"
				return p
			}(),
		},
		{
			name: "no block kind", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.blockKind = ""
				return p
			}(),
		},
		{
			name: "no context id", parts: func() claudeCAISTestParts {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.contextID = ""
				return p
			}(),
		},
		{name: "future model", parts: defaultClaudeCAISTestParts("claude-opus-6-preview")},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				signature := tt.parts.encode()
				if _, err := inspectClaudeCAISSignature(signature); err != nil {
					t.Fatalf("InspectClaudeCAISSignature failed: %v", err)
				}
				if got := DetectProvider(signature); got != ProviderClaude {
					t.Fatalf("DetectProvider = %q, want %q", got, ProviderClaude)
				}
			},
		)
	}
}

func TestClaudeCAISSignatureRejectsMalformedPayloads(t *testing.T) {
	truncated := func() string {
		decoded, err := base64.StdEncoding.DecodeString(observedClaudeCAISFable5Sample)
		if err != nil {
			t.Fatalf("decode observed sample: %v", err)
		}
		return base64.StdEncoding.EncodeToString(decoded[:len(decoded)/2])
	}()

	tests := []struct {
		name      string
		signature string
	}{
		{name: "empty"},
		{name: "not base64", signature: "CAIS!!!not-base64"},
		{name: "truncated", signature: truncated},
		{name: "classic Claude", signature: validClaudeThinkingSignatureForTest()},
		{name: "wrong marker", signature: base64.StdEncoding.EncodeToString([]byte{0x0a, 0x00})},
		{
			name: "no envelope version", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeEnvelopeVersion = false
				return p.encode()
			}(),
		},
		{
			name: "no container", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeContainer = false
				return p.encode()
			}(),
		},
		{
			name: "no channel block", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeChannelBlock = false
				return p.encode()
			}(),
		},
		{
			name: "no channel id", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeChannelID = false
				return p.encode()
			}(),
		},
		{
			name: "channel id wrong wire type", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.channelIDAsBytes = true
				return p.encode()
			}(),
		},
		{
			name: "no signature bytes", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeSignature = false
				return p.encode()
			}(),
		},
		{
			name: "empty signature bytes", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.signatureLen = 0
				return p.encode()
			}(),
		},
		{
			name: "no model text", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.includeModelText = false
				return p.encode()
			}(),
		},
		{name: "foreign model text", signature: defaultClaudeCAISTestParts("gemini-3-pro").encode()},
		{
			name: "invalid UTF-8 model text", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.modelText = []byte{'c', 'l', 'a', 'u', 'd', 'e', '-', 0xff}
				return p.encode()
			}(),
		},
		{
			name: "invalid context id", signature: func() string {
				p := defaultClaudeCAISTestParts("claude-opus-5")
				p.contextID = "not-a-canonical-uuid-value-000000000"
				return p.encode()
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if isValidClaudeCAISSignature(tt.signature) {
					t.Fatalf("IsValidClaudeCAISSignature(%q) = true, want false", tt.signature)
				}
			},
		)
	}
}
