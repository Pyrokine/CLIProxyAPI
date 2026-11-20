package openai

import (
	"bytes"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseOutputIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"
	callCache.record(
		sessionKey, "call-1", []byte(
			`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"}`,
		),
	)
	raw := []byte(`{"previous_response_id":"resp-latest","input":[` +
		`{"type":"function_call_output","call_id":"call-1","id":"tool-out-1","output":"ok"},` +
		`{"type":"message","id":"msg-1"}]}`)
	before := bytes.Clone(raw)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)
	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "function_call_output" ||
		input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
	if !bytes.Equal(raw, before) || !bytes.Equal(repaired, before) {
		t.Fatal("native continuation or caller buffer changed")
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseCallIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"
	outputCache.record(
		sessionKey, "call-1", []byte(
			`{"type":"function_call_output","call_id":"call-1","id":"tool-out-1","output":"ok"}`,
		),
	)
	raw := []byte(`{"previous_response_id":"resp-latest","input":[` +
		`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"},` +
		`{"type":"message","id":"msg-1"}]}`)
	before := bytes.Clone(raw)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)
	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "function_call" ||
		input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
	if !bytes.Equal(raw, before) || !bytes.Equal(repaired, before) {
		t.Fatal("native continuation or caller buffer changed")
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseCustomToolOutputIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"
	callCache.record(
		sessionKey, "call-1", []byte(
			`{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"}`,
		),
	)
	raw := []byte(`{"previous_response_id":"resp-latest","input":[` +
		`{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"},` +
		`{"type":"message","id":"msg-1"}]}`)
	before := bytes.Clone(raw)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)
	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "custom_tool_call_output" ||
		input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
	if !bytes.Equal(raw, before) || !bytes.Equal(repaired, before) {
		t.Fatal("native continuation or caller buffer changed")
	}
}
