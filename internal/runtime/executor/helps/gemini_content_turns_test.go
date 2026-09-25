package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnsureGeminiLeadingUserContent(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string
		path      string
		wantRoles string
	}{
		{
			name:      "user first unchanged",
			inputJSON: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			path:      "contents",
			wantRoles: "user",
		},
		{
			name:      "model first prepends empty user",
			inputJSON: `{"contents":[{"role":"model","parts":[{"text":"answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`,
			path:      "contents",
			wantRoles: "user,model,user",
		},
		{
			name:      "nested model first prepends empty user",
			inputJSON: `{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"run"}}]}]}}`,
			path:      "request.contents",
			wantRoles: "user,model",
		},
		{name: "empty contents unchanged", inputJSON: `{"contents":[]}`, path: "contents"},
		{name: "missing contents unchanged", inputJSON: `{"model":"test"}`, path: "contents"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(test.inputJSON)
			output := EnsureGeminiLeadingUserContent(input, test.path)
			contents := gjson.GetBytes(output, test.path).Array()
			roles := make([]string, 0, len(contents))
			for _, content := range contents {
				roles = append(roles, content.Get("role").String())
			}
			if got := strings.Join(roles, ","); got != test.wantRoles {
				t.Fatalf("roles = %q, want %q; output=%s", got, test.wantRoles, output)
			}
			if strings.HasPrefix(test.wantRoles, "user,model") {
				text := gjson.GetBytes(output, test.path+".0.parts.0.text")
				if !text.Exists() || text.String() != "" {
					t.Fatalf("leading empty user turn missing: %s", output)
				}
			}
		})
	}
}

func TestEnsureGeminiTrailingUserContent(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string
		path      string
		wantRoles string
	}{
		{
			name:      "user last unchanged",
			inputJSON: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			path:      "contents",
			wantRoles: "user",
		},
		{
			name:      "model last appends empty user",
			inputJSON: `{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"answer"}]}]}`,
			path:      "contents",
			wantRoles: "user,model,user",
		},
		{
			name:      "assistant last appends empty user",
			inputJSON: `{"contents":[{"role":"assistant","parts":[{"text":"answer"}]}]}`,
			path:      "contents",
			wantRoles: "assistant,user",
		},
		{
			name:      "trailing function response unchanged",
			inputJSON: `{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"run","response":{"result":"ok"}}}]}]}`,
			path:      "contents",
			wantRoles: "model",
		},
		{name: "empty contents unchanged", inputJSON: `{"contents":[]}`, path: "contents"},
		{name: "missing contents unchanged", inputJSON: `{"model":"test"}`, path: "contents"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := EnsureGeminiTrailingUserContent([]byte(test.inputJSON), test.path)
			contents := gjson.GetBytes(output, test.path).Array()
			roles := make([]string, 0, len(contents))
			for _, content := range contents {
				roles = append(roles, content.Get("role").String())
			}
			if got := strings.Join(roles, ","); got != test.wantRoles {
				t.Fatalf("roles = %q, want %q; output=%s", got, test.wantRoles, output)
			}
			if strings.HasSuffix(test.wantRoles, ",user") {
				last := contents[len(contents)-1]
				text := last.Get("parts.0.text")
				if !text.Exists() || text.String() != "" {
					t.Fatalf("trailing empty user turn missing: %s", output)
				}
			}
		})
	}
}

func TestEnsureGeminiBoundaryUserContent(t *testing.T) {
	output := EnsureGeminiBoundaryUserContent(
		[]byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
		"contents",
	)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents len = %d, want 3; output=%s", len(contents), output)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("first role = %q, want user", got)
	}
	if got := contents[1].Get("role").String(); got != "model" {
		t.Fatalf("middle role = %q, want model", got)
	}
	if got := contents[2].Get("role").String(); got != "user" {
		t.Fatalf("last role = %q, want user", got)
	}
}

func TestEnsureGeminiBoundaryUserContentReusesValidPayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	output := EnsureGeminiBoundaryUserContent(input, "contents")
	if &output[0] != &input[0] {
		t.Fatal("valid request should reuse the input payload")
	}
}
