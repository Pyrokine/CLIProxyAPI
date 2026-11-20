package responses_test

import (
	"context"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/interactions/chat-completions"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsChatSDKFailureAndLateEvents(t *testing.T) {
	for _, body := range []string{
		`{"status":"failed","error":{"message":"synthetic failure","code":"429"}}`,
		`{"interaction":{"status":"failed","steps":[{"type":"model_output","content":[{"type":"text","text":"` +
			`partial"}]}],"error":{"message":"synthetic failure","code":"429"}}}`,
	} {
		var state any
		out := sdk.TranslateNonStream(
			context.Background(),
			sdk.FormatInteractions,
			sdk.FormatOpenAI,
			"fixture",
			nil,
			nil,
			[]byte(body),
			&state,
		)
		if gjson.GetBytes(out, "error.message").String() != "synthetic failure" || gjson.GetBytes(
			out, "choices",
		).Exists() {
			t.Fatalf("failure became success: %s", out)
		}
	}
	var state any
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatInteractions, "fixture", nil, nil,
		[]byte(`{"choices":[{"delta":{"content":"first"}}]}`), &state,
	)
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatInteractions, "fixture", nil, nil,
		[]byte(`{"choices":[],"usage":{"total_tokens":1}}`), &state,
	)
	late := []byte(`{"choices":[{"delta":{"content":"late"}}]}`)
	if frames := sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatInteractions, "fixture", nil, nil, late, &state,
	); len(frames) != 0 {
		t.Fatalf("content after completed emitted: %s", frames)
	}
	sdk.Default().TranslateStream(
		context.Background(),
		sdk.FormatOpenAI, sdk.FormatInteractions, "fixture", nil, nil, []byte(`[DONE]`), &state,
	)
	if frames := sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatInteractions, "fixture", nil, nil, late, &state,
	); len(frames) != 0 {
		t.Fatalf("content after done emitted: %s", frames)
	}
}

func TestChatResponsesSDKZeroReasoningDetails(t *testing.T) {
	for _, detail := range []string{
		``, ` ,"output_tokens_details":{"reasoning_tokens":0}`, ` ,"completion_tokens_details":{"reasoning_tokens":4}`,
	} {
		want := int64(0)
		if strings.Contains(detail, `:4`) {
			want = 4
		}
		usage := `{"prompt_tokens":2,"completion_tokens":5,"total_tokens":7` + detail + `}`
		var state any
		out := sdk.TranslateNonStream(
			context.Background(),
			sdk.FormatOpenAI,
			sdk.FormatOpenAIResponse,
			"fixture",
			nil,
			nil,
			[]byte(
				`{"choices":[{"message":{"role":"assistant","content":"answer"},`+
					`"finish_reason":"stop"}],"usage":`+
					usage+
					`}`,
			),
			&state,
		)
		value := gjson.GetBytes(out, "usage.output_tokens_details.reasoning_tokens")
		if !value.Exists() || value.Int() != want {
			t.Fatalf("nonstream details changed: %s", out)
		}
		state = nil
		sdk.Default().TranslateStream(
			context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", nil, nil,
			[]byte(
				`{"id":"fixture","choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":`+usage+`}`,
			),
			&state,
		)
		frames := sdk.Default().TranslateStream(
			context.Background(),
			sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", nil, nil, []byte(`[DONE]`),
			&state,
		)
		found := false
		for _, frame := range frames {
			for line := range strings.SplitSeq(string(frame), "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok || gjson.Get(data, "type").String() != "response.completed" {
					continue
				}
				found = true
				value = gjson.Get(data, "response.usage.output_tokens_details.reasoning_tokens")
				if !value.Exists() || value.Int() != want {
					t.Fatalf("stream details changed: %s", data)
				}
			}
		}
		if !found {
			t.Fatal("missing stream terminal")
		}
	}
}
