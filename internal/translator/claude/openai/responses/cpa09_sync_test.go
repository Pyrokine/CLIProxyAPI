package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestCPA09ResponsesRequestIdentityAgentMessageAndStructuredOutput(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"metadata":{"user_id":"caller-user"},
			"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}},
			"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"delegated task"}]}]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	if got := root.Get("metadata.user_id").String(); got != "caller-user" {
		t.Fatalf("metadata.user_id = %q, want caller-user", got)
	}
	if got := root.Get("messages.0.role").String(); got != "user" {
		t.Fatalf("agent message role = %q, want user; output=%s", got, out)
	}
	if got := root.Get("messages.0.content").String(); got != "delegated task" {
		t.Fatalf("agent message text = %q, want delegated task; output=%s", got, out)
	}
	instruction := root.Get("system.0.text").String()
	if !strings.Contains(instruction, "Schema Name: answer") || !strings.Contains(instruction, `"ok"`) {
		t.Fatalf("structured output instruction missing: %q", instruction)
	}
}

func TestCPA09ResponsesRequestDeduplicatesToolOutputsByRawID(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"input":[
				{"type":"function_call","call_id":"call.raw/1","name":"work","arguments":"{}"},
				{"type":"function_call_output","call_id":"call.raw/1","output":"first"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"between"}]},
				{"type":"function_call_output","call_id":"call.raw/1","output":"last"}
			]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	var results []gjson.Result
	for _, message := range root.Get("messages").Array() {
		for _, part := range message.Get("content").Array() {
			if part.Get("type").String() == "tool_result" {
				results = append(results, part)
			}
		}
	}
	if len(results) != 1 {
		t.Fatalf("tool result count = %d, want 1; output=%s", len(results), out)
	}
	if got := results[0].Get("tool_use_id").String(); got != "call_raw_1" {
		t.Fatalf("tool_use_id = %q, want call_raw_1", got)
	}
	if got := results[0].Get("content").String(); got != "last" {
		t.Fatalf("tool output = %q, want last", got)
	}
}

func TestCPA09ResponsesRequestReplaysWebSearchAndCitations(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"input":[
				{"id":"ws_native-1","type":"web_search_call","action":{"type":"search","query":"Qwen"},"results":[{"type":"computer_initialize_state","url":"https://example.com","encrypted_content":"cipher"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"web_search_result_location","encrypted_index":"idx"}]}]}
			]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	content := root.Get("messages.0.content")
	if got := content.Get("0.type").String(); got != "server_tool_use" {
		t.Fatalf("content[0].type = %q, want server_tool_use; output=%s", got, out)
	}
	if got := content.Get("0.id").String(); got != "srvtoolu_native_1" {
		t.Fatalf("server tool id = %q, want srvtoolu_native_1", got)
	}
	if got := content.Get("1.type").String(); got != "web_search_tool_result" {
		t.Fatalf("content[1].type = %q, want web_search_tool_result", got)
	}
	if got := content.Get("1.content.0.encrypted_content").String(); got != "cipher" {
		t.Fatalf("encrypted search result = %q, want cipher", got)
	}
	if got := content.Get("2.citations.0.encrypted_index").String(); got != "idx" {
		t.Fatalf("citation encrypted_index = %q, want idx", got)
	}
}

func TestCPA09ResponsesRequestAssistantPrefillBoundary(t *testing.T) {
	request := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prefill"}]}
	]}`)
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5", "claude-sonnet-4-6"} {
		out := convertOpenAIResponsesRequestToClaudeDefault(model, request, false)
		if got := gjson.GetBytes(out, "messages.#").Int(); got != 1 {
			t.Fatalf("%s message count = %d, want 1; output=%s", model, got, out)
		}
	}
	out := convertOpenAIResponsesRequestToClaudeDefault("compatible-unknown", request, false)
	if got := gjson.GetBytes(out, "messages.#").Int(); got != 2 {
		t.Fatalf("unknown model message count = %d, want 2; output=%s", got, out)
	}
}

func TestCPA09ResponsesRequestIdentityIsStableAndRequestScoped(t *testing.T) {
	requestA := []byte(`{"prompt_cache_key":"a","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	requestB := []byte(`{"prompt_cache_key":"b","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	first := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	retry := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	second := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestB, false), "metadata.user_id").String()
	if first == "" || first != retry {
		t.Fatalf("identity is not stable: first=%q retry=%q", first, retry)
	}
	if first == second {
		t.Fatalf("different request scopes share identity %q", first)
	}
}

func TestCPA09ResponsesRequestContractToolPairing(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantResults int
		wantErrors  int
		wantText    string
		wantResult  string
	}{
		{name: "standalone", input: `[{"type":"function_call_output","call_id":"orphan","output":"context"}]`, wantText: "context"},
		{name: "empty standalone", input: `[{"type":"custom_tool_call_output","call_id":"orphan","output":[{"type":"input_text","text":""}]}]`, wantText: "Tool result was empty."},
		{name: "trailing", input: `[{"role":"user","content":"Run"},{"type":"function_call","call_id":"tail","name":"exec","arguments":"{}"}]`, wantResults: 1, wantErrors: 1},
		{name: "late", input: `[{"role":"user","content":"Run"},{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"role":"user","content":"Wait"},{"role":"assistant","content":"Interlude"},{"type":"function_call_output","call_id":"a","output":"late result"}]`, wantResults: 1, wantErrors: 1, wantText: "late result"},
		{name: "late empty", input: `[{"role":"user","content":"Run"},{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"role":"user","content":"Wait"},{"role":"assistant","content":"Interlude"},{"type":"function_call_output","call_id":"a","output":""}]`, wantResults: 1, wantErrors: 1, wantText: "Tool result was empty."},
		{name: "raw ID collision", input: `[{"type":"function_call","call_id":"call.a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"unrelated"},{"type":"function_call_output","call_id":"call.a","output":"real result"}]`, wantResults: 1, wantText: "unrelated", wantResult: "real result"},
		{name: "two raw calls collide", input: `[{"type":"function_call","call_id":"call.a","name":"first","arguments":"{}"},{"type":"function_call","call_id":"call_a","name":"second","arguments":"{}"},{"type":"function_call_output","call_id":"call.a","output":"one"},{"type":"function_call_output","call_id":"call_a","output":"two"}]`, wantResults: 2},
		{name: "duplicate final payload", input: `[{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"first"},{"role":"user","content":"between"},{"type":"function_call_output","call_id":"a","output":"last"}]`, wantResults: 1, wantResult: "last"},
		{name: "paired whitespace", input: `[{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"\n  "}]`, wantResults: 1, wantResult: "\n  "},
		{name: "paired whitespace array", input: `[{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"  "}]}]`, wantResults: 1, wantResult: "  "},
		{name: "unknown result at tail", input: `[{"type":"custom_tool_call","call_id":"a","name":"exec","input":"pwd"},{"type":"custom_tool_call_output","call_id":"other","output":"unknown result"}]`, wantResults: 1, wantErrors: 1, wantText: "unknown result"},
		{name: "heartbeat before result", input: `[{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","id":"fco_seed","name":"heartbeat","output":"tick"},{"type":"function_call_output","call_id":"a","output":"real result"}]`, wantResults: 1, wantText: "tick", wantResult: "real result"},
	}
	for _, tc := range tests {
		for _, compat := range []bool{false, true} {
			name := tc.name + "/registered"
			if compat {
				name = tc.name + "/compat"
			}
			t.Run(name, func(t *testing.T) {
				input := []byte(`{"input":` + tc.input + `}`)
				var out []byte
				var err error
				if compat {
					out, err = ConvertOpenAIResponsesRequestToClaudeWithCompatReturningError("claude-fable-5-1", input, false)
				} else {
					out, err = sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
						sdktranslator.FormatClaude, "claude-fable-5-1", input, false)
				}
				if err != nil {
					t.Fatal(err)
				}
				assertCPA09ClaudeToolPairing(t, out)
				results, errors := 0, 0
				textFound, resultFound := tc.wantText == "", tc.wantResult == ""
				for _, message := range gjson.GetBytes(out, "messages").Array() {
					content := message.Get("content")
					if content.Type == gjson.String && content.String() == tc.wantText {
						textFound = true
					}
					for _, part := range content.Array() {
						if part.Get("type").String() == "tool_result" {
							results++
							if part.Get("is_error").Bool() {
								errors++
							}
							if part.Get("content").String() == tc.wantResult {
								resultFound = true
							}
						}
						if part.Get("type").String() == "text" && part.Get("text").String() == tc.wantText {
							textFound = true
						}
					}
				}
				if results != tc.wantResults || errors != tc.wantErrors || !textFound || !resultFound {
					t.Fatalf("results=%d errors=%d text=%v result=%v; output=%s", results, errors, textFound, resultFound, out)
				}
			})
		}
	}
}

func assertCPA09ClaudeToolPairing(t *testing.T, out []byte) {
	t.Helper()
	pending := make(map[string]bool)
	for _, message := range gjson.GetBytes(out, "messages").Array() {
		if message.Get("role").String() == "assistant" {
			if len(pending) != 0 {
				t.Fatalf("tool calls unanswered before next assistant: %v; output=%s", pending, out)
			}
			for _, part := range message.Get("content").Array() {
				if part.Get("type").String() == "tool_use" {
					id := part.Get("id").String()
					if id == "" || pending[id] {
						t.Fatalf("empty or duplicate tool ID %q; output=%s", id, out)
					}
					pending[id] = true
				}
			}
			continue
		}
		seenContent := false
		for _, part := range message.Get("content").Array() {
			if part.Get("type").String() != "tool_result" {
				seenContent = true
				continue
			}
			id := part.Get("tool_use_id").String()
			if seenContent || !pending[id] {
				t.Fatalf("unpaired or misplaced result %q; output=%s", id, out)
			}
			delete(pending, id)
		}
		if len(pending) != 0 {
			t.Fatalf("tool calls unanswered after user turn: %v; output=%s", pending, out)
		}
	}
	if len(pending) != 0 {
		t.Fatalf("trailing unanswered calls: %v; output=%s", pending, out)
	}
}

func TestCPA09ResponsesRequestContractReasoningReplay(t *testing.T) {
	signature := validClaudeThinkingSignature()
	for _, tc := range []struct {
		name    string
		item    string
		kind    string
		payload string
	}{
		{name: "redacted", item: `{"type":"reasoning","encrypted_content":"claude-redacted-thinking:opaque"}`, kind: "redacted_thinking", payload: "opaque"},
		{name: "content text", item: `{"type":"reasoning","encrypted_content":"` + signature + `","summary":[],"content":[{"type":"reasoning_text","text":"restored"}]}`, kind: "thinking", payload: "restored"},
		{name: "summary priority", item: `{"type":"reasoning","encrypted_content":"` + signature + `","summary":[{"text":"summary"}],"content":[{"type":"reasoning_text","text":"duplicate"}]}`, kind: "thinking", payload: "summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"input":[` + tc.item + `,{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`)
			out, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, "claude-test", input, false)
			if err != nil {
				t.Fatal(err)
			}
			part := gjson.GetBytes(out, "messages.0.content.0")
			if part.Get("type").String() != tc.kind {
				t.Fatalf("reasoning kind = %q, want %q; output=%s", part.Get("type").String(), tc.kind, out)
			}
			if tc.kind == "redacted_thinking" {
				if part.Get("data").String() != tc.payload || part.Get("signature").Exists() {
					t.Fatalf("invalid redacted replay: %s", part.Raw)
				}
			} else if part.Get("thinking").String() != tc.payload || part.Get("signature").String() != signature {
				t.Fatalf("invalid signed replay: %s", part.Raw)
			}
		})
	}
}

func TestCPA09ResponsesRequestContractToolResultImage(t *testing.T) {
	for _, url := range []string{"data:image/png;base64,iVBORw0KGgo=", "https://example.com/image.png"} {
		t.Run(url, func(t *testing.T) {
			input := []byte(`{"input":[{"type":"function_call","call_id":"image","name":"view","arguments":"{}"},{"type":"function_call_output","call_id":"image","cache_control":{"type":"ephemeral"},"output":[{"type":"input_text","text":"image result"},{"type":"input_image","image_url":"` + url + `"}]}]}`)
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, "claude-test", input, false)
			if err != nil {
				t.Fatal(err)
			}
			assertCPA09ClaudeToolPairing(t, out)
			result := gjson.GetBytes(out, "messages.1.content.0")
			if result.Get("content.0.text").String() != "image result" || result.Get("content.1.type").String() != "image" || result.Get("cache_control.type").String() != "ephemeral" {
				t.Fatalf("image or cache was lost: %s", out)
			}
			image := result.Get("content.1.source")
			if strings.HasPrefix(url, "data:") {
				if image.Get("type").String() != "base64" || image.Get("media_type").String() != "image/png" || image.Get("data").String() != "iVBORw0KGgo=" {
					t.Fatalf("base64 image changed: %s", image.Raw)
				}
			} else if image.Get("type").String() != "url" || image.Get("url").String() != url {
				t.Fatalf("URL image changed: %s", image.Raw)
			}
		})
	}
}

func TestCPA09ResponsesRequestContractToolDeclarationCache(t *testing.T) {
	input := []byte(`{"input":"hi","tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch","description":"Edit files.","format":{"type":"grammar","definition":"start: patch"},"cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"function","name":"lookup","cache_control":{"type":"ephemeral"}}]}]}`)
	out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude, "claude-test", input, false)
	if err != nil {
		t.Fatal(err)
	}
	patch := gjson.GetBytes(out, `tools.#(name=="editor__apply_patch")`)
	if patch.Get("cache_control.ttl").String() != "1h" || patch.Get("input_schema.additionalProperties").Bool() || !patch.Get("input_schema.additionalProperties").Exists() {
		t.Fatalf("patch cache or schema was lost: %s", patch.Raw)
	}
	if gjson.GetBytes(out, `tools.#(name=="editor__lookup").cache_control.type`).String() != "ephemeral" {
		t.Fatalf("function cache was lost: %s", out)
	}
	for _, token := range []string{"*** Add File:", "*** Delete File:", "*** Update File:", "*** Move to:", "*** End of File", "@@", "start: patch"} {
		if !strings.Contains(patch.Get("description").String(), token) {
			t.Errorf("patch instruction %q was lost", token)
		}
	}
}

func TestCPA09ResponsesRequestContractPrefillModelNames(t *testing.T) {
	request := []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"prefill"}]}`)
	for _, tc := range []struct {
		model string
		count int64
	}{
		{model: "claude-fable-5-1", count: 1},
		{model: "fable", count: 1},
		{model: "sonnet-4.6", count: 1},
		{model: "claude-sonnet-4-10", count: 1},
		{model: "claude-sonnet-5", count: 1},
		{model: "claude-opus-6", count: 1},
		{model: "anthropic/claude-opus-5-thinking", count: 1},
		{model: "claude-sonnet-4-6-20260217", count: 1},
		{model: "claude-fable-5-1(high)", count: 1},
		{model: "claude-opus-5-5[1m](high)", count: 1},
		{model: "my-opus-5-wrapper(high)", count: 2},
		{model: "claude-opus-20240229", count: 2},
		{model: "claude-sonnet-4-20260217", count: 2},
		{model: "my-sonnet-4-6-wrapper", count: 2},
		{model: "not-a-fable-model", count: 2},
		{model: "claude-fabled-5", count: 2},
		{model: "claude-opus-5foo", count: 2},
		{model: "claude-sonnet-4-6foo", count: 2},
		{model: "fable/gpt-4o", count: 2},
	} {
		t.Run(tc.model, func(t *testing.T) {
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, tc.model, request, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "messages.#").Int(); got != tc.count {
				t.Fatalf("message count = %d, want %d; output=%s", got, tc.count, out)
			}
		})
	}
}

func TestCPA09ResponsesRequestContractByteEntryRejectsUnsupportedAttachments(t *testing.T) {
	input := []byte(`{"input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`)
	for name, convert := range map[string]func(string, []byte, bool) []byte{
		"default": convertOpenAIResponsesRequestToClaudeDefault,
		"compat":  ConvertOpenAIResponsesRequestToClaudeWithCompat,
	} {
		t.Run(name, func(t *testing.T) {
			if out := convert("claude-test", input, false); len(out) != 0 {
				t.Fatalf("byte entry returned a body for unsupported attachment: %s", out)
			}
		})
	}
	out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude, "claude-test", input, false)
	if err == nil || len(out) != 0 {
		t.Fatalf("checked entry did not preserve rejection: error=%v body=%s", err, out)
	}
}

func TestCPA09ResponsesRequestContractWarnings(t *testing.T) {
	logger := log.StandardLogger()
	hooks, level := logger.ReplaceHooks(make(log.LevelHooks)), logger.GetLevel()
	logger.SetLevel(log.WarnLevel)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(level) })
	for _, tc := range []struct {
		name     string
		input    string
		fragment string
	}{
		{name: "unmapped item", input: `[{"type":"future_item","payload":"do-not-log-raw-payload"},{"role":"user","content":"hi"}]`, fragment: "future_item"},
		{name: "additional tools consumed", input: `[{"type":"additional_tools","tools":[{"type":"function","name":"lookup"}]},{"role":"user","content":"hi"}]`},
		{name: "partial unsupported attachment", input: `[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_audio","input_audio":{"data":"do-not-log-raw-payload","format":"wav"}}]}]`, fragment: "input_audio"},
		{name: "first assistant", input: `[{"role":"assistant","content":"do-not-log-raw-payload"}]`, fragment: "first message is not user"},
		{name: "valid tool turn", input: `[{"role":"user","content":"Run"},{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"done"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, "claude-test", []byte(`{"input":`+tc.input+`}`), false)
			if err != nil || !gjson.ValidBytes(out) {
				t.Fatalf("request failed: %v; output=%s", err, out)
			}
			entries := hook.AllEntries()
			if tc.fragment == "" {
				if len(entries) != 0 {
					t.Fatalf("unexpected warnings: %v", entries)
				}
				return
			}
			if len(entries) != 1 || entries[0].Level != log.WarnLevel || !strings.Contains(entries[0].Message, tc.fragment) || strings.Contains(entries[0].Message, "do-not-log-raw-payload") {
				t.Fatalf("missing or unsafe warning: %v", entries)
			}
		})
	}
}

func TestCPA09ResponsesRequestContractThinkingAfterServerSearch(t *testing.T) {
	signature := validClaudeThinkingSignature()
	input := []byte(`{"input":[{"type":"reasoning","encrypted_content":"` + signature + `","summary":[{"type":"summary_text","text":"reason"}]},{"type":"web_search_call","id":"ws_search","action":{"type":"search","query":"example"},"results":[{"type":"computer_initialize_state","url":"https://example.com","encrypted_content":"synthetic-search"}]},{"type":"function_call","call_id":"tail","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"tail","output":"done"}]}`)
	out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude, "claude-test", input, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCPA09ClaudeToolPairing(t, out)
	parts := gjson.GetBytes(out, "messages.0.content").Array()
	want := []string{"thinking", "server_tool_use", "web_search_tool_result", "thinking", "tool_use"}
	if len(parts) != len(want) {
		t.Fatalf("assistant blocks = %d, want %d; output=%s", len(parts), len(want), out)
	}
	for index, kind := range want {
		if parts[index].Get("type").String() != kind {
			t.Fatalf("block[%d] = %q, want %q; output=%s", index, parts[index].Get("type").String(), kind, out)
		}
	}
	if parts[3].Get("thinking").String() != "reason" || parts[3].Get("signature").String() != signature {
		t.Fatalf("thinking separator changed its signed payload: %s", parts[3].Raw)
	}
}

func TestCPA09ResponsesRequestContractFormalHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{name: "LateOrphanToolResultFoldsToText", input: `{
		"model":"claude-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run ls."}]},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"wait"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"interlude"}]},
			{"type":"function_call_output","call_id":"call_a","output":"a.txt"}
		]
	}`, want: []string{"M:user", "T:Run ls.", "M:assistant", "C:call_a:exec:{}", "M:user", "R:call_a:true", "T:wait", "M:assistant", "T:interlude", "M:user", "T:a.txt"}},
		{name: "EmptyLateOrphanToolResultKeepsNonEmptyUserMessage", input: `{
		"model":"claude-fable-5-1",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run the tool."}]},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Wait."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Interlude."}]},
			{"type":"function_call_output","call_id":"call_a","output":""}
		]
	}`, want: []string{"M:user", "T:Run the tool.", "M:assistant", "C:call_a:exec:{}", "M:user", "R:call_a:true", "T:Wait.", "M:assistant", "T:Interlude.", "M:user", "T:Tool result was empty."}},
		{name: "LateOrphanToolResultArrayOfEmptyTextKeepsMarker", input: `{
		"model":"claude-fable-5-1",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Run."}]},
			{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Wait."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Interlude."}]},
			{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":""},{"type":"input_text","text":""}]}
		]
	}`, want: []string{"M:user", "T:Run.", "M:assistant", "C:a:exec:{}", "M:user", "R:a:true", "T:Wait.", "M:assistant", "T:Interlude.", "M:user", "T:Tool result was empty."}},
		{name: "SanitizationCollisions", input: `{
		"model": "claude-sonnet-5-5",
		"input": "test",
		"stream": false,
		"tools": [
			{"type": "function", "name": "a_b", "description": "ab", "parameters": {"type": "object", "properties": {}}},
			{"type": "function", "name": "a.b", "description": "a.b", "parameters": {"type": "object", "properties": {}}}
		]
	}`, want: []string{"M:user", "T:test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, gjson.Get(tc.input, "model").String(), []byte(tc.input), false)
			if err != nil {
				t.Fatal(err)
			}
			assertCPA09ClaudeToolPairing(t, out)
			var events []string
			for _, message := range gjson.GetBytes(out, "messages").Array() {
				events = append(events, "M:"+message.Get("role").String())
				content := message.Get("content")
				if content.Type == gjson.String {
					events = append(events, "T:"+content.String())
					continue
				}
				for _, part := range content.Array() {
					switch part.Get("type").String() {
					case "text":
						events = append(events, "T:"+part.Get("text").String())
					case "tool_use":
						events = append(events, "C:"+part.Get("id").String()+":"+part.Get("name").String()+":"+part.Get("input").Raw)
					case "tool_result":
						if part.Get("content").String() == "" {
							t.Fatalf("empty interrupted result: %s", part.Raw)
						}
						events = append(events, "R:"+part.Get("tool_use_id").String()+":"+part.Get("is_error").String())
					default:
						t.Fatalf("unexpected history block: %s", part.Raw)
					}
				}
			}
			if strings.Join(events, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("history events=%q, want %q; output=%s", events, tc.want, out)
			}
			if tc.name == "SanitizationCollisions" {
				mapping := util.ChatFunctionNameMap([]string{"a_b", "a.b"})
				tools := gjson.GetBytes(out, "tools").Array()
				if len(tools) != 2 || tools[0].Get("name").String() != mapping["a_b"] || tools[1].Get("name").String() != mapping["a.b"] || mapping["a_b"] == mapping["a.b"] {
					t.Fatalf("shared name mapping changed: %s", out)
				}
			}
		})
	}
}
