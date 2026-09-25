package common

import (
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesToolIdentityRoundTripNamespaceCustom(t *testing.T) {
	request := []byte(`{
		"tools":[{
			"type":"namespace",
			"name":"mcp__editor",
			"tools":[{"type":"custom","name":"apply_patch"}]
		}]
	}`)
	identity := NewResponsesToolIdentity(request)

	if got := identity.ResolveName("apply_patch", "mcp__editor"); got != "mcp__editor__apply_patch" {
		t.Fatalf("qualified name = %q, want mcp__editor__apply_patch", got)
	}
	if !identity.IsCustom("mcp__editor__apply_patch") {
		t.Fatal("qualified tool should be custom")
	}

	item := identity.SetToolCallIdentity([]byte(`{"name":""}`), "mcp__editor__apply_patch", "")
	if got := gjson.GetBytes(item, "name").String(); got != "apply_patch" {
		t.Fatalf("round-trip name = %q, want apply_patch", got)
	}
	if got := gjson.GetBytes(item, "namespace").String(); got != "mcp__editor" {
		t.Fatalf("round-trip namespace = %q, want mcp__editor", got)
	}
}

func TestResponsesToolIdentityCollisionUsesDirectTopLevelDeclaration(t *testing.T) {
	request := []byte(`{
		"tools":[
			{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]},
			{"type":"function","name":"mcp__editor__apply_patch","parameters":{"type":"object"}}
		],
		"input":[{
			"type":"additional_tools",
			"tools":[{"type":"custom","name":"mcp__editor__apply_patch"}]
		}]
	}`)
	identity := NewResponsesToolIdentity(request)
	declarations := identity.Declarations()
	if len(declarations) != 1 {
		t.Fatalf("declaration count = %d, want 1", len(declarations))
	}
	if declarations[0].Kind != ResponsesFunctionTool || !declarations[0].Direct {
		t.Fatalf("winner = %#v, want direct function", declarations[0])
	}
	if identity.IsCustom("mcp__editor__apply_patch") {
		t.Fatal("discarded custom declaration must not reclassify the function winner")
	}

	item := identity.SetToolCallIdentity([]byte(`{"name":""}`), "mcp__editor__apply_patch", "")
	if got := gjson.GetBytes(item, "name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("collision name = %q, want flat direct name", got)
	}
	if gjson.GetBytes(item, "namespace").Exists() {
		t.Fatalf("direct collision winner must not gain namespace: %s", item)
	}
}

func TestParseResponsesCustomToolInputRequiresCompleteWrapper(t *testing.T) {
	if _, ok := ParseResponsesCustomToolInput(`{"input":"partial`); ok {
		t.Fatal("partial wrapper should not be accepted")
	}
	if got, ok := ParseResponsesCustomToolInput(`{"input":{"patch":"x"}}`); !ok || got != `{"patch":"x"}` {
		t.Fatalf("parsed input = %q, %v", got, ok)
	}
}

func TestExtractResponsesCallID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "call id precedence", raw: `{"call_id":"call-a","tool_call_id":"call-b","callId":"call-c","id":"call-d"}`, want: "call-a"},
		{name: "tool call id", raw: `{"tool_call_id":"call-b"}`, want: "call-b"},
		{name: "camel call id", raw: `{"callId":"call-c"}`, want: "call-c"},
		{name: "generic id", raw: `{"id":"call-d"}`, want: "call-d"},
		{name: "output item id", raw: `{"id":"fco_item"}`, want: ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ExtractResponsesCallID(gjson.Parse(testCase.raw)); got != testCase.want {
				t.Fatalf("call ID = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestNormalizeResponsesToolCallOutputsMatchesNameThenFIFO(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call","call_id":"call-a","name":"alpha"},
		{"type":"function_call","call_id":"call-b","name":"beta"},
		{"type":"function_call_output","name":"beta","output":"two"},
		{"type":"function_call_output","output":"one"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	if got := normalized[2].Get("call_id").String(); got != "call-b" {
		t.Fatalf("named output call_id = %q, want call-b", got)
	}
	if got := normalized[3].Get("call_id").String(); got != "call-a" {
		t.Fatalf("FIFO output call_id = %q, want call-a", got)
	}
}

func TestNormalizeResponsesToolCallOutputsReservesFutureExplicitID(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call","call_id":"call-a","name":"same"},
		{"type":"function_call","call_id":"call-b","name":"same"},
		{"type":"function_call_output","name":"same","output":"two"},
		{"type":"message","role":"user","content":"between"},
		{"type":"function_call_output","call_id":"call-a","output":"one"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	if got := normalized[2].Get("call_id").String(); got != "call-b" {
		t.Fatalf("unidentified output call_id = %q, want call-b", got)
	}
	if got := normalized[4].Get("call_id").String(); got != "call-a" {
		t.Fatalf("explicit output call_id = %q, want call-a", got)
	}
}

func TestNormalizeResponsesToolCallOutputsGeneratesIDsAndIgnoresFCOItemID(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call","name":"alpha"},
		{"type":"function_call","call_id":"call_responses_0000000000000001","name":"beta"},
		{"type":"function_call_output","id":"fco_item","name":"alpha","output":"one"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	const generated = "call_responses_0000000000000002"
	if got := normalized[0].Get("call_id").String(); got != generated {
		t.Fatalf("generated call_id = %q, want %q", got, generated)
	}
	if got := normalized[2].Get("call_id").String(); got != generated {
		t.Fatalf("fco output call_id = %q, want %q", got, generated)
	}
	if got := normalized[2].Get("id").String(); got != "fco_item" {
		t.Fatalf("output item id = %q, want fco_item", got)
	}
}

func TestNormalizeResponsesToolCallOutputsDoesNotReservePastExplicitID(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call_output","call_id":"call-a","output":"early"},
		{"type":"function_call","call_id":"call-a","name":"alpha"},
		{"type":"function_call_output","output":"late"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	if got := normalized[2].Get("call_id").String(); got != "call-a" {
		t.Fatalf("late output call_id = %q, want call-a", got)
	}
}

func TestNormalizeResponsesToolCallOutputsMatchesCustomTools(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"custom_tool_call","name":"apply_patch","input":"patch"},
		{"type":"custom_tool_call_output","name":"apply_patch","output":"applied"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	callID := normalized[0].Get("call_id").String()
	if callID != "call_responses_0000000000000001" {
		t.Fatalf("custom call ID = %q", callID)
	}
	if got := normalized[1].Get("call_id").String(); got != callID {
		t.Fatalf("custom output call_id = %q, want %q", got, callID)
	}
}

func TestNormalizeResponsesToolCallOutputsPreservesUnmatchedExplicitID(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call","call_id":"call-a","name":"alpha"},
		{"type":"function_call_output","tool_call_id":"external","output":"external"},
		{"type":"function_call_output","output":"one"}
	]`).Array()

	normalized := NormalizeResponsesToolCallOutputs(items)
	if got := ExtractResponsesCallID(normalized[1]); got != "external" {
		t.Fatalf("unmatched explicit ID = %q, want external", got)
	}
	if normalized[1].Get("call_id").Exists() {
		t.Fatalf("unmatched explicit output was rewritten: %s", normalized[1].Raw)
	}
	if got := normalized[2].Get("call_id").String(); got != "call-a" {
		t.Fatalf("following output call_id = %q, want call-a", got)
	}
}

func TestResponsesToolIdentityRejectsAmbiguousLocalAliases(t *testing.T) {
	request := []byte(`{
		"tools":[
			{"type":"namespace","name":"first","tools":[{"type":"custom","name":"lookup"}]},
			{"type":"namespace","name":"second","tools":[{"type":"function","name":"lookup"}]}
		]
	}`)
	identity := NewResponsesToolIdentity(request)

	if _, err := identity.ResolveDeclaration("lookup", ""); !errors.Is(err, ErrResponsesToolNameAmbiguous) {
		t.Fatalf("ambiguous local alias error = %v", err)
	}
	if got := identity.ResolveName("lookup", ""); got != "lookup" {
		t.Fatalf("legacy unresolved alias = %q, want lookup", got)
	}
	first, err := identity.ResolveDeclaration("lookup", "first")
	if err != nil || first.QualifiedName != "first__lookup" {
		t.Fatalf("first namespace = %#v, %v", first, err)
	}
	declaration, err := identity.ResolveDeclaration("second__lookup", "")
	if err != nil {
		t.Fatalf("exact qualified name failed: %v", err)
	}
	if declaration.Kind != ResponsesFunctionTool || declaration.Namespace != "second" {
		t.Fatalf("resolved declaration = %#v", declaration)
	}
}

func TestResponsesToolIdentityResolutionIsDeterministicWithDirectName(t *testing.T) {
	request := []byte(`{
		"tools":[
			{"type":"namespace","name":"nested","tools":[{"type":"custom","name":"lookup"}]},
			{"type":"function","name":"lookup"}
		]
	}`)

	for range 100 {
		identity := NewResponsesToolIdentity(request)
		declaration, err := identity.ResolveDeclaration("lookup", "")
		if err != nil {
			t.Fatalf("exact direct name failed: %v", err)
		}
		if declaration.QualifiedName != "lookup" || declaration.Kind != ResponsesFunctionTool {
			t.Fatalf("resolved declaration = %#v", declaration)
		}
	}
}
