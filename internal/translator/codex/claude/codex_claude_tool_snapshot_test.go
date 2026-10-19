package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexClaudeRegisteredArgumentSnapshots(t *testing.T) {
	cases := []struct {
		name    string
		events  []string
		wantErr bool
	}{
		{"prefix completion", []string{
			`{"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":"{\"v\":"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":1}"}`,
			`{"type":"response.output_item.done","item":{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup","arguments":"{\"v\":1}"}}`,
		}, false},
		{"repeated complete snapshot", []string{
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":1}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":1}"}`,
			`{"type":"response.output_item.done","item":{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup","arguments":"{\"v\":1}"}}`,
		}, false},
		{"conflicting prefix", []string{
			`{"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":"{\"v\":1}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":2}"}`,
		}, true},
		{"changed completed snapshot", []string{
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":1}"}`,
			`{"type":"response.output_item.done","item":{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup","arguments":"{\"v\":12}"}}`,
		}, true},
		{"changed terminal snapshot", []string{
			`{"type":"response.output_item.done","item":{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup","arguments":"{\"v\":1}"}}`,
			`{"type":"response.completed","response":{"output":[{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup","arguments":"{\"v\":2}"}]}}`,
		}, true},
		{"empty completed snapshot", []string{
			`{"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":"{\"v\":"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":""}`,
		}, true},
		{"delta after completed snapshot", []string{
			`{"type":"response.function_call_arguments.done","item_id":"fc_fixture","arguments":"{\"v\":1}"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":" "}`,
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			request := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)
			events := []string{`{"type":"response.output_item.added","item":{"id":"fc_fixture","type":"function_call","call_id":"call_fixture","name":"lookup"},"output_index":0}`}
			events = append(events, tc.events...)
			var output []byte
			for _, event := range events {
				frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatCodex, sdktranslator.FormatClaude, "gpt-fixture", request, nil, []byte("data: "+event), &state)
				output = append(output, bytes.Join(frames, nil)...)
			}
			err := sdktranslator.ResponseTranslationError(&state)
			if tc.wantErr {
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 502 {
					t.Fatalf("translation error = %v, want SDK 502; output=%s", err, output)
				}
				frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatCodex, sdktranslator.FormatClaude, "gpt-fixture", request, nil, []byte(`data: {"type":"response.completed","response":{}}`), &state)
				if bytes.Contains(bytes.Join(frames, nil), []byte(`"message_stop"`)) || bytes.Contains(output, []byte(`"message_stop"`)) {
					t.Fatalf("conflicting snapshot emitted success: %s %q", output, frames)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var arguments strings.Builder
			for _, line := range strings.Split(string(output), "\n") {
				if strings.HasPrefix(line, "data: ") {
					root := gjson.Parse(strings.TrimPrefix(line, "data: "))
					if root.Get("delta.type").String() == "input_json_delta" {
						arguments.WriteString(root.Get("delta.partial_json").String())
					}
				}
			}
			if arguments.String() != `{"v":1}` {
				t.Fatalf("arguments = %q, want complete snapshot once; output=%s", arguments.String(), output)
			}
		})
	}
}
