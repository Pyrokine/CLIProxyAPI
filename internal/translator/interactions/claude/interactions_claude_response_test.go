package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestInteractionsClaudeUsageAndStopReasons(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			for _, tc := range []struct{ terminal, want string }{
				{`"status":"incomplete"`, "max_tokens"},
				{`"finish_reason":"length"`, "max_tokens"},
				{`"finish_reason":"max_tokens"`, "max_tokens"},
				{`"status":"completed"`, "end_turn"},
			} {
				steps := `[]`
				if tool {
					steps = `[{"type":"function_call","id":"fixture-tool","name":"lookup","arguments":{}}]`
				}
				body := `{"id":"fixture-interaction",` + tc.terminal + `,"steps":` + steps + `,"usage":{"total_input_tokens":100,"total_cached_tokens":40,"cache_creation_tokens":10,"total_output_tokens":8}}`
				if nested {
					body = `{"interaction":` + body + `}`
				}
				want := tc.want
				if tool && want == "end_turn" {
					want = "tool_use"
				}
				out := convertInteractionsResponseToClaudeNonStream(context.Background(), "fixture-model", nil, nil, []byte(body), nil)
				if gjson.GetBytes(out, "stop_reason").String() != want || gjson.GetBytes(out, "usage.input_tokens").Int() != 50 ||
					gjson.GetBytes(out, "usage.cache_read_input_tokens").Int() != 40 || gjson.GetBytes(out, "usage.cache_creation_input_tokens").Int() != 10 || gjson.GetBytes(out, "usage.output_tokens").Int() != 8 {
					t.Fatalf("non-stream result = %s", out)
				}
				state := &interactionsToClaudeStreamState{SawToolCall: tool}
				events := appendClaudeMessageDelta(nil, gjson.Parse(body), state)
				var delta gjson.Result
				for _, event := range events {
					payload := gjson.ParseBytes(interactionsSSEPayload(event))
					if payload.Get("type").String() == "message_delta" {
						delta = payload
					}
				}
				if delta.Get("delta.stop_reason").String() != want || delta.Get("usage.input_tokens").Int() != 50 || delta.Get("usage.cache_read_input_tokens").Int() != 40 {
					t.Fatalf("stream delta = %s", delta.Raw)
				}
			}
		}
	}
	for _, tc := range []struct {
		usage string
		want  int64
	}{
		{`{"input_tokens":100,"total_input_tokens":200,"total_cached_tokens":40}`, 100},
		{`{"total_input_tokens":10,"total_cached_tokens":40}`, 0},
		{`{"prompt_tokens":100,"cache_read_tokens":40,"cache_write_tokens":10}`, 50},
		{`{"total_input_tokens":100,"total_cached_tokens":0}`, 100},
	} {
		out := setClaudeUsageFromInteractions([]byte(`{}`), "usage", gjson.Parse(tc.usage))
		if gjson.GetBytes(out, "usage.input_tokens").Int() != tc.want {
			t.Fatalf("usage = %s, want input %d", out, tc.want)
		}
	}
}

func TestInteractionsFailedEventToClaudeError(t *testing.T) {
	for _, event := range []string{
		`{"event_type":"response.failed","error":{"type":"api_error","message":"model capacity exhausted"}}`,
		`{"event_type":"interaction.failed","response":{"error":{"type":"api_error","message":"model capacity exhausted"}}}`,
	} {
		var state any
		out := convertInteractionsResponseToClaude(context.Background(), "claude-test", nil, nil, []byte(event), &state)
		if len(out) != 1 || !bytes.Contains(out[0], []byte("event: error")) ||
			!bytes.Contains(out[0], []byte("model capacity exhausted")) {
			t.Fatalf("failed event = %q, want Claude error SSE", out)
		}
		for _, following := range []string{`{"event_type":"interaction.completed"}`, `[DONE]`} {
			completed := convertInteractionsResponseToClaude(context.Background(), "claude-test", nil, nil,
				[]byte(following), &state)
			if len(completed) != 0 {
				t.Fatalf("events after failure = %q", completed)
			}
		}
	}
}

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
