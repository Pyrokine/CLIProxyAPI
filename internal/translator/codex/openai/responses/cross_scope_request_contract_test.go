package responses_test

import (
	"context"
	"fmt"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesSDKStreamOptionsWhitelist(t *testing.T) {
	for _, options := range []struct{ input, want string }{
		{`{"include_usage":true}`, ""},
		{`{"include_usage":false,"unknown":"discard"}`, ""},
		{
			`{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true,"unknown":"discard"}`,
			`{"reasoning_summary_delivery":"sequential_cutoff"}`,
		},
		{`{"reasoning_summary_delivery":"sequential_cutoff"}`, `{"reasoning_summary_delivery":"sequential_cutoff"}`},
		{`null`, ""},
	} {
		for _, stream := range []bool{false, true} {
			body := []byte(`{"model":"gpt-5.6","input":"query","stream_options":` + options.input + `}`)
			out, err := sdk.TranslateRequestChecked(
				context.Background(), sdk.FormatOpenAIResponse, sdk.FormatCodex, "gpt-5.6", body, stream,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "stream_options").Raw; got != options.want {
				t.Fatalf("stream=%v input=%s output=%s want=%s", stream, options.input, out, options.want)
			}
		}
	}
}

func TestCodexResponsesSDKPreservesImageGenerationToolConfiguration(t *testing.T) {
	for _, model := range []string{
		"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare-2026-09-08",
		"gpt-image-2.5-sunburst-2026-09-08",
	} {
		for _, quality := range []string{"auto", "low", "medium", "high", "synthetic-provider-quality"} {
			tool := fmt.Sprintf(
				`{"type":"image_generation","model":%q,"quality":%q,"input_image_mask":`+
					`{"image_url":"data:image/png;base64,`+
					`AA=="}}`,
				model,
				quality,
			)
			input := `[{"role":"user","content":[{"type":"input_text","text":"Edit the image` +
				`"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]`
			body := []byte(`{"model":"gpt-5.6","input":` + input + `,"tools":[` + tool + `]}`)
			out, err := sdk.TranslateRequestChecked(
				context.Background(), sdk.FormatOpenAIResponse, sdk.FormatCodex, "gpt-5.6", body, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(out, "tools.0").Raw != tool || gjson.GetBytes(out, "input").Raw != input {
				t.Fatalf("image model/quality/mask/input fields changed: %s", out)
			}
		}
	}
}
