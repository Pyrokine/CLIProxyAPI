package claude

import (
	"context"
	"fmt"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravitySDKAlignsPermutedParallelToolResultsWithMixedText(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.7-flash-high",
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_1620603","name":"Read","input":{"file_path":"/tmp/1"}},
				{"type":"tool_use","id":"call_1620604","name":"Read","input":{"file_path":"/tmp/2"}},
				{"type":"tool_use","id":"call_1620605","name":"Read","input":{"file_path":"/tmp/3"}},
				{"type":"tool_use","id":"call_1620606","name":"Read","input":{"file_path":"/tmp/4"}},
				{"type":"tool_use","id":"call_1620607","name":"Read","input":{"file_path":"/tmp/5"}},
				{"type":"tool_use","id":"call_1620608","name":"Read","input":{"file_path":"/tmp/6"}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"Tool results follow."},
				{"type":"tool_result","tool_use_id":"call_1620608","content":"six"},
				{"type":"tool_result","tool_use_id":"call_1620605","content":"three"},
				{"type":"tool_result","tool_use_id":"call_1620603","content":"one"},
				{"type":"text","text":"Continue after reading."},
				{"type":"tool_result","tool_use_id":"call_1620607","content":"five"},
				{"type":"tool_result","tool_use_id":"call_1620604","content":"two"},
				{"type":"tool_result","tool_use_id":"call_1620606","content":"four"}
			]}
		]
	}`)

	output, err := sdk.TranslateRequestChecked(
		context.Background(),
		sdk.FormatClaude,
		sdk.FormatAntigravity,
		"gemini-3.7-flash-high",
		inputJSON,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(output, "request.contents.1.parts").Array()
	if len(parts) != 8 {
		t.Fatalf("parts = %d, want eight parts; output=%s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "Tool results follow." {
		t.Fatalf("leading text = %q; output=%s", got, output)
	}
	for index := range 3 {
		wantID := fmt.Sprintf("call_162060%d", index+3)
		if gotID := parts[index+1].Get("functionResponse.id").String(); gotID != wantID {
			t.Fatalf("functionResponse[%d].id = %q, want %q; output=%s", index+1, gotID, wantID, output)
		}
	}
	if got := parts[4].Get("text").String(); got != "Continue after reading." {
		t.Fatalf("middle text = %q; output=%s", got, output)
	}
	for index := 3; index < 6; index++ {
		wantID := fmt.Sprintf("call_162060%d", index+3)
		if gotID := parts[index+2].Get("functionResponse.id").String(); gotID != wantID {
			t.Fatalf("functionResponse[%d].id = %q, want %q; output=%s", index+2, gotID, wantID, output)
		}
	}
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(output); errPairing != nil {
		t.Fatalf("translated parallel tool history is invalid: %v; output=%s", errPairing, output)
	}
}

func TestAntigravitySDKLeadingNativeCarrierTargetsUnsignedThought(t *testing.T) {
	signature := testGeminiEPrefixSignature(t)
	var state any
	response := []byte(
		(`{"response":{"candidates":[{"content":{"parts":[{"text":"",` +
			`"thoughtSignature":` +
			`"`) +
			signature +
			(`"},{"thought":` +
				`true,"text":` +
				`"reason"}]},` +
				`"finishReason"` +
				`:"STOP"}]}}`),
	)
	message := sdk.TranslateNonStream(
		context.Background(), sdk.FormatAntigravity, sdk.FormatClaude, "gemini-3.7-flash-high", nil,
		[]byte(`{"model":"gemini-3.7-flash-high"}`), response, &state,
	)
	if content := gjson.GetBytes(message, "content").Array(); len(content) != 2 {
		t.Fatalf("native carrier response must retain two blocks: %s", message)
	}
	request := []byte(
		`{"messages":[{"role":"user","content":"query"},{"role":"assistant","content":` + gjson.GetBytes(
			message, "content",
		).Raw + `},{"role":"user","content":"continue"}]}`,
	)
	request = StripInvalidGeminiSignatureThinkingBlocks(request)
	if content := gjson.GetBytes(request, "messages.1.content").Array(); len(content) != 2 {
		t.Fatalf("signature prevalidation must retain two blocks: %s", request)
	}
	out, err := sdk.TranslateRequestChecked(
		context.Background(), sdk.FormatClaude, sdk.FormatAntigravity, "gemini-3.7-flash-high", request, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, content := range gjson.GetBytes(out, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("thought").Bool() && part.Get("text").String() == "reason" {
				found = true
				if part.Get("thoughtSignature").String() != signature {
					t.Fatalf("native carrier lost target: %s", out)
				}
			}
		}
	}
	if !found {
		t.Fatalf("thought missing: source=%s translated=%s", message, out)
	}
}
