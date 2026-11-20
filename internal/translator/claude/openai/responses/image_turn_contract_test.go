package responses

import (
	"context"
	"testing"

	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesClaudeImageTurnCheckedEntrypoints(t *testing.T) {
	paths := map[string]func([]byte) ([]byte, error){
		"direct default": func(body []byte) ([]byte, error) {
			return convertOpenAIResponsesRequestToClaudeChecked("claude-test", body, false, false)
		},
		"SDK default": func(body []byte) ([]byte, error) {
			return sdk.TranslateRequestChecked(
				context.Background(), sdk.FormatOpenAIResponse, sdk.FormatClaude, "claude-test", body, false,
			)
		},
		"explicit compatibility": func(body []byte) ([]byte, error) {
			return ConvertOpenAIResponsesRequestToClaudeWithCompatReturningError("claude-test", body, false)
		},
	}
	for name, convert := range paths {
		t.Run(
			name, func(t *testing.T) {
				for _, input := range []string{
					`[{"role":"user","content":[{"type":"input_image","file_id":"synthetic-file"}]}]`,
					`[{"role":"user","content":"history"},{"role":"assistant","content":"answer"},{"role":"` +
						`user","content":[{"type":"input_image","file_id":"synthetic-file"}]}]`,
					`[{"role":"user","content":[{"type":"input_text","text":" \t"},{"type":` +
						`"input_image","file_id":` +
						`"synthetic-file"}]}]`,
				} {
					out, err := convert([]byte(`{"input":` + input + `}`))
					if scoped, ok := err.(interface{ StatusCode() int }); !ok || scoped.StatusCode() != 400 ||
						len(out) != 0 {
						t.Fatalf("unrepresentable image accepted: err=%v body=%s", err, out)
					}
				}
				for _, content := range []string{
					`[{"type":"input_text","text":"visible"},{"type":"input_image","file_id":"synthetic-file"}]`,
					`[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]`,
				} {
					out, err := convert([]byte(`{"input":[{"role":"user","content":` + content + `}]}`))
					if err != nil || len(gjson.GetBytes(out, "messages").Array()) != 1 {
						t.Fatalf("valid image/text control rejected: err=%v body=%s", err, out)
					}
				}
			},
		)
	}
}
