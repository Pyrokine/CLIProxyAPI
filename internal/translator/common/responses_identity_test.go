package common

import (
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
	if declarations[0].Kind != responsesFunctionTool || !declarations[0].Direct {
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
