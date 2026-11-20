package responses

import (
	"testing"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToAntigravity_MidSessionDeveloperMessageDoesNotMutateSystemInstruction(
	t *testing.T,
) {
	inputJSON := `{
		"model": "gemini-3-flash",
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

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	if !gjson.ValidBytes(out) {
		t.Fatalf("invalid JSON output: %s", out)
	}

	// In Antigravity envelope, systemInstruction is at request.systemInstruction
	sysParts := gjson.GetBytes(out, "request.systemInstruction.parts").Array()
	if len(sysParts) != 1 {
		t.Fatalf("request.systemInstruction parts count = %d, want 1; output=%s", len(sysParts), out)
	}
	if got := sysParts[0].Get("text").String(); got != "Be a helpful assistant" {
		t.Fatalf("systemInstruction part = %q, want %q; output=%s", got, "Be a helpful assistant", out)
	}

	contents := gjson.GetBytes(out, "request.contents").Array()
	if len(contents) != 3 {
		t.Fatalf("request.contents count = %d, want 3; output=%s", len(contents), out)
	}
	if contents[2].Get("role").String() != "user" {
		t.Fatalf("turn 2 role = %q, want user; output=%s", contents[2].Get("role").String(), out)
	}
	turn2Parts := contents[2].Get("parts").Array()
	if len(turn2Parts) != 2 {
		t.Fatalf("turn 2 parts count = %d, want 2; output=%s", len(turn2Parts), out)
	}
	expectedDevText := "<system-reminder>\n<image_resize_notice>Image 1 was resized to " +
		"800x600</image_resize_notice>\n</" +
		"system-reminder>"
	if got := turn2Parts[0].Get("text").String(); got != expectedDevText {
		t.Fatalf("turn 2 part 0 = %q, want %q; output=%s", got, expectedDevText, out)
	}
	if got := turn2Parts[1].Get("text").String(); got != "Turn 2 user" {
		t.Fatalf("turn 2 part 1 = %q, want Turn 2 user; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravity_MidSessionSystemReminderEnvelope(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
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
				"role": "system",
				"content": "Please decide which tool to call next."
			}
		]
	}`

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	result := gjson.ParseBytes(out)

	contents := result.Get("request.contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), out)
	}
	expectedReminder := "<system-reminder>\nPlease decide which tool to call next.\n</system-reminder>"
	if got := contents[2].Get("parts.0.text").String(); got != expectedReminder {
		t.Fatalf("mid-session system reminder mismatch:\ngot:  %q\nwant: %q", got, expectedReminder)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravity_InterveningDeveloperMessagePreservesToolPairing(
	t *testing.T,
) {
	inputJSON := `{
		"model": "gemini-3-flash",
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

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	if !gjson.ValidBytes(out) {
		t.Fatalf("invalid JSON output: %s", out)
	}

	rawRequest := gjson.GetBytes(out, "request").Raw
	if errPair := sigcompat.ValidateGeminiFunctionCallPairing([]byte(rawRequest)); errPair != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed on Antigravity request: %v; output=%s", errPair, out)
	}
}
