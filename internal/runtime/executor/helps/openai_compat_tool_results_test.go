package helps

import (
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIToolResultsTextOnly(t *testing.T) {
	input := []byte(`{"messages":[
        {"role":"assistant","content":[{"type":"text","text":"before"}]},
        {"role":"tool","tool_call_id":"call_1","content":[
            {"type":"text","text":"image inspected"},
            {"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}
        ]},
        {"role":"tool","tool_call_id":"call_2","content":"already text"},
        {"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/user.png"}}]}
    ]}`)

	got := NormalizeOpenAIToolResultsTextOnly(input)

	toolContent := gjson.GetBytes(got, "messages.1.content")
	if toolContent.Type != gjson.String {
		t.Fatalf("tool content type = %s, want string", toolContent.Type)
	}
	if toolContent.String() != "image inspected\n\n"+openAIToolResultImageOmittedText {
		t.Fatalf("tool content = %q", toolContent.String())
	}
	if gotContent := gjson.GetBytes(got, "messages.2.content"); gotContent.String() != "already text" {
		t.Fatalf("existing string tool content = %q", gotContent.String())
	}
	if !gjson.GetBytes(got, "messages.0.content").IsArray() {
		t.Fatal("assistant content array was unexpectedly changed")
	}
	if !gjson.GetBytes(got, "messages.3.content").IsArray() {
		t.Fatal("non-tool content array was unexpectedly changed")
	}
}

func TestNormalizeOpenAIToolImageRelaysTextOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  string
		extra string
		want  string
	}{
		{"image-only", claudeToolResultImagePlaceholder, "", openAIToolResultImageOmittedText},
		{"text-and-image", "tool text", "", "tool text\n\n" + openAIToolResultImageOmittedText},
		{
			"extra-user-text", "tool text", `,{"type":"text","text":"real user text"}`,
			"tool text\n\n" + openAIToolResultImageOmittedText,
		},
		{"existing-marker", openAIToolResultImageOmittedText, "", openAIToolResultImageOmittedText},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				body := []byte(
					`{"messages":[{"role":"tool","tool_call_id":"fixture-call","content":"` +
						tc.tool +
						`"},{"role":"user","content":[{"type":"text","text":"` +
						claudeToolResultImageRelayNotice +
						(`"},{"type":` +
							`"image_url",` +
							`"image_url":` +
							`{"url":"data:` +
							`image/png;` +
							`base64,AA=="}}`) +
						tc.extra +
						(`]},{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://` +
							`fixture.invalid/user.png"}}]}]}`),
				)
				got := NormalizeOpenAIToolResultsTextOnly(body)
				if content := gjson.GetBytes(got, "messages.0.content").String(); content != tc.want {
					t.Fatalf("tool text = %q, want %q", content, tc.want)
				}
				messages := gjson.GetBytes(got, "messages").Array()
				wantCount := 2
				if tc.extra != "" {
					wantCount = 3
					if messages[1].Get("content.#").Int() != 1 ||
						messages[1].Get("content.0.text").String() != "real user text" {
						t.Fatalf("real text was not retained: %s", got)
					}
				}
				if len(messages) != wantCount ||
					messages[len(messages)-1].Get("content.0.image_url.url").
						String() !=
						"https://fixture.invalid/user.png" {
					t.Fatalf("normal user image or relay count changed: %s", got)
				}
				if again := NormalizeOpenAIToolResultsTextOnly(got); string(again) != string(got) {
					t.Fatalf("normalization is not idempotent: %s", again)
				}
			},
		)
	}
	quotedNotice := []byte(
		`{"messages":[{"role":"user","content":[{"type":"text","text":"` +
			claudeToolResultImageRelayNotice +
			(`"},{"type":` +
				`"image_url",` +
				`"image_url":` +
				`{"url":"https:` +
				`//fixture.inva` +
				`lid/user.png"}` +
				`}]}]}`),
	)
	if got := NormalizeOpenAIToolResultsTextOnly(quotedNotice); string(got) != string(quotedNotice) {
		t.Fatal("user image without a preceding tool was treated as a synthetic relay")
	}
}

func TestNormalizeOpenAIToolResultsTextOnlyRelayedImages(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  [2]string
	}{
		{
			name: "multiple preceding tools where one is placeholder",
			input: `{"messages":[
				{"role":"assistant","content":"","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"tool1","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"tool2","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"text result only"},
				{"role":"tool","tool_call_id":"call_2","content":"` + claudeToolResultImagePlaceholder + `"},
				{"role":"user","content":[
					{"type":"text","text":"Images returned by the preceding tool call(s):"},
					{"type":"image_url","image_url":{"url":"https://example.com/tool2.png"}}
				]}
			]}`,
			want: [2]string{"text result only", openAIToolResultImageOmittedText},
		},
		{
			name: "multiple preceding tools where image tool precedes text tool",
			input: `{"messages":[
				{"role":"assistant","content":"","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"tool1","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"tool2","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"` + claudeToolResultImagePlaceholder + `"},
				{"role":"tool","tool_call_id":"call_2","content":"text result only"},
				{"role":"user","content":[
					{"type":"text","text":"Images returned by the preceding tool call(s):"},
					{"type":"image_url","image_url":{"url":"https://example.com/tool1.png"}}
				]}
			]}`,
			want: [2]string{openAIToolResultImageOmittedText, "text result only"},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				got := NormalizeOpenAIToolResultsTextOnly([]byte(test.input))
				if strings.Contains(string(got), "image_url") {
					t.Fatalf("text-only normalization left image_url in payload: %s", got)
				}
				messages := gjson.GetBytes(got, "messages").Array()
				if len(messages) != 3 {
					t.Fatalf("message count = %d, want 3; output=%s", len(messages), got)
				}
				for index, want := range test.want {
					message := messages[index+1]
					wantID := [2]string{"call_1", "call_2"}[index]
					if message.Get("role").String() != "tool" ||
						message.Get("tool_call_id").String() != wantID {
						t.Fatalf("tool result %d changed identity or order: %s", index+1, got)
					}
					if content := message.Get("content").String(); content != want {
						t.Fatalf("tool %d content = %q, want %q", index+1, content, want)
					}
				}
			},
		)
	}
}

func TestNormalizeClaudeToolResultsTextOnly(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"inspect_image","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_1","content":[
				{"type":"text","text":"image inspected"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}
			]},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BB=="}}
		]}
	]}`)

	got := NormalizeClaudeToolResultsTextOnly(input)

	want := "image inspected\n\n" + openAIToolResultImageOmittedText
	if content := gjson.GetBytes(got, "messages.1.content.0.content"); content.String() != want {
		t.Fatalf("tool result content = %q, want %q", content.String(), want)
	}
	if image := gjson.GetBytes(got, "messages.1.content.1"); image.Get("type").String() != "image" {
		t.Fatalf("ordinary user image changed: %s", image.Raw)
	}
}

func TestNormalizeOpenAIToolResultsTextOnlyImageAndUnknownContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "image-only array",
			input: `{"messages":[{"role":"tool","content":[{"type":"image_url",` +
				`"image_url":{"url":"https://example.com/` +
				`image.png"}}]}]}`,
			want: openAIToolResultImageOmittedText,
		},
		{
			name:  "image object",
			input: `{"messages":[{"role":"tool","content":{"type":"image","source":{"type":"base64","data":"AA=="}}}]}`,
			want:  openAIToolResultImageOmittedText,
		},
		{
			name:  "unknown object",
			input: `{"messages":[{"role":"tool","content":[{"type":"custom","value":1}]}]}`,
			want:  `{"type":"custom","value":1}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := NormalizeOpenAIToolResultsTextOnly([]byte(tt.input))
				if content := gjson.GetBytes(got, "messages.0.content").String(); content != tt.want {
					t.Fatalf("tool content = %q, want %q", content, tt.want)
				}
			},
		)
	}
}

func TestShouldNormalizeOpenAIToolResultsForModel(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-text", Alias: "alias-text", InputModalities: []string{"text"}},
			{
				Name:            "upstream-multimodal",
				Alias:           "alias-multimodal",
				InputModalities: []string{"text", "image"},
			},
			{Name: "upstream-unspecified", Alias: "alias-unspecified"},
			{Name: "upstream-uppercase", Alias: "alias-uppercase", InputModalities: []string{"TEXT"}},
			{Name: "pool-text", Alias: "shared-alias", InputModalities: []string{"text"}},
			{Name: "pool-image", Alias: "shared-alias", InputModalities: []string{"text", "image"}},
		},
	}

	tests := []struct {
		name           string
		upstreamModel  string
		requestedModel string
		want           bool
	}{
		{name: "upstream text", upstreamModel: "upstream-text", want: true},
		{name: "upstream suffix", upstreamModel: "upstream-text(high)", want: true},
		{name: "requested alias", upstreamModel: "unknown", requestedModel: "alias-text", want: true},
		{name: "multimodal", upstreamModel: "upstream-multimodal", want: false},
		{name: "unspecified", upstreamModel: "upstream-unspecified", want: false},
		{name: "case insensitive modality", upstreamModel: "upstream-uppercase", want: true},
		{name: "mixed alias pool", upstreamModel: "unknown", requestedModel: "shared-alias", want: false},
		{name: "unknown", upstreamModel: "unknown", requestedModel: "missing", want: false},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := ShouldNormalizeOpenAIToolResultsForModel(
					compat, tt.upstreamModel, tt.requestedModel,
				); got != tt.want {
					t.Fatalf("normalize = %t, want %t", got, tt.want)
				}
			},
		)
	}

	if ShouldNormalizeOpenAIToolResultsForModel(nil, "upstream-text", "alias-text") {
		t.Fatal("nil compatibility config unexpectedly enabled normalization")
	}
}
