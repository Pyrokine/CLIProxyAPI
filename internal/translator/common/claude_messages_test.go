package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeMessageAccumulatorGroupsAndOrdersAssistantParts(t *testing.T) {
	accumulator := NewClaudeMessageAccumulator(3)
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"first","input":{}}]}`))
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"answer"}]}`))
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"second","input":{}}]}`))

	messages := accumulator.Messages()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(messages))
	}
	content := gjson.GetBytes(messages[0], "content").Array()
	wantTypes := []string{"thinking", "text", "tool_use", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("content count = %d, want %d. Message: %s", len(content), len(wantTypes), string(messages[0]))
	}
	for i, wantType := range wantTypes {
		if got := content[i].Get("type").String(); got != wantType {
			t.Fatalf("content[%d].type = %q, want %q", i, got, wantType)
		}
	}
	if got := content[2].Get("id").String(); got != "call_1" {
		t.Fatalf("first tool_use id = %q, want call_1", got)
	}
	if got := content[3].Get("id").String(); got != "call_2" {
		t.Fatalf("second tool_use id = %q, want call_2", got)
	}
}

func TestClaudeMessageAccumulatorPreservesUserOrderAndRoleBoundaries(t *testing.T) {
	accumulator := NewClaudeMessageAccumulator(3)
	accumulator.Append([]byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}`))
	accumulator.Append([]byte(`{"role":"user","content":[{"type":"text","text":"continue"}]}`))
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"text","text":"done"}]}`))

	messages := accumulator.Messages()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	if got := gjson.GetBytes(messages[0], "role").String(); got != "user" {
		t.Fatalf("messages[0].role = %q, want user", got)
	}
	if got := gjson.GetBytes(messages[0], "content.0.type").String(); got != "tool_result" {
		t.Fatalf("first user block type = %q, want tool_result", got)
	}
	if got := gjson.GetBytes(messages[0], "content.1.text").String(); got != "continue" {
		t.Fatalf("second user block text = %q, want continue", got)
	}
	if got := gjson.GetBytes(messages[1], "role").String(); got != "assistant" {
		t.Fatalf("messages[1].role = %q, want assistant", got)
	}
}

func TestClaudeMessageAccumulatorSkipsEmptyMessagesWithoutBreakingTurn(t *testing.T) {
	accumulator := NewClaudeMessageAccumulator(3)
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"text","text":"first"}]}`))
	accumulator.Append([]byte(`{"role":"user"}`))
	accumulator.Append([]byte(`{"role":"user","content":null}`))
	accumulator.Append([]byte(`{"role":"user","content":""}`))
	accumulator.Append([]byte(`{"role":"user","content":[]}`))
	accumulator.Append([]byte(`{"role":"invalid","content":[{"type":"text","text":"ignored"}]}`))
	accumulator.Append([]byte(`{"role":"assistant","content":[{"type":"text","text":"second"}]}`))

	messages := accumulator.Messages()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(messages))
	}
	if got := gjson.GetBytes(messages[0], "content.#").Int(); got != 2 {
		t.Fatalf("assistant content count = %d, want 2. Message: %s", got, string(messages[0]))
	}
}

func TestClaudeMessageAccumulatorFlushPreservesExplicitBoundary(t *testing.T) {
	accumulator := NewClaudeMessageAccumulator(2)
	accumulator.Append([]byte(`{"role":"user","content":"system reminder"}`))
	accumulator.Flush()
	accumulator.Append([]byte(`{"role":"user","content":[{"type":"text","text":"question"}]}`))

	messages := accumulator.Messages()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	if got := gjson.GetBytes(messages[0], "content.0.text").String(); got != "system reminder" {
		t.Fatalf("first message text = %q, want system reminder", got)
	}
	if got := gjson.GetBytes(messages[1], "content.0.text").String(); got != "question" {
		t.Fatalf("second message text = %q, want question", got)
	}
}

func TestClaudeMessageAccumulatorPreservesBlockCacheControl(t *testing.T) {
	accumulator := NewClaudeMessageAccumulator(2)
	accumulator.Append([]byte(`{"role":"user","content":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}]}`))
	accumulator.Append([]byte(`{"role":"user","content":[{"type":"text","text":"fresh"}]}`))

	messages := accumulator.Messages()
	if got := gjson.GetBytes(messages[0], "content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("cache_control.type = %q, want ephemeral", got)
	}
	if gjson.GetBytes(messages[0], "content.1.cache_control").Exists() {
		t.Fatalf("second block should not have cache_control: %s", string(messages[0]))
	}
}

func TestAlignClaudeToolResultsSupportsPartialNonContiguousResults(t *testing.T) {
	content := gjson.Parse(`[
		{"type":"tool_result","tool_use_id":"call-c","content":"three"},
		{"type":"text","text":"keep text here"},
		{"type":"tool_result","tool_use_id":"external","content":"external"},
		{"type":"image","source":{"type":"base64","data":"image"}},
		{"type":"tool_result","tool_use_id":"call-a","content":"one"}
	]`)

	aligned := AlignClaudeToolResults(content, []string{"call-a", "call-b", "call-c"})
	parts := aligned.Array()
	if got := parts[0].Get("tool_use_id").String(); got != "call-a" {
		t.Fatalf("first matched result = %q, want call-a", got)
	}
	if got := parts[1].Get("text").String(); got != "keep text here" {
		t.Fatalf("text slot moved or changed: %q", got)
	}
	if got := parts[2].Get("tool_use_id").String(); got != "external" {
		t.Fatalf("unmatched result slot moved or changed: %q", got)
	}
	if got := parts[3].Get("source.data").String(); got != "image" {
		t.Fatalf("image slot moved or changed: %q", got)
	}
	if got := parts[4].Get("tool_use_id").String(); got != "call-c" {
		t.Fatalf("second matched result = %q, want call-c", got)
	}
}

func TestAlignClaudeToolResultsMatchesRawIDsExactly(t *testing.T) {
	content := gjson.Parse(`[
		{"type":"tool_result","tool_use_id":"call_a","content":"underscore"},
		{"type":"tool_result","tool_use_id":"call.a","content":"dot"}
	]`)

	aligned := AlignClaudeToolResults(content, []string{"call.a", "call_a"})
	parts := aligned.Array()
	if got := parts[0].Get("content").String(); got != "dot" {
		t.Fatalf("raw ID call.a result = %q, want dot", got)
	}
	if got := parts[1].Get("content").String(); got != "underscore" {
		t.Fatalf("raw ID call_a result = %q, want underscore", got)
	}
}

func TestAlignGeminiFunctionResponsesPreservesOtherSlots(t *testing.T) {
	parts := gjson.Parse(`[
		{"functionResponse":{"id":"call_a","name":"second","response":{"result":"underscore"}}},
		{"text":"keep text"},
		{"functionResponse":{"id":"external","name":"external","response":{"result":{"$ref":"#/Item"}}}},
		{"inlineData":{"mime_type":"image/png","data":"image"}},
		{"functionResponse":{"id":"call.a","name":"first","response":{"result":"dot"}}}
	]`)
	aligned := AlignGeminiFunctionResponses(parts, []string{"call.a", "missing", "call_a"})
	for path, want := range map[string]string{
		"0.functionResponse.id":                   "call.a",
		"0.functionResponse.response.result":      "dot",
		"1.text":                                  "keep text",
		"2.functionResponse.id":                   "external",
		"2.functionResponse.response.result.$ref": "#/Item",
		"3.inlineData.data":                       "image",
		"4.functionResponse.id":                   "call_a",
		"4.functionResponse.response.result":      "underscore",
	} {
		if got := aligned.Get(path).String(); got != want {
			t.Fatalf("%s=%q, want %q; parts=%s", path, got, want, aligned.Raw)
		}
	}
}

func TestClaudeToolResultContent(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		images            []string
	}{
		{"string", `"alpha\nquoted"`, `"alpha\nquoted"`, nil},
		{"object", `{"value":1}`, `{"value":1}`, nil},
		{"number", `42`, `42`, nil},
		{"null", `null`, `null`, nil},
		{"empty array", `[]`, `[]`, nil},
		{"text blocks", `[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]`, `{"text":"alpha\nbeta"}`, nil},
		{"text and images", `[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"one"}},{"type":"text","text":"beta"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"two"}}]`, `{"text":"alpha\nbeta"}`, []string{"one", "two"}},
		{"image only", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"one"}}]`, `{"text":""}`, []string{"one"}},
		{"opaque blocks", `[{"type":"text","text":"alpha"},{"$ref":"#/Item"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"one"}}]`, `[{"type":"text","text":"alpha"},{"$ref":"#/Item"}]`, []string{"one"}},
		{"URL image preserved as JSON", `[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}]`, `[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}]`, nil},
		{"incomplete image preserved as JSON", `[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]`, `[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, images := ClaudeToolResultContent(gjson.Parse(tc.input))
			if result.Raw != tc.want || len(images) != len(tc.images) {
				t.Fatalf("result=%s, images=%v, want %s and %v", result.Raw, images, tc.want, tc.images)
			}
			for index, data := range tc.images {
				if source := images[index]; source.Get("type").String() != "base64" || source.Get("media_type").String() == "" || source.Get("data").String() != data {
					t.Fatalf("image[%d]=%s, want base64 data %q", index, source.Raw, data)
				}
			}
		})
	}
}
