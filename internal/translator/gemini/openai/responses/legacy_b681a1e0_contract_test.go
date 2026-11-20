package responses

import (
	"strings"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGemini_MidSessionDeveloperMessageDoesNotMutateSystemInstruction(
	t *testing.T,
) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Turn 1 user"}
				]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "output_text", "text": "Turn 1 assistant"}
				]
			},
			{
				"type": "message",
				"role": "developer",
				"content": "<image_resize_notice>Image 1 was resized to 800x600</image_resize_notice>"
			},
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Turn 2 user"}
				]
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	// systemInstruction must remain strictly unchanged (only original instructions, not developer notice)
	systemInstruction := result.Get("systemInstruction")
	if !systemInstruction.Exists() {
		t.Fatalf("systemInstruction missing; output=%s", output)
	}
	parts := systemInstruction.Get("parts").Array()
	if len(parts) != 1 {
		t.Fatalf("systemInstruction parts count = %d, want 1; output=%s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "Be a helpful assistant" {
		t.Fatalf("systemInstruction part = %q, want %q; output=%s", got, "Be a helpful assistant", output)
	}

	// contents should contain user, model, user (with merged developer notice + turn 2 user text)
	contents := result.Get("contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), output)
	}
	if contents[0].Get("role").String() != "user" || contents[0].Get("parts.0.text").String() != "Turn 1 user" {
		t.Fatalf("turn 1 user content malformed; output=%s", output)
	}
	if contents[1].Get("role").String() != "model" ||
		contents[1].Get("parts.0.text").String() != "Turn 1 assistant" {
		t.Fatalf("turn 1 model content malformed; output=%s", output)
	}
	if contents[2].Get("role").String() != "user" {
		t.Fatalf(
			"turn 2 user content role = %q, want user; output=%s",
			contents[2].Get("role").String(),
			output,
		)
	}
	turn2Parts := contents[2].Get("parts").Array()
	if len(turn2Parts) != 2 {
		t.Fatalf("turn 2 parts count = %d, want 2; output=%s", len(turn2Parts), output)
	}
	expectedDevText := "<system-reminder>\n<image_resize_notice>Image 1 was resized to " +
		"800x600</image_resize_notice>\n</" +
		"system-reminder>"
	if got := turn2Parts[0].Get("text").String(); got != expectedDevText {
		t.Fatalf("turn 2 part 0 = %q, want %q; output=%s", got, expectedDevText, output)
	}
	if got := turn2Parts[1].Get("text").String(); got != "Turn 2 user" {
		t.Fatalf("turn 2 part 1 = %q, want Turn 2 user; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MidSessionSystemReminderEnvelope(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Turn 1 user"}
				]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "output_text", "text": "Turn 1 assistant"}
				]
			},
			{
				"type": "message",
				"role": "developer",
				"content": "Please decide which tool to call next."
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	contents := result.Get("contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), output)
	}
	expectedReminder := "<system-reminder>\nPlease decide which tool to call next.\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expectedReminder {
		t.Fatalf("mid-session system reminder mismatch:\ngot:  %q\nwant: %q", got, expectedReminder)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MidSessionDeveloperMultiPartContentWrappedOnce(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": "Turn 1"
			},
			{
				"type": "message",
				"role": "assistant",
				"content": "Reply 1"
			},
			{
				"type": "message",
				"role": "developer",
				"content": [
					{"type": "input_text", "text": "Rule line 1"},
					{"type": "input_text", "text": "Rule line 2"}
				]
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	contents := result.Get("contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), output)
	}
	expected := "<system-reminder>\nRule line 1\nRule line 2\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expected {
		t.Fatalf("multi-part developer reminder mismatch:\ngot:  %q\nwant: %q", got, expected)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MultipleMidSessionDeveloperMessagesArrayContent(t *testing.T) {
	inputJSON := "{\n\t\t\"model\": \"gemini-3.5-flash\",\n\t\t\"instructions\": \"Be a helpful assistant\"," +
		"\n\t\t\"input\": [\n\t\t\t{\n\t\t\t\t\"type\": \"message\",\n\t\t\t\t\"role\": \"user\",\n\t\t\t\t\"" +
		"content\": [\n\t\t\t\t\t{\"type\": \"input_text\", \"text\": \"Turn 1\"}\n\t\t\t\t]\n\t\t\t},\n\t\t" +
		"\t{\n\t\t\t\t\"type\": \"message\",\n\t\t\t\t\"role\": \"assistant\",\n\t\t\t\t\"content\": [\n\t\t" +
		"\t\t\t{\"type\": \"output_text\", \"text\": \"Reply 1\"}\n\t\t\t\t]\n\t\t\t},\n\t\t\t{\n\t\t\t\t\"" +
		"type\": \"message\",\n\t\t\t\t\"role\": \"developer\",\n\t\t\t\t\"content\": [\n\t\t\t\t\t{\"type\":" +
		" \"input_text\", \"text\": \"<permissions instructions>\\nApproved: git\\n</permissions " +
		"instructions>\"}\n\t\t\t\t]\n\t\t\t},\n\t\t\t{\n\t\t\t\t\"type\": \"message\",\n\t\t\t\t\"role\": \"" +
		"developer\",\n\t\t\t\t\"content\": [\n\t\t\t\t\t{\"type\": \"input_text\", \"text\": \"<" +
		"collaboration_mode>\\nPlan\\n</collaboration_mode>\"}\n\t\t\t\t]\n\t\t\t},\n\t\t\t{\n\t\t\t\t\"" +
		"type\": \"message\",\n\t\t\t\t\"role\": \"user\",\n\t\t\t\t\"content\": [\n\t\t\t\t\t{\"type\": \"" +
		"input_text\", \"text\": \"Proceed\"}\n\t\t\t\t]\n\t\t\t}\n\t\t]\n\t}"

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	// systemInstruction only contains original instructions
	parts := result.Get("systemInstruction.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "Be a helpful assistant" {
		t.Fatalf("systemInstruction corrupted: %s", output)
	}

	// All mid-session developer messages coalesced into the final user turn
	contents := result.Get("contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), output)
	}
	turn2Parts := contents[2].Get("parts").Array()
	if len(turn2Parts) != 3 {
		t.Fatalf("turn 2 parts count = %d, want 3; output=%s", len(turn2Parts), output)
	}
	if !strings.Contains(turn2Parts[0].Get("text").String(), "permissions instructions") {
		t.Fatalf("part 0 mismatch; output=%s", output)
	}
	if !strings.Contains(turn2Parts[1].Get("text").String(), "collaboration_mode") {
		t.Fatalf("part 1 mismatch; output=%s", output)
	}
	if turn2Parts[2].Get("text").String() != "Proceed" {
		t.Fatalf("part 2 mismatch; output=%s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_InterveningDeveloperMessagePreservesToolPairing(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Run tool"}
				]
			},
			{
				"type": "function_call",
				"call_id": "call-1",
				"name": "run_command",
				"arguments": "{\"command\":\"echo test\"}"
			},
			{
				"type": "message",
				"role": "developer",
				"content": "<permissions instructions>\nApproved: echo\n</permissions instructions>"
			},
			{
				"type": "function_call_output",
				"call_id": "call-1",
				"output": "test"
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	// Validate function call pairing passes strictly (no content turn before pending functionResponse)
	if errPair := internalsignature.ValidateGeminiFunctionCallPairing(output); errPair != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPair, output)
	}

	// systemInstruction only contains original instructions
	parts := result.Get("systemInstruction.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "Be a helpful assistant" {
		t.Fatalf("systemInstruction corrupted: %s", output)
	}

	// Function response should have matching call id and name
	foundFR := false
	for _, content := range result.Get("contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionResponse.name").String() == "run_command" {
				foundFR = true
			}
		}
	}
	if !foundFR {
		t.Fatalf("functionResponse run_command not found or lost pairing: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_InterveningDeveloperAndUserMessageFlushesInOrder(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "function_call",
				"call_id": "call-1",
				"name": "run_command",
				"arguments": "{\"command\":\"test\"}"
			},
			{
				"type": "message",
				"role": "developer",
				"content": "<permissions instructions>\nApproved: test\n</permissions instructions>"
			},
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Wait, also check this"}
				]
			},
			{
				"type": "function_call_output",
				"call_id": "call-1",
				"output": "done"
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)

	// Pairing should be valid
	if errPair := internalsignature.ValidateGeminiFunctionCallPairing(output); errPair != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v; output=%s", errPair, output)
	}

	contents := result.Get("contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), output)
	}
	if contents[0].Get("role").String() != "model" {
		t.Fatalf("turn 0 role = %q, want model", contents[0].Get("role").String())
	}
	midParts := contents[2].Get("parts").Array()
	if len(midParts) != 2 {
		t.Fatalf("turn 2 parts count = %d, want 2; output=%s", len(midParts), output)
	}
	if !strings.Contains(midParts[0].Get("text").String(), "permissions instructions") {
		t.Fatalf("turn 2 part 0 should be developer notice; got %s", midParts[0].Raw)
	}
	if midParts[1].Get("text").String() != "Wait, also check this" {
		t.Fatalf("turn 2 part 1 should be user text; got %s", midParts[1].Raw)
	}
	if !contents[1].Get("parts.0.functionResponse").Exists() {
		t.Fatalf("turn 1 should be functionResponse; got %s", contents[1].Raw)
	}
}
