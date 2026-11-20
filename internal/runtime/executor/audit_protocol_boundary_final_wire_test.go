package executor

import (
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditProtocolStrictConflictStreamingFinalWire(t *testing.T) {
	for _, provider := range []string{"gemini", "claude"} {
		for _, strict := range []bool{false, true} {
			t.Run(
				provider+map[bool]string{false: "/nested_false", true: "/nested_true"}[strict],
				func(t *testing.T) {
					nested, outer, mode, model := "false", "true", "AUTO", "gemini-3-flash"
					if strict {
						nested, outer, mode = "true", "false", "VALIDATED"
					}
					if provider == "claude" {
						model = "claude-sonnet-4-6"
					}
					payload := []byte(
						(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":` +
							`"function","strict":`) +
							outer +
							(`,"function":` +
								`{"name":` +
								`"tool_a",` +
								`"parameters":` +
								`{"type":` +
								`"object",` +
								`"properties":` +
								`{}},"strict":`) +
							nested +
							`}}],"tool_choice":"auto"}`,
					)
					wire := auditProtocolFinalWire(t, provider, model, translator.FormatOpenAI, payload, true)
					if provider == "gemini" && gjson.GetBytes(
						wire, "toolConfig.functionCallingConfig.mode",
					).String() != mode || provider == "claude" && gjson.GetBytes(wire, "tools.0.strict").Raw != nested {
						t.Fatalf("streaming strict conflict changed: %s", wire)
					}
				},
			)
		}
	}
}

func TestAuditProtocolClaudeStrictToGeminiFinalWire(t *testing.T) {
	for _, sample := range []struct{ name, strict, choice, mode, allowed string }{
		{"auto false", "false", `{"type":"auto"}`, "AUTO", ""},
		{"auto true", "true", `{"type":"auto"}`, "VALIDATED", ""},
		{"none true", "true", `{"type":"none"}`, "NONE", ""},
		{"any false", "false", `{"type":"any"}`, "ANY", ""},
		{"named true", "true", `{"type":"tool","name":"tool_a"}`, "ANY", "tool_a"},
		{"auto nonboolean", `"false"`, `{"type":"auto"}`, "AUTO", ""},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				payload := []byte((`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"tool_a"` +
					`,"input_schema":{"type":"object","properties":{}},"strict":`) +
					sample.strict +
					`}],"tool_choice":` +
					sample.choice +
					`}`)
				wire := auditProtocolFinalWire(
					t,
					"gemini",
					"gemini-3-flash",
					translator.FormatClaude,
					payload,
					false,
				)
				if gjson.GetBytes(
					wire, "toolConfig.functionCallingConfig.mode",
				).String() != sample.mode || gjson.GetBytes(
					wire, "toolConfig.functionCallingConfig.allowedFunctionNames.0",
				).String() != sample.allowed || gjson.GetBytes(wire, "tools.0.functionDeclarations.0.strict").Exists() {
					t.Fatalf("Claude strict or tool choice changed in final Gemini payload: %s", wire)
				}
			},
		)
	}
}

func TestAuditProtocolToolOnlyResultPrefixFinalWire(t *testing.T) {
	for _, target := range []struct {
		name, provider, model, path string
		source                      translator.Format
	}{
		{"Gemini Chat", "gemini", "gemini-3-flash", "contents", translator.FormatOpenAI},
		{"Gemini Responses", "gemini", "gemini-3-flash", "contents", translator.FormatOpenAIResponse},
		{"Antigravity Gemini Chat", "antigravity", "gemini-3-flash", "request.contents", translator.FormatOpenAI},
		{"Antigravity Claude Chat", "antigravity", "claude-sonnet-4-6", "request.contents", translator.FormatOpenAI},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(
				target.name+map[bool]string{false: "/Execute", true: "/ExecuteStream"}[stream],
				func(t *testing.T) {
					payload := []byte(
						(`{"messages":[{"role":"tool","tool_call_id":"orphan","content":"orphan-` +
							`result"},{"role":"developer","content":"keep-local-directive"}]}`),
					)
					if target.source == translator.FormatOpenAIResponse {
						payload = auditProtocolResponsesHistory(t, payload)
					}
					wire := auditProtocolFinalWire(
						t,
						target.provider,
						target.model,
						target.source,
						payload,
						stream,
					)
					var texts []string
					for _, content := range gjson.GetBytes(wire, target.path).Array() {
						for _, part := range content.Get("parts").Array() {
							texts = append(texts, part.Get("text").String())
						}
					}
					joined := strings.Join(texts, "\n")
					result := strings.Index(joined, "orphan-result")
					reminder := strings.Index(
						joined,
						"<system-reminder>\nkeep-local-directive\n</system-reminder>",
					)
					if result < 0 || reminder <= result || strings.Count(
						joined, "orphan-result",
					) != 1 || gjson.GetBytes(wire, "systemInstruction").Exists() || gjson.GetBytes(
						wire, "request.systemInstruction",
					).Exists() {
						t.Fatalf("tool-only prefix or local reminder changed in final payload: %s", wire)
					}
				},
			)
		}
	}
}

func TestAuditProtocolClaudeEmptyReasoningFinalWire(t *testing.T) {
	const payload = `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "reasoning_content": "unsigned reasoning"},
			{"role": "user", "content": "say ok"}
		]
	}`
	for _, stream := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "Execute", true: "ExecuteStream"}[stream], func(t *testing.T) {
				wire := auditProtocolFinalWire(
					t, "antigravity", "claude-sonnet-4-6", translator.FormatOpenAI, []byte(payload), stream,
				)
				contents := gjson.GetBytes(wire, "request.contents").Array()
				if len(contents) != 2 || contents[0].Get("role").String() != "user" ||
					contents[1].Get("role").String() != "user" ||
					contents[0].Get("parts.0.text").String() != "hi" ||
					contents[1].Get("parts.0.text").String() != "say ok" ||
					strings.Contains(string(wire), "unsigned reasoning") {
					t.Fatalf("empty reasoning assistant changed final Claude history: %s", wire)
				}
			},
		)
	}
}
