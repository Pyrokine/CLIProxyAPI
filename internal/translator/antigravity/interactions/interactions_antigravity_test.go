package interactions

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertInteractionsRequestToAntigravityKeepsSchemaReferencesOpaque(t *testing.T) {
	payload := []byte(`{"input":[{"type":"function_result","name":"lookup","call_id":"call-1","result":{"schema":{"$ref":"#/components/schemas/Item"}}}]}`)
	out := convertInteractionsRequestToAntigravity("gemini-3-flash", payload, false)
	result := gjson.GetBytes(out, "request.contents.0.parts.0.functionResponse.response.result")
	if result.Type != gjson.String || result.String() != `{"schema":{"$ref":"#/components/schemas/Item"}}` {
		t.Fatalf("function result with $ref = %s, output = %s", result.Raw, out)
	}
}

func TestConvertInteractionsRequestToAntigravityEffortDoesNotSetSummary(t *testing.T) {
	out := convertInteractionsRequestToAntigravity(
		"gemini-3-flash",
		[]byte(`{"reasoning":{"effort":"high"},"input":"hello"}`),
		false,
	)
	if got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "high" {
		t.Fatalf("thinkingLevel = %q, want high; output=%s", got, out)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts").Exists() {
		t.Fatalf("effort alone set includeThoughts: %s", out)
	}
}

func TestConvertInteractionsRequestToAntigravityReasoningSummaryControlsVisibility(t *testing.T) {
	for _, testCase := range []struct {
		summary string
		want    bool
	}{
		{summary: "auto", want: true},
		{summary: "none", want: false},
	} {
		t.Run(
			testCase.summary, func(t *testing.T) {
				out := convertInteractionsRequestToAntigravity(
					"gemini-3-flash",
					[]byte(`{"reasoning":{"effort":"high","summary":"`+testCase.summary+`"},"input":"hello"}`),
					false,
				)
				if got := gjson.GetBytes(
					out, "request.generationConfig.thinkingConfig.thinkingLevel",
				).String(); got != "high" {
					t.Fatalf("thinkingLevel = %q, want high; output=%s", got, out)
				}
				includeThoughts := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
				if !includeThoughts.Exists() || includeThoughts.Bool() != testCase.want {
					t.Fatalf("includeThoughts = %s, want %v; output=%s", includeThoughts.Raw, testCase.want, out)
				}
			},
		)
	}
}

func TestConvertInteractionsRequestToAntigravityRequiresSummaryEnum(t *testing.T) {
	for _, summary := range []string{"true", `"false"`, `"detailed"`} {
		out := convertInteractionsRequestToAntigravity(
			"gemini-3-flash",
			[]byte(`{"generation_config":{"thinking_summaries":`+summary+`},"input":"hello"}`),
			false,
		)
		if gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts").Exists() {
			t.Fatalf("invalid summary %s set includeThoughts: %s", summary, out)
		}
	}
}

func TestConvertInteractionsRequestToAntigravityRequiresBooleanIncludeThoughts(t *testing.T) {
	for _, body := range []string{
		`{"generation_config":{"include_thoughts":"true"},"input":"hello"}`,
		`{"generation_config":{"thinking_config":{"include_thoughts":1}},"input":"hello"}`,
	} {
		out := convertInteractionsRequestToAntigravity("gemini-3-flash", []byte(body), false)
		if value := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts"); value.Exists() {
			t.Fatalf("invalid includeThoughts selector survived as %s: %s", value.Raw, out)
		}
	}
}

func TestConvertInteractionsRequestToAntigravityDeduplicatesAndDisambiguatesTools(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	inputJSON := []byte(`{
		"input":[
			{"type":"function_call","name":"` + second + `","call_id":"call_1","arguments":{}},
			{"type":"function_result","name":"` + second + `","call_id":"call_1","result":{}}
		],
		"tools":[
			{"functionDeclarations":[{"name":"lookup"},{"name":"` + first + `"}]},
			{"function_declarations":[{"name":"lookup"},{"name":"` + second + `"}]}
		],
		"tool_choice":{"type":"function","function":{"name":"` + second + `"}}
	}`)

	out := convertInteractionsRequestToAntigravity("antigravity-test", inputJSON, false)
	declarations := gjson.GetBytes(out, "request.tools.0.functionDeclarations").Array()
	if len(declarations) != 3 {
		t.Fatalf("declaration count = %d, want 3. Output: %s", len(declarations), out)
	}
	firstMapped := declarations[1].Get("name").String()
	secondMapped := declarations[2].Get("name").String()
	if firstMapped == secondMapped || len(secondMapped) > 64 {
		t.Fatalf("collision names = %q and %q, want distinct names <= 64 chars", firstMapped, secondMapped)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.name").String(); got != secondMapped {
		t.Fatalf("functionCall.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(out, "request.contents.1.parts.0.functionResponse.name").String(); got != secondMapped {
		t.Fatalf("functionResponse.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(
		out, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	).String(); got != secondMapped {
		t.Fatalf("allowedFunctionNames.0 = %q, want %q. Output: %s", got, secondMapped, out)
	}
}

func TestConvertAntigravityResponseToInteractionsStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[{"name":"` + first + `"},{"name":"` + second + `"}]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	raw := []byte(`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)
	var param any

	events := convertAntigravityResponseToInteractions(
		context.Background(),
		"antigravity-test",
		original,
		nil,
		raw,
		&param,
	)
	for _, event := range events {
		for line := range bytes.SplitSeq(event, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if gjson.GetBytes(payload, "event_type").String() == "step.start" {
				if got := gjson.GetBytes(payload, "step.name").String(); got != second {
					t.Fatalf("stream function call name = %q, want %q. Payload: %s", got, second, payload)
				}
				return
			}
		}
	}
	t.Fatalf("step.start event missing: %q", events)
}

func TestConvertAntigravityResponseToInteractionsRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[{"name":"` + first + `"},{"name":"` + second + `"}]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)

	out := convertAntigravityResponseToInteractionsNonStream(
		context.Background(),
		"antigravity-test",
		original,
		nil,
		raw,
		nil,
	)
	if got := gjson.GetBytes(out, "steps.0.name").String(); got != second {
		t.Fatalf("function call name = %q, want %q. Output: %s", got, second, out)
	}
}

func TestRewriteInteractionsFunctionNamesReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}],"toolConfig":{"functionCallingConfig":{"allowedFunctionNames":["lookup"]}}}}`)

	output := rewriteInteractionsFunctionNames(input, nil)

	if &output[0] != &input[0] {
		t.Fatal("normalized function names caused a payload copy")
	}
}

func TestRewriteInteractionsFunctionNamesNormalizesNonStringNames(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":true,"args":{}}}]}],"toolConfig":{"functionCallingConfig":{"allowedFunctionNames":[true]}}}}`)

	output := rewriteInteractionsFunctionNames(input, nil)

	if name := gjson.GetBytes(
		output, "request.contents.0.parts.0.functionCall.name",
	); name.Type != gjson.String || name.String() != "true" {
		t.Fatalf("functionCall.name = %s, want string true", name.Raw)
	}
	if name := gjson.GetBytes(
		output, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	); name.Type != gjson.String || name.String() != "true" {
		t.Fatalf("allowedFunctionNames.0 = %s, want string true", name.Raw)
	}
}

func TestConvertInteractionsRequestToAntigravityToolChoiceNoneOmitsTools(t *testing.T) {
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		input := []byte(`{"input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":` + choice + `}`)
		out := convertInteractionsRequestToAntigravity("gemini-test", input, false)
		if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
			t.Fatalf("tool mode = %q, want NONE; output=%s", got, out)
		}
		if gjson.GetBytes(out, "request.tools").Exists() {
			t.Fatalf("request.tools must be omitted; output=%s", out)
		}
	}
}

func TestConvertAntigravityResponseToInteractionsValidatesSignatures(t *testing.T) {
	valid := antigravityInteractionsSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "foreign", signature: antigravityClaudeSignatureForTest()},
		{name: "invalid", signature: "gemini#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"` + testCase.signature + `"}]}}]}}`)
			out := convertAntigravityResponseToInteractionsNonStream(
				context.Background(), "antigravity-test", nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "steps.0.signature").String(); got != testCase.want {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.want, out)
			}
			if testCase.want == "" && gjson.GetBytes(out, "steps.#").Int() != 0 {
				t.Fatalf("invalid signature-only part produced a step: %s", out)
			}
		})
	}
}

func TestConvertAntigravityResponseToInteractionsStreamPreservesSignatureOnlyPart(t *testing.T) {
	valid := antigravityInteractionsSignatureForTest()
	var state any
	events := convertAntigravityResponseToInteractions(
		context.Background(),
		"antigravity-test",
		nil,
		nil,
		[]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"`+valid+`"}]}}]}}`),
		&state,
	)
	for _, event := range events {
		for line := range bytes.SplitSeq(event, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := gjson.ParseBytes(bytes.TrimSpace(line[len("data:"):]))
			if got := payload.Get("delta.signature").String(); got == valid {
				return
			}
		}
	}
	t.Fatalf("valid signature delta missing: %q", events)
}

func antigravityInteractionsSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func antigravityClaudeSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}
