package chat_completions_test

import (
	"context"
	"fmt"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiChatSDKRequestContracts(t *testing.T) {
	for _, role := range []string{"user", "assistant"} {
		body := []byte(fmt.Sprintf(
			`{"messages":[{"role":%q,"content":[{"type":"image_url","image_url":{"u`+
				`rl":"https://example.invalid/picture.png;base64,abc"}}]}]}`,
			role,
		))
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAI, sdk.FormatGemini, "gemini-test", body, false,
		)
		if scoped, ok := err.(interface{ StatusCode() int }); !ok || scoped.StatusCode() != 400 ||
			len(out) != 0 {
			t.Fatalf("%s remote image accepted: error=%v body=%s", role, err, out)
		}
	}
	body := []byte((`{"messages":[{"role":"user","content":"call"}],"tools":[{"type":"function","function":{"` +
		`name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"required","parallel_tool_calls":` +
		`false}`))
	out, err := sdk.TranslateRequestChecked(
		context.Background(), sdk.FormatOpenAI, sdk.FormatGemini, "gemini-test", body, false,
	)
	if scoped, ok := err.(interface{ StatusCode() int }); !ok || scoped.StatusCode() != 400 || len(out) != 0 {
		t.Fatalf("required tools disabled instead of rejected: error=%v body=%s", err, out)
	}
	//goland:noinspection SpellCheckingInspection
	//goland:noinspection SpellCheckingInspection
	for _, carrier := range []string{
		`"extra_content":{"google":{"thought_signature":"CgQBBQYH"}}`, `"thought_signature":"CgQBBQYH"`,
	} {
		body = []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call-a","type":"` +
			`function","function":{"name":"lookup","arguments":"{}"},` +
			carrier +
			`}]},{"role":"tool","tool_call_id":"call-a","content":"result"}]}`)
		out, err = sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAI, sdk.FormatGemini, "gemini-test", body, false,
		)
		//goland:noinspection SpellCheckingInspection
		if err != nil || gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String() != "CgQBBQYH" {
			t.Fatalf("native function signature lost: error=%v body=%s", err, out)
		}
	}
}

func TestGeminiChatSDKToolNamesAndCurrentFrameTerminal(t *testing.T) {
	original := []byte(
		`{"tools":[{"type":"function","function":{"name":"send/message","parameters":{"type":"object"}}}]}`,
	)
	response := []byte((`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"send_message",` +
		`"args":{"value":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":3}}`))
	var state any
	out := sdk.TranslateNonStream(
		context.Background(),
		sdk.FormatGemini,
		sdk.FormatOpenAI,
		"gemini-test",
		original,
		nil,
		response,
		&state,
	)
	if gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String() != "send/message" {
		t.Fatalf("nonstream original name lost: %s", out)
	}
	state = nil
	frames := sdk.Default().TranslateStream(
		context.Background(),
		sdk.FormatGemini, sdk.FormatOpenAI, "gemini-test", original, nil, response, &state,
	)
	if len(frames) != 1 || gjson.GetBytes(
		frames[0], "choices.0.delta.tool_calls.0.function.name",
	).String() != "send/message" {
		t.Fatalf("stream original name lost: %s", frames)
	}
	state = nil
	for i, frame := range []string{
		`{"candidates":[{"index":0,"content":{"parts":[{"text":"first"}]}}],"usageMetadata":{"totalTokenCount":2}}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"}]}`,
		`{"usageMetadata":{"totalTokenCount":3}}`,
		`{"usageMetadata":{"totalTokenCount":3}}`,
	} {
		frames = sdk.Default().TranslateStream(
			context.Background(),
			sdk.FormatGemini, sdk.FormatOpenAI, "gemini-test", nil, nil, []byte(frame), &state,
		)
		if len(frames) != 1 {
			t.Fatalf("frame %d produced %s", i, frames)
		}
		reason := gjson.GetBytes(frames[0], "choices.0.finish_reason")
		if i == 2 && reason.String() != "stop" || i != 2 && reason.Type != gjson.Null {
			t.Fatalf("frame %d terminal=%s: %s", i, reason.Raw, frames[0])
		}
	}
}
