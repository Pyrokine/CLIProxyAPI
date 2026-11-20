package util_test

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestToolNameMapKeepsDistinctLeadingUnderscoreDeclarations(t *testing.T) {
	for _, names := range [][]string{{"lookup", "_lookup"}, {"_lookup", "lookup"}, {"lookup", "_lookup", "__lookup"}} {
		tools := make([]map[string]any, 0, len(names))
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name})
		}
		raw, err := json.Marshal(map[string]any{"tools": tools})
		if err != nil {
			t.Fatal(err)
		}
		mapping := util.ToolNameMapFromClaudeRequest(raw)
		for _, name := range names {
			if got := util.MapToolName(mapping, name); got != name {
				t.Fatalf("declared name %q became %q with %v", name, got, names)
			}
		}
		if got := util.MapToolName(mapping, "___lookup"); got != "___lookup" {
			t.Fatalf("ambiguous undeclared tool became %q", got)
		}
	}
	mapping := util.ToolNameMapFromClaudeRequest([]byte(`{"tools":[{"name":"Lookup"}]}`))
	for _, upstream := range []string{"Lookup", "lookup", "_lookup", "__LOOKUP"} {
		if got := util.MapToolName(mapping, upstream); got != "Lookup" {
			t.Fatalf("unambiguous legacy spelling %q became %q", upstream, got)
		}
	}
}

func TestGeminiSDKToolCallsKeepDeclaredLeadingUnderscores(t *testing.T) {
	ctx := sdktranslator.WithPluginHooks(t.Context(), nil)
	for _, testCase := range []struct {
		name     string
		declared []string
		chosen   string
	}{
		{"plain control", []string{"lookup"}, "lookup"},
		{"underscore control", []string{"_lookup"}, "_lookup"},
		{"both select underscore", []string{"lookup", "_lookup"}, "_lookup"},
		{"reversed select plain", []string{"_lookup", "lookup"}, "lookup"},
		{"two underscores", []string{"lookup", "_lookup", "__lookup"}, "__lookup"},
	} {
		for _, stream := range []bool{false, true} {
			mode := "nonstream"
			if stream {
				mode = "stream"
			}
			t.Run(
				testCase.name+"/"+mode, func(t *testing.T) {
					tools := make([]map[string]any, 0, len(testCase.declared))
					for _, name := range testCase.declared {
						tools = append(
							tools, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}},
						)
					}
					raw, err := json.Marshal(
						map[string]any{
							"model": "synthetic-gemini", "max_tokens": 128,
							"messages": []map[string]any{{"role": "user", "content": "lookup"}}, "tools": tools,
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					request, err := sdktranslator.TranslateRequestChecked(
						ctx, sdktranslator.FormatClaude, sdktranslator.FormatGemini, "synthetic-gemini", raw, stream,
					)
					if err != nil {
						t.Fatal(err)
					}
					declarations := gjson.GetBytes(request, "tools.0.functionDeclarations").Array()
					for _, name := range testCase.declared {
						found := false
						for _, declaration := range declarations {
							found = found || declaration.Get("name").String() == name
						}
						if !found {
							t.Fatalf("registered request changed declaration %q: %s", name, request)
						}
					}
					response, err := json.Marshal(
						map[string]any{
							"responseId": "synthetic-response", "modelVersion": "synthetic-gemini",
							"candidates": []map[string]any{
								{
									"content": map[string]any{
										"role": "model", "parts": []map[string]any{
											{
												"functionCall": map[string]any{
													"id": "synthetic-call", "name": testCase.chosen,
													"args": map[string]any{"query": "x"},
												},
											},
										},
									}, "finishReason": "STOP",
								},
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					var state any
					if !stream {
						out := sdktranslator.TranslateNonStream(
							ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "synthetic-gemini", raw,
							request, response, &state,
						)
						if err := sdktranslator.ResponseTranslationError(&state); err != nil {
							t.Fatal(err)
						}
						if got := gjson.GetBytes(out, "content.0.name").String(); got != testCase.chosen {
							t.Fatalf("Gemini selected %q, Claude got %q: %s", testCase.chosen, got, out)
						}
						if got := gjson.GetBytes(out, "stop_reason").String(); got != "tool_use" {
							t.Fatalf("tool response lost its terminal state: %s", out)
						}
						return
					}
					frames := sdktranslator.TranslateStream(
						ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "synthetic-gemini", raw, request,
						append([]byte("data: "), response...), &state,
					)
					frames = append(
						frames, sdktranslator.TranslateStream(
							ctx, sdktranslator.FormatGemini, sdktranslator.FormatClaude, "synthetic-gemini", raw,
							request, []byte("[DONE]"), &state,
						)...,
					)
					if err := sdktranslator.ResponseTranslationError(&state); err != nil {
						t.Fatal(err)
					}
					gotName, stopReason := "", ""
					for _, frame := range frames {
						for line := range strings.SplitSeq(string(frame), "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
							if name := gjson.Get(payload, "content_block.name").String(); name != "" {
								gotName = name
							}
							if stop := gjson.Get(payload, "delta.stop_reason").String(); stop != "" {
								stopReason = stop
							}
						}
					}
					if gotName != testCase.chosen || stopReason != "tool_use" {
						t.Fatalf(
							"Gemini selected %q, stream name=%q stop=%q: %s", testCase.chosen, gotName, stopReason,
							frames,
						)
					}
				},
			)
		}
	}
}

func TestOpenAISDKLeadingUnderscoreToolIdentity(t *testing.T) {
	ctx := sdktranslator.WithPluginHooks(t.Context(), nil)
	for _, stream := range []bool{false, true} {
		mode := "nonstream"
		if stream {
			mode = "stream"
		}
		t.Run(
			mode, func(t *testing.T) {
				raw, err := json.Marshal(
					map[string]any{
						"model": "synthetic-openai", "max_tokens": 128, "stream": stream,
						"messages": []map[string]any{{"role": "user", "content": "lookup"}},
						"tools": []map[string]any{
							{
								"name": "lookup", "input_schema": map[string]any{"type": "object"},
							}, {"name": "_lookup", "input_schema": map[string]any{"type": "object"}},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				request, err := sdktranslator.TranslateRequestChecked(
					ctx, sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "synthetic-openai", raw, stream,
				)
				if err != nil {
					t.Fatal(err)
				}
				var state any
				if !stream {
					response := []byte((`{"id":"synthetic-response","model":"synthetic-openai","choices":[{"ind` +
						`ex":0,"message":{"role":"assistant","tool_calls":[{"id":"synthetic-cal` +
						`l","type":"function","function":{"name":"_lookup","arguments":"{}"}}]}` +
						`,"finish_reason":"tool_calls"}]}`))
					out := sdktranslator.TranslateNonStream(
						ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "synthetic-openai", raw, request,
						response, &state,
					)
					if err := sdktranslator.ResponseTranslationError(&state); err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(out, "content.0.name").String() != "_lookup" || gjson.GetBytes(
						out, "stop_reason",
					).String() != "tool_use" {
						t.Fatalf("OpenAI shared helper lost tool identity or terminal state: %s", out)
					}
					return
				}
				frames := sdktranslator.TranslateStream(
					ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "synthetic-openai", raw, request,
					[]byte((`data: {"id":"synthetic-response","model":"synthetic-openai","choices":[{"index":` +
						`0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"synthetic-call","type":` +
						`"function","function":{"name":"_lookup","arguments":"{}"}}]},"finish_reason":null}]}`)),
					&state,
				)
				frames = append(
					frames, sdktranslator.TranslateStream(
						ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "synthetic-openai", raw, request,
						[]byte((`data: {"id":"synthetic-response","model":"synthetic-openai","choices":`+
							`[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)),
						&state,
					)...,
				)
				frames = append(
					frames, sdktranslator.TranslateStream(
						ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "synthetic-openai", raw, request,
						[]byte("data: [DONE]"), &state,
					)...,
				)
				if err := sdktranslator.ResponseTranslationError(&state); err != nil {
					t.Fatal(err)
				}
				gotName, stopReason := "", ""
				for _, frame := range frames {
					for line := range strings.SplitSeq(string(frame), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
						if name := gjson.Get(payload, "content_block.name").String(); name != "" {
							gotName = name
						}
						if stop := gjson.Get(payload, "delta.stop_reason").String(); stop != "" {
							stopReason = stop
						}
					}
				}
				if gotName != "_lookup" || stopReason != "tool_use" {
					t.Fatalf(
						"OpenAI stream lost tool identity or terminal state: name=%q stop=%q frames=%s", gotName,
						stopReason, frames,
					)
				}
			},
		)
	}
}
