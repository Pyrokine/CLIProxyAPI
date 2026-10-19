package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertGeminiResponseToClaude_EmitsSignatureOnlyChunk(t *testing.T) {
	var param any
	thinking := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"private reasoning","thought":true}]}}]
	}`)
	validSignature := geminiClaudeSignatureForTest()
	signature := []byte(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"` + validSignature + `"}]}}]}`)

	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, thinking, &param)
	out := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, signature, &param)[0])

	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, validSignature) {
		t.Fatalf("signature_delta missing: %s", out)
	}
	if strings.Contains(out, "text_delta") {
		t.Fatalf("signature-only chunk emitted text_delta: %s", out)
	}
}

func geminiClaudeSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertGeminiResponseToClaude_PreservesStandaloneSignaturePart(t *testing.T) {
	var param any
	validSignature := geminiClaudeSignatureForTest()
	chunks := convertGeminiResponseToClaude(
		context.Background(),
		"gemini-2.5-pro",
		nil,
		nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"`+validSignature+`"}]}}]}`),
		&param,
	)
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	output := string(chunks[0])
	if !strings.Contains(output, `"content_block":{"type":"thinking"`) {
		t.Fatalf("standalone signature did not open a thinking block: %s", output)
	}
	if !strings.Contains(output, `"type":"signature_delta"`) || !strings.Contains(output, validSignature) {
		t.Fatalf("standalone signature delta missing: %s", output)
	}
}

func TestConvertGeminiResponseToClaude_StrictlyDropsInvalidSignatures(t *testing.T) {
	for _, invalidSignature := range []string{"opaque-client-signature", "EgQKAggM"} {
		t.Run(invalidSignature, func(t *testing.T) {
			var param any
			chunks := convertGeminiResponseToClaude(
				context.Background(),
				"gemini-2.5-pro",
				nil,
				nil,
				[]byte(`{"candidates":[{"content":{"parts":[{"text":"private","thought":true,"thoughtSignature":"`+invalidSignature+`"}]}}]}`),
				&param,
			)
			output := string(chunks[0])
			if strings.Contains(output, "signature_delta") || strings.Contains(output, invalidSignature) {
				t.Fatalf("invalid signature was emitted: %s", output)
			}
			if !strings.Contains(output, `"type":"thinking_delta"`) || strings.Contains(output, `"type":"text_delta"`) {
				t.Fatalf("hidden thought isolation changed: %s", output)
			}
		})
	}
}

func TestConvertGeminiResponseToClaudeNonStreamPreservesThoughtSignature(t *testing.T) {
	validSignature := geminiClaudeSignatureForTest()
	output := convertGeminiResponseToClaudeNonStream(
		context.Background(),
		"gemini-2.5-pro",
		nil,
		nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"private","thought":true},{"thoughtSignature":"`+validSignature+`"},{"text":"visible"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`),
		nil,
	)
	thinking := gjson.GetBytes(output, "content.0")
	if thinking.Get("type").String() != "thinking" || thinking.Get("thinking").String() != "private" {
		t.Fatalf("thinking block = %s", thinking.Raw)
	}
	if got := thinking.Get("signature").String(); got != validSignature {
		t.Fatalf("thinking signature = %q, want %q", got, validSignature)
	}
	if got := gjson.GetBytes(output, "content.1.text").String(); got != "visible" {
		t.Fatalf("visible text = %q, want visible; output=%s", got, output)
	}
}

func TestConvertGeminiResponseToClaudeKeepsSignedVisibleText(t *testing.T) {
	sig := geminiClaudeSignatureForTest()
	payload := []byte(`{"candidates":[{"content":{"parts":[{"text":"ok","thoughtSignature":"` + sig + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`)
	var state any
	stream := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, payload, &state)[0])
	if !strings.Contains(stream, `"type":"signature_delta"`) ||
		!strings.Contains(stream, `"type":"text_delta","text":"ok"`) ||
		strings.Contains(stream, `"type":"thinking_delta","thinking":"ok"`) ||
		strings.Index(stream, `"type":"signature_delta"`) >= strings.Index(stream, `"type":"text_delta","text":"ok"`) {
		t.Fatalf("signed visible text was not emitted after its signature: %s", stream)
	}
	nonStream := convertGeminiResponseToClaudeNonStream(context.Background(), "gemini-2.5-pro", nil, nil, payload, nil)
	if carrier := gjson.GetBytes(nonStream, "content.0"); carrier.Get("type").String() != "thinking" ||
		carrier.Get("thinking").String() != "" || carrier.Get("signature").String() != sig {
		t.Fatalf("signature carrier = %s; output = %s", carrier.Raw, nonStream)
	}
	if visible := gjson.GetBytes(nonStream, "content.1"); visible.Get("type").String() != "text" || visible.Get("text").String() != "ok" {
		t.Fatalf("signed visible text = %s; output = %s", visible.Raw, nonStream)
	}
}

func TestConvertGeminiResponseToClaudeClosesPartlessSafetyRefusal(t *testing.T) {
	payload := []byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":0}}`)
	var state any
	stream := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, payload, &state)[0])
	if !strings.Contains(stream, `"stop_reason":"refusal"`) || !strings.Contains(stream, `"input_tokens":3`) ||
		!strings.Contains(stream, `"content_block":{"type":"text","text":""}`) {
		t.Fatalf("partless safety response missing terminal data: %s", stream)
	}
	end := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &state)[0])
	if !strings.Contains(end, `"type":"message_stop"`) || strings.Contains(end, `"type":"message_delta"`) {
		t.Fatalf("partless safety response did not close exactly once: %s", end)
	}
	nonStream := convertGeminiResponseToClaudeNonStream(context.Background(), "gemini-2.5-pro", nil, nil, payload, nil)
	if got := gjson.GetBytes(nonStream, "stop_reason").String(); got != "refusal" {
		t.Fatalf("non-stream safety stop reason = %q; response = %s", got, nonStream)
	}
}

func TestConvertGeminiResponseToClaudeFinishesWithoutFinishReason(t *testing.T) {
	var state any
	payload := []byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`)
	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, payload, &state)
	end := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &state)[0])
	if !strings.Contains(end, `"stop_reason":"end_turn"`) || !strings.Contains(end, `"type":"message_stop"`) {
		t.Fatalf("clean stream end did not close the message: %s", end)
	}
}

func TestConvertGeminiResponseToClaudePreservesToolEnvelope(t *testing.T) {
	validSignature := geminiClaudeSignatureForTest()
	part := `{"functionCall":{"id":"native-call-id","name":"lookup","args":{"query":"status"}},"thoughtSignature":"` + validSignature + `"}`

	var param any
	chunks := convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[`+part+`]}}]}`),
		&param,
	)
	streamOutput := string(chunks[0])
	if !strings.Contains(streamOutput, `"id":"native-call-id"`) ||
		!strings.Contains(streamOutput, `"signature":"`+validSignature+`"`) {
		t.Fatalf("stream tool envelope changed: %s", streamOutput)
	}

	nonStreamOutput := convertGeminiResponseToClaudeNonStream(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[`+part+`]},"finishReason":"STOP"}]}`),
		nil,
	)
	tool := gjson.GetBytes(nonStreamOutput, "content.0")
	if tool.Get("id").String() != "native-call-id" || tool.Get("signature").String() != validSignature {
		t.Fatalf("non-stream tool envelope changed: %s", tool.Raw)
	}
}

func TestConvertGeminiResponseToClaudeSubtractsCacheReadUsage(t *testing.T) {
	var param any
	convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`),
		&param,
	)
	stream := convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":2}}`),
		&param,
	)
	streamOutput := string(stream[0])
	if !strings.Contains(streamOutput, `"input_tokens":6`) ||
		!strings.Contains(streamOutput, `"cache_read_input_tokens":4`) {
		t.Fatalf("stream cache usage mismatch: %s", streamOutput)
	}

	nonStreamOutput := convertGeminiResponseToClaudeNonStream(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"cachedContentTokenCount":5,"candidatesTokenCount":2}}`),
		nil,
	)
	if got := gjson.GetBytes(nonStreamOutput, "usage.input_tokens").Int(); got != 0 {
		t.Fatalf("non-stream input_tokens = %d, want 0; output=%s", got, nonStreamOutput)
	}
	if got := gjson.GetBytes(nonStreamOutput, "usage.cache_read_input_tokens").Int(); got != 5 {
		t.Fatalf("non-stream cache_read_input_tokens = %d, want 5; output=%s", got, nonStreamOutput)
	}
}

func TestConvertGeminiResponseToClaude_FinalEventsOnlyOnce(t *testing.T) {
	var param any
	thinking := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"answer"}]}}]
	}`)
	final := []byte(`{
		"candidates":[{"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}
	}`)

	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, thinking, &param)
	first := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, final, &param)[0])
	second := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, final, &param)[0])

	if !strings.Contains(first, "message_delta") {
		t.Fatalf("first final chunk missing message_delta: %s", first)
	}
	if strings.Contains(second, "message_delta") {
		t.Fatalf("second final chunk repeated message_delta: %s", second)
	}
}

func TestConvertGeminiResponseToClaudePreservesRegisteredToolSignature(t *testing.T) {
	sig := geminiClaudeSignatureForTest()
	body := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"native-call-id","name":"lookup","args":{"query":"status"}},"thoughtSignature":"` + sig + `"}]},"finishReason":"STOP"}]}`)
	ctx := context.Background()
	var state any
	stream := string(bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, body, &state), nil))
	if !strings.Contains(stream, `"id":"native-call-id"`) || !strings.Contains(stream, `"signature":"`+sig+`"`) || strings.Contains(stream, `"type":"thinking"`) {
		t.Fatalf("registered stream tool signature changed: %s", stream)
	}
	nonStream := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, body, nil)
	tool := gjson.GetBytes(nonStream, "content.0")
	if tool.Get("type").String() != "tool_use" || tool.Get("id").String() != "native-call-id" || tool.Get("signature").String() != sig {
		t.Fatalf("registered non-stream tool signature changed: %s", nonStream)
	}
	history := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"messages":[{"role":"user","content":"status"},{"role":"assistant","content":` + gjson.GetBytes(nonStream, "content").Raw + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"native-call-id","content":"available"}]}]}`)
	replay, err := sdktranslator.TranslateRequestChecked(ctx, sdktranslator.FormatClaude, sdktranslator.FormatGemini, "gemini-2.5-pro", history, false)
	if err != nil {
		t.Fatal(err)
	}
	part := gjson.GetBytes(replay, "contents.1.parts.0")
	if part.Get("functionCall.id").String() != "native-call-id" || part.Get("functionCall.name").String() != "lookup" || part.Get("functionCall.args.query").String() != "status" || part.Get("thoughtSignature").String() != sig {
		t.Fatalf("registered checked replay lost tool identity or signature: %s", replay)
	}
}

func TestConvertGeminiResponseToClaudePreservesSignedToolContinuation(t *testing.T) {
	validSignature := geminiClaudeSignatureForTest()
	for _, tc := range []struct {
		name          string
		signaturePath string
		signature     string
		wantSignature bool
	}{
		{"native-camel", "thoughtSignature", validSignature, true},
		{"native-snake", "thought_signature", validSignature, true},
		{"invalid", "thoughtSignature", "opaque-client-signature", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			ctx := context.Background()
			first := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"native-call-id","name":"lookup"}}]}}]}`)
			last := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"query":"status"}},"` + tc.signaturePath + `":"` + tc.signature + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`)
			output := bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, first, &state), nil)
			output = append(output, bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, last, &state), nil)...)
			output = append(output, bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &state), nil)...)
			text := string(output)
			if !strings.Contains(text, `"id":"native-call-id"`) || strings.Count(text, `"type":"tool_use"`) != 1 {
				t.Fatalf("tool identity changed: %s", text)
			}
			var signatures []string
			var arguments []string
			for _, line := range strings.Split(text, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				event := gjson.Parse(strings.TrimPrefix(line, "data: "))
				switch event.Get("delta.type").String() {
				case "signature_delta":
					signatures = append(signatures, event.Get("delta.signature").String())
				case "input_json_delta":
					arguments = append(arguments, event.Get("delta.partial_json").String())
				}
			}
			if len(arguments) != 1 || arguments[0] != `{"query":"status"}` {
				t.Fatalf("continuation arguments = %q; output = %s", arguments, text)
			}
			if tc.wantSignature {
				if len(signatures) != 1 || signatures[0] != tc.signature {
					t.Fatalf("continuation signatures = %q, want %q; output = %s", signatures, tc.signature, text)
				}
				if strings.Count(text, `"type":"content_block_stop","index":0`) != 1 || strings.Index(text, `"type":"content_block_stop","index":0`) >= strings.Index(text, `"type":"signature_delta"`) {
					t.Fatalf("signature carrier must follow the tool block: %s", text)
				}
			} else if len(signatures) != 0 || strings.Contains(text, tc.signature) {
				t.Fatalf("invalid continuation signature survived: %s", text)
			}
			if strings.Count(text, `"type":"message_stop"`) != 1 || !strings.Contains(text, `"stop_reason":"tool_use"`) {
				t.Fatalf("tool terminal events changed: %s", text)
			}
			if duplicate := sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &state); len(duplicate) != 0 {
				t.Fatalf("duplicate terminal output: %q", duplicate)
			}
		})
	}
}

func TestConvertGeminiResponseToClaudePreservesConsecutiveSignatureCarriers(t *testing.T) {
	firstSignature := geminiClaudeSignatureForTest()
	payload, err := base64.StdEncoding.DecodeString(firstSignature)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1]++
	secondSignature := base64.StdEncoding.EncodeToString(payload)
	for _, textField := range []string{"", `"text":"",`} {
		t.Run(textField, func(t *testing.T) {
			body := []byte(`{"candidates":[{"content":{"parts":[{` + textField + `"thoughtSignature":"` + firstSignature + `"},{` + textField + `"thoughtSignature":"` + secondSignature + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0}}`)
			ctx := context.Background()
			var state any
			stream := bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, body, &state), nil)
			stream = append(stream, bytes.Join(sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &state), nil)...)
			starts := 0
			stops := 0
			var signatures []string
			for _, line := range strings.Split(string(stream), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				event := gjson.Parse(strings.TrimPrefix(line, "data: "))
				switch event.Get("type").String() {
				case "content_block_start":
					if event.Get("content_block.type").String() != "thinking" || event.Get("content_block.thinking").String() != "" {
						t.Fatalf("signature carrier = %s", event.Raw)
					}
					starts++
				case "content_block_stop":
					stops++
				case "content_block_delta":
					if event.Get("delta.type").String() != "signature_delta" {
						t.Fatalf("signature carrier delta = %s", event.Raw)
					}
					signatures = append(signatures, event.Get("delta.signature").String())
				}
			}
			if starts != 2 || stops != 2 || len(signatures) != 2 || signatures[0] != firstSignature || signatures[1] != secondSignature {
				t.Errorf("stream carriers starts=%d stops=%d signatures=%q; output=%s", starts, stops, signatures, stream)
			}
			nonStream := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "gemini-2.5-pro", nil, nil, body, nil)
			blocks := gjson.GetBytes(nonStream, "content").Array()
			if len(blocks) != 2 {
				t.Fatalf("non-stream carrier count = %d, want 2; output=%s", len(blocks), nonStream)
			}
			for i, want := range []string{firstSignature, secondSignature} {
				if blocks[i].Get("type").String() != "thinking" || blocks[i].Get("thinking").String() != "" || blocks[i].Get("signature").String() != want {
					t.Fatalf("non-stream carrier %d = %s, want signature %q", i, blocks[i].Raw, want)
				}
			}
		})
	}
}

func TestConvertGeminiResponseToClaude_SignatureOnlyPartDoesNotOpenEmptyTextBlock(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-test","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	thinkingChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "thinking text", "thought": true}]
			}
		}],
		"modelVersion": "gemini-test",
		"responseId": "resp-test"
	}`)
	signatureChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "", "thoughtSignature": "EgYKBAECAws="}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"thoughtsTokenCount": 2,
			"totalTokenCount": 12
		},
		"modelVersion": "gemini-test",
		"responseId": "resp-test"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-test", requestJSON, requestJSON, thinkingChunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-test", requestJSON, requestJSON, signatureChunk, &param), nil)...)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-test", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if strings.Contains(outputText, `"content_block":{"type":"text"`) {
		t.Fatalf("signature-only part must not open an empty text block: %s", outputText)
	}
	if strings.Contains(outputText, `"type":"content_block_stop","index":1`) {
		t.Fatalf("signature-only part must not produce a stop for unopened index 1: %s", outputText)
	}
	if !strings.Contains(outputText, `"type":"signature_delta"`) || !strings.Contains(outputText, `"signature":"EgYKBAECAws="`) {
		t.Fatalf("signature-only part must be emitted as a thinking signature delta: %s", outputText)
	}
	if got := strings.Count(outputText, `"type":"content_block_stop","index":0`); got != 1 {
		t.Fatalf("expected exactly one stop for thinking index 0, got %d: %s", got, outputText)
	}
	if !strings.Contains(outputText, `"type":"message_delta"`) || !strings.Contains(outputText, `"output_tokens":2`) {
		t.Fatalf("finish chunk without candidatesTokenCount must still emit final message_delta: %s", outputText)
	}
	if !strings.Contains(outputText, `"type":"message_stop"`) {
		t.Fatalf("DONE chunk must still emit message_stop after final events: %s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_PreservesThoughtSignature(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "thinking step 1\n", "thought": true},
					{"text": "thinking step 2", "thought": true, "thoughtSignature": "EgYKBAECAw8="},
					{"text": "visible answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "thinking step 1\nthinking step 2" {
		t.Fatalf("unexpected thinking content: %s", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "EgYKBAECAw8=" {
		t.Fatalf("expected signature 'EgYKBAECAw8=', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "visible answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_PartWithThoughtSignatureWithoutThoughtBool(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "inferred reasoning", "thought_signature": "EgYKBAECAwY="},
					{"text": "final answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream-2"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "" {
		t.Fatalf("carrier thinking block must have empty thinking, got %q", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "EgYKBAECAwY=" {
		t.Fatalf("expected signature 'EgYKBAECAwY=', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "inferred reasoningfinal answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_TrailingSignatureOnlyPart(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "thinking step 1\n", "thought": true},
					{"text": "", "thoughtSignature": "EgYKBAECAw0="},
					{"text": "visible answer"}
				]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 5
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-non-stream-trailing"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected first block to be thinking, got %s", thinkingBlock.Get("type").String())
	}
	if thinkingBlock.Get("thinking").String() != "thinking step 1\n" {
		t.Fatalf("unexpected thinking content: %s", thinkingBlock.Get("thinking").String())
	}
	if thinkingBlock.Get("signature").String() != "EgYKBAECAw0=" {
		t.Fatalf("expected signature 'EgYKBAECAw0=', got %q. Output: %s", thinkingBlock.Get("signature").String(), string(output))
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "visible answer" {
		t.Fatalf("unexpected text block: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaude_UsageWithCachedContentTokenCount(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	chunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "Hello world"}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 7,
			"cachedContentTokenCount": 91
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-usage-cache"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, chunk, &param), nil)
	outputText := string(output)

	if !strings.Contains(outputText, `"type":"message_delta"`) {
		t.Fatalf("expected message_delta event in output, got: %s", outputText)
	}

	foundMessageDelta := false
	// Find the message_delta event data
	for _, line := range strings.Split(outputText, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"message_delta"`) {
			foundMessageDelta = true
			deltaJSON := gjson.Parse(strings.TrimPrefix(line, "data: "))
			inputTokens := deltaJSON.Get("usage.input_tokens").Int()
			if inputTokens != 9 {
				t.Fatalf("expected usage.input_tokens = 9 (100 - 91), got %d. Payload: %s", inputTokens, line)
			}
			cacheReadTokens := deltaJSON.Get("usage.cache_read_input_tokens").Int()
			if cacheReadTokens != 91 {
				t.Fatalf("expected usage.cache_read_input_tokens = 91, got %d. Payload: %s", cacheReadTokens, line)
			}
			outputTokens := deltaJSON.Get("usage.output_tokens").Int()
			if outputTokens != 7 {
				t.Fatalf("expected usage.output_tokens = 7, got %d. Payload: %s", outputTokens, line)
			}
		}
	}
	if !foundMessageDelta {
		t.Fatalf("failed to locate parsed message_delta event in payload: %s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_UsageWithCachedContentTokenCount(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "Hello world"}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 7,
			"cachedContentTokenCount": 91
		},
		"modelVersion": "gemini-2.5-pro",
		"responseId": "resp-usage-cache-nonstream"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-2.5-pro", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	inputTokens := outputJSON.Get("usage.input_tokens").Int()
	if inputTokens != 9 {
		t.Fatalf("expected usage.input_tokens = 9 (100 - 91), got %d. Output: %s", inputTokens, string(output))
	}
	cacheReadTokens := outputJSON.Get("usage.cache_read_input_tokens").Int()
	if cacheReadTokens != 91 {
		t.Fatalf("expected usage.cache_read_input_tokens = 91, got %d. Output: %s", cacheReadTokens, string(output))
	}
	outputTokens := outputJSON.Get("usage.output_tokens").Int()
	if outputTokens != 7 {
		t.Fatalf("expected usage.output_tokens = 7, got %d. Output: %s", outputTokens, string(output))
	}
}

func TestConvertGeminiResponseToClaudeStream_PartlessSafetyClosesMessageWithRefusal(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	chunk := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"index": 0,
			"finishReason": "SAFETY"
		}],
		"modelVersion": "m",
		"usageMetadata": {
			"promptTokenCount": 120,
			"candidatesTokenCount": 37,
			"totalTokenCount": 157
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	lastIndex := -1
	for _, eventName := range []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"} {
		index := strings.Index(outputText, "event: "+eventName+"\n")
		if index < 0 {
			t.Fatalf("event %q not found in output:\n%s", eventName, outputText)
		}
		if index <= lastIndex {
			t.Fatalf("event %q is out of order in output:\n%s", eventName, outputText)
		}
		lastIndex = index
	}

	var foundDelta bool
	for _, line := range strings.Split(outputText, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"message_delta"`) {
			foundDelta = true
			deltaJSON := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if got := deltaJSON.Get("delta.stop_reason").String(); got != "refusal" {
				t.Fatalf("stop_reason = %q, want refusal. Payload: %s", got, line)
			}
			if got := deltaJSON.Get("usage.input_tokens").Int(); got != 120 {
				t.Fatalf("input_tokens = %d, want 120. Payload: %s", got, line)
			}
			if got := deltaJSON.Get("usage.output_tokens").Int(); got != 37 {
				t.Fatalf("output_tokens = %d, want 37. Payload: %s", got, line)
			}
		}
	}
	if !foundDelta {
		t.Fatalf("failed to find message_delta event in output:\n%s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_PartlessMalformedFunctionCallClosesMessage(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	chunk := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": []},
			"index": 0,
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m",
		"usageMetadata": {
			"promptTokenCount": 50,
			"candidatesTokenCount": 10,
			"totalTokenCount": 60
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if !strings.Contains(outputText, `"type":"message_delta"`) {
		t.Fatalf("expected message_delta in output:\n%s", outputText)
	}
	if !strings.Contains(outputText, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop in output:\n%s", outputText)
	}

	var foundDelta bool
	for _, line := range strings.Split(outputText, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"message_delta"`) {
			foundDelta = true
			deltaJSON := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if got := deltaJSON.Get("delta.stop_reason").String(); got != "refusal" {
				t.Fatalf("stop_reason = %q, want refusal. Payload: %s", got, line)
			}
		}
	}
	if !foundDelta {
		t.Fatalf("failed to find message_delta in output:\n%s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_PartlessStopClosesMessageWithEndTurn(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	chunk := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": ""}]},
			"index": 0,
			"finishReason": "STOP"
		}],
		"modelVersion": "m",
		"usageMetadata": {
			"promptTokenCount": 80,
			"candidatesTokenCount": 0,
			"totalTokenCount": 80
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, chunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "claude-opus-5-5", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	lastIndex := -1
	for _, eventName := range []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"} {
		index := strings.Index(outputText, "event: "+eventName+"\n")
		if index < 0 {
			t.Fatalf("event %q not found in output:\n%s", eventName, outputText)
		}
		if index <= lastIndex {
			t.Fatalf("event %q is out of order in output:\n%s", eventName, outputText)
		}
		lastIndex = index
	}

	var foundDelta bool
	for _, line := range strings.Split(outputText, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"message_delta"`) {
			foundDelta = true
			deltaJSON := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if got := deltaJSON.Get("delta.stop_reason").String(); got != "end_turn" {
				t.Fatalf("stop_reason = %q, want end_turn. Payload: %s", got, line)
			}
		}
	}
	if !foundDelta {
		t.Fatalf("failed to find message_delta in output:\n%s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_SafetyAndMalformedFunctionCallRefusal(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	testCases := []struct {
		finishReason string
		wantReason   string
	}{
		{finishReason: "SAFETY", wantReason: "refusal"},
		{finishReason: "MALFORMED_FUNCTION_CALL", wantReason: "refusal"},
		{finishReason: "RECITATION", wantReason: "refusal"},
		{finishReason: "PROHIBITED_CONTENT", wantReason: "refusal"},
		{finishReason: "SPII", wantReason: "refusal"},
		{finishReason: "BLOCKLIST", wantReason: "refusal"},
		{finishReason: "MAX_TOKENS", wantReason: "max_tokens"},
		{finishReason: "STOP", wantReason: "end_turn"},
	}

	ctx := context.Background()
	for _, tc := range testCases {
		t.Run(tc.finishReason, func(t *testing.T) {
			rawJSON := []byte(`{
				"candidates": [{
					"content": {"role": "model", "parts": []},
					"finishReason": "` + tc.finishReason + `"
				}],
				"usageMetadata": {
					"promptTokenCount": 120,
					"candidatesTokenCount": 37,
					"totalTokenCount": 157
				},
				"modelVersion": "m",
				"responseId": "resp-test"
			}`)
			output := convertGeminiResponseToClaudeNonStream(ctx, "claude-opus-5-5", requestJSON, requestJSON, rawJSON, nil)
			outputJSON := gjson.ParseBytes(output)
			if got := outputJSON.Get("stop_reason").String(); got != tc.wantReason {
				t.Fatalf("stop_reason = %q, want %q. Output: %s", got, tc.wantReason, string(output))
			}
		})
	}
}

func TestConvertGeminiResponseToClaudeNonStream_Issue6409_SignedVisibleText(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "ok", "thoughtSignature": "EgYKBAECAwE="}
				],
				"role": "model"
			},
			"finishReason": "STOP",
			"index": 0
		}],
		"usageMetadata": {
			"promptTokenCount": 5,
			"candidatesTokenCount": 1
		},
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "5Qm-as2qBuLI-sAP76KVkAk"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (carrier thinking + text), got %d: %s", len(blocks), string(output))
	}

	thinkingBlock := blocks[0]
	if thinkingBlock.Get("type").String() != "thinking" {
		t.Fatalf("expected block 0 to be thinking carrier, got %s", thinkingBlock.Get("type").String())
	}
	if got := thinkingBlock.Get("thinking").String(); got != "" {
		t.Fatalf("carrier thinking block must have empty thinking, got %q", got)
	}
	if got := thinkingBlock.Get("signature").String(); got != "EgYKBAECAwE=" {
		t.Fatalf("expected signature 'EgYKBAECAwE=', got %q", got)
	}

	textBlock := blocks[1]
	if textBlock.Get("type").String() != "text" || textBlock.Get("text").String() != "ok" {
		t.Fatalf("expected visible text block with 'ok', got: %s", textBlock.Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_Issue6409_ThinkingFollowedBySignedVisibleText(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "reasoning step", "thought": true, "thoughtSignature": "EgYKBAECAww="},
					{"text": "final answer", "thoughtSignature": "EgYKBAECAw4="}
				],
				"role": "model"
			},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-mixed"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (thinking, carrier thinking, text), got %d: %s", len(blocks), string(output))
	}

	if blocks[0].Get("type").String() != "thinking" || blocks[0].Get("thinking").String() != "reasoning step" || blocks[0].Get("signature").String() != "EgYKBAECAww=" {
		t.Fatalf("block 0 must be thinking with its own signature, got: %s", blocks[0].Raw)
	}
	if blocks[1].Get("type").String() != "thinking" || blocks[1].Get("thinking").String() != "" || blocks[1].Get("signature").String() != "EgYKBAECAw4=" {
		t.Fatalf("block 1 must be detached carrier thinking for visible text signature, got: %s", blocks[1].Raw)
	}
	if blocks[2].Get("type").String() != "text" || blocks[2].Get("text").String() != "final answer" {
		t.Fatalf("block 2 must be text block with visible answer, got: %s", blocks[2].Raw)
	}
}

func TestConvertGeminiResponseToClaudeNonStream_Issue6409_ConsecutiveSignedVisibleTexts(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	geminiResponse := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "part A", "thoughtSignature": "EgYKBAECAwI="},
					{"text": "part B", "thoughtSignature": "EgYKBAECAwM="}
				],
				"role": "model"
			},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-consecutive"
	}`)

	ctx := context.Background()
	output := convertGeminiResponseToClaudeNonStream(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, geminiResponse, nil)
	outputJSON := gjson.ParseBytes(output)

	blocks := outputJSON.Get("content").Array()
	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks (carrier A, text A, carrier B, text B), got %d: %s", len(blocks), string(output))
	}

	if blocks[0].Get("type").String() != "thinking" || blocks[0].Get("signature").String() != "EgYKBAECAwI=" {
		t.Fatalf("block 0 must be carrier for EgYKBAECAwI=, got: %s", blocks[0].Raw)
	}
	if blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != "part A" {
		t.Fatalf("block 1 must be text part A, got: %s", blocks[1].Raw)
	}
	if blocks[2].Get("type").String() != "thinking" || blocks[2].Get("signature").String() != "EgYKBAECAwM=" {
		t.Fatalf("block 2 must be carrier for EgYKBAECAwM=, got: %s", blocks[2].Raw)
	}
	if blocks[3].Get("type").String() != "text" || blocks[3].Get("text").String() != "part B" {
		t.Fatalf("block 3 must be text part B, got: %s", blocks[3].Raw)
	}
}

func TestConvertGeminiResponseToClaudeStream_Issue6409_SignedVisibleText(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	chunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "ok", "thoughtSignature": "EgYKBAECAwc="}
				],
				"role": "model"
			},
			"finishReason": "STOP",
			"index": 0
		}],
		"usageMetadata": {
			"promptTokenCount": 5,
			"candidatesTokenCount": 1
		},
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-s1"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, chunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if strings.Contains(outputText, `"thinking_delta"`) {
		t.Fatalf("signed visible text must not emit thinking_delta, got: %s", outputText)
	}

	// Verify block 0 is carrier thinking
	if !strings.Contains(outputText, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) {
		t.Fatalf("expected block 0 to be carrier thinking, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EgYKBAECAwc="}}`) {
		t.Fatalf("expected block 0 to have signature_delta, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_stop","index":0}`) {
		t.Fatalf("expected block 0 stop, got: %s", outputText)
	}

	// Verify block 1 is visible text
	if !strings.Contains(outputText, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) {
		t.Fatalf("expected block 1 to be text block, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`) {
		t.Fatalf("expected block 1 to have text_delta with 'ok', got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_stop","index":1}`) {
		t.Fatalf("expected block 1 stop, got: %s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_Issue6409_SplitTrailingSignature(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	textChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "ok"}]
			}
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-s2"
	}`)
	sigChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "", "thoughtSignature": "EgYKBAECAwk="}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 5,
			"candidatesTokenCount": 1
		},
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-s2"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, textChunk, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, sigChunk, &param), nil)...)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if strings.Contains(outputText, `"thinking_delta"`) {
		t.Fatalf("unexpected thinking_delta in stream: %s", outputText)
	}

	// Block 0: text block with "ok"
	if !strings.Contains(outputText, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) {
		t.Fatalf("expected block 0 text start, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`) {
		t.Fatalf("expected block 0 text delta, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_stop","index":0}`) {
		t.Fatalf("expected block 0 text stop, got: %s", outputText)
	}

	// Block 1: trailing carrier thinking block
	if !strings.Contains(outputText, `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`) {
		t.Fatalf("expected block 1 trailing carrier thinking, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"EgYKBAECAwk="}}`) {
		t.Fatalf("expected block 1 signature_delta, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_stop","index":1}`) {
		t.Fatalf("expected block 1 stop, got: %s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_Issue6409_FunctionCallContinuationSignature(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	chunk1 := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"functionCall": {"name": "test_tool", "args": {}}}
				]
			}
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-fc-cont"
	}`)
	chunk2 := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"thoughtSignature": "EgYKBAECAwQ=", "functionCall": {"args": {"key":"val"}}}
				]
			},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-fc-cont"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, chunk1, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, chunk2, &param), nil)...)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	if !strings.Contains(outputText, `"type":"input_json_delta"`) {
		t.Fatalf("expected input_json_delta, got: %s", outputText)
	}
	if !strings.Contains(outputText, `"signature":"EgYKBAECAwQ="`) {
		t.Fatalf("expected signature_delta with 'EgYKBAECAwQ=', got: %s", outputText)
	}
}

func TestConvertGeminiResponseToClaudeStream_Issue6409_ThinkingFollowedBySignedVisibleText(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.5-flash-lite","messages":[{"role":"user","content":"hi"}]}`)
	chunk1 := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "thinking step", "thought": true, "thoughtSignature": "EgYKBAECAwg="}
				]
			}
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-mixed-stream"
	}`)
	chunk2 := []byte(`{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "visible text", "thoughtSignature": "EgYKBAECAwo="}
				]
			},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-3.5-flash-lite",
		"responseId": "resp-mixed-stream"
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, chunk1, &param), nil)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, chunk2, &param), nil)...)
	output = append(output, bytes.Join(convertGeminiResponseToClaude(ctx, "gemini-3.5-flash-lite", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)...)
	outputText := string(output)

	// Block 0: thinking with "EgYKBAECAwg="
	if !strings.Contains(outputText, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) {
		t.Fatalf("expected block 0 thinking start, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EgYKBAECAwg="}}`) {
		t.Fatalf("expected block 0 thinking signature, got: %s", outputText)
	}

	// Block 1: detached carrier thinking for visible text signature "EgYKBAECAwo="
	if !strings.Contains(outputText, `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`) {
		t.Fatalf("expected block 1 detached carrier thinking start, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"EgYKBAECAwo="}}`) {
		t.Fatalf("expected block 1 signature_delta 'EgYKBAECAwo=', got: %s", outputText)
	}

	// Block 2: text block with "visible text"
	if !strings.Contains(outputText, `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`) {
		t.Fatalf("expected block 2 text start, got: %s", outputText)
	}
	if !strings.Contains(outputText, `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"visible text"}}`) {
		t.Fatalf("expected block 2 text_delta, got: %s", outputText)
	}
}
