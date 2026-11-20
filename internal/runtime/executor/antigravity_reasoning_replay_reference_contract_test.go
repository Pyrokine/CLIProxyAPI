package executor

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAntigravityReasoningReplayItemsNilnessMatchesLegacy(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "missing contents", payload: []byte(`{}`)},
		{name: "malformed contents", payload: []byte(`{"request":{"contents":`)},
		{name: "contents not an array", payload: []byte(`{"request":{"contents":{"role":"model"}}}`)},
		{name: "empty contents", payload: []byte(`{"request":{"contents":[]}}`)},
		{
			name: "no model turn", payload: []byte(`{"request":{"contents":[{"role":"user",` +
				`"parts":[{"text":"hi"}]}]}}`),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				want := legacyAntigravityReasoningReplayItemsFromRequestForTest(test.payload)
				got := antigravityReasoningReplayItemsFromRequest(test.payload)
				if (want == nil) != (got == nil) {
					t.Fatalf("nil-ness differs: legacy nil=%t, indexed nil=%t", want == nil, got == nil)
				}
				if len(got) != len(want) {
					t.Fatalf("items = %d, want %d", len(got), len(want))
				}
			},
		)
	}
}

func TestFilterAntigravityReasoningReplayItemsWithIndexMatchesLegacy(t *testing.T) {
	payload := []byte(`{
		"request":{"contents":[
			{"role":"user","parts":[{"text":"hello"}]},` +
		`
			{"role":"model","parts":[
				{"text":"answer",` +
		`"thoughtSignature":"sig-text"},
				{"functionCall":{"id":"call-1",` +
		`"name":"lookup","args":{"value":1}},` +
		`"thoughtSignature":"sig-call"}
			]},` +
		`
			{"role":"model","parts":[{"functionResponse":{"id":"call` +
		`-1","name":"lookup",` +
		`"response":{"result":"ok"}}}]}
		]}
	}`)
	items := legacyAntigravityReasoningReplayItemsFromRequestForTest(payload)
	withoutSignatures, errDelete := sjson.DeleteBytes(payload, "request.contents.1.parts.0.thoughtSignature")
	if errDelete != nil {
		t.Fatal(errDelete)
	}
	withoutSignatures, errDelete = sjson.DeleteBytes(withoutSignatures, "request.contents.1.parts.1.thoughtSignature")
	if errDelete != nil {
		t.Fatal(errDelete)
	}

	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "already present", payload: payload},
		{name: "missing signatures", payload: withoutSignatures},
		{
			name: "missing call", payload: []byte(`{"request":{"contents":[{"role":"user",` +
				`"parts":[{"text":"hello"}]}]}}`),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				want := legacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(test.payload, items, nil)
				got := filterAntigravityReasoningReplayItemsForRequestWithSchemas(test.payload, items, nil)
				if len(got) != len(want) {
					t.Fatalf("filtered items = %d, want %d", len(got), len(want))
				}
				for itemIndex := range want {
					if !bytes.Equal(got[itemIndex], want[itemIndex]) {
						t.Fatalf("item %d differs\n got: %s\nwant: %s", itemIndex, got[itemIndex], want[itemIndex])
					}
				}
			},
		)
	}
}

func TestAntigravityReasoningReplayAccumulatorUsesIndexedContextHash(t *testing.T) {
	payload := []byte(`{
		"request": {
			"systemInstruction":{"parts":[{"text":"system"}]},
			"contents":[{"role":"user","parts":[{"text":"hello"}]}]
		}
	}`)
	scope := antigravityReasoningReplayScope{modelName: "gemini-test", sessionKey: "session:test"}
	accumulator := newAntigravityReasoningReplayAccumulator(scope, payload)
	if accumulator == nil {
		t.Fatal("accumulator is nil")
	}
	wantContextHash := legacyAntigravityReplayContextFingerprint(payload, 1)
	if accumulator.responseContextHash != wantContextHash {
		t.Fatalf("response context hash = %q, want %q", accumulator.responseContextHash, wantContextHash)
	}
	accumulator.observeResponsePayload(
		[]byte(`{
		"response":{"candidates":[{
			"content":{"parts":[{"functionCall":{"id":"ca` +
			`ll-1","name":"lookup","args":{"value":1}},` +
			`"thoughtSignature":"sig-call"}]},` +
			`
			"finishReason":"STOP"
		}]}
	}`),
	)
	if len(accumulator.items) != 1 {
		t.Fatalf("items = %d, want 1", len(accumulator.items))
	}
	if got := gjson.GetBytes(accumulator.items[0], "contextHash").String(); got != wantContextHash {
		t.Fatalf("item context hash = %q, want %q", got, wantContextHash)
	}
}

func TestApplyAntigravityReasoningReplayItemsRebuildsIndexAfterMutation(t *testing.T) {
	items := [][]byte{
		[]byte(`{"type":"function_call_part","contentIndex":1,"partIndex":0,` +
			`"name":"Read",` +
			`"call_id":"id1","args":{"file_path":"/a"},` +
			`"thoughtSignature":"sig-first"}`),
		[]byte(`{"type":"function_call_part","contentIndex":3,"partIndex":0,` +
			`"name":"Write",` +
			`"call_id":"id2","args":{"file_path":"/b"},` +
			`"thoughtSignature":"sig-second"}`),
	}
	payload := []byte(`{
		"request":{"contents":[
			{"role":"user","parts":[{"text":"hi"}]},` +
		`
			{"role":"model","parts":[{"functionResponse":{"id":"id1"` +
		`,"name":"Read",` +
		`"response":{"result":"ok"}}}]},
			{"role":"user","parts":[{"text":"next"}]},` +
		`
			{"role":"model","parts":[{"functionResponse":{"id":"id2"` +
		`,"name":"Write",` +
		`"response":{"result":"ok"}}}]}
		]}
	}`)

	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	got, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged {
		t.Fatalf("changed = %t, want %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload differs\n got: %s\nwant: %s", got, want)
	}
}

func TestAntigravityReplayMergeRandomizedDifferential(t *testing.T) {
	const randomSeed = 20260811
	const turns = 6
	randomSource := rand.New(rand.NewSource(randomSeed))
	basePayload := syntheticAntigravityReplayMixedPayload(turns)

	items := legacyAntigravityReasoningReplayItemsFromRequestForTest(basePayload)
	thoughtItems, callItems := 0, 0
	for _, item := range items {
		switch gjson.GetBytes(item, "type").String() {
		case "thought_signature":
			thoughtItems++
		case "function_call_part":
			callItems++
		}
	}
	if thoughtItems == 0 || callItems == 0 {
		t.Fatalf("ledger must mix item kinds: thought=%d call=%d", thoughtItems, callItems)
	}

	applied := 0
	for caseIndex := range 300 {
		payload := bytes.Clone(basePayload)
		for range 1 + randomSource.Intn(5) {
			turn := randomSource.Intn(turns)
			modelIndex := 1 + turn*2
			responseIndex := modelIndex + 1
			part := randomSource.Intn(3)
			var errSet error
			switch randomSource.Intn(9) {
			case 0:
				payload, errSet = sjson.DeleteBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", modelIndex, part),
				)
			case 1:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.2.functionCall.args.turn", modelIndex),
					turn+50,
				)
			case 2:
				payload, errSet = sjson.DeleteBytes(
					payload, fmt.Sprintf("request.contents.%d.parts.2.functionCall.id", modelIndex),
				)
			case 3:
				payload, errSet = sjson.SetBytes(
					payload, fmt.Sprintf("request.contents.%d.parts.1.text", modelIndex), "drifted",
				)
			case 4:
				payload, errSet = sjson.SetBytes(
					payload, fmt.Sprintf("request.contents.%d.role", responseIndex), "model",
				)
			case 5:
				payload, errSet = sjson.DeleteBytes(
					payload, fmt.Sprintf("request.contents.%d.parts.%d", modelIndex, part),
				)
			case 6:
				payload, errSet = sjson.SetBytes(
					payload, fmt.Sprintf("request.contents.%d.parts.0.thought", modelIndex), false,
				)
			case 7:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.2.functionCall.id", modelIndex),
					"call-0",
				)
			case 8:
				payload, errSet = sjson.SetBytes(payload, "request.toolConfig.functionCallingConfig.mode", "ANY")
			}
			if errSet != nil {
				t.Fatalf("case %d mutation failed: %v", caseIndex, errSet)
			}
		}

		wantPayload, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
		gotPayload, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
		if gotChanged != wantChanged {
			t.Fatalf("seed=%d case=%d changed=%t want=%t", randomSeed, caseIndex, gotChanged, wantChanged)
		}
		if !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf(
				"seed=%d case=%d payload differs\n got: %s\nwant: %s", randomSeed, caseIndex, gotPayload, wantPayload,
			)
		}
		if wantChanged {
			applied++
		}
	}
	if applied == 0 {
		t.Fatal("no case applied a replay item; the differential proved nothing")
	}
	t.Logf(
		"cases=300 casesThatApplied=%d ledgerItems=%d (thought=%d call=%d)",
		applied,
		len(items),
		thoughtItems,
		callItems,
	)
}

func TestAntigravityReplayNonArrayPartsFailsClosed(t *testing.T) {
	payload := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"hi"}]},` +
		`{"role":"model","parts":{"functionCall":{"name":"lookup",` +
		`"args":{"value":1}}}}` +
		`]}}`)
	items := [][]byte{
		[]byte(`{"type":"function_call_part","contentIndex":1,"partIndex":0,` +
			`"name":"lookup",` +
			`"call_id":"call-1","args":{"value":1},"thoughtSignature":"sig-x"}`),
	}

	if kept := filterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil); len(kept) != 0 {
		t.Fatalf("malformed parts must not yield an eligible item, kept=%d", len(kept))
	}
	got, changed := applyAntigravityReasoningReplayItems(payload, items, nil)
	if changed || !bytes.Equal(got, payload) {
		t.Fatalf("malformed parts must not be mutated: changed=%t body=%s", changed, got)
	}
	// The legacy oracle accepted the item at the filter layer but could not write
	// it either, so the observable end state was already identical.
	if _, legacyChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil); legacyChanged {
		t.Fatal("legacy oracle unexpectedly mutated malformed parts")
	}
}

func TestAntigravityReplayLegacyItemWithoutTargetHash(t *testing.T) {
	payload := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"ask"}]},` +
		`{"role":"model","parts":[{"text":"chunk-a"},` +
		`{"text":"chunk-b"},{"text":"chunk-c"}]}` +
		`]}}`)
	const signature = "legacy-positional-signature-12345"
	// partIndex 7 is out of range on purpose: legacy entries pointed at a
	// streamed signature-only part that no longer exists.
	item := buildAntigravityThoughtSignatureItem(1, 7, signature, "", "")
	item = antigravityReplayItemContextHashForTest(item, payload, 1)
	if gjson.GetBytes(item, "targetHash").Exists() {
		t.Fatal("this test must exercise the no-targetHash path")
	}

	got, changed := applyAntigravityReasoningReplayItems(payload, [][]byte{item}, nil)
	if !changed {
		t.Fatalf("legacy positional item was not applied: %s", got)
	}
	if sig := gjson.GetBytes(got, "request.contents.1.parts.2.thoughtSignature").String(); sig != signature {
		t.Fatalf("signature must attach to the LAST semantic part, got parts.2=%q body=%s", sig, got)
	}
	for _, path := range []string{
		"request.contents.1.parts.0.thoughtSignature",
		"request.contents.1.parts.1.thoughtSignature",
	} {
		if gjson.GetBytes(got, path).Exists() {
			t.Fatalf("signature leaked to %s: %s", path, got)
		}
	}

	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, [][]byte{item}, nil)
	if wantChanged != changed || !bytes.Equal(want, got) {
		t.Fatalf("legacy oracle disagrees\n got: %s\nwant: %s", got, want)
	}

	// Context drift must reject the positional guess entirely.
	drifted, errSet := sjson.SetBytes(payload, "request.contents.0.parts.0.text", "different question")
	if errSet != nil {
		t.Fatal(errSet)
	}
	driftedOut, driftedChanged := applyAntigravityReasoningReplayItems(drifted, [][]byte{item}, nil)
	if driftedChanged || !bytes.Equal(driftedOut, drifted) {
		t.Fatalf("context drift must reject a positional legacy item: changed=%t body=%s", driftedChanged, driftedOut)
	}
}
