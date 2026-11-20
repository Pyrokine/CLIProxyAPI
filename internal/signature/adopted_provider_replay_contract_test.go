package signature

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSWESealedReplayUsesItsOwnProvider(t *testing.T) {
	const sealed = "sealed.v1.test"
	for _, model := range []string{"swe-1.5", "SWE-1.5"} {
		for _, sig := range []string{sealed, "swe#" + sealed} {
			t.Run(
				model+"/"+sig, func(t *testing.T) {
					if got := ProviderFromModelName(model); got != providerSWE {
						t.Fatalf("target provider=%q want SWE", got)
					}
					body := []byte(
						(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
							`"thinking":"keep","signature":` +
							`"`) +
							sig +
							(`"},{"type":` +
								`"text","text":` +
								`"visible"}]}]}`),
					)
					output, report := sanitizeClaudeMessagesSignaturesForModel(body, model)
					if report.TargetProvider != providerSWE || report.Preserved != 1 ||
						report.DroppedBlocks != 0 ||
						len(report.Decisions) != 1 {
						t.Fatalf("SWE replay report=%+v", report)
					}
					decision := report.Decisions[0]
					if decision.TargetProvider != providerSWE || decision.DetectedProvider != providerSWE ||
						!decision.Compatible {
						t.Fatalf("provider attribution=%+v", decision)
					}
					if got := gjson.GetBytes(output, "messages.0.content.0.signature").String(); got != sealed {
						t.Fatalf("replay signature=%q want=%q", got, sealed)
					}
				},
			)
		}
	}
	for _, sig := range []string{"sealed.v1.", "swe#claude#sealed.v1.test", validClaudeThinkingSignatureForTest()} {
		if got, ok := CompatibleSignatureForProvider(providerSWE, sig, BlockKindClaudeThinking); ok ||
			got != "" {
			t.Fatalf("SWE accepted invalid or foreign signature=%q", sig)
		}
	}
	if got, ok := CompatibleSignatureForProvider(ProviderClaude, sealed, BlockKindClaudeThinking); ok ||
		got != "" {
		t.Fatal("Claude accepted a foreign SWE signature")
	}
}

func TestAdoptedLegacyField1InputReplaysWithoutBypass(t *testing.T) {
	const path = "contents.0.parts.0.thoughtSignature"
	for name, raw := range map[string][]byte{
		"original two records": {0x0a, 0x02, 0x01, 0x8f, 0x0a, 0x03, 0x01, 0x90, 0x91},
		"truncated record":     {0x0a, 0x02, 0x01},
		"empty record":         {0x0a, 0x00},
		"wrong wire type":      {0x08, 0x01},
	} {
		t.Run(
			name, func(t *testing.T) {
				sig := base64.StdEncoding.EncodeToString(raw)
				body := []byte(
					(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"keep",` +
						`"thoughtSignature":` +
						`"`) +
						sig +
						`"}]}]}`,
				)
				output := SanitizeGeminiRequestThoughtSignatures(body, "contents")
				got := gjson.GetBytes(output, path).String()
				if name == "original two records" {
					if got != sig || DetectProvider(sig) != ProviderGemini {
						t.Fatalf("legacy field-1 changed: signature=%q provider=%q", got, DetectProvider(sig))
					}
					return
				}
				if DetectProvider(sig) == ProviderGemini || got == sig {
					t.Fatalf("malformed field-1 accepted: %q", sig)
				}
			},
		)
	}
}

func TestAdoptedCAISDiagnosticsKeepStructuredProviders(t *testing.T) {
	sig := defaultClaudeCAISTestParts("claude-opus-5").encode()
	body := []byte(
		(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
			`"thinking":"keep","signature":` +
			`"`) +
			sig +
			`"}]}]}`,
	)
	_, report := SanitizeClaudeMessagesForClaudeUpstream(body, "claude-sonnet-4-6")
	if report.Preserved != 1 || len(report.Decisions) != 1 {
		t.Fatalf("CAIS replay report=%+v", report)
	}
	decision := report.Decisions[0]
	if decision.TargetProvider != ProviderClaude || decision.DetectedProvider != ProviderClaude ||
		decision.Reason == "" {
		t.Fatalf("CAIS provider attribution=%+v", decision)
	}
	output := captureGeminiSanitizeLog(t)
	gemini := []byte(
		(`{"contents":[{"role":"model","parts":[{"thought":true,"text":` +
			`"private-thought","thoughtSignature":` +
			`"`) +
			sig +
			`"}]}]}`,
	)
	SanitizeGeminiRequestThoughtSignatures(gemini, "contents")
	entry := decodeGeminiSanitizeLog(t, output)
	positions, ok := entry["positions"].([]any)
	if !ok || len(positions) != 1 {
		t.Fatalf("signature summary positions=%v", entry["positions"])
	}
	position := positions[0].(map[string]any)
	if entry["target_provider"] != "gemini" || position["detected_provider"] != "claude" ||
		position["reason"] == "" {
		t.Fatalf("signature summary attribution=%s", output.String())
	}
	if strings.Contains(output.String(), sig) || strings.Contains(output.String(), "private-thought") {
		t.Fatal("signature summary disclosed signature or thought content")
	}
}
