package executor

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRepairAntigravityGeminiFunctionResponseNames(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{}}},{"functionCall":{"id":"call-2","name":"search","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"unknown","response":{"ok":true}}},{"functionResponse":{"id":"call-2","response":{"ok":true}}}]}]}}`)

	got := repairAntigravityGeminiFunctionResponseNames(payload)
	if name := gjson.GetBytes(got, "request.contents.1.parts.0.functionResponse.name").String(); name != "lookup" {
		t.Fatalf("first response name = %q, want lookup; payload=%s", name, got)
	}
	if name := gjson.GetBytes(got, "request.contents.1.parts.1.functionResponse.name").String(); name != "search" {
		t.Fatalf("second response name = %q, want search; payload=%s", name, got)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesAndOrder(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-b","name":"second","args":{}}},{"functionCall":{"id":"call-a","name":"first","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call-a","name":"first","response":{"value":"a"}}},{"functionResponse":{"id":"call-b","name":"second","response":{"value":"b"}}}]}]}}`)

	got := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	responseTurn := gjson.GetBytes(got, "request.contents.1")
	if role := responseTurn.Get("role").String(); role != "model" {
		t.Fatalf("response role = %q, want model; payload=%s", role, got)
	}
	parts := responseTurn.Get("parts").Array()
	if len(parts) != 2 {
		t.Fatalf("response part count = %d, want 2", len(parts))
	}
	if id := parts[0].Get("functionResponse.id").String(); id != "call-b" {
		t.Fatalf("first response ID = %q, want call-b; payload=%s", id, got)
	}
	if id := parts[1].Get("functionResponse.id").String(); id != "call-a" {
		t.Fatalf("second response ID = %q, want call-a; payload=%s", id, got)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesKeepsMixedTurnRole(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{}}}]},{"role":"user","parts":[{"text":"context"},{"functionResponse":{"id":"call-1","name":"lookup","response":{"ok":true}}}]}]}}`)

	got := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if role := gjson.GetBytes(got, "request.contents.1.role").String(); role != "user" {
		t.Fatalf("mixed response role = %q, want user; payload=%s", role, got)
	}
	parts := gjson.GetBytes(got, "request.contents.1.parts").Array()
	if len(parts) != 2 || parts[0].Get("functionResponse.id").String() != "call-1" || parts[1].Get("text").String() != "context" {
		t.Fatalf("mixed response parts = %s", gjson.GetBytes(got, "request.contents.1.parts").Raw)
	}
}

func TestEnsureAntigravityGeminiBoundaryUserContent(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"last"}]}]}}`)
	got := ensureAntigravityGeminiBoundaryUserContent("gemini-3.7-flash", payload)
	contents := gjson.GetBytes(got, "request.contents").Array()
	if len(contents) != 4 {
		t.Fatalf("content count = %d, want 4; payload=%s", len(contents), got)
	}
	leadingText := contents[0].Get("parts.0.text")
	if contents[0].Get("role").String() != "user" || !leadingText.Exists() || leadingText.String() != "" {
		t.Fatalf("leading boundary turn = %s", contents[0].Raw)
	}
	trailingText := contents[3].Get("parts.0.text")
	if contents[3].Get("role").String() != "user" || !trailingText.Exists() || trailingText.String() != "" {
		t.Fatalf("trailing boundary turn = %s", contents[3].Raw)
	}
}

func TestEnsureAntigravityGeminiBoundaryUserContentBypassesClaudeAndFunctionResponse(t *testing.T) {
	claude := []byte(`{"request":{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}}`)
	if got := ensureAntigravityGeminiBoundaryUserContent("claude-sonnet-4-6", claude); !bytes.Equal(got, claude) {
		t.Fatalf("Claude payload changed: %s", got)
	}

	functionResponse := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"functionResponse":{"id":"call-1","name":"lookup","response":{}}}]}]}}`)
	if got := ensureAntigravityGeminiTrailingUserContent("gemini-3.7-flash", functionResponse); !bytes.Equal(got, functionResponse) {
		t.Fatalf("functionResponse tail changed: %s", got)
	}
}

func TestApplyAntigravityIndexedEditsUsesOffsetsAndSJSONFallback(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"one"}]},{"role":"user","parts":[{"text":"two"}]}]},"preserve":"spacing"}`)
	first := gjson.GetBytes(payload, "request.contents.0")
	second := gjson.GetBytes(payload, "request.contents.1")
	edits := []antigravityContentEdit{
		{
			index:       0,
			path:        "invalid.path.0",
			start:       first.Index,
			end:         first.Index + len(first.Raw),
			replacement: []byte(`{"role":"model","parts":[{"text":"one"}]}`),
		},
		{
			index:       1,
			path:        "invalid.path.1",
			start:       second.Index,
			end:         second.Index + len(second.Raw),
			replacement: []byte(`{"role":"model","parts":[{"text":"two"}]}`),
		},
	}
	indexed := applyAntigravityIndexedEdits(payload, edits, true)
	if gjson.GetBytes(indexed, "request.contents.0.role").String() != "model" ||
		gjson.GetBytes(indexed, "request.contents.1.role").String() != "model" {
		t.Fatalf("indexed edits were not applied: %s", indexed)
	}
	if gjson.GetBytes(indexed, "invalid").Exists() {
		t.Fatalf("indexed edit unexpectedly used SJSON paths: %s", indexed)
	}

	fallbackEdits := []antigravityContentEdit{
		{index: 0, replacement: []byte(`{"role":"model","parts":[{"text":"fallback"}]}`)},
	}
	fallback := applyAntigravityIndexedEdits(payload, fallbackEdits, false)
	if text := gjson.GetBytes(fallback, "request.contents.0.parts.0.text").String(); text != "fallback" {
		t.Fatalf("SJSON fallback text = %q, want fallback; payload=%s", text, fallback)
	}
}
