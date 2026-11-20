package chat_completions_test

import (
	"context"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAIChatSDKIgnoresPayloadAfterDone(t *testing.T) {
	first := []byte(`data: {"choices":[{"index":0,"delta":{"content":"first"}}]}`)
	var state any
	frames := sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAI, "fixture", nil, nil, first, &state,
	)
	if len(frames) != 1 || gjson.GetBytes(frames[0], "choices.0.delta.content").String() != "first" {
		t.Fatalf("initial content changed: %q", frames)
	}
	for _, chunk := range [][]byte{
		[]byte("data: [DONE]"), []byte(`data: {"choices":[{"delta":{"content":"late"}}]}`), []byte("[DONE]"),
	} {
		frames = sdk.Default().TranslateStream(
			context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAI, "fixture", nil, nil, chunk, &state,
		)
		if len(frames) != 0 {
			t.Fatalf("terminal or late content escaped: %q", frames)
		}
	}
	var nextRequest any
	frames = sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAI, "fixture", nil, nil, first, &nextRequest,
	)
	if len(frames) != 1 {
		t.Fatalf("terminal state affected a different request: %q", frames)
	}
}
