package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeRegisteredToolResultContracts(t *testing.T) {
	for _, test := range []struct {
		name, messages              string
		roles, ids, names, contents []string
	}{
		{
			name:     "complete parallel results",
			messages: `[{"role":"user","content":"fixture"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"first","input":{"n":9007199254740993}},{"type":"tool_use","id":"b","name":"second","input":{}}]},{"role":"assistant","content":[{"type":"text","text":"waiting"}]},{"role":"user","content":"reminder"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"second result"},{"type":"tool_result","tool_use_id":"a","content":"first result"}]}]`,
			roles:    []string{"user", "assistant", "tool", "tool", "assistant", "user"},
			ids:      []string{"b", "a"}, names: []string{"second", "first"}, contents: []string{"second result", "first result"},
		},
		{
			name:     "unknown result ID",
			messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"orphan result"}]}]`,
			roles:    []string{"tool"}, ids: []string{"orphan"}, names: []string{""}, contents: []string{"orphan result"},
		},
		{
			name:     "incomplete calls and orphan result",
			messages: `[{"role":"user","content":"fixture"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"first","input":{}},{"type":"tool_use","id":"b","name":"second","input":{}}]},{"role":"user","content":"waiting"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"orphan result"}]}]`,
			roles:    []string{"user", "assistant", "user", "tool"}, ids: []string{"orphan"}, names: []string{""}, contents: []string{"orphan result"},
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.RequestEnvelope{
					Format: sdktranslator.FormatClaude, Model: "fixture-openai", Body: []byte(`{"messages":` + test.messages + `}`), Stream: stream,
				})
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				var roles []string
				var results []gjson.Result
				for _, message := range gjson.GetBytes(envelope.Body, "messages").Array() {
					role := message.Get("role").String()
					roles = append(roles, role)
					if role == "tool" {
						results = append(results, message)
					}
				}
				if strings.Join(roles, ",") != strings.Join(test.roles, ",") || len(results) != len(test.ids) {
					t.Fatalf("tool adjacency or incomplete history changed: %s", envelope.Body)
				}
				for index, result := range results {
					if result.Get("tool_call_id").String() != test.ids[index] || result.Get("name").String() != test.names[index] || result.Get("content").String() != test.contents[index] {
						t.Fatalf("result identity/content changed: %s", result.Raw)
					}
					if test.names[index] == "" && result.Get("name").Exists() {
						t.Fatalf("unknown result acquired a name: %s", result.Raw)
					}
				}
				if test.name == "complete parallel results" && gjson.GetBytes(envelope.Body, "messages.1.tool_calls.0.function.arguments").String() != `{"n":9007199254740993}` {
					t.Fatalf("call argument precision changed: %s", envelope.Body)
				}
			})
		}
	}
}

func TestClaudeRegisteredAttachmentContracts(t *testing.T) {
	for _, test := range []struct {
		name, content, wantText, wantFile string
		wantError                         bool
	}{
		{"uncached file only", `[{"type":"container_upload","file_id":"file-absent"}]`, "", "", true},
		{"text and uncached file", `[{"type":"text","text":"fixture"},{"type":"container_upload","file_id":"file-absent"}]`, "fixture", "", false},
		{"base64 document", `[{"type":"document","filename":"fixture.pdf","source":{"type":"base64","media_type":"application/pdf","data":"Zml4dHVyZQ=="}}]`, "", "data:application/pdf;base64,Zml4dHVyZQ==", false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.RequestEnvelope{
					Format: sdktranslator.FormatClaude, Model: "fixture-openai", Body: []byte(`{"messages":[{"role":"user","content":` + test.content + `}]}`), Stream: stream,
				})
				if test.wantError {
					var partError *translatorcommon.UnsupportedPartError
					if !errors.As(envelope.Err, &partError) || partError.StatusCode() != 400 || !partError.IsRequestScoped() {
						t.Fatalf("file-only request was not rejected: body=%s error=%v", envelope.Body, envelope.Err)
					}
					return
				}
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				part := gjson.GetBytes(envelope.Body, "messages.0.content.0")
				if part.Get("text").String() != test.wantText || part.Get("file.file_data").String() != test.wantFile {
					t.Fatalf("sendable user content changed: %s", envelope.Body)
				}
				if test.wantFile != "" && part.Get("file.filename").String() != "fixture.pdf" {
					t.Fatalf("document filename changed: %s", part.Raw)
				}
				if test.wantFile == "" && strings.Contains(string(envelope.Body), "file_data") {
					t.Fatalf("uncached file was fabricated: %s", envelope.Body)
				}
			})
		}
	}
}

func TestClaudeRegisteredToolChoiceContracts(t *testing.T) {
	for _, test := range []struct{ name, choice, want, parallel string }{
		{"none", `{"type":"none"}`, `"none"`, ""},
		{"serial auto", `{"type":"auto","disable_parallel_tool_use":true}`, `"auto"`, "false"},
		{"any", `{"type":"any"}`, `"required"`, ""},
		{"named", `{"type":"tool","name":"fixture"}`, `{"type":"function","function":{"name":"fixture"}}`, ""},
		{"unknown", `{"type":"unknown"}`, `"none"`, ""},
		{"empty name", `{"type":"tool","name":""}`, `"none"`, ""},
		{"null", `null`, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.RequestEnvelope{
				Format: sdktranslator.FormatClaude, Model: "fixture-openai", Body: []byte(`{"messages":[{"role":"user","content":"fixture"}],"tool_choice":` + test.choice + `}`),
			})
			if envelope.Err != nil {
				t.Fatal(envelope.Err)
			}
			if gjson.GetBytes(envelope.Body, "tool_choice").Raw != test.want || gjson.GetBytes(envelope.Body, "parallel_tool_calls").Raw != test.parallel {
				t.Fatalf("choice restriction changed: %s", envelope.Body)
			}
		})
	}
}
