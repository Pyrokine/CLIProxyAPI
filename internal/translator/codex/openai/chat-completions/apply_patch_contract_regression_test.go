package chat_completions_test

import (
	"context"
	"encoding/json"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexChatSDKApplyPatchJSONRoundTrip(t *testing.T) {
	original := []byte(
		`{"tools":[{"type":"custom","name":"apply_patch"}],"messages":[{"role":"user","content":"patch"}]}`,
	)
	patch := "*** Begin Patch\n+中文 \"value\" \\path\n*** End Patch"
	item := []byte(`{"id":"item-a","type":"custom_tool_call","call_id":"call-a","name":"apply_patch","input":""}`)
	item, _ = sjson.SetBytes(item, "input", patch)
	completed := []byte(
		`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	completed, _ = sjson.SetRawBytes(completed, "response.output.-1", item)
	assertArguments := func(args string) {
		t.Helper()
		if !json.Valid([]byte(args)) || gjson.Get(args, "input").String() != patch {
			t.Fatalf("invalid JSON envelope: %q", args)
		}
		request, _ := sjson.SetBytes(
			original, "messages.-1", map[string]any{
				"role": "assistant", "tool_calls": []any{
					map[string]any{
						"id": "call-a", "type": "function",
						"function": map[string]any{"name": "apply_patch", "arguments": args},
					},
				},
			},
		)
		request, _ = sjson.SetBytes(
			request, "messages.-1", map[string]any{"role": "tool", "tool_call_id": "call-a", "content": "done"},
		)
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAI, sdk.FormatCodex, "codex-test", request, false,
		)
		if err != nil || gjson.GetBytes(out, "input.1.input").String() != patch || gjson.GetBytes(
			out, "input.1.type",
		).String() != "custom_tool_call" {
			t.Fatalf("patch round-trip changed: error=%v body=%s", err, out)
		}
	}
	var state any
	nonstream := sdk.TranslateNonStream(
		context.Background(), sdk.FormatCodex, sdk.FormatOpenAI, "codex-test", original, nil, completed, &state,
	)
	assertArguments(gjson.GetBytes(nonstream, "choices.0.message.tool_calls.0.function.arguments").String())
	added := `{"type":"response.output_item.added","output_index":0,"item":{"id":"it` +
		`em-a","type":"custom_tool_call","call_id":"call-a","name":"apply_patch` +
		`","input":""}}`
	delta := []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"item-a","delta":""}`)
	delta, _ = sjson.SetBytes(delta, "delta", patch)
	done := []byte(`{"type":"response.custom_tool_call_input.done","item_id":"item-a","input":""}`)
	done, _ = sjson.SetBytes(done, "input", patch)
	itemDone := `{"type":"response.output_item.done","output_index":0,"item":` + string(item) + `}`
	for _, events := range [][]string{
		{added, string(delta), string(done), itemDone, string(completed)}, {itemDone, string(completed)},
		{string(completed)},
	} {
		state = nil
		args, identities, terminals := "", 0, 0
		for _, event := range events {
			for _, out := range sdk.Default().TranslateStream(
				context.Background(), sdk.FormatCodex, sdk.FormatOpenAI, "codex-test", original, nil,
				[]byte("data: "+event), &state,
			) {
				for _, call := range gjson.GetBytes(out, "choices.0.delta.tool_calls").Array() {
					args += call.Get("function.arguments").String()
					if call.Get("id").String() != "" {
						identities++
					}
				}
				if gjson.GetBytes(out, "choices.0.finish_reason").String() != "" {
					terminals++
				}
			}
		}
		assertArguments(args)
		if identities != 1 || terminals != 1 {
			t.Fatalf("identity=%d terminal=%d", identities, terminals)
		}
	}
}
