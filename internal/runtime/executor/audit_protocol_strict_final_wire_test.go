package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditProtocolStrictPrecedenceFinalWire(t *testing.T) {
	for _, sample := range []struct {
		name, nested, outer, mode, strict string
	}{
		{"nested false overrides outer true", `,"strict":false`, `,"strict":true`, "AUTO", "false"},
		{"nested true overrides outer false", `,"strict":true`, `,"strict":false`, "VALIDATED", "true"},
		{"outer true when nested absent", "", `,"strict":true`, "VALIDATED", "true"},
		{"nested false", `,"strict":false`, "", "AUTO", "false"},
		{"nested true", `,"strict":true`, "", "VALIDATED", "true"},
		{"both false", `,"strict":false`, `,"strict":false`, "AUTO", "false"},
		{"both absent", "", "", "AUTO", ""},
		{"nested null is not absent", `,"strict":null`, `,"strict":true`, "AUTO", ""},
		{"nested string is not boolean", `,"strict":"false"`, `,"strict":true`, "AUTO", ""},
	} {
		for _, provider := range []string{"gemini", "claude"} {
			t.Run(
				provider+"/"+sample.name, func(t *testing.T) {
					payload := []byte(
						(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":` +
							`"function"`) +
							sample.outer +
							(`,"function":` +
								`{"name":` +
								`"tool_a",` +
								`"parameters":` +
								`{"type":` +
								`"object",` +
								`"properties":` +
								`{}}`) +
							sample.nested +
							`}}],"tool_choice":"auto"}`,
					)
					model := "gemini-3-flash"
					if provider == "claude" {
						model = "claude-sonnet-4-6"
					}
					wire := auditProtocolFinalWire(t, provider, model, translator.FormatOpenAI, payload, false)
					if provider == "gemini" {
						if mode := gjson.GetBytes(
							wire, "toolConfig.functionCallingConfig.mode",
						).String(); mode != sample.mode {
							t.Fatalf("wire mode=%s want=%s payload=%s", mode, sample.mode, wire)
						}
						if gjson.GetBytes(wire, "tools.0.functionDeclarations.0.strict").Exists() {
							t.Fatal("Gemini wire contains unsupported strict declaration")
						}
						if gjson.GetBytes(wire, "generationConfig.temperature").Float() != 0.25 {
							t.Fatal("wire did not pass the final payload rule")
						}
					} else {
						if strict := gjson.GetBytes(wire, "tools.0.strict").Raw; strict != sample.strict {
							t.Fatalf("wire strict=%q want=%q payload=%s", strict, sample.strict, wire)
						}
						if gjson.GetBytes(wire, "tool_choice.type").String() != "auto" || gjson.GetBytes(
							wire, "max_tokens",
						).Int() != 16 {
							t.Fatalf("Claude tool choice or final payload rule changed: %s", wire)
						}
					}
				},
			)
		}
	}
}

func TestAuditProtocolGeminiExplicitToolChoiceFinalWire(t *testing.T) {
	for _, sample := range []struct{ choice, mode, allowed string }{
		{`"none"`, "NONE", ""},
		{`"required"`, "ANY", ""},
		{`{"type":"function","function":{"name":"tool_a"}}`, "ANY", "tool_a"},
	} {
		t.Run(
			sample.mode+"/"+sample.allowed, func(t *testing.T) {
				payload := []byte((`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","` +
					`strict":true,"function":{"name":"tool_a","parameters":{"type":"object","properties":{}},` +
					`"strict":false}}],"tool_choice":`) + sample.choice + `}`)
				wire := auditProtocolFinalWire(
					t,
					"gemini",
					"gemini-3-flash",
					translator.FormatOpenAI,
					payload,
					false,
				)
				if gjson.GetBytes(
					wire, "toolConfig.functionCallingConfig.mode",
				).String() != sample.mode || gjson.GetBytes(
					wire, "toolConfig.functionCallingConfig.allowedFunctionNames.0",
				).String() != sample.allowed {
					t.Fatalf("explicit tool choice changed: %s", wire)
				}
			},
		)
	}
}
