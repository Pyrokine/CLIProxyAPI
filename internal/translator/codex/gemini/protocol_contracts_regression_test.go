package gemini_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/gemini"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexGeminiSDKToolChoiceAndCallIdentity(t *testing.T) {
	prefix := strings.Repeat("x", 70)
	body := []byte(fmt.Sprintf(
		`{"tools":[{"functionDeclarations":[{"name":%q,"parameters":{"type":"object"}},{"name":%q,"`+
			`parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","`+
			`allowedFunctionNames":[%q]}},"contents":[{"role":"user","parts":[{"text":"call"}]}]}`,
		prefix+"a", prefix+"b", prefix+"b",
	))
	out, err := sdk.TranslateRequestChecked(
		context.Background(), sdk.FormatGemini, sdk.FormatCodex, "codex-test", body, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if name := gjson.GetBytes(out, "tools.1.name").String(); name == "" || name == gjson.GetBytes(
		out, "tools.0.name",
	).String() || name != gjson.GetBytes(out, "tool_choice.name").String() {
		t.Fatalf("tool choice and declaration diverged: %s", out)
	}
	item := `{"type":"function_call","id":"item-a","call_id":"call-original","name"` +
		`:"lookup","arguments":"{\"n\":9007199254740993}"}`
	completed := []byte(
		`{"type":"response.completed","response":{"id":"response-a","output":[` +
			item +
			`],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	var state any
	out = sdk.TranslateNonStream(
		context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil, completed, &state,
	)
	if gjson.GetBytes(
		out, "candidates.0.content.parts.0.functionCall.id",
	).String() != "call-original" || gjson.GetBytes(
		out, "candidates.0.content.parts.0.functionCall.args.n",
	).Raw != "9007199254740993" {
		t.Fatalf("nonstream call identity or arguments lost: %s", out)
	}
	state = nil
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil,
		[]byte(`data: {"type":"response.output_item.done","item":`+item+`}`), &state,
	)
	frames := sdk.Default().TranslateStream(
		context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil,
		append([]byte("data: "), completed...), &state,
	)
	if len(frames) < 1 || gjson.GetBytes(
		frames[0], "candidates.0.content.parts.0.functionCall.id",
	).String() != "call-original" {
		t.Fatalf("stream call identity lost: %s", frames)
	}
}

func TestCodexGeminiSDKGeneratedImages(t *testing.T) {
	item := `{"type":"image_generation_call","id":"image-a","result":"aGVsbG8=","output_format":"jpg"}`
	completed := []byte(
		`{"type":"response.completed","response":{"output":[` +
			item +
			`],"usage":{"input_tokens":2,"output_tokens":3}}}`,
	)
	assertImage := func(body []byte, data, mime string) {
		t.Helper()
		if gjson.GetBytes(body, "candidates.0.content.parts.0.inlineData.data").String() != data ||
			gjson.GetBytes(
				body, "candidates.0.content.parts.0.inlineData.mimeType",
			).String() != mime {
			t.Fatalf("generated image lost: %s", body)
		}
	}
	var state any
	assertImage(
		sdk.TranslateNonStream(
			context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil, completed, &state,
		), "aGVsbG8=", "image/jpeg",
	)
	for _, frame := range []string{
		`{"type":"response.image_generation_call.partial_image","item_id":"imag` +
			`e-a","output_index":0,"partial_image_b64":"cGFydA==","output_format":"` +
			`webp"}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		string(completed),
	} {
		state = nil
		frames := sdk.Default().TranslateStream(
			context.Background(),
			sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil, []byte("data: "+frame),
			&state,
		)
		if len(frames) != 1 {
			t.Fatalf("image event lost: %s", frames)
		}
		if strings.Contains(frame, "partial_image_b64") {
			assertImage(frames[0], "cGFydA==", "image/webp")
		} else {
			assertImage(frames[0], "aGVsbG8=", "image/jpeg")
		}
	}
	state = nil
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil,
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`), &state,
	)
	frames := sdk.Default().TranslateStream(
		context.Background(), sdk.FormatCodex, sdk.FormatGemini, "codex-test", nil, nil,
		append([]byte("data: "), completed...), &state,
	)
	if len(frames) != 1 || len(
		gjson.GetBytes(
			frames[0], "candidates.0.content.parts",
		).Array(),
	) != 0 || gjson.GetBytes(frames[0], "usageMetadata.totalTokenCount").Int() != 5 {
		t.Fatalf("completed duplicated image or lost usage: %s", frames)
	}
}
