package interactions

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertGeminiResponseToInteractionsNonStreamSnakeCaseUsage(t *testing.T) {
	out := convertGeminiResponseToInteractionsNonStream(
		context.Background(),
		"gemini-test",
		nil,
		nil,
		[]byte(`{"responseId":"resp_snake","candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usage_metadata":{"prompt_token_count":11,"candidates_token_count":22,"total_token_count":33,"thoughts_token_count":44,"cached_content_token_count":55}}`),
		nil,
	)
	assertInteractionsUsage(
		t, out, "usage", map[string]int64{
			"input_tokens":     11,
			"output_tokens":    22,
			"reasoning_tokens": 44,
			"total_tokens":     33,
			"cached_tokens":    55,
		},
	)
}

func TestConvertGeminiResponseToInteractionsStreamSnakeCaseUsage(t *testing.T) {
	var state any
	chunks := convertGeminiResponseToInteractions(
		context.Background(),
		"gemini-test",
		nil,
		nil,
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usage_metadata":{"prompt_token_count":11,"candidates_token_count":22,"total_token_count":33,"thoughts_token_count":44,"cached_content_token_count":55}}`),
		&state,
	)
	var completed []byte
	for _, chunk := range chunks {
		if bytes.Contains(chunk, []byte(`"event_type":"interaction.completed"`)) {
			completed = chunk
			break
		}
	}
	if len(completed) == 0 {
		t.Fatalf("interaction.completed event missing: %q", chunks)
	}
	assertInteractionsUsage(
		t, completed, "interaction.usage", map[string]int64{
			"total_input_tokens":   11,
			"total_output_tokens":  22,
			"total_thought_tokens": 44,
			"total_tokens":         33,
			"total_cached_tokens":  55,
		},
	)
}

func assertInteractionsUsage(t *testing.T, payload []byte, prefix string, want map[string]int64) {
	t.Helper()
	for field, expected := range want {
		path := prefix + "." + field
		value := gjson.GetBytes(payload, path)
		if !value.Exists() || value.Int() != expected {
			t.Fatalf("%s = %s, want %d; payload: %s", path, value.Raw, expected, payload)
		}
	}
}

func TestGeminiResponseToInteractionsValidatesSignatures(t *testing.T) {
	valid := geminiInteractionsSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "foreign", signature: claudeInteractionsSignatureForTest()},
		{name: "invalid", signature: "gemini#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"responseId":"resp-1","candidates":[{"content":{"role":"model","parts":[{"thought":true,"thoughtSignature":"` + testCase.signature + `"}]}}]}`)
			out := convertGeminiResponseToInteractionsNonStream(
				context.Background(), "gemini-test", nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "steps.0.signature").String(); got != testCase.want {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.want, out)
			}
			if testCase.want == "" && gjson.GetBytes(out, "steps.#").Int() != 0 {
				t.Fatalf("invalid signature-only part produced a step: %s", out)
			}
		})
	}
}

func TestInteractionsResponseToGeminiPreservesRecognizedSignatureOnlyParts(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		signature string
	}{
		{name: "gemini", model: "gemini-test", signature: geminiInteractionsSignatureForTest()},
		{name: "claude", model: "claude-test", signature: claudeInteractionsSignatureForTest()},
		{name: "gpt", model: "gpt-test", signature: gptInteractionsSignatureForTest()},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"interaction":{"id":"interaction-1","model":"` + testCase.model + `","steps":[{"type":"thought","signature":"` + testCase.signature + `","content":[]}]}}`)
			out := convertInteractionsResponseToGeminiNonStream(
				context.Background(), testCase.model, nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "candidates.0.content.parts.0.thoughtSignature").String(); got != testCase.signature {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.signature, out)
			}
		})
	}
}

func TestInteractionsResponseToGeminiDropsForeignSignature(t *testing.T) {
	raw := []byte(`{"interaction":{"id":"interaction-1","model":"gemini-test","steps":[{"type":"thought","signature":"` + claudeInteractionsSignatureForTest() + `","content":[{"type":"text","text":"reason"}]}]}}`)
	out := convertInteractionsResponseToGeminiNonStream(
		context.Background(), "gemini-test", nil, nil, raw, nil,
	)
	if signature := gjson.GetBytes(out, "candidates.0.content.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("foreign signature survived: %s; output=%s", signature.Raw, out)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.text").String(); got != "reason" {
		t.Fatalf("thought text = %q, want reason; output=%s", got, out)
	}
}

func geminiInteractionsSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func claudeInteractionsSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func gptInteractionsSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestInteractionsFunctionArgumentsAreAssembledAtStepStop(t *testing.T) {
	var state any
	start := convertInteractionsResponseToGemini(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"weather","arguments":{}}}`),
		&state,
	)
	if len(start) != 0 {
		t.Fatalf("step.start chunks = %d, want 0", len(start))
	}
	for _, fragment := range []string{
		`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"ci"}}`,
		`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"ty\":\"Paris\"}"}}`,
	} {
		chunks := convertInteractionsResponseToGemini(
			context.Background(),
			"test-model",
			nil,
			nil,
			[]byte(fragment),
			&state,
		)
		if len(chunks) != 0 {
			t.Fatalf("arguments delta chunks = %d, want 0", len(chunks))
		}
	}
	stop := convertInteractionsResponseToGemini(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.stop","index":1}`),
		&state,
	)
	if len(stop) != 1 {
		t.Fatalf("step.stop chunks = %d, want 1", len(stop))
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.name").String(); got != "weather" {
		t.Fatalf("function name = %q, want weather", got)
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.id").String(); got != "call-1" {
		t.Fatalf("function id = %q, want call-1", got)
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.args.city").String(); got != "Paris" {
		t.Fatalf("function city = %q, want Paris", got)
	}
}
