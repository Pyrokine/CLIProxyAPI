package responses_test

import (
	"context"
	"fmt"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/interactions"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/interactions/claude"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsSDKNonStreamFailure(t *testing.T) {
	for _, target := range []sdk.Format{sdk.FormatClaude, sdk.FormatGemini} {
		for _, body := range []string{
			`{"status":"failed","steps":[],"error":{"code":"429","message":"synthetic quota failure"}}`,
			`{"interaction":{"status":"failed","steps":[],"error":{"code":"429","message":"synthetic quota failure"}}}`,
			`{"interaction":{"status":"failed","steps":[{"type":"model_output","con` +
				`tent":[{"type":"text","text":"partial"}]}],"error":{"code":"429","mess` +
				`age":"synthetic quota failure"}}}`,
		} {
			var state any
			out := sdk.TranslateNonStream(
				context.Background(), sdk.FormatInteractions, target, "fixture", nil, nil, []byte(body), &state,
			)
			if gjson.GetBytes(out, "error.message").String() != "synthetic quota failure" || gjson.GetBytes(
				out, "stop_reason",
			).Exists() || gjson.GetBytes(out, "candidates").Exists() {
				t.Fatalf("%s failed interaction became success: %s", target, out)
			}
			if target == sdk.FormatGemini && gjson.GetBytes(out, "error.code").Int() != 429 {
				t.Fatalf("Gemini status lost: %s", out)
			}
		}
	}
}

func TestInteractionsSDKThinkingSignatureRoundTrip(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	for _, tc := range []struct{ model, signature string }{{"claude-test", "EgQKAggM"}, {"gemini-test", "CgQBBQYH"}} {
		for _, text := range []string{"reason", ""} {
			response := []byte(fmt.Sprintf(
				`{"model":%q,"status":"completed","steps":[{"type":"thought",`+
					`"signature":%q,"content":[{"type":"text","text":`+
					`%q}]}]}`,
				tc.model,
				tc.signature,
				text,
			))
			var state any
			out := sdk.TranslateNonStream(
				context.Background(),
				sdk.FormatInteractions,
				sdk.FormatClaude,
				tc.model,
				nil,
				nil,
				response,
				&state,
			)
			content := gjson.GetBytes(out, "content").Raw
			request := []byte(
				`{"messages":[{"role":"user","content":"query"},{"role":"assistant",` +
					`"content":` +
					content +
					`},{"role":"user","content":"continue"}]}`,
			)
			next, err := sdk.TranslateRequestChecked(
				context.Background(), sdk.FormatClaude, sdk.FormatInteractions, tc.model, request, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, step := range gjson.GetBytes(next, "input").Array() {
				if step.Get("type").String() == "thought" && step.Get("signature").String() == tc.signature &&
					step.Get("content.0.text").String() == text {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s signed thought lost: %s", tc.model, next)
			}
		}
	}
}

func TestInteractionsSDKSchemaNamesAndTerminal(t *testing.T) {
	schema := `{"type":"object","properties":{"maxOutputTokens":{"type":"integer"},"snake_name":{"type":"` +
		`string"}},"required":["maxOutputTokens","snake_name"]}`
	for _, tc := range []struct {
		from, to   sdk.Format
		body, path string
	}{
		{
			sdk.FormatGemini, sdk.FormatInteractions,
			`{"generationConfig":{"maxOutputTokens":400,"responseSchema":` + schema + `},"contents":[]}`,
			"generation_config.response_schema",
		}, {
			sdk.FormatInteractions, sdk.FormatGemini,
			`{"generation_config":{"max_output_tokens":400,"response_schema":` + schema + `},"input":[]}`,
			"generationConfig.responseSchema",
		},
	} {
		out, err := sdk.TranslateRequestChecked(
			context.Background(), tc.from, tc.to, "gemini-test", []byte(tc.body), false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, tc.path).Raw != schema {
			t.Fatalf("schema properties or required changed: %s", out)
		}
	}
	var state any
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatGemini, sdk.FormatInteractions, "gemini-test", nil, nil,
		[]byte(
			`{"candidates":[{"content":{"parts":[{"text":"first"}]},"finishReason":`+
				`"STOP"}],"usageMetadata":{"totalTokenCount":`+
				`2}}`,
		),
		&state,
	)
	sdk.Default().TranslateStream(
		context.Background(),
		sdk.FormatGemini, sdk.FormatInteractions, "gemini-test", nil, nil, []byte(`[DONE]`),
		&state,
	)
	for _, frame := range []string{`{"candidates":[{"content":{"parts":[{"text":"late"}]}}]}`, `[DONE]`} {
		if out := sdk.Default().TranslateStream(
			context.Background(), sdk.FormatGemini, sdk.FormatInteractions, "gemini-test", nil, nil, []byte(frame),
			&state,
		); len(out) != 0 {
			t.Fatalf("late Gemini event emitted: %s", out)
		}
	}
}

func TestInteractionsSDKSWESealedSignatureRoundTrip(t *testing.T) {
	var state any
	out := sdk.TranslateNonStream(
		context.Background(), sdk.FormatInteractions, sdk.FormatOpenAIResponse, "devin/swe-2", nil, nil,
		[]byte(`{"model":"devin/swe-2","status":"completed","steps":[{"type":"thought"`+
			`,"signature":"sealed.v1.sig","content":[{"type":"text","text":"reason"`+
			`}]}]}`),
		&state,
	)
	if gjson.GetBytes(out, "output.0.encrypted_content").String() != "sealed.v1.sig" {
		t.Fatalf("SWE signature lost: %s", out)
	}
	state = nil
	back := sdk.TranslateNonStream(
		context.Background(),
		sdk.FormatOpenAIResponse,
		sdk.FormatInteractions,
		"devin/swe-2",
		nil,
		nil,
		out,
		&state,
	)
	if gjson.GetBytes(back, "steps.0.signature").String() != "sealed.v1.sig" {
		t.Fatalf("SWE signature return lost: %s", back)
	}
}

func TestConvertOpenAIResponsesRequestToInteractions_LogFilePayloadGenerationConfig(t *testing.T) {
	body := []byte((`{"model":"devin/gemini-3-7-flash","max_output_tokens":400,"temperature":0.7,"top_p":0.95,` +
		`"tools":[{"type":"function","name":"t0"},{"type":"function","name":"t1"},{"type":"function","name":"` +
		`t2"},{"type":"function","name":"t3"},{"type":"function","name":"t4"},{"type":"function","name":"t5"}` +
		`,{"type":"function","name":"t6"},{"type":"function","name":"t7"},{"type":"namespace","name":"ns8","` +
		`tools":[{"type":"function","name":"c1"},{"type":"function","name":"c2"}]},{"type":"namespace","name"` +
		`:"ns9","tools":[{"type":"function","name":"c1"}]},{"type":"function","name":"t10"}]}`))
	out, err := sdk.TranslateRequestChecked(
		context.Background(),
		sdk.FormatOpenAIResponse,
		sdk.FormatInteractions,
		"devin/gemini-3-7-flash",
		body,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"generation_config.max_output_tokens": "400", "generation_config.temperature": "0.7",
		"generation_config.top_p": "0.95",
	} {
		if got := gjson.GetBytes(out, path).Raw; got != want {
			t.Fatalf("LogFilePayload %s=%s want %s; %s", path, got, want, out)
		}
	}
}
