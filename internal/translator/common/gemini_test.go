package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestReorderGeminiUserPartsKeepsImageSlots(t *testing.T) {
	parts := [][]byte{
		[]byte(`{"functionResponse":{"name":"first","response":{"content":"one"}}}`),
		[]byte(`{"inlineData":{"mimeType":"image/png","data":"image-one"}}`),
		[]byte(`{"text":"trailing one"}`),
		[]byte(`{"function_response":{"name":"second","response":{"content":"two"}}}`),
		[]byte(`{"fileData":{"mimeType":"image/png","fileUri":"image-two"}}`),
		[]byte(`{"text":"trailing two"}`),
	}

	reordered := ReorderGeminiUserParts(parts)
	if got := gjson.GetBytes(reordered[0], "text").String(); got != "trailing one" {
		t.Fatalf("parts[0].text = %q, want trailing one", got)
	}
	if got := gjson.GetBytes(reordered[1], "inlineData.data").String(); got != "image-one" {
		t.Fatalf("inline image slot moved or changed: %q", got)
	}
	if got := gjson.GetBytes(reordered[2], "text").String(); got != "trailing two" {
		t.Fatalf("parts[2].text = %q, want trailing two", got)
	}
	if got := gjson.GetBytes(reordered[3], "functionResponse.name").String(); got != "first" {
		t.Fatalf("parts[3] function response = %q, want first", got)
	}
	if got := gjson.GetBytes(reordered[4], "fileData.fileUri").String(); got != "image-two" {
		t.Fatalf("file image slot moved or changed: %q", got)
	}
	if got := gjson.GetBytes(reordered[5], "function_response.name").String(); got != "second" {
		t.Fatalf("parts[5] function response = %q, want second", got)
	}
}

func TestMergeAdjacentGeminiContentsMergesReminderWithToolResults(t *testing.T) {
	contents := [][]byte{
		[]byte(`{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"call-a"}}]}`),
		[]byte(`{"role":"user","parts":[{"text":"<system-reminder>updated rule</system-reminder>"}]}`),
		[]byte(`{
			"role":"user",
			"parts":[
				{"functionResponse":{"name":"lookup","id":"call-a","response":{"content":"result"}}},
				{"inlineData":{"data":"image"}}
			]
		}`),
	}

	merged := MergeAdjacentGeminiContents(contents)
	if len(merged) != 2 {
		t.Fatalf("content count = %d, want 2: %s", len(merged), JoinRawArray(merged))
	}
	parts := gjson.GetBytes(merged[1], "parts").Array()
	if len(parts) != 3 {
		t.Fatalf("merged user part count = %d, want 3", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "<system-reminder>updated rule</system-reminder>" {
		t.Fatalf("reminder order = %q", got)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call-a" {
		t.Fatalf("function response = %q, want call-a", got)
	}
	if got := parts[2].Get("inlineData.data").String(); got != "image" {
		t.Fatalf("image slot moved or changed: %q", got)
	}
}

func TestMergeAdjacentGeminiUserContentsPreservesToolBoundaries(t *testing.T) {
	contents := [][]byte{
		[]byte(`{"role":"user","parts":[{"text":"question"}]}`),
		[]byte(`{"role":"user","parts":[{"text":"<system-reminder>first</system-reminder>"}]}`),
		[]byte(`{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"call-a"}}]}`),
		[]byte(`{
			"role":"user",
			"parts":[{"functionResponse":{"name":"lookup","id":"call-a","response":{"content":"result"}}}]
		}`),
		[]byte(`{"role":"user","parts":[{"text":"<system-reminder>second</system-reminder>"}]}`),
		[]byte(`{"role":"user","parts":[{"text":"follow-up"}]}`),
	}

	merged := MergeAdjacentGeminiUserContents(contents)
	if len(merged) != 4 {
		t.Fatalf("content count = %d, want 4: %s", len(merged), JoinRawArray(merged))
	}
	firstParts := gjson.GetBytes(merged[0], "parts").Array()
	if len(firstParts) != 2 || firstParts[0].Get("text").String() != "question" ||
		firstParts[1].Get("text").String() != "<system-reminder>first</system-reminder>" {
		t.Fatalf("leading user boundary lost order: %s", merged[0])
	}
	if !ContentHasGeminiFunctionResponse(merged[2]) {
		t.Fatalf("function response boundary was removed: %s", merged[2])
	}
	trailingParts := gjson.GetBytes(merged[3], "parts").Array()
	if len(trailingParts) != 2 ||
		trailingParts[0].Get("text").String() != "<system-reminder>second</system-reminder>" ||
		trailingParts[1].Get("text").String() != "follow-up" {
		t.Fatalf("trailing user boundary lost order: %s", merged[3])
	}
}

func TestSplitGeminiFunctionResponseTurnsCombinesParallelResults(t *testing.T) {
	contents := [][]byte{
		[]byte(`{"role":"model","parts":[{"functionCall":{"name":"first","id":"a"}},{"function_call":{"name":"second","id":"b"}}]}`),
		[]byte(`{"role":"user","turn_metadata":"preserved","parts":[{"functionResponse":{"id":"a","name":"first","response":{"result":1},"extra":"kept"}}]}`),
		[]byte(`{"role":"user","parts":[{"function_response":{"id":"b","name":"second","response":{"result":2}}}]}`),
	}
	result := MergeAdjacentGeminiUserContents(SplitGeminiFunctionResponseTurns(contents))
	if len(result) != 2 || string(result[0]) != string(contents[0]) {
		t.Fatalf("parallel response turns = %s, want two turns", JoinRawArray(result))
	}
	if len(gjson.GetBytes(result[1], "parts").Array()) != 2 ||
		gjson.GetBytes(result[1], "parts.0.functionResponse.id").String() != "a" ||
		gjson.GetBytes(result[1], "parts.0.functionResponse.extra").String() != "kept" ||
		gjson.GetBytes(result[1], "parts.1.function_response.id").String() != "b" ||
		gjson.GetBytes(result[1], "turn_metadata").String() != "preserved" {
		t.Fatalf("parallel response order or metadata changed: %s", result[1])
	}
	if unmatched := SplitGeminiFunctionResponseTurns(contents[1:]); len(unmatched) != 2 {
		t.Fatalf("results without preceding calls must remain separate: %s", JoinRawArray(unmatched))
	}
}

func TestMergeAdjacentGeminiUserContentsDropsOnlyEmptyContents(t *testing.T) {
	contents := [][]byte{
		nil,
		[]byte(`{"role":"user","parts":[]}`),
		[]byte(`{"role":"user","parts":[{"text":"kept"}]}`),
	}

	merged := MergeAdjacentGeminiUserContents(contents)
	if len(merged) != 1 || gjson.GetBytes(merged[0], "parts.0.text").String() != "kept" {
		t.Fatalf("merged contents = %s, want one kept turn", JoinRawArray(merged))
	}
}
