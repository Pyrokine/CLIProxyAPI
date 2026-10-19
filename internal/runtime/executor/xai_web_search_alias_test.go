package executor

import (
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIClientWebSearchAliasSurvivesChoicesHistoryAndResponse(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"function","name":"web_search","parameters":{"type":"object"}},
			{"type":"function","name":"clientfn_web_search","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"web_search"}]},
		"input":[
			{"type":"function_call","name":"web_search","call_id":"call-1","arguments":"{}"},
			{"type":"function_call_output","name":"web_search","call_id":"call-1","output":"found"}
		]
	}`)
	normalizer := newXAIToolIdentityNormalizer(body)
	prepared, err := normalizer.normalizeDeclarations(body)
	if err != nil {
		t.Fatal(err)
	}
	prepared = normalizer.normalizeInputHistory(prepared)
	prepared, err = normalizer.aliasClientWebSearchInput(prepared)
	if err != nil {
		t.Fatal(err)
	}
	const alias = "clientfn_web_search_1"
	for _, path := range []string{"tools.0.name", "tool_choice.tools.0.name", "input.0.name", "input.1.name"} {
		if got := gjson.GetBytes(prepared, path).String(); got != alias {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, alias, prepared)
		}
	}
	if got := gjson.GetBytes(prepared, "tools.1.name").String(); got != "clientfn_web_search" {
		t.Fatalf("existing function changed: %s", prepared)
	}

	response := normalizer.newResponseNormalizer(false)
	for _, event := range []string{
		`{"type":"response.output_item.done","item":{"type":"function_call","name":"clientfn_web_search_1","call_id":"call-2"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"clientfn_web_search_1","call_id":"call-2"}]}}`,
	} {
		got := response.normalize([]byte(event))
		if strings.Contains(string(got), alias) || !strings.Contains(string(got), `"name":"web_search"`) {
			t.Fatalf("client tool name was not restored: %s", got)
		}
	}
}

func TestXAIHostedAndNamespacedWebSearchToolsRemainUnchanged(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`,
		`{"tools":[{"type":"namespace","name":"ops","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]}]}`,
	} {
		normalizer := newXAIToolIdentityNormalizer([]byte(body))
		prepared, err := normalizer.normalizeDeclarations([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if normalizer.clientWebSearchAlias != "" || strings.Contains(string(prepared), "clientfn_web_search") {
			t.Fatalf("non-client web_search aliased: %s", prepared)
		}
	}
}

func TestXAIPreparationAliasesClientWebSearchAcrossRequestAndResponse(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model: "grok-4.3",
		Payload: []byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],
			"tool_choice":{"type":"function","name":"web_search"},
			"input":[{"type":"function_call","name":"web_search","call_id":"call-1","arguments":"{}"}]
		}`),
	}
	prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(
		req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	const alias = "clientfn_web_search"
	for _, path := range []string{"tools.0.name", "tool_choice.name", "input.0.name"} {
		if got := gjson.GetBytes(prepared.body, path).String(); got != alias {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, alias, prepared.body)
		}
	}
	got := prepared.newResponseNormalizer().normalize([]byte(`{
		"type":"response.completed",
		"response":{"output":[{"type":"function_call","name":"clientfn_web_search","call_id":"call-2"}]}
	}`))
	if len(got) != 1 {
		t.Fatalf("expected one restored response, got %q", got)
	}
	if name := gjson.GetBytes(got[0], "response.output.0.name").String(); name != "web_search" {
		t.Fatalf("client tool response name = %q; event=%s", name, got[0])
	}
}
