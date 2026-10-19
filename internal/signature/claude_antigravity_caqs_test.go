package signature

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func antigravityCAQSTestParts() claudeCAISTestParts {
	parts := defaultClaudeCAQSTestParts("thinking")
	parts.channelID = 18
	parts.includeInfrastructure = true
	parts.infrastructure = 2
	return parts
}

func antigravityCAQSTestSignature(parts claudeCAISTestParts) string {
	return base64.StdEncoding.EncodeToString([]byte(parts.encode()))
}

func TestAntigravityCAQSReplayIsLimitedToAntigravity(t *testing.T) {
	sig := antigravityCAQSTestSignature(antigravityCAQSTestParts())
	for _, test := range []struct {
		name, prefix string
		provider     Provider
		replay       bool
	}{
		{"bare", "", ProviderClaude, true},
		{"single prefix", "claude#", ProviderClaude, true},
		{"alias prefix", "anthropic#", ProviderClaude, true},
		{"duplicate prefix", "claude#claude#", providerUnknown, false},
		{"nested unknown prefix", "claude#junk#", providerUnknown, false},
		{"nested alias prefix", "anthropic#cais#", providerUnknown, false},
		{"unknown prefix", "junk#", providerUnknown, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := test.prefix + sig
			if got := DetectProvider(raw); got != test.provider {
				t.Fatalf("DetectProvider() = %q, want %q", got, test.provider)
			}
			if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(raw); ok != test.replay ||
				(ok && normalized != sig) || (!ok && normalized != "") {
				t.Fatalf("Antigravity replay = %t, want %t", ok, test.replay)
			}
			if decision := decideCompatibility(ProviderClaude, raw, BlockKindClaudeThinking); decision.Compatible || decision.Action != actionDropBlock || decision.NormalizedSignature != "" {
				t.Fatal("native Claude accepted Antigravity CAQS wrapper")
			}
			payload := []byte(`{"messages":[{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"work","signature":"` + raw + `"},` +
				`{"type":"text","text":"answer"}]}]}`)
			output, report := SanitizeClaudeMessagesForClaudeUpstream(payload, "claude-opus-5-5")
			content := gjson.GetBytes(output, "messages.0.content").Array()
			if len(content) != 1 || content[0].Get("type").String() != "text" ||
				content[0].Get("text").String() != "answer" || report.DroppedBlocks != 1 || report.Preserved != 0 {
				t.Fatalf("native Claude preserved Q thinking: report=%+v output=%s", report, output)
			}
		})
	}
	inner, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	if !isValidClaudeCAISSignature(string(inner)) {
		t.Fatal("inner CAQS must remain valid as a native CAQS envelope")
	}
	if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(string(inner)); ok || normalized != "" {
		t.Fatal("bare CAQS envelope was promoted to Antigravity wire format")
	}
	if isRecognizedGeminiSignature(sig) {
		t.Fatal("Gemini classified Antigravity CAQS as its own signature")
	}
	input := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"work","signature":"` + sig + `"},` +
		`{"type":"text","text":"answer"}]}]}`)
	output, report := SanitizeClaudeMessagesForClaudeUpstream(input, "claude-opus-5-5")
	if report.DroppedBlocks != 1 || gjson.GetBytes(output, "messages.0.content.0.type").String() != "text" {
		t.Fatalf("native Claude preserved Q thinking: report=%+v output=%s", report, output)
	}
}

func TestAntigravityCAQSRejectsUnsupportedAndAmbiguousWrappers(t *testing.T) {
	base := antigravityCAQSTestParts()
	sig := antigravityCAQSTestSignature(base)
	inner, _ := base64.StdEncoding.DecodeString(sig)
	raw, _ := base64.StdEncoding.DecodeString(string(inner))
	container, err := extractClaudeBytesField(raw, 2, "container")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := extractClaudeBytesField(container, 1, "channel")
	if err != nil {
		t.Fatal(err)
	}
	bytesField := func(field protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), value)
	}
	wrap := func(value []byte) string {
		return base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(value)))
	}
	duplicateContainer := append(append([]byte(nil), raw...), bytesField(2, container)...)
	duplicateChannelContainer := append(append([]byte(nil), container...), bytesField(1, channel)...)
	duplicateChannel := append([]byte{8, 4}, bytesField(2, duplicateChannelContainer)...)
	wrongInfrastructure := base
	wrongInfrastructure.infrastructure = 1
	wrongChannel := base
	wrongChannel.channelID = 17
	wrongVersion := base
	wrongVersion.envelopeVersion = 5
	missingInfrastructure := base
	missingInfrastructure.includeInfrastructure = false
	misplacedSignature := base
	misplacedSignature.includeSignature = true
	missingSignature := base
	missingSignature.includeContainerSignature = false
	narration := base
	narration.blockKind = "narration"
	for name, value := range map[string]string{
		"empty": "", "truncated": sig[:len(sig)-8],
		"wrong infrastructure":   antigravityCAQSTestSignature(wrongInfrastructure),
		"wrong channel":          antigravityCAQSTestSignature(wrongChannel),
		"future version":         antigravityCAQSTestSignature(wrongVersion),
		"missing infrastructure": antigravityCAQSTestSignature(missingInfrastructure),
		"signature in channel":   antigravityCAQSTestSignature(misplacedSignature),
		"missing signature":      antigravityCAQSTestSignature(missingSignature),
		"narration":              antigravityCAQSTestSignature(narration),
		"duplicate container":    wrap(duplicateContainer),
		"duplicate channel":      wrap(duplicateChannel),
		"inner whitespace":       base64.StdEncoding.EncodeToString(append(append([]byte(nil), inner...), ' ')),
		"outer newline":          sig[:8] + "\n" + sig[8:],
		"triple wrapper":         base64.StdEncoding.EncodeToString([]byte(sig)),
	} {
		t.Run(name, func(t *testing.T) {
			if isValidAntigravityClaudeCAQSSignature(value) {
				t.Fatal("unsupported CAQS wrapper accepted")
			}
			if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(value); ok || normalized != "" {
				t.Fatal("unsupported CAQS wrapper was replayable")
			}
			if strings.HasPrefix(value, "Q") {
				if normalized, ok := CompatibleSignatureForProviderWithOptions(ProviderClaude, value, BlockKindClaudeThinking,
					SignatureCompatibilityOptions{PreserveUnknownNonEmpty: true}); ok || normalized != "" {
					t.Fatal("native Claude accepted CAQS wrapper")
				}
			}
		})
	}
}

func TestAntigravityCAQSDuplicateSubmessages(t *testing.T) {
	inner, err := base64.StdEncoding.DecodeString(antigravityCAQSTestSignature(antigravityCAQSTestParts()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		t.Fatal(err)
	}
	container, err := extractClaudeBytesField(raw, 2, "container")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := extractClaudeBytesField(container, 1, "channel")
	if err != nil {
		t.Fatal(err)
	}
	bytesField := func(field protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), value)
	}
	payload := func(containers ...[]byte) []byte {
		out := []byte{8, 4}
		for _, value := range containers {
			out = append(out, bytesField(2, value)...)
		}
		return append(out, 24, 1)
	}
	assertRejected := func(t *testing.T, raw []byte) {
		t.Helper()
		inner := base64.StdEncoding.EncodeToString(raw)
		if _, err := inspectClaudeCAISSignature(inner); err != nil {
			t.Fatalf("native CAIS parsing changed: %v", err)
		}
		sig := base64.StdEncoding.EncodeToString([]byte(inner))
		if _, err := inspectAntigravityClaudeCAQSSignature(sig); err == nil {
			t.Fatal("duplicate submessage passed Antigravity validation")
		}
		if got := DetectProvider(sig); got != providerUnknown {
			t.Fatalf("duplicate provider = %q, want unknown", got)
		}
		if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(sig); ok || normalized != "" {
			t.Fatal("duplicate submessage was replayable")
		}
		for _, preserveUnknown := range []bool{false, true} {
			if normalized, ok := CompatibleSignatureForProviderWithOptions(ProviderClaude, sig, BlockKindClaudeThinking,
				SignatureCompatibilityOptions{PreserveUnknownNonEmpty: preserveUnknown}); ok || normalized != "" {
				t.Fatal("native compatibility accepted duplicate Antigravity wrapper")
			}
		}
	}
	for _, test := range []struct {
		name    string
		channel []byte
	}{
		{"malformed tag", []byte{0x80}},
		{"infrastructure wire type", []byte{0x12, 0x01, 0x02}},
		{"old signature slot", []byte{0x2a, 0x01, 0x01}},
		{"empty", nil},
		{"valid duplicate", channel},
	} {
		for _, scope := range []string{"channel", "container"} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				first := bytesField(1, test.channel)
				if scope == "channel" {
					assertRejected(t, payload(append(first, container...)))
				} else {
					assertRejected(t, payload(first, container))
				}
			})
		}
	}
	for _, test := range []struct {
		name      string
		container []byte
	}{
		{"malformed container", []byte{0x80}},
		{"empty container", nil},
		{"valid container duplicate", container},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRejected(t, payload(test.container, container))
		})
	}
}

func TestAntigravityCAQSStrictEncoding(t *testing.T) {
	parts := antigravityCAQSTestParts()
	parts.containerSignatureLen = 1020
	sig := antigravityCAQSTestSignature(parts)
	info, err := inspectAntigravityClaudeCAQSSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if info.EnvelopeVersion != 4 || info.ChannelID != 18 || info.Infrastructure == nil || *info.Infrastructure != 2 ||
		info.BlockKind != "thinking" || info.ModelText != "" || info.SignatureLen != 1020 || !info.SignatureInContainer {
		t.Fatalf("unexpected Antigravity envelope: %+v", info)
	}
	for _, target := range []Provider{ProviderClaude, ProviderGemini, ProviderGPT, providerKimi, providerGrok} {
		if decision := decideCompatibility(target, sig, BlockKindClaudeThinking); decision.Compatible {
			t.Fatalf("Antigravity wrapper accepted directly by %q", target)
		}
	}
	inner, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		t.Fatal(err)
	}
	padded := append(append([]byte(nil), raw...), 32, 0)
	for len(padded)%3 == 0 {
		padded = append(padded, 32, 0)
	}
	badPadding := func(encoded string) string {
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		value := []byte(encoded)
		index := len(value) - 1
		for index >= 0 && value[index] == '=' {
			index--
		}
		if index == len(value)-1 || index < 0 {
			t.Fatal("fixture has no padding")
		}
		value[index] = alphabet[strings.IndexByte(alphabet, value[index])|1]
		return string(value)
	}
	paddedInner := base64.StdEncoding.EncodeToString(padded)
	for name, value := range map[string]string{
		"wrapped native CAIS": base64.StdEncoding.EncodeToString([]byte(defaultClaudeCAISTestParts("claude-opus-5-5").encode())),
		"wrapped native CAQS": base64.StdEncoding.EncodeToString([]byte(defaultClaudeCAQSTestParts("thinking").encode())),
		"inner padding bits":  base64.StdEncoding.EncodeToString([]byte(badPadding(paddedInner))),
		"outer padding bits":  badPadding(base64.StdEncoding.EncodeToString([]byte(paddedInner))),
		"inner trailing tab":  base64.StdEncoding.EncodeToString(append(append([]byte(nil), inner...), '\t')),
		"inner newline":       base64.StdEncoding.EncodeToString(append([]byte("CAQS\n"), inner[4:]...)),
		"inner label":         base64.StdEncoding.EncodeToString(append([]byte("Claude#"), inner...)),
		"inner leading space": base64.StdEncoding.EncodeToString(append([]byte(" "), inner...)),
		"prefix only":         "Q0FRUw==",
		"oversize":            "Q" + strings.Repeat("A", maxSignatureLen),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectAntigravityClaudeCAQSSignature(value); err == nil {
				t.Fatal("invalid Antigravity encoding accepted")
			}
			if normalized, ok := CompatibleAntigravityClaudeThinkingSignature(value); ok || normalized != "" {
				t.Fatal("invalid Antigravity encoding was replayable")
			}
			if normalized, ok := CompatibleSignatureForProvider(ProviderClaude, value, BlockKindClaudeThinking); ok || normalized != "" {
				t.Fatal("strict native compatibility accepted invalid Antigravity encoding")
			}
			normalized, ok := CompatibleSignatureForProviderWithOptions(ProviderClaude, value, BlockKindClaudeThinking,
				SignatureCompatibilityOptions{PreserveUnknownNonEmpty: true})
			if strings.HasPrefix(value, "Q") {
				if ok || normalized != "" {
					t.Fatal("opaque compatibility accepted an invalid Q wrapper")
				}
			} else if !ok || normalized != value {
				t.Fatal("explicit opaque compatibility changed an unknown non-Q value")
			}
		})
	}
}
