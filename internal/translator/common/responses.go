package common

import (
	"fmt"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponseIDCounter provides a process-wide unique counter for synthesized response identifiers.
var ResponseIDCounter uint64

// FuncCallIDCounter provides a process-wide unique counter for function call identifiers.
var FuncCallIDCounter uint64

// NextResponseID returns a new unique response ID suffix via atomic increment.
func NextResponseID() uint64 {
	return atomic.AddUint64(&ResponseIDCounter, 1)
}

// NextFuncCallID returns a new unique function call ID suffix via atomic increment.
func NextFuncCallID() uint64 {
	return atomic.AddUint64(&FuncCallIDCounter, 1)
}

// EmitEvent formats an SSE event line pair: "event: <event>\ndata: <payload>".
func EmitEvent(event string, payload string) string {
	return fmt.Sprintf("event: %s\ndata: %s", event, payload)
}

// PickRequestJSON returns the first valid JSON byte slice among originalRequestRawJSON and requestRawJSON.
func PickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

// EchoRequestFields copies standard OpenAI Responses request fields from req into target JSON.
// prefix is prepended to each field path (e.g. "response." for streaming completed events,
// or "" for non-streaming top-level response objects).
func EchoRequestFields(target string, req gjson.Result, prefix string) string {
	if v := req.Get("instructions"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"instructions", v.String())
	}
	if v := req.Get("max_output_tokens"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"max_output_tokens", v.Int())
	}
	if v := req.Get("max_tool_calls"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"max_tool_calls", v.Int())
	}
	if v := req.Get("model"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"model", v.String())
	}
	if v := req.Get("parallel_tool_calls"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"parallel_tool_calls", v.Bool())
	}
	if v := req.Get("previous_response_id"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"previous_response_id", v.String())
	}
	if v := req.Get("prompt_cache_key"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"prompt_cache_key", v.String())
	}
	if v := req.Get("reasoning"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"reasoning", v.Value())
	}
	if v := req.Get("safety_identifier"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"safety_identifier", v.String())
	}
	if v := req.Get("service_tier"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"service_tier", v.String())
	}
	if v := req.Get("store"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"store", v.Bool())
	}
	if v := req.Get("temperature"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"temperature", v.Float())
	}
	if v := req.Get("text"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"text", v.Value())
	}
	if v := req.Get("tool_choice"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"tool_choice", v.Value())
	}
	if v := req.Get("tools"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"tools", v.Value())
	}
	if v := req.Get("top_logprobs"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"top_logprobs", v.Int())
	}
	if v := req.Get("top_p"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"top_p", v.Float())
	}
	if v := req.Get("truncation"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"truncation", v.String())
	}
	if v := req.Get("user"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"user", v.Value())
	}
	if v := req.Get("metadata"); v.Exists() {
		target, _ = sjson.Set(target, prefix+"metadata", v.Value())
	}
	return target
}

// EmitFuncCallDone emits response.function_call_arguments.done and
// response.output_item.done events for a completed function call.
func EmitFuncCallDone(out *[]string, nextSeq func() int, idx int, callID, name, args string) {
	fcDone := `{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`
	fcDone, _ = sjson.Set(fcDone, "sequence_number", nextSeq())
	fcDone, _ = sjson.Set(fcDone, "item_id", fmt.Sprintf("fc_%s", callID))
	fcDone, _ = sjson.Set(fcDone, "output_index", idx)
	fcDone, _ = sjson.Set(fcDone, "arguments", args)
	*out = append(*out, EmitEvent("response.function_call_arguments.done", fcDone))

	itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`
	itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
	itemDone, _ = sjson.Set(itemDone, "output_index", idx)
	itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("fc_%s", callID))
	itemDone, _ = sjson.Set(itemDone, "item.arguments", args)
	itemDone, _ = sjson.Set(itemDone, "item.call_id", callID)
	itemDone, _ = sjson.Set(itemDone, "item.name", name)
	*out = append(*out, EmitEvent("response.output_item.done", itemDone))
}
