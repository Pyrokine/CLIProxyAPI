package executor

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexIntegerToolTypesFollowTargetExecutor(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"},"cmd":{"type":"number"}}}}]}`)
	headers := http.Header{"User-Agent": {"codex-tui/0.145.0"}}
	for _, test := range []struct {
		target string
		want   string
	}{
		{"codex", "number"},
		{"codex-websockets", "number"},
		{"claude", "integer"},
		{"xai", "integer"},
	} {
		t.Run(test.target, func(t *testing.T) {
			out, _ := applyPayloadConfigWithRequestTracked(nil, "gpt-test", "codex", "claude", "", body, body, "gpt-test", "", headers, test.target)
			if got := gjson.GetBytes(out, "tools.0.parameters.properties.timeout_ms.type").String(); got != test.want {
				t.Fatalf("target %s: type = %q, want %q; body=%s", test.target, got, test.want, out)
			}
			if got := gjson.GetBytes(out, "tools.0.parameters.properties.cmd.type").String(); got != "number" {
				t.Fatalf("unrelated field was rewritten: %s", out)
			}
		})
	}
	untrusted := http.Header{"User-Agent": {"curl/8 codex-tui/0.145.0"}}
	if out := normalizeCodexToolIntegerTypes(body, untrusted); &out[0] != &body[0] {
		t.Fatal("unrecognized Codex client identity changed request")
	}
}

func TestCodexIntegerToolPrefixesPreserveUnknownTools(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		want      string
	}{
		{name: "exec_command", want: "integer"},
		{name: "functions__exec_command", want: "integer"},
		{name: "collab__exec_command", want: "integer"},
		{name: "functions__collab__exec_command", want: "number"},
		{name: "collab__exec_command", namespace: "functions", want: "number"},
		{name: "functions__exec_command", namespace: "collab", want: "number"},
		{name: "collab__functions__exec_command", want: "number"},
		{name: "functions__functions__exec_command", want: "number"},
	} {
		t.Run(tc.namespace+"/"+tc.name, func(t *testing.T) {
			tool := `{"type":"function","name":"` + tc.name + `","parameters":{"properties":{"timeout_ms":{"type":"number"},"ratio":{"type":"number"}}}}`
			path := "tools.0.parameters.properties.timeout_ms.type"
			if tc.namespace != "" {
				tool = `{"type":"namespace","name":"` + tc.namespace + `","tools":[` + tool + `]}`
				path = "tools.0.tools.0.parameters.properties.timeout_ms.type"
			}
			body := []byte(`{"tools":[` + tool + `]}`)
			out := normalizeCodexToolIntegerTypes(body, http.Header{"User-Agent": {"codex-tui/0.154.0"}})
			if got := gjson.GetBytes(out, path).String(); got != tc.want {
				t.Fatalf("type = %q, want %q; body=%s", got, tc.want, out)
			}
		})
	}
}

func TestCodexIntegerToolTypesCoverAllToolDeclarations(t *testing.T) {
	body := []byte(`{"tools":[{"name":"exec_command","input_schema":{"properties":{"timeout_ms":{"type":"number"}}}},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait_agent","parameters":{"properties":{"timeout_ms":{"type":["number","null","integer"]}}}}]},{"function_declarations":[{"name":"tool_search","parametersJsonSchema":{"properties":{"limit":{"type":"number"}}}}]},{"functionDeclarations":[{"name":"sleep","parameters":{"properties":{"duration_ms":{"type":"number"}}}}]}],"input":[{"type":"additional_tools","tools":[{"name":"notes__read_file","parameters":{"properties":{"start_line":{"anyOf":[{"type":"number"},{"type":"null"}]}}}}]}]}`)
	headers := http.Header{"User-Agent": {"codex_cli_rs/0.145.0"}}
	out := normalizeCodexToolIntegerTypes(body, headers)
	for _, path := range []string{
		"tools.0.input_schema.properties.timeout_ms.type",
		"tools.2.function_declarations.0.parametersJsonSchema.properties.limit.type",
		"tools.3.functionDeclarations.0.parameters.properties.duration_ms.type",
		"input.0.tools.0.parameters.properties.start_line.anyOf.0.type",
	} {
		if got := gjson.GetBytes(out, path).String(); got != "integer" {
			t.Fatalf("%s = %q, want integer; body=%s", path, got, out)
		}
	}
	if got := gjson.GetBytes(out, "tools.1.tools.0.parameters.properties.timeout_ms.type").Raw; got != `["integer","null"]` {
		t.Fatalf("nested nullable type = %s; body=%s", got, out)
	}
	if again := normalizeCodexToolIntegerTypes(out, headers); string(again) != string(out) {
		t.Fatalf("second normalization changed body: %s", again)
	}
}

func TestCodexIntegerToolTypesRunBeforePayloadOverrides(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"exec_command","parameters":{"properties":{"timeout_ms":{"type":"number"},"yield_time_ms":{"type":"number"}}}}]}`)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-test", Protocol: "codex"}},
		Params: map[string]any{"tools.0.parameters.properties.timeout_ms.type": "number"},
	}}}}
	out := applyPayloadConfigWithRequest(cfg, "gpt-test", "codex", "codex", "", body, body, "gpt-test", "", http.Header{"User-Agent": {"codex-tui/0.145.0"}}, "xai")
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.timeout_ms.type").String(); got != "number" {
		t.Fatalf("explicit payload override lost: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.yield_time_ms.type").String(); got != "integer" {
		t.Fatalf("other Codex integer type was not normalized: %s", out)
	}
}

func TestClaudePreparationConvertsCodexIntegerToolsWithoutDispatch(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"exec_command","input_schema":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}]}`)
	prepared, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://claude-compatible.example.invalid", "api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Headers: http.Header{"User-Agent": {"codex-tui/0.145.0"}}},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.httpReq.Body.Close()
	body, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "tools.0.input_schema.properties.timeout_ms.type").String(); got != "integer" {
		t.Fatalf("prepared Claude tool timeout type = %q; body=%s", got, body)
	}
}

func TestCodexDesktopIntegerToolsAtGoogleExecutors(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex"} {
		for _, route := range []string{"generate", "stream", "count"} {
			for _, sample := range []struct {
				name, userAgent, want string
			}{
				{name: "desktop", userAgent: "codex-desktop/0.159.0", want: "integer"},
				{name: "cli", userAgent: "codex_cli_rs/0.159.0", want: "integer"},
				{name: "other", userAgent: "curl/8.7.1", want: "number"},
			} {
				t.Run(provider+"/"+route+"/"+sample.name, func(t *testing.T) {
					payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"function_declarations":[{"name":"sleep","parameters":{"type":"object","properties":{"duration_ms":{"type":"number"},"ratio":{"type":"number"}}}}]}]}`)
					body := googlePayloadCall(t, provider, route, "gemini-2.5-flash", &config.Config{}, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: http.Header{"User-Agent": {sample.userAgent}}})
					if route == "count" {
						if gjson.GetBytes(body, "tools").Exists() || gjson.GetBytes(body, "contents.0.parts.0.text").String() != "hello" {
							t.Fatalf("countTokens body=%s", body)
						}
						return
					}
					if got := gjson.GetBytes(body, "tools.0.function_declarations.0.parametersJsonSchema.properties.duration_ms.type").String(); got != sample.want {
						t.Fatalf("duration_ms type=%q, want %q, body=%s", got, sample.want, body)
					}
					if got := gjson.GetBytes(body, "tools.0.function_declarations.0.parametersJsonSchema.properties.ratio.type").String(); got != "number" {
						t.Fatalf("unrelated type=%q, body=%s", got, body)
					}
				})
			}
		}
	}
}
