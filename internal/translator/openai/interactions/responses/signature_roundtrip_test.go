package responses

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestInteractionsResponsesPreservesProviderSignaturesNonStream(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		model     string
		signature string
	}{
		{name: "claude", model: "claude-sonnet-4", signature: responsesClaudeSignatureForTest()},
		{name: "gemini", model: "gemini-2.5-pro", signature: responsesGeminiSignatureForTest()},
		{name: "gpt", model: "gpt-5", signature: responsesGPTSignatureForTest()},
		{name: "detected provider", model: "custom-route", signature: responsesGPTSignatureForTest()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			interaction := []byte(`{"id":"interaction-1","model":"` + testCase.model + `","steps":[{"type":"thought","content":[],"signature":"` + testCase.signature + `"}]}`)
			response := convertInteractionsResponseToOpenAIResponsesNonStream(
				context.Background(), testCase.model, nil, nil, interaction, nil,
			)
			if got := gjson.GetBytes(response, "output.0.encrypted_content").String(); got != testCase.signature {
				t.Fatalf("Interactions -> Responses signature = %q, want %q; output=%s", got, testCase.signature, response)
			}

			responsesPayload := []byte(`{"id":"response-1","model":"` + testCase.model + `","output":[{"type":"reasoning","encrypted_content":"` + testCase.signature + `","summary":[]}]}`)
			roundTrip := convertOpenAIResponsesResponseToInteractionsNonStream(
				context.Background(), testCase.model, nil, nil, responsesPayload, nil,
			)
			if got := gjson.GetBytes(roundTrip, "steps.0.signature").String(); got != testCase.signature {
				t.Fatalf("Responses -> Interactions signature = %q, want %q; output=%s", got, testCase.signature, roundTrip)
			}
			if got := gjson.GetBytes(roundTrip, "steps.#").Int(); got != 1 {
				t.Fatalf("signature-only reasoning step count = %d, want 1; output=%s", got, roundTrip)
			}
		})
	}
}

func TestInteractionsResponsesRejectsInvalidSignaturesNonStream(t *testing.T) {
	validGPT := responsesGPTSignatureForTest()
	for _, testCase := range []struct {
		name      string
		model     string
		signature string
	}{
		{name: "foreign", model: "gpt-5", signature: responsesClaudeSignatureForTest()},
		{name: "invalid known prefix", model: "gpt-5", signature: "gpt#invalid"},
		{name: "unknown opaque", model: "custom-route", signature: "opaque-signature"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			interaction := []byte(`{"id":"interaction-1","model":"` + testCase.model + `","steps":[{"type":"thought","content":[{"type":"text","text":"kept"}],"signature":"` + testCase.signature + `"}]}`)
			response := convertInteractionsResponseToOpenAIResponsesNonStream(
				context.Background(), testCase.model, nil, nil, interaction, nil,
			)
			if got := gjson.GetBytes(response, "output.0.encrypted_content"); got.Exists() {
				t.Fatalf("invalid signature survived Interactions -> Responses: %s", response)
			}
			if got := gjson.GetBytes(response, "output.0.summary.0.text").String(); got != "kept" {
				t.Fatalf("reasoning text = %q, want kept; output=%s", got, response)
			}

			responsesPayload := []byte(`{"id":"response-1","model":"` + testCase.model + `","output":[{"type":"reasoning","encrypted_content":"` + testCase.signature + `","summary":[]}]}`)
			roundTrip := convertOpenAIResponsesResponseToInteractionsNonStream(
				context.Background(), testCase.model, nil, nil, responsesPayload, nil,
			)
			if got := gjson.GetBytes(roundTrip, "steps.#").Int(); got != 0 {
				t.Fatalf("invalid signature-only reasoning produced %d steps; output=%s", got, roundTrip)
			}
		})
	}

	interaction := []byte(`{"id":"interaction-1","model":"gpt-5","steps":[{"type":"thought","content":[],"encrypted_content":"gpt#invalid","signature":"` + validGPT + `"}]}`)
	response := convertInteractionsResponseToOpenAIResponsesNonStream(
		context.Background(), "gpt-5", nil, nil, interaction, nil,
	)
	if got := gjson.GetBytes(response, "output.0.encrypted_content").String(); got != validGPT {
		t.Fatalf("later valid signature = %q, want %q; output=%s", got, validGPT, response)
	}
}

func TestInteractionsResponsesStreamValidatesLateSignature(t *testing.T) {
	validGPT := responsesGPTSignatureForTest()
	var state any
	convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"interaction.created","interaction":{"id":"interaction-1","model":"gpt-5"}}`),
		&state,
	)
	start := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"thought","signature":"gpt#invalid"}}`),
		&state,
	)
	if got := responseEventPayload(start[0]).Get("item.encrypted_content").String(); got != "" {
		t.Fatalf("invalid start signature = %q, want empty", got)
	}
	convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"`+responsesClaudeSignatureForTest()+`"}}`),
		&state,
	)
	convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"`+validGPT+`"}}`),
		&state,
	)
	stop := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.stop","index":0}`),
		&state,
	)
	if got := responseEventPayload(stop[0]).Get("item.encrypted_content").String(); got != validGPT {
		t.Fatalf("done signature = %q, want %q; events=%q", got, validGPT, stop)
	}
	terminal := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-1","model":"gpt-5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`),
		&state,
	)
	payload := responseEventPayload(terminal[0])
	if got := payload.Get("type").String(); got != "response.incomplete" {
		t.Fatalf("terminal type = %q, want response.incomplete", got)
	}
	if got := payload.Get("response.output.0.encrypted_content").String(); got != validGPT {
		t.Fatalf("completed signature = %q, want %q; payload=%s", got, validGPT, payload.Raw)
	}
}

func TestResponsesInteractionsStreamEmitsLateSignatureDelta(t *testing.T) {
	validGPT := responsesGPTSignatureForTest()
	var state any
	convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"type":"response.created","response":{"id":"response-1","model":"gpt-5","status":"in_progress","output":[]}}`),
		&state,
	)
	convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking"}`),
		&state,
	)
	events := convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"custom-route",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"`+validGPT+`","summary":[]}}`),
		&state,
	)
	var signature string
	for _, event := range events {
		payload := responseEventPayload(event)
		if payload.Get("delta.type").String() == "thought_signature" {
			signature = payload.Get("delta.signature").String()
		}
	}
	if signature != validGPT {
		t.Fatalf("late signature delta = %q, want %q; events=%q", signature, validGPT, events)
	}
}

func responsesGPTSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}

func responsesClaudeSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func responsesGeminiSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}
