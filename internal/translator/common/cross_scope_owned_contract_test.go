package common_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCrossScopeSDKInteractionsGenerationControls(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	body := []byte(`{"model":"client-model","messages":[{"role":"user","content":"query"}]` +
		`,"top_p":0.8,"presence_penalty":0.4,"frequency_penalty":-0.25,"stop":[` +
		`"END"]}`)
	out, err := sdk.TranslateRequestChecked(
		ctx, sdk.FormatOpenAI, sdk.FormatInteractions, "selected-model", body, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"generation_config.top_p": "0.8", "generation_config.presence_penalty": "0.4",
		"generation_config.frequency_penalty": "-0.25", "generation_config.stop_sequences.0": "END",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("native control %s = %q, want %q; %s", path, got, want, out)
		}
	}
}

func TestCrossScopeSDKClaudeToolResultSlots(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	const callA = `{"type":"function_call","call_id":"call-a","name":"lookup","arguments":"{\"n\":1}"}`
	const callB = `{"type":"function_call","call_id":"call-b","name":"lookup","arguments":"{\"n\":2}"}`
	const resultA = `{"type":"function_call_output","call_id":"call-a","output":"result-a"}`
	const resultB = `{"type":"function_call_output","call_id":"call-b","output":"result-b"}`
	const unknown = `{"type":"function_call_output","call_id":"unknown","output":"unpaired-result"}`
	for _, tc := range []struct {
		name, input string
		ids, texts  []string
	}{
		{
			"reverse parallel slots", callA + "," + callB + "," + resultB + "," + resultA, []string{"call-b", "call-a"},
			[]string{"result-a", "result-b"},
		},
		{
			"delayed by user content",
			callA + `,{"type":"message","role":"user","content":"keep-user-text"},` + resultA, []string{"call-a"},
			[]string{"result-a", "keep-user-text"},
		},
		{"standalone", unknown, nil, []string{"unpaired-result"}},
		{
			"unknown does not occupy known slot", callA + "," + unknown + "," + resultA, []string{"call-a"},
			[]string{"result-a", "unpaired-result"},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				body := []byte((`{"model":"client-model","tools":[{"type":"function","name":"lookup","` +
					`parameters":{"type":"object"}}],"input":[{"type":"message","role":"user","content":"` +
					`initial query"},`) + tc.input + `]}`)
				out, err := sdk.TranslateRequestChecked(
					ctx, sdk.FormatOpenAIResponse, sdk.FormatClaude, "selected-claude", body, false,
				)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, message := range gjson.GetBytes(out, "messages").Array() {
					for _, part := range message.Get("content").Array() {
						if part.Get("type").String() == "tool_result" {
							id := part.Get("tool_use_id").String()
							ids = append(ids, id)
							want := map[string]string{"call-a": "result-a", "call-b": "result-b"}[id]
							if want != "" && part.Get("content").String() != want {
								t.Fatalf("result %s lost its own content: %s", id, out)
							}
						}
					}
				}
				if len(ids) != len(tc.ids) {
					t.Fatalf("result slots %v, want %v; %s", ids, tc.ids, out)
				}
				for i := range ids {
					if ids[i] != tc.ids[i] {
						t.Fatalf("result slot %d = %q, want %q; %s", i, ids[i], tc.ids[i], out)
					}
				}
				for _, text := range tc.texts {
					if !bytes.Contains(out, []byte(text)) {
						t.Fatalf("lost accepted text %q; %s", text, out)
					}
				}
			},
		)
	}
}

func TestCrossScopeSDKResponsesStartingModels(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	for _, tc := range []struct {
		name string
		from sdk.Format
		body string
	}{
		{
			"Claude", sdk.FormatClaude,
			`{"type":"message_start","message":{"id":"msg-fixture","model":"provide` +
				`r-model","usage":{"input_tokens":1,"output_tokens":0}}}`,
		},
		{
			"Gemini", sdk.FormatGemini,
			`{"responseId":"gemini-fixture","modelVersion":"provider-model","candid` +
				`ates":[{"content":{"role":"model","parts":[{"text":"text"}]}}]}`,
		},
		{
			"Gemini absent version", sdk.FormatGemini,
			`{"responseId":"gemini-fixture","candidates":[{"content":{"role":"model","parts":[{"text":"text"}]}}]}`,
		},
		{
			"Chat", sdk.FormatOpenAI,
			`{"id":"chat-fixture","model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant",` +
				`"content":"text"},"finish_reason":null}]}`,
		},
		{
			"Codex created", sdk.FormatCodex,
			`{"type":"response.created","response":{"id":"codex-fixture","status":"in_progress","output":[]}}`,
		},
		{
			"Codex in progress", sdk.FormatCodex,
			`{"type":"response.in_progress","response":{"id":"codex-fixture","status":"in_progress","output":[]}}`,
		},
		{
			"Interactions", sdk.FormatInteractions,
			`{"event_type":"interaction.created","interaction":{"id":"interaction-fixture","model":"provider-model"}}`,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				for _, request := range []struct {
					name, original, translated, fallback, model string
				}{
					{
						"original request", `{"request":{"model":"original-client-model"}}`,
						`{"model":"translated-model"}`, "fallback-model", "original-client-model",
					},
					{
						"translated request", `{"model":" "}`, `{"request":{"model":"translated-model"}}`,
						"fallback-model", "translated-model",
					},
					{"model parameter", `{}`, `{}`, "fallback-model", "fallback-model"},
					{"all absent", `{}`, `{}`, "", ""},
				} {
					t.Run(
						request.name, func(t *testing.T) {
							var state any
							frames := sdk.TranslateStream(
								ctx, tc.from, sdk.FormatOpenAIResponse, request.fallback,
								[]byte(request.original), []byte(request.translated), []byte("data: "+tc.body), &state,
							)
							if err := sdk.ResponseTranslationError(&state); err != nil {
								t.Fatal(err)
							}
							found := false
							for _, frame := range frames {
								for line := range strings.SplitSeq(string(frame), "\n") {
									if !strings.HasPrefix(line, "data:") {
										continue
									}
									payload := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
									if kind := payload.Get("type").String(); kind != "response.created" &&
										kind != "response.in_progress" {
										continue
									}
									found = true
									wantModel := request.model
									if tc.name == "Gemini" {
										wantModel = "provider-model"
									}
									output := payload.Get("response.output")
									if payload.Get("response.model").String() != wantModel || !output.IsArray() ||
										len(output.Array()) != 0 {
										t.Fatalf("starting event lost model priority or empty output: %s", payload.Raw)
									}
								}
							}
							if !found {
								t.Fatalf("no starting event: %s", frames)
							}
						},
					)
				}
			},
		)
	}
}

func TestCrossScopeSDKSelectedModelAndStructuredToolIDs(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	for _, target := range []sdk.Format{sdk.FormatGemini, sdk.FormatGeminiCLI, sdk.FormatAntigravity} {
		t.Run(
			target.String(), func(t *testing.T) {
				body := []byte((`{"model":"client-alias","system":"retained system","tools":[{"name":"lookup",` +
					`"input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"` +
					`tool_use","id":"toolu_structured_01","name":"lookup","input":{"key":"value"}}]},{"role":` +
					`"user","content":[{"type":"tool_result","tool_use_id":"toolu_structured_01","content":"` +
					`result-value"}]}]}`))
				out, err := sdk.TranslateRequestChecked(ctx, sdk.FormatClaude, target, "selected-model", body, false)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(out, "model").String(); got != "selected-model" {
					t.Fatalf("selected model = %q; %s", got, out)
				}
				contents := gjson.GetBytes(out, "contents")
				if target != sdk.FormatGemini {
					contents = gjson.GetBytes(out, "request.contents")
				}
				callID, resultID := "", ""
				for _, content := range contents.Array() {
					for _, part := range content.Get("parts").Array() {
						if call := part.Get("functionCall"); call.Exists() {
							callID = call.Get("id").String()
						}
						if result := part.Get("functionResponse"); result.Exists() {
							resultID = result.Get("id").String()
						}
					}
				}
				if callID != "toolu_structured_01" || resultID != callID {
					t.Fatalf("structured tool identity lost: call=%q result=%q; %s", callID, resultID, out)
				}
			},
		)
	}
}

func TestCrossScopeSDKJSONSchemaCarrierConstraintsAndOrder(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	schema := `{"type":"object","properties":{"zeta":{"type":"integer","minimum":2,"maximum":9},"alpha":{"` +
		`type":"string","pattern":"^[a-z]+$"}},"required":["zeta","alpha"],"additionalProperties":false}`
	body := []byte(
		`{"messages":[{"role":"user","content":"query"}],"tools":[{"name":"lookup","input_schema":` + schema + `}]}`,
	)
	out, err := sdk.TranslateRequestChecked(ctx, sdk.FormatClaude, sdk.FormatAntigravity, "fixture", body, false)
	if err != nil {
		t.Fatal(err)
	}
	result := gjson.GetBytes(out, "request.tools.0.functionDeclarations.0.parametersJsonSchema")
	for path, want := range map[string]string{
		"properties.zeta.type": "integer", "properties.zeta.minimum": "2", "properties.zeta.maximum": "9",
		"properties.alpha.pattern": "^[a-z]+$", "required.0": "zeta", "required.1": "alpha",
		"additionalProperties": "false",
	} {
		if got := result.Get(path).String(); got != want {
			t.Fatalf("schema %s = %q, want %q; %s", path, got, want, out)
		}
	}
	var first, second any
	if json.Unmarshal([]byte(result.Raw), &first) != nil || json.Unmarshal([]byte(schema), &second) != nil {
		t.Fatal("invalid schema JSON")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("legal JSONSchema carrier changed constraints: %s", result.Raw)
	}
}

func TestCrossScopeSDKClaudeImageConsumers(t *testing.T) {
	ctx := sdk.WithPluginHooks(t.Context(), nil)
	body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":[{"type":` +
		`"text","text":"retained image prompt"},{"type":"image_url","image_url"` +
		`:{"url":"https://example.invalid/synthetic.png"}}]}]}`)
	out, err := sdk.TranslateRequestChecked(ctx, sdk.FormatOpenAI, sdk.FormatClaude, "selected-claude", body, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "model").String() != "selected-claude" || gjson.GetBytes(
		out, "messages.0.content.0.text",
	).String() != "retained image prompt" || gjson.GetBytes(
		out, "messages.0.content.1.source.url",
	).String() != "https://example.invalid/synthetic.png" {
		t.Fatalf("image consumer changed model, text or URL: %s", out)
	}
}
