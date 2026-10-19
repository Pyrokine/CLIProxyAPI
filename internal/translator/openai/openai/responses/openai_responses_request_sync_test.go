package responses

import (
	"context"
	"errors"
	"fmt"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesToolOutputsDoNotFabricateToolPairings(t *testing.T) {
	cases := []struct {
		name  string
		input string
		roles []string
	}{
		{
			name:  "orphan explicit output is user content",
			input: `[{"type":"function_call_output","call_id":"unknown","output":"reported"}]`,
			roles: []string{"user"},
		},
		{
			name: "ambiguous missing call ID is user content",
			input: `[
				{"type":"function_call","call_id":"first","name":"one","arguments":"{}"},
				{"type":"function_call","call_id":"second","name":"two","arguments":"{}"},
				{"type":"function_call_output","output":"which one?"}
			]`,
			roles: []string{"assistant", "user"},
		},
		{
			name: "duplicate output ID only matches once",
			input: `[
				{"type":"function_call","call_id":"first","name":"one","arguments":"{}"},
				{"type":"function_call_output","call_id":"first","output":"ok"},
				{"type":"function_call_output","call_id":"first","output":"again"}
			]`,
			roles: []string{"assistant", "tool", "user"},
		},
		{
			name:  "empty orphan output is omitted",
			input: `[{"type":"function_call_output","call_id":"unknown","output":""}]`,
			roles: []string{},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(`{"input":`+test.input+`}`), false)
			messages := gjson.GetBytes(output, "messages").Array()
			if len(messages) != len(test.roles) {
				t.Fatalf("roles = %v, want %v; output = %s", messages, test.roles, output)
			}
			for index, role := range test.roles {
				if got := messages[index].Get("role").String(); got != role {
					t.Fatalf("messages[%d] role = %q, want %q; output = %s", index, got, role, output)
				}
			}
		})
	}
}

func TestResponsesMaxOutputTokensAreNotSilentlyCoercedToZero(t *testing.T) {
	for _, raw := range []string{`"invalid"`, `null`, `9007199254740993`} {
		output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(`{"max_output_tokens":`+raw+`,"input":"hello"}`), false)
		if got := gjson.GetBytes(output, "max_tokens").Raw; got != raw {
			t.Fatalf("max_tokens = %s, want %s; output = %s", got, raw, output)
		}
	}
}

func TestResponsesVideoPartPreservesProviderValidation(t *testing.T) {
	input := []byte(`{"input":[{"role":"user","content":[{"type":"input_video","video_url":"https://example.test/clip.mp4","processing":{"fps":2}},{"type":"video_url"}]}]}`)
	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", input, false)
	if got := gjson.GetBytes(output, "messages.0.content.0.video_url.url").String(); got != "https://example.test/clip.mp4" {
		t.Fatalf("video URL = %q; output = %s", got, output)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.video_url.processing.fps").Int(); got != 2 {
		t.Fatalf("video processing fps = %d; output = %s", got, output)
	}
	if !gjson.GetBytes(output, "messages.0.content.1.video_url").Exists() {
		t.Fatalf("malformed video was silently discarded; output = %s", output)
	}
}

func TestResponsesToolCallReasoningFollowsSessionIntent(t *testing.T) {
	cases := []struct {
		name  string
		input string
		index int
		want  string
	}{
		{
			name:  "reasoning requested without summary",
			input: `{"reasoning":{"effort":"medium"},"input":[{"type":"function_call","call_id":"a","name":"fetch","arguments":"{}"}]}`,
			want:  "[reasoning unavailable]",
		},
		{
			name: "reuse latest reasoning in the same assistant turn",
			input: `{"input":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"inspect"}]},
				{"type":"function_call","call_id":"a","name":"fetch","arguments":"{}"},
				{"type":"function_call_output","call_id":"a","output":"ok"},
				{"type":"function_call","call_id":"b","name":"fetch","arguments":"{}"}
			]}`,
			index: 2,
			want:  "inspect",
		},
		{
			name: "user message clears previous reasoning",
			input: `{"input":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"inspect"}]},
				{"type":"function_call","call_id":"a","name":"fetch","arguments":"{}"},
				{"type":"function_call_output","call_id":"a","output":"ok"},
				{"role":"user","content":"next"},
				{"type":"function_call","call_id":"b","name":"fetch","arguments":"{}"}
			]}`,
			index: 3,
			want:  "[reasoning unavailable]",
		},
		{
			name:  "no reasoning intent",
			input: `{"reasoning":{"effort":"none"},"input":[{"type":"function_call","call_id":"a","name":"fetch","arguments":"{}"}]}`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(test.input), false)
			got := gjson.GetBytes(output, "messages").Array()[test.index].Get("reasoning_content").String()
			if got != test.want {
				t.Fatalf("reasoning_content = %q, want %q; output = %s", got, test.want, output)
			}
		})
	}
}

func TestResponsesVideoTranslationPreservesURLAndProcessing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		part           string
		wantURL        string
		wantProcessing string
	}{
		{name: "remote URL", part: `{"type":"input_video","video_url":"https://example.test/clip.mp4?part=1&name=a%20b"}`, wantURL: `"https://example.test/clip.mp4?part=1&name=a%20b"`},
		{name: "data URL", part: `{"type":"input_video","video_url":"data:video/mp4;base64,AAECAwQ="}`, wantURL: `"data:video/mp4;base64,AAECAwQ="`},
		{name: "object URL", part: `{"type":"video_url","video_url":{"url":"https://example.test/clip.webm","processing":"static"}}`, wantURL: `"https://example.test/clip.webm"`, wantProcessing: `"static"`},
		{name: "override processing", part: `{"type":"input_video","video_url":{"url":"https://example.test/clip.mp4","processing":"static"},"processing":"agentic"}`, wantURL: `"https://example.test/clip.mp4"`, wantProcessing: `"agentic"`},
		{name: "missing URL", part: `{"type":"input_video"}`},
		{name: "invalid URL", part: `{"type":"input_video","video_url":123}`, wantURL: `123`},
	} {
		for _, stream := range []bool{false, true} {
			name := tc.name + "/nonstream"
			if stream {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				input := []byte(`{"input":[{"role":"user","content":[` + tc.part + `]}]}`)
				output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("video-model", input, stream)
				parts := gjson.GetBytes(output, "messages.0.content").Array()
				if len(parts) != 1 || parts[0].Get("type").String() != "video_url" {
					t.Fatalf("video part missing in output: %s", output)
				}
				if got := parts[0].Get("video_url.url").Raw; got != tc.wantURL {
					t.Fatalf("video URL = %s, want %s; output=%s", got, tc.wantURL, output)
				}
				if got := parts[0].Get("video_url.processing").Raw; got != tc.wantProcessing {
					t.Fatalf("video processing = %s, want %s; output=%s", got, tc.wantProcessing, output)
				}
			})
		}
	}
}

func TestResponsesVideoTranslationPreservesMixedMediaOrder(t *testing.T) {
	input := []byte(`{"input":[{"role":"user","content":[
		{"type":"input_text","text":"first"},
		{"type":"input_video","video_url":"https://example.test/first.mp4"},
		{"type":"input_image","image_url":"https://example.test/frame.png"},
		{"type":"input_video","video_url":"data:video/mp4;base64,AAECAwQ=","processing":"static"},
		{"type":"input_text","text":"last"}
	]}]}`)
	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("video-model", input, false)
	parts := gjson.GetBytes(output, "messages.0.content").Array()
	if len(parts) != 5 {
		t.Fatalf("mixed media parts = %d, want 5; output=%s", len(parts), output)
	}
	for index, wantType := range []string{"text", "video_url", "image_url", "video_url", "text"} {
		if got := parts[index].Get("type").String(); got != wantType {
			t.Fatalf("part %d type = %q, want %q; output=%s", index, got, wantType, output)
		}
	}
	if parts[0].Get("text").String() != "first" || parts[1].Get("video_url.url").String() != "https://example.test/first.mp4" || parts[2].Get("image_url.url").String() != "https://example.test/frame.png" || parts[3].Get("video_url.processing").String() != "static" || parts[4].Get("text").String() != "last" {
		t.Fatalf("mixed media content changed: %s", output)
	}
}

func TestResponsesChatAttachmentMappingAndRequestRejection(t *testing.T) {
	cases := []struct {
		name, parts, wantPath, wantValue, unsupported string
	}{
		{name: "file ID", parts: `{"type":"input_file","file_id":"file-offline"}`, wantPath: "file.file_id", wantValue: "file-offline"},
		{name: "file data", parts: `{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}`, wantPath: "file.file_data", wantValue: "data:application/pdf;base64,JVBERi0="},
		{name: "audio", parts: `{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav","opaque":"kept"}}`, wantPath: "input_audio.opaque", wantValue: "kept"},
		{name: "unsupported file URL", parts: `{"type":"input_file","file_url":"https://example.test/a.pdf"}`, unsupported: "input_file"},
		{name: "numeric file ID", parts: `{"type":"input_file","file_id":123}`, unsupported: "input_file"},
		{name: "missing audio bytes", parts: `{"type":"input_audio","input_audio":{"format":"wav"}}`, unsupported: "input_audio"},
		{name: "numeric audio bytes", parts: `{"type":"input_audio","input_audio":{"data":123,"format":"wav"}}`, unsupported: "input_audio"},
		{name: "missing audio format", parts: `{"type":"input_audio","input_audio":{"data":"UklGRg=="}}`, wantPath: "input_audio.data", wantValue: "UklGRg=="},
		{name: "top-level audio", parts: `{"type":"input_audio","data":"UklGRg==","format":"wav"}`, wantPath: "input_audio.format", wantValue: "wav"},
		{name: "top-level audio without format", parts: `{"type":"input_audio","data":"UklGRg=="}`, wantPath: "input_audio.data", wantValue: "UklGRg=="},
		{name: "nested data with top-level format", parts: `{"type":"input_audio","input_audio":{"data":"UklGRg=="},"format":"mp3"}`, wantPath: "input_audio.format", wantValue: "mp3"},
		{name: "top-level data with nested metadata", parts: `{"type":"input_audio","data":"UklGRg==","input_audio":{"format":"wav","opaque":"kept"}}`, wantPath: "input_audio.opaque", wantValue: "kept"},
		{name: "nested audio data takes precedence", parts: `{"type":"input_audio","data":"ignored","format":"mp3","input_audio":{"data":"UklGRg==","format":"wav"}}`, wantPath: "input_audio.data", wantValue: "UklGRg=="},
		{name: "empty text beside file URL", parts: `{"type":"input_text","text":" "},{"type":"input_file","file_url":"https://example.test/a.pdf"}`, unsupported: "input_file"},
		{name: "text beside file URL", parts: `{"type":"input_text","text":"keep"},{"type":"input_file","file_url":"https://example.test/a.pdf"}`, wantPath: "text", wantValue: "keep"},
		{name: "numeric image URL", parts: `{"type":"input_image","image_url":123}`, unsupported: "input_image"},
	}
	for _, test := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				request := []byte(`{"instructions":"system","input":[{"role":"user","content":"history"},{"role":"assistant","content":"reply"},{"role":"user","content":[` + test.parts + `]},{"role":"assistant","content":"later"},{"role":"user","content":"next"}]}`)
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, "offline-model", request, stream)
				if test.unsupported != "" {
					var unsupported *translatorcommon.UnsupportedPartError
					if !errors.As(err, &unsupported) || unsupported.Type != test.unsupported || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || body != nil {
						t.Fatalf("request accepted an emptied turn: body=%s error=%v", body, err)
					}
					if legacy := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline-model", request, stream); legacy != nil {
						t.Fatalf("byte wrapper hid the request error: %s", legacy)
					}
					return
				}
				if err != nil || !gjson.ValidBytes(body) {
					t.Fatalf("valid attachment failed: body=%s error=%v", body, err)
				}
				if value := gjson.GetBytes(body, "messages.3.content.0."+test.wantPath).String(); value != test.wantValue {
					t.Fatalf("attachment value=%q, want %q: %s", value, test.wantValue, body)
				}
			})
		}
	}
}

func TestResponsesChatDeveloperContentDoesNotMaskUserAttachmentDrops(t *testing.T) {
	for _, test := range []struct {
		name, input string
		wantError   bool
	}{
		{name: "developer attachment", input: `{"role":"developer","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]}`},
		{name: "developer attachment then user text", input: `{"role":"developer","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]},{"role":"user","content":"send"}`},
		{name: "developer text then empty user attachment", input: `{"role":"developer","content":"instructions"},{"role":"user","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]}`, wantError: true},
		{name: "empty user attachment then developer text", input: `{"role":"user","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]},{"role":"developer","content":"instructions"}`, wantError: true},
		{name: "developer attachment then empty user attachment", input: `{"role":"developer","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]},{"role":"user","content":[{"type":"input_file","file_url":"https://example.test/a.pdf"}]}`, wantError: true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				request := []byte(`{"instructions":"system","input":[` + test.input + `]}`)
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, "offline-model", request, stream)
				if test.wantError {
					var unsupported *translatorcommon.UnsupportedPartError
					if body != nil || !errors.As(err, &unsupported) || unsupported.Type != "input_file" || unsupported.StatusCode() != 400 {
						t.Fatalf("developer content masked a user attachment drop: body=%s error=%v", body, err)
					}
					return
				}
				if err != nil || !gjson.ValidBytes(body) || gjson.GetBytes(body, "messages.1.role").String() != "user" {
					t.Fatalf("developer content was refused or lost its mapped role: body=%s error=%v", body, err)
				}
			})
		}
	}
}

func TestResponsesChatImageDetailAndToolOutputTypes(t *testing.T) {
	for _, kind := range []string{"input_image", "image_url"} {
		for _, invalidField := range []string{"", "url", "detail"} {
			t.Run(kind+"/"+invalidField, func(t *testing.T) {
				part := `{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"}`
				if kind == "image_url" {
					part = `{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"original"}}`
				}
				if invalidField != "" {
					if kind == "input_image" {
						if invalidField == "url" {
							part = `{"type":"input_image","image_url":123}`
						} else {
							part = `{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":123}`
						}
					} else if invalidField == "url" {
						part = `{"type":"image_url","image_url":{"url":123}}`
					} else {
						part = `{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":123}}`
					}
				}
				output := "[" + part + "]"
				request := []byte(fmt.Sprintf(`{"input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":%q}]}`, output))
				body := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline", request, false)
				content := gjson.GetBytes(body, "messages.1.content")
				if invalidField == "" {
					if content.Get("0.image_url.detail").String() != "high" || content.Get("0.image_url.url").String() != "data:image/png;base64,AA==" {
						t.Fatalf("image output changed: %s", body)
					}
				} else if content.Type != gjson.String || content.String() != output {
					t.Fatalf("invalid image fields were treated as image content: %s", body)
				}
			})
		}
	}
	request := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"https://example.test/frame.png","detail":"original"},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}},{"type":"input_file","file_id":"file-1"}]}]}`)
	body := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline", request, false)
	parts := gjson.GetBytes(body, "messages.0.content").Array()
	if len(parts) != 4 || parts[1].Get("image_url.detail").String() != "high" || parts[2].Get("type").String() != "input_audio" || parts[3].Get("type").String() != "file" {
		t.Fatalf("mixed attachment order changed: %s", body)
	}
}

func TestResponsesChatToolSettingsRequireTools(t *testing.T) {
	for _, test := range []struct {
		declarations string
		wantTools    bool
	}{
		{declarations: `"tools":[]`, wantTools: false},
		{declarations: `"tools":[{"type":"web_search"}]`, wantTools: false},
		{declarations: `"tools":[{"type":"function","name":"lookup"}]`, wantTools: true},
		{declarations: `"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup"}]}]`, wantTools: true},
		{declarations: `"tools":[{"type":"shell","environment":{"type":"local"}}]`, wantTools: true},
	} {
		t.Run(test.declarations, func(t *testing.T) {
			request := []byte(`{` + test.declarations + `,"tool_choice":"required","parallel_tool_calls":false}`)
			body := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline", request, false)
			for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
				if present := gjson.GetBytes(body, field).Exists(); present != test.wantTools {
					t.Fatalf("%s present=%t, want %t: %s", field, present, test.wantTools, body)
				}
			}
		})
	}
}
