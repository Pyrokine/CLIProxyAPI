package claude

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestInteractionsToClaudePreservesRecognizedProviderSignatures(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		signature string
	}{
		{name: "claude", model: "claude-test", signature: claudeInteractionSignatureForTest()},
		{name: "gemini", model: "gemini-test", signature: geminiInteractionSignatureForTest()},
		{name: "gpt", model: "gpt-test", signature: gptInteractionSignatureForTest()},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"interaction":{"id":"interaction-1","model":"` + testCase.model + `","steps":[{"type":"thought","signature":"` + testCase.signature + `","content":[]}]}}`)
			out := convertInteractionsResponseToClaudeNonStream(
				context.Background(), testCase.model, nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "content.0.signature").String(); got != testCase.signature {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.signature, out)
			}
			if got := gjson.GetBytes(out, "content.0.thinking").String(); got != "" {
				t.Fatalf("signature-only thinking text = %q, want empty; output=%s", got, out)
			}
		})
	}
}

func TestInteractionsToClaudeDropsForeignAndInvalidSignatures(t *testing.T) {
	for _, rawSignature := range []string{geminiInteractionSignatureForTest(), "claude#invalid"} {
		raw := []byte(`{"interaction":{"id":"interaction-1","model":"claude-test","steps":[{"type":"thought","signature":"` + rawSignature + `","content":[{"type":"text","text":"reason"}]}]}}`)
		out := convertInteractionsResponseToClaudeNonStream(
			context.Background(), "claude-test", nil, nil, raw, nil,
		)
		if signature := gjson.GetBytes(out, "content.0.signature"); signature.Exists() {
			t.Fatalf("incompatible signature survived: %s; output=%s", signature.Raw, out)
		}
		if got := gjson.GetBytes(out, "content.0.thinking").String(); got != "reason" {
			t.Fatalf("thinking text = %q, want reason; output=%s", got, out)
		}
	}
}

func TestInteractionsToClaudeStreamValidatesThoughtSignature(t *testing.T) {
	valid := claudeInteractionSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "invalid", signature: "claude#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var state any
			convertInteractionsResponseToClaude(
				context.Background(),
				"claude-test",
				nil,
				nil,
				[]byte(`{"event_type":"step.start","index":0,"step":{"type":"thought"}}`),
				&state,
			)
			out := convertInteractionsResponseToClaude(
				context.Background(),
				"claude-test",
				nil,
				nil,
				[]byte(`{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"`+testCase.signature+`"}}`),
				&state,
			)
			var got string
			for _, event := range out {
				if value := gjson.GetBytes(interactionsSSEPayload(event), "delta.signature"); value.Exists() {
					got = value.String()
				}
			}
			if got != testCase.want {
				t.Fatalf("stream signature = %q, want %q; events=%q", got, testCase.want, out)
			}
		})
	}
}

func claudeInteractionSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func geminiInteractionSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func gptInteractionSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}
