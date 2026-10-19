package executor

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIToolIdentityNestedNamespaceAndBoundarySafety(t *testing.T) {
	body := []byte(`{
		"tools":[{
			"type":"namespace","name":"outer","tools":[{
				"type":"namespace","name":"inner","tools":[
					{"type":"function","name":"lookup","parameters":{"type":"object"}},
					{"type":"function","name":"example_tool","parameters":{"type":"object"}}
				]
			}]
		}],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[
			{"type":"function","namespace":"outer__inner","name":"lookup"}
		]}
	}`)
	normalizer := newXAIToolIdentityNormalizer(body)
	out, err := normalizer.normalizeDeclarations(body)
	if err != nil {
		t.Fatalf("normalizeDeclarations() error = %v", err)
	}

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "outer__inner__lookup" {
		t.Fatalf("nested tool name = %q, want outer__inner__lookup; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "outer__inner__example_tool" {
		t.Fatalf("boundary-safe tool name = %q, want outer__inner__example_tool; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.tools.0.name").String(); got != "outer__inner__lookup" {
		t.Fatalf("allowed tool name = %q, want outer__inner__lookup; body=%s", got, out)
	}
	if gjson.GetBytes(out, "tool_choice.tools.0.namespace").Exists() {
		t.Fatalf("allowed tool namespace should be removed: %s", out)
	}

	event := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"outer__inner__lookup","call_id":"call-1"}}`)
	restored := normalizer.newResponseNormalizer(false).normalize(event)
	if got := gjson.GetBytes(restored, "item.name").String(); got != "lookup" {
		t.Fatalf("restored name = %q, want lookup; event=%s", got, restored)
	}
	if got := gjson.GetBytes(restored, "item.namespace").String(); got != "outer__inner" {
		t.Fatalf("restored namespace = %q, want outer__inner; event=%s", got, restored)
	}
}

func TestXAIToolIdentityCollisionDoesNotRestoreAmbiguousRoute(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		wantFailure bool
	}{
		{
			name: "namespace collision",
			body: []byte(`{"tools":[
				{"type":"namespace","name":"a","tools":[{"type":"function","name":"b__c"}]},
				{"type":"namespace","name":"a__b","tools":[{"type":"function","name":"c"}]}
			]}`),
			wantFailure: true,
		},
		{
			name: "top level collision",
			body: []byte(`{"tools":[
				{"type":"namespace","name":"a","tools":[{"type":"function","name":"b"}]},
				{"type":"function","name":"a__b"}
			]}`),
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				normalizer := newXAIToolIdentityNormalizer(tt.body)
				event := []byte(`{"item":{"type":"function_call","name":"a__b__c"}}`)
				if tt.name == "top level collision" {
					event = []byte(`{"item":{"type":"function_call","name":"a__b"}}`)
				}
				got := normalizer.newResponseNormalizer(false).normalize(event)
				if tt.wantFailure {
					if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.failed" {
						t.Fatalf("ambiguous namespace route was preserved: got=%s", got)
					}
					return
				}
				if !bytes.Equal(got, event) {
					t.Fatalf("direct declared route changed: got=%s want=%s", got, event)
				}
			},
		)
	}
}

func TestXAIToolIdentityPromotesAdditionalToolsAndNormalizesChoices(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"x_search"},{"type":"web_search"}],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[
			{"type":"function","namespace":"extra","name":"run"}
		]},
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"extra","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}]},
			{"type":"message","role":"user","content":"hello"}
		]
	}`)
	normalizer := newXAIToolIdentityNormalizer(body)
	out, err := normalizer.normalizeDeclarations(body)
	if err != nil {
		t.Fatalf("normalizeDeclarations() error = %v", err)
	}

	if got := gjson.GetBytes(out, "input.#").Int(); got != 1 {
		t.Fatalf("input item count = %d, want 1; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.2.name").String(); got != "extra__run" {
		t.Fatalf("promoted tool name = %q, want extra__run; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.tools.0.name").String(); got != "extra__run" {
		t.Fatalf("allowed namespaced tool = %q, want extra__run; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, `tool_choice.tools.#(type=="x_search")#`).Array(); len(got) != 1 {
		t.Fatalf("allowed x_search count = %d, want 1; body=%s", len(got), out)
	}

	forced, err := normalizer.normalizeDeclarations(
		[]byte(`{
		"tools":[{"type":"web_search"}],
		"tool_choice":{"type":"web_search"}
	}`),
	)
	if err != nil {
		t.Fatalf("normalizeDeclarations() forced error = %v", err)
	}
	if got := gjson.GetBytes(forced, "tool_choice").String(); got != "required" {
		t.Fatalf("forced web_search choice = %q, want required; body=%s", got, forced)
	}
}

func TestXAIToolIdentityNormalizesCustomAndNamespaceHistoryInOnePass(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"custom_tool_call","call_id":"call-1","name":"shell","input":"{\"cmd\":\"pwd\"}"},
		{"type":"custom_tool_call_output","call_id":"call-1","output":{"ok":true}},
		{"type":"function_call","call_id":"call-2","namespace":"ops","name":"run","arguments":"{}"},
		{"type":"custom_tool_call","name":"missing-id","input":"ignored"},
		{"type":"custom_tool_call_output","output":"missing-id"}
	]}`)
	out := newXAIToolIdentityNormalizer(body).normalizeInputHistory(body)

	if got := gjson.GetBytes(out, "input.#").Int(); got != 3 {
		t.Fatalf("input item count = %d, want 3; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.0.type").String(); got != "function_call" {
		t.Fatalf("custom call type = %q, want function_call; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.0.arguments").String(); got != `{"cmd":"pwd"}` {
		t.Fatalf("custom call arguments = %q, want JSON object string; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.1.type").String(); got != "function_call_output" {
		t.Fatalf("custom output type = %q, want function_call_output; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.1.output").String(); got != `{"ok":true}` {
		t.Fatalf("custom output = %q, want serialized object; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.2.name").String(); got != "ops__run" {
		t.Fatalf("namespace history name = %q, want ops__run; body=%s", got, out)
	}
	if gjson.GetBytes(out, "input.2.namespace").Exists() {
		t.Fatalf("namespace history field should be removed: %s", out)
	}
}

func TestXAIToolIdentityFiltersOnlyInternalXSearchTrace(t *testing.T) {
	body := []byte(`{"tools":[
		{"type":"x_search"},
		{"type":"custom","name":"x_keyword_search"},
		{"type":"namespace","name":"social","tools":[{"type":"function","name":"x_user_search"}]}
	]}`)
	normalizer := newXAIToolIdentityNormalizer(body)
	response := normalizer.newResponseNormalizer(true)

	internal := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","name":"x_keyword_search","call_id":"xs_call-1"}}`)
	if got := response.normalize(internal); got != nil {
		t.Fatalf("internal X Search trace was preserved: %s", got)
	}
	if got := response.normalize([]byte(`{"type":"response.custom_tool_call_input.delta","output_index":0,"call_id":"xs_call-1","delta":"x"}`)); got != nil {
		t.Fatalf("internal X Search child event was preserved: %s", got)
	}

	clientFunction := []byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"x_keyword_search","call_id":"call-user"}}`)
	kept := response.normalize(clientFunction)
	if len(kept) == 0 {
		t.Fatal("client-declared same-name function call was filtered")
	}
	if got := gjson.GetBytes(kept, "output_index").Int(); got != 0 {
		t.Fatalf("compacted output_index = %d, want 0; event=%s", got, kept)
	}

	namespaced := []byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","name":"social__x_user_search","call_id":"call-social"}}`)
	kept = response.normalize(namespaced)
	if got := gjson.GetBytes(kept, "item.namespace").String(); got != "social" {
		t.Fatalf("namespaced client tool namespace = %q, want social; event=%s", got, kept)
	}
	if got := gjson.GetBytes(kept, "item.name").String(); got != "x_user_search" {
		t.Fatalf("namespaced client tool name = %q, want x_user_search; event=%s", got, kept)
	}

	completed := []byte(`{"type":"response.completed","response":{"output":[
		{"type":"custom_tool_call","name":"x_thread_fetch","call_id":"xs_call-2"},
		{"type":"function_call","name":"x_keyword_search","call_id":"call-user"}
	]}}`)
	completed = response.normalize(completed)
	if got := gjson.GetBytes(completed, "response.output.#").Int(); got != 1 {
		t.Fatalf("completed output count = %d, want 1; event=%s", got, completed)
	}
	if got := gjson.GetBytes(completed, "response.output.0.call_id").String(); got != "call-user" {
		t.Fatalf("completed client call_id = %q, want call-user; event=%s", got, completed)
	}
}

func TestXAIToolIdentityResponseStateIsolatedAcrossConcurrentRequests(t *testing.T) {
	body := []byte(`{"tools":[{"type":"x_search"},{"type":"function","name":"client_tool"}]}`)
	normalizer := newXAIToolIdentityNormalizer(body)

	const requests = 32
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for range requests {
		wg.Go(func() {
			response := normalizer.newResponseNormalizer(true)
			if got := response.normalize([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","name":"x_keyword_search","call_id":"xs_call-internal"}}`)); got != nil {
				errs <- fmt.Errorf("internal X Search trace was preserved: %s", got)
				return
			}
			kept := response.normalize([]byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"client_tool","call_id":"call-client"}}`))
			if got := gjson.GetBytes(kept, "output_index").Int(); got != 0 {
				errs <- fmt.Errorf("compacted output_index = %d, want 0; event=%s", got, kept)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestXAIToolIdentityPreservesMalformedInput(t *testing.T) {
	malformed := []byte(`{"tools":[{"type":"namespace","name":"broken"}`)
	normalizer := newXAIToolIdentityNormalizer(malformed)
	got, err := normalizer.normalizeDeclarations(malformed)
	if err != nil {
		t.Fatalf("normalizeDeclarations() malformed error = %v", err)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatalf("malformed request changed: got=%q want=%q", got, malformed)
	}
	if got := normalizer.normalizeInputHistory(malformed); !bytes.Equal(got, malformed) {
		t.Fatalf("malformed history changed: got=%q want=%q", got, malformed)
	}
	event := []byte(`{"item":{"type":"function_call","name":"broken"}`)
	if got := normalizer.newResponseNormalizer(true).normalize(event); !bytes.Equal(got, event) {
		t.Fatalf("malformed response changed: got=%q want=%q", got, event)
	}
}

func TestXAIToolIdentityHTTPWebSocketParity(t *testing.T) {
	payload := []byte(`{
		"model":"grok-4.3",
		"tools":[
			{"type":"x_search"},
			{"type":"namespace","name":"ops","tools":[{"type":"custom","name":"run","parameters":{"type":"object"}}]}
		],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","namespace":"ops","name":"run"}]},
		"input":[
			{"type":"custom_tool_call","call_id":"call-1","name":"ops__run","input":{"cmd":"pwd"}},
			{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}
		]
	}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	httpExecutor := newXAIExecutor(&config.Config{})
	httpPrepared, err := httpExecutor.prepareResponsesRequest(req, opts, true)
	if err != nil {
		t.Fatalf("HTTP prepare error = %v", err)
	}
	wsExecutor := newXAIWebsocketsExecutor(&config.Config{})
	wsPrepared, err := wsExecutor.prepareResponsesWebsocketRequest(context.Background(), nil, req, opts)
	if err != nil {
		t.Fatalf("WebSocket prepare error = %v", err)
	}
	if !bytes.Equal(buildXAIWebsocketRequestBody(httpPrepared.body), wsPrepared.body) {
		t.Fatalf("HTTP/WS prepared bodies differ:\nHTTP=%s\nWS=%s", httpPrepared.body, wsPrepared.body)
	}

	events := [][]byte{
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","name":"x_user_search","call_id":"xs_call-1"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"ops__run","call_id":"call-1"}}`),
	}
	httpResponse := httpPrepared.toolIdentity.newResponseNormalizer(httpPrepared.filterInternalXSearch)
	wsResponse := wsPrepared.toolIdentity.newResponseNormalizer(wsPrepared.filterInternalXSearch)
	for _, event := range events {
		httpOut := httpResponse.normalize(event)
		wsOut := wsResponse.normalize(event)
		if !bytes.Equal(httpOut, wsOut) {
			t.Fatalf("HTTP/WS response normalization differs for %s:\nHTTP=%s\nWS=%s", event, httpOut, wsOut)
		}
	}
}
