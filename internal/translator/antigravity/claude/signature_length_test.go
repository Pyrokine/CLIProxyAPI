package claude

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestAntigravitySignatureLengthFailureReachesCheckedTranslation(t *testing.T) {
	oversized := strings.Repeat("R", maxBypassSignatureLen+1)
	for _, tc := range []struct {
		name      string
		blockType string
		path      string
		carrier   bool
	}{
		{name: "thinking", blockType: "thinking", path: "signature"},
		{name: "marked-carrier", blockType: "thinking", path: "signature", carrier: true},
		{name: "tool-signature", blockType: "tool_use", path: "signature"},
		{name: "tool-thought-signature", blockType: "tool_use", path: "thought_signature"},
		{name: "tool-extra-signature", blockType: "tool_use", path: "extra_content.google.thought_signature"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				value := oversized
				if tc.carrier {
					value = encodeGeminiClaudeCarrierSignature(
						value,
						geminiClaudeCarrierNext,
						geminiClaudeCarrierText,
					)
				}
				block := []byte(
					`{"type":"` + tc.blockType + `","thinking":"","id":"call-1","name":"run","input":{}}`,
				)
				block, err := sjson.SetBytes(block, tc.path, value)
				if err != nil {
					t.Fatal(err)
				}
				body := []byte(
					`{"messages":[{"role":"assistant","content":[` + string(
						block,
					) + `,{"type":"text","text":"visible"}]}]}`,
				)
				out, err := sdktranslator.TranslateRequestChecked(
					t.Context(),
					sdktranslator.FormatClaude,
					sdktranslator.FormatAntigravity,
					"gemini-3-pro-preview",
					body,
					false,
				)
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || len(out) != 0 {
					t.Fatalf(
						"oversized signature: error=%v output bytes=%d, want request-scoped HTTP 400",
						err,
						len(out),
					)
				}
				var scoped interface{ IsRequestScoped() bool }
				if !errors.As(err, &scoped) || !scoped.IsRequestScoped() ||
					strings.Contains(err.Error(), "RRRR") {
					t.Fatalf("signature rejection lost request scope or exposed signature: %v", err)
				}
			},
		)
	}
}

func TestValidateBypassMode_StrictAcceptsSignatureBetween16KiBAnd32MiB(t *testing.T) {
	native := base64.StdEncoding.EncodeToString(
		buildClaudeSignaturePayload(
			t, 12, new(uint64(2)), strings.Repeat("m", 20000), true,
		),
	)
	wrapped := base64.StdEncoding.EncodeToString([]byte(native))
	if len(native) <= 1<<14 || len(wrapped) > maxBypassSignatureLen {
		t.Fatalf("original strict fixture lengths: native=%d wrapped=%d", len(native), len(wrapped))
	}
	for _, value := range []string{native, wrapped} {
		body := []byte(
			(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
				`"thinking":"t","signature":` +
				`"`) +
				value +
				`"}]}]}`,
		)
		out, err := sdktranslator.TranslateRequestChecked(
			t.Context(),
			sdktranslator.FormatClaude,
			sdktranslator.FormatAntigravity,
			"claude-sonnet-4-6",
			body,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String() != wrapped {
			t.Fatal("original strict input did not reach the Claude thinking part")
		}
	}
}

func TestAntigravitySignatureLengthKeepsExact32MiBBoundary(t *testing.T) {
	opaque := make([]byte, maxBypassSignatureLen*3/4-10)
	opaque[0] = 0x01
	inner := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), opaque)
	wire := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), inner)
	native := base64.RawStdEncoding.EncodeToString(wire)
	if len(native) != maxBypassSignatureLen {
		t.Fatalf("native fixture length = %d", len(native))
	}
	for _, name := range []string{"native", "carrier", "carrier-newline"} {
		t.Run(
			name, func(t *testing.T) {
				value := native
				if name != "native" {
					value = encodeGeminiClaudeCarrierSignature(
						value,
						geminiClaudeCarrierNext,
						geminiClaudeCarrierText,
					)
					if name == "carrier-newline" {
						value = value[:80] + "\r\n" + value[80:]
					}
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				body := []byte(
					(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
						`"thinking":"","signature":`) +
						string(
							encoded,
						) + `},{"type":"text","text":"visible"}]}]}`,
				)
				out, err := sdktranslator.TranslateRequestChecked(
					t.Context(),
					sdktranslator.FormatClaude,
					sdktranslator.FormatAntigravity,
					"gemini-3-pro-preview",
					body,
					false,
				)
				if err != nil {
					t.Fatal(err)
				}
				parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
				if len(parts) != 1 || parts[0].Get("text").String() != "visible" ||
					parts[0].Get("thoughtSignature").String() != native {
					t.Fatalf("32 MiB signature was changed: parts=%d output bytes=%d", len(parts), len(out))
				}
			},
		)
	}
}
