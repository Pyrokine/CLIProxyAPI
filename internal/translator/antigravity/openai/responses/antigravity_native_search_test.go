package responses

import (
	"bytes"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRegisteredAntigravitySearchDoesNotBorrowOtherProviderCapability(t *testing.T) {
	const model = "synthetic-cross-provider-search"
	catalog := registry.GetGlobalRegistry()
	catalog.RegisterClient("synthetic-search-aistudio", "aistudio", []*registry.ModelInfo{{ID: model, SupportsWebSearch: true}})
	catalog.RegisterClient("synthetic-search-antigravity", "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		catalog.UnregisterClient("synthetic-search-aistudio")
		catalog.UnregisterClient("synthetic-search-antigravity")
	})
	request := []byte(`{"input":"Search weather","tools":[{"type":"web_search"}]}`)
	for _, stream := range []bool{false, true} {
		body, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
			sdktranslator.FormatAntigravity, model+"(high)", request, stream)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(body, "requestType").String() == "web_search" || gjson.GetBytes(body, "request.tools.0.googleSearch").Exists() {
			t.Fatalf("search capability crossed provider boundary: %s", body)
		}
	}
}

func TestAntigravitySearchUsesProviderLocalProbeAndCatalogVeto(t *testing.T) {
	trueValue, falseValue := true, false
	for _, tc := range []struct {
		name       string
		capability *bool
		probe      bool
		want       bool
	}{
		{name: "unknown without probe"},
		{name: "unknown with probe", probe: true, want: true},
		{name: "false without probe", capability: &falseValue},
		{name: "false vetoes probe", capability: &falseValue, probe: true},
		{name: "true still requires probe", capability: &trueValue},
		{name: "true with probe", capability: &trueValue, probe: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "synthetic-antigravity-search-probe"
			const client = "synthetic-antigravity-search-probe-client"
			catalog := registry.GetGlobalRegistry()
			catalog.RegisterClient(client, "antigravity", []*registry.ModelInfo{{
				ID: model, SupportsWebSearch: tc.probe, NativeCapabilities: &registry.NativeCapabilities{WebSearch: tc.capability},
			}})
			t.Cleanup(func() { catalog.UnregisterClient(client) })
			body := []byte(`{"input":"Search weather","tools":[{"type":"web_search"}]}`)
			for _, info := range []*registry.ModelInfo{nil, {ID: model, NativeCapabilities: &registry.NativeCapabilities{}}} {
				out, err := convertOpenAIResponsesRequestToAntigravityChecked(model+"(high)", body, false, info)
				if err != nil {
					t.Fatal(err)
				}
				envelope := sdktranslator.TranslateRequestEnvelope(t.Context(), sdktranslator.FormatOpenAIResponse,
					sdktranslator.FormatAntigravity, sdktranslator.RequestEnvelope{Model: model + "(high)", ModelInfo: info, Body: body, Stream: true})
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				for entry, output := range map[string][]byte{"checked": out, "envelope": envelope.Body} {
					if got := gjson.GetBytes(output, "requestType").String() == "web_search"; got != tc.want {
						t.Fatalf("%s search route = %t, want %t: %s", entry, got, tc.want, output)
					}
					if gjson.GetBytes(output, "request.tools.0.googleSearch").Exists() != tc.want {
						t.Fatalf("%s search tool did not follow the provider capability: %s", entry, output)
					}
				}
			}
		})
	}
}

func TestAntigravitySearchSelectedCapabilityOverridesLocalAndOtherProvider(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		local := !enabled
		const model = "synthetic-antigravity-selected-search"
		catalog := registry.GetGlobalRegistry()
		catalog.RegisterClient("synthetic-antigravity-selected", "antigravity", []*registry.ModelInfo{{
			ID: model, SupportsWebSearch: true, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &local},
		}})
		catalog.RegisterClient("synthetic-gemini-selected", "gemini", []*registry.ModelInfo{{
			ID: model, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &local},
		}})
		info := &registry.ModelInfo{ID: model, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &enabled}}
		body := []byte(`{"input":"Search weather","tools":[{"type":"web_search"}]}`)
		out := sdktranslator.TranslateRequestEnvelope(sdktranslator.WithPluginHooks(t.Context(), nil),
			sdktranslator.FormatOpenAIResponse, sdktranslator.FormatAntigravity,
			sdktranslator.RequestEnvelope{Model: model, ModelInfo: info, Body: body})
		catalog.UnregisterClient("synthetic-antigravity-selected")
		catalog.UnregisterClient("synthetic-gemini-selected")
		if out.Err != nil || (gjson.GetBytes(out.Body, "requestType").String() == "web_search") != enabled {
			t.Fatalf("selected capability = %t: %s, err = %v", enabled, out.Body, out.Err)
		}
	}
}

func TestAntigravitySearchToolChoiceAndMixedTools(t *testing.T) {
	for _, tc := range []struct {
		name, choice string
		mixed, want  bool
	}{
		{name: "automatic", choice: `"auto"`, want: true},
		{name: "required", choice: `"required"`, want: true},
		{name: "explicit search", choice: `{"type":"web_search"}`, want: true},
		{name: "allowed search", choice: `{"type":"allowed_tools","tools":[{"type":"web_search"}]}`, want: true},
		{name: "disabled", choice: `"none"`},
		{name: "explicit function", choice: `{"type":"function","name":"lookup"}`},
		{name: "allowed function", choice: `{"type":"allowed_tools","tools":[{"type":"function","name":"lookup"}]}`},
		{name: "mixed tools", choice: `"auto"`, mixed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := true
			info := &registry.ModelInfo{NativeCapabilities: &registry.NativeCapabilities{WebSearch: &enabled}}
			body := []byte(`{"instructions":"Keep my instruction","input":"Search weather","tools":[{"type":"web_search"}]}`)
			body, err := sjson.SetRawBytes(body, "tool_choice", []byte(tc.choice))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mixed {
				body, err = sjson.SetRawBytes(body, "tools.-1", []byte(`{"type":"function","name":"lookup","parameters":{"type":"object"}}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err := convertOpenAIResponsesRequestToAntigravityChecked("synthetic-search", body, false, info)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "requestType").String() == "web_search"; got != tc.want {
				t.Fatalf("search route = %t, want %t: %s", got, tc.want, out)
			}
			if tc.want && (!bytes.Contains(out, []byte("Keep my instruction")) ||
				!bytes.Contains(out, []byte(antigravityWebSearchSystemInstruction))) {
				t.Fatalf("search instructions were not preserved: %s", out)
			}
			if !tc.want && gjson.GetBytes(out, "request.tools.0.googleSearch").Exists() {
				t.Fatalf("unsupported or disabled search was retained: %s", out)
			}
			if tc.mixed && !gjson.GetBytes(out, "request.tools.0.functionDeclarations.0").Exists() {
				t.Fatalf("mixed tool request lost the function: %s", out)
			}
		})
	}
}

func TestAntigravitySearchRejectsInvalidDomainsAndDoesNotDuplicateInstruction(t *testing.T) {
	enabled := true
	info := &registry.ModelInfo{NativeCapabilities: &registry.NativeCapabilities{WebSearch: &enabled}}
	body := []byte(`{"input":"Search weather","tools":[{"type":"web_search","filters":{"allowed_domains":"example.org"}}]}`)
	out, err := convertOpenAIResponsesRequestToAntigravityChecked("synthetic-search", body, false, info)
	if err == nil || out != nil {
		t.Fatalf("invalid domains were accepted: %s, err = %v", out, err)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 400 {
		t.Fatalf("invalid domains error is not request-scoped HTTP 400: %v", err)
	}
	body, err = sjson.SetBytes([]byte(`{"input":"Search weather","tools":[{"type":"web_search"}]}`),
		"instructions", antigravityWebSearchSystemInstruction)
	if err != nil {
		t.Fatal(err)
	}
	out, err = convertOpenAIResponsesRequestToAntigravityChecked("synthetic-search", body, false, info)
	if err != nil || bytes.Count(out, []byte(antigravityWebSearchSystemInstruction)) != 1 {
		t.Fatalf("search instruction was duplicated: %s, err = %v", out, err)
	}
}
