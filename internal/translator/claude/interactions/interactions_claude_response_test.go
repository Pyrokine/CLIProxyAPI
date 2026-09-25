package interactions

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestClaudeResponseToInteractionsValidatesSignatures(t *testing.T) {
	valid := claudeSourceSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "foreign", signature: geminiSourceSignatureForTest()},
		{name: "invalid", signature: "claude#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"id":"message-1","model":"claude-test","content":[{"type":"thinking","thinking":"","signature":"` + testCase.signature + `"}]}`)
			out := convertClaudeResponseToInteractionsNonStream(
				context.Background(), "claude-test", nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "steps.0.signature").String(); got != testCase.want {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.want, out)
			}
			if got := gjson.GetBytes(out, "steps.0.content.0.text").String(); got != "" {
				t.Fatalf("signature-only thought text = %q, want empty; output=%s", got, out)
			}
		})
	}
}

func TestClaudeResponseToInteractionsStreamValidatesSignatureDelta(t *testing.T) {
	valid := claudeSourceSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "foreign", signature: geminiSourceSignatureForTest()},
		{name: "invalid", signature: "claude#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var state any
			convertClaudeResponseToInteractions(
				context.Background(),
				"claude-test",
				nil,
				nil,
				[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
				&state,
			)
			events := convertClaudeResponseToInteractions(
				context.Background(),
				"claude-test",
				nil,
				nil,
				[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+testCase.signature+`"}}`),
				&state,
			)
			var got string
			for _, event := range events {
				_, payload, ok := bytes.Cut(event, []byte("data: "))
				if !ok {
					continue
				}
				if end := bytes.IndexByte(payload, '\n'); end >= 0 {
					payload = payload[:end]
				}
				if value := gjson.GetBytes(payload, "delta.signature"); value.Exists() {
					got = value.String()
				}
			}
			if got != testCase.want {
				t.Fatalf("stream signature = %q, want %q; events=%q", got, testCase.want, events)
			}
		})
	}
}

func claudeSourceSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func geminiSourceSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}
