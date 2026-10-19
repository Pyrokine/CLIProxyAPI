package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	interactionsclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/interactions/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeInteractionsMediaURI(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, shape := range []string{"wrapped", "bare", "native"} {
			for _, field := range []string{"uri", "file_uri", "fileUri", "url"} {
				for _, media := range []string{"image", "document", "file"} {
					t.Run(fmt.Sprintf("%t/%s/%s/%s", stream, shape, field, media), func(t *testing.T) {
						part := fmt.Sprintf(`{"type":%q,%q:"  HTTP://example.test/media  "}`, media, field)
						input := `[{"type":"user_input","content":[` + part + `]}]`
						switch shape {
						case "bare":
							input = `[` + part + `]`
						case "native":
							input = `{"role":"user","parts":[` + part + `]}`
						}
						body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", []byte(`{"input":`+input+`}`), stream)
						block := gjson.GetBytes(body, "messages.0.content.0")
						wantType := media
						if wantType == "file" {
							wantType = "document"
						}
						if err != nil || block.Get("type").String() != wantType || block.Get("source.type").String() != "url" || block.Get("source.url").String() != "HTTP://example.test/media" {
							t.Fatalf("URI conversion = (%s, %v)", body, err)
						}
					})
				}
			}
		}
	}
}

func TestClaudeInteractionsRejectedBodies(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, uri := range []string{"gs://bucket/a.pdf", "files/abc", "ftp://example.test/a", "data:image/png;base64,aGk=", "https://", ""} {
			for _, note := range []string{
				`{"role":"developer","content":"note"}`,
				`{"type":"system","role":"user","text":"note"}`,
				`{"role":"developer","steps":[{"role":"user","content":"note"}]}`,
			} {
				t.Run(fmt.Sprintf("%t/%s/%s", stream, uri, note), func(t *testing.T) {
					input := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":"history"},{"type":"model_output","content":"reply"},{"type":"user_input","content":[{"type":"document","uri":%q}]},%s]}`, uri, note))
					body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", input, stream)
					var unsupported *translatorcommon.UnsupportedPartError
					if !errors.As(err, &unsupported) || unsupported.Type != "document" || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || len(body) != 0 {
						t.Fatalf("rejected request = (%s, %v)", body, err)
					}
					if legacy := sdktranslator.TranslateRequest(sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", input, stream); len(legacy) != 0 {
						t.Fatalf("byte entrypoint retained rejected body: %s", legacy)
					}
				})
			}
		}
		for _, parts := range []string{
			`{"type":"text","text":" \n\t"},{"type":"image","source":{"type":"file","file_id":"file-1"}}`,
			`{"type":"container_upload","file_id":"file-1"}`,
		} {
			input := []byte(`{"messages":[{"role":"system","content":"note"},{"role":"user","content":[` + parts + `]}]}`)
			body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "test-model", input, stream)
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(err, &unsupported) || len(body) != 0 || !unsupported.IsRequestScoped() {
				t.Fatalf("Claude rejection = (%s, %v)", body, err)
			}
			if legacy := interactionsclaude.ConvertClaudeRequestToInteractionsWithCompat("test-model", input, stream); len(legacy) != 0 {
				t.Fatalf("compatibility byte entrypoint retained rejected body: %s", legacy)
			}
		}
	}
}

func TestClaudeInteractionsToolResultRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","is_error":true,"content":[{"type":"text","text":"failed","exit_code":2},{"text":"retry","retryable":true},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]}`)
		body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "test-model", input, stream)
		result := gjson.GetBytes(body, "input.1")
		if err != nil || result.Get("name").String() != "lookup" || !result.Get("is_error").Bool() || result.Get("result.0.exit_code").Int() != 2 || !result.Get("result.1.retryable").Bool() || result.Get("result.2.data").String() != "aGk=" {
			t.Fatalf("tool result = (%s, %v)", body, err)
		}
		replayed, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", []byte(`{"input":`+gjson.GetBytes(body, "input").Raw+`}`), stream)
		blocks := gjson.GetBytes(replayed, "messages.1.content.0.content")
		if err != nil || gjson.Get(blocks.Get("0.text").String(), "exit_code").Int() != 2 || !gjson.Get(blocks.Get("1.text").String(), "retryable").Bool() || blocks.Get("2.source.data").String() != "aGk=" {
			t.Fatalf("replayed result = (%s, %v)", replayed, err)
		}
		plain, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", []byte(`{"input":[{"type":"function_result","call_id":"a","result":"plain result"}]}`), stream)
		if err != nil || gjson.GetBytes(plain, "messages.0.content.0.content").String() != "plain result" {
			t.Fatalf("string result = (%s, %v)", plain, err)
		}
	}
}

func TestClaudeInteractionsToolNameRoundTrip(t *testing.T) {
	input := []byte(`{"tools":[{"name":"a b","parameters":{}},{"name":"a_b","parameters":{}}],"generation_config":{"tool_choice":{"type":"function","name":"a b"}},"input":[{"type":"function_call","id":"a","name":"a b","arguments":{}}]}`)
	body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatClaude, "claude-test", input, true)
	if err != nil {
		t.Fatal(err)
	}
	first := gjson.GetBytes(body, "tools.0.name").String()
	second := gjson.GetBytes(body, "tools.1.name").String()
	if first == "" || first == second || util.SanitizeClaudeFunctionName(first) != first || gjson.GetBytes(body, "tool_choice.name").String() != first || gjson.GetBytes(body, "messages.0.content.0.name").String() != first {
		t.Fatalf("tool identities = %s", body)
	}
	response := []byte(fmt.Sprintf(`{"content":[{"type":"tool_use","id":"b","name":%q,"input":{}}]}`, first))
	translated := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "claude-test", input, body, response, nil)
	if gjson.GetBytes(translated, "steps.0.name").String() != "a b" {
		t.Fatalf("nonstream identity = %s", translated)
	}
	var state any
	frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "claude-test", input, body, []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"b","name":%q,"input":{}}}`, first)), &state)
	found := false
	for _, payload := range claudeInteractionsPayloads(frames) {
		if payload.Get("event_type").String() == "step.start" && payload.Get("step.type").String() == "function_call" {
			found = payload.Get("step.name").String() == "a b"
		}
	}
	if !found {
		t.Fatalf("streaming identity = %s", frames)
	}
}

func TestClaudeInteractionsProviderFailure(t *testing.T) {
	const failure = `{"type":"error","error":{"type":"overloaded_error","message":"provider busy"}}`
	for _, response := range []string{failure, "data: " + failure + "\n"} {
		translated := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "claude-test", nil, nil, []byte(response), nil)
		if gjson.GetBytes(translated, "status").String() != "failed" || gjson.GetBytes(translated, "error.message").String() != "provider busy" {
			t.Fatalf("failure reported as success: %s", translated)
		}
	}
	var state any
	frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "claude-test", nil, nil, []byte("data: "+failure), &state)
	found := false
	for _, payload := range claudeInteractionsPayloads(frames) {
		if payload.Get("event_type").String() == "interaction.completed" {
			t.Fatalf("failure emitted a success event: %s", frames)
		}
		if payload.Get("event_type").String() == "interaction.failed" {
			found = payload.Get("interaction.error.message").String() == "provider busy"
		}
	}
	if !found {
		t.Fatalf("missing failure event: %s", frames)
	}
	for _, late := range []string{`data: {"type":"message_delta","usage":{"output_tokens":1}}`, `data: {"type":"message_stop"}`, "data: [DONE]"} {
		if frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatInteractions, "claude-test", nil, nil, []byte(late), &state); len(frames) != 0 {
			t.Fatalf("late event emitted after failure: %s", frames)
		}
	}
}

func claudeInteractionsPayloads(frames [][]byte) []gjson.Result {
	var payloads []gjson.Result
	for _, frame := range frames {
		for _, line := range strings.Split(string(frame), "\n") {
			if strings.HasPrefix(line, "data:") {
				payloads = append(payloads, gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
			}
		}
	}
	return payloads
}
