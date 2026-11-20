package executor

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyAntigravityReasoningReplayItemsFromRequestForTest(payload []byte) [][]byte {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return nil
	}
	items := make([][]byte, 0)
	contents.ForEach(
		func(contentKey, content gjson.Result) bool {
			if !strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "model") {
				return true
			}
			contentIndex := int(contentKey.Int())
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			partArray := parts.Array()
			functionCallOccurrences := make(map[string]int)
			for partIndex, part := range partArray {
				signature := antigravityNativePartThoughtSignature(part)
				if !antigravityHasNativeThoughtSignature(signature) {
					signature = ""
				}
				if functionCall := part.Get("functionCall"); functionCall.Exists() {
					key := antigravityFunctionCallKey(
						functionCall.Get("name").String(), functionCall.Get("args").Raw, "",
					)
					occurrence := functionCallOccurrences[key]
					if key != "" {
						functionCallOccurrences[key] = occurrence + 1
					}
					if item := buildAntigravityFunctionCallPartItem(
						contentIndex, partIndex, occurrence, functionCall, signature,
					); len(
						item,
					) > 0 {
						items = append(
							items,
							legacyAntigravitySetReplayItemContextHash(item, payload, contentIndex),
						)
					}
					continue
				}
				if signature == "" {
					continue
				}
				targetPart := part
				targetPartIndex := partIndex
				kind, fingerprint := antigravityReplayPartFingerprint(targetPart)
				if fingerprint == "" && partIndex > 0 {
					targetPartIndex = partIndex - 1
					targetPart = partArray[targetPartIndex]
					kind, fingerprint = antigravityReplayPartFingerprint(targetPart)
				}
				if fingerprint == "" {
					continue
				}
				item := buildAntigravityThoughtSignatureItem(
					contentIndex, targetPartIndex, signature, kind, fingerprint,
				)
				item, _ = sjson.SetBytes(
					item,
					"targetOccurrence",
					antigravityReplayPartOccurrence(partArray, targetPartIndex, kind, fingerprint),
				)
				items = append(items, legacyAntigravitySetReplayItemContextHash(item, payload, contentIndex))
			}
			return true
		},
	)
	return items
}

func legacyApplyAntigravityReasoningReplayItemsForTest(
	payload []byte,
	items [][]byte,
	toolSchemas map[string]any,
) ([]byte, bool) {
	updated := payload
	changed := false
	for _, item := range items {
		eligible := legacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(
			updated, [][]byte{item}, toolSchemas,
		)
		if len(eligible) != 1 {
			continue
		}
		next, applied := legacyInsertAntigravityReasoningReplayItemsWithSchemas(updated, eligible, toolSchemas)
		if !applied {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

func syntheticAntigravityReplayIndexPayload(inlineBytes, turns int, signatures bool) []byte {
	var payload strings.Builder
	payload.Grow(inlineBytes + turns*256)
	payload.WriteString(
		`{"request":{"contents":[{"role":"user",` +
			`"parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"`,
	)
	payload.WriteString(strings.Repeat("a", inlineBytes))
	payload.WriteString(`"}}]}`)
	for turn := range turns {
		// strings.Builder.Write always returns the full length and a nil error.
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionCall":{"id":"call-%d","name":"lookup",`+
				`"args":{"turn":%d}}`,
			turn,
			turn,
		)
		if signatures {
			_, _ = fmt.Fprintf(&payload, `,"thoughtSignature":"signature-%d"`, turn)
		}
		payload.WriteString(`}]}`)
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionResponse":{"id":"call-%d","name":"lookup",`+
				`"response":{"result":"ok"}}}]}`,
			turn,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func TestAntigravityReplayContextFingerprintsMatchLegacy(t *testing.T) {
	t.Run(
		"original inputs", func(t *testing.T) {
			tests := []struct {
				name    string
				payload []byte
			}{
				{name: "empty", payload: []byte(`{}`)},
				{
					name:    "system without contents",
					payload: []byte(`{"request":{"systemInstruction":{"parts":[{"text":"system"}]}}}`),
				},
				{name: "malformed contents", payload: []byte(`{"request":{"systemInstruction":{},"contents":`)},
				{name: "empty contents", payload: []byte(`{"request":{"contents":[]}}`)},
				{
					name: "system tools and signatures",
					payload: []byte(`{
				"request": {
					"systemInstruction": {"parts":[{"text":"system"}]},` +
						`
					"tools": [{"functionDeclarations":[{"name":"lookup",` +
						`"parameters":{"type":"object"}}]}],` +
						`
					"toolConfig": {"functionCallingConfig":{"mode":"AUTO"}},` +
						`
					"contents": [
						{"role":"user","parts":[{"text":"hello"}]},` +
						`
						{"role":"model","parts":[{"thought":true,"text":"think",` +
						`"thoughtSignature":"sig-a"},{"functionCall":{"id":"call-1","name":"lookup",` +
						`"args":{"z":1,"a":2}},` +
						`"extra_content":{"google":{"thought_signature":"sig-b"}}}]},` +
						`
						{"role":"model"},
						{"role":"user","parts":null}
					]
				}
			}`),
				},
			}

			for _, test := range tests {
				t.Run(
					test.name, func(t *testing.T) {
						index := newAntigravityReplayRequestIndex(test.payload)
						for beforeContentIndex := -1; beforeContentIndex <=
							len(index.contents)+
								1; beforeContentIndex++ {
							want := legacyAntigravityReplayContextFingerprint(test.payload, beforeContentIndex)
							if got := index.contextFingerprint(beforeContentIndex); got != want {
								t.Fatalf("contextFingerprint(%d) = %q, want %q", beforeContentIndex, got, want)
							}
						}
					},
				)
			}

		},
	)

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: []byte(`{}`)},
		{name: "malformed contents", payload: []byte(`{"request":{"contents":`)},
		{name: "empty contents", payload: []byte(`{"request":{"contents":[]}}`)},
		{
			name: "system tools and signatures",
			payload: []byte(`{
				"request":{
					"systemInstruction":{"parts":[{"text":"system"}]},` +
				`
					"tools":[{"functionDeclarations":[{"name":"lookup",` +
				`"parameters":{"type":"object"}}]}],` +
				`
					"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},` +
				`
					"contents":[
						{"role":"user","parts":[{"text":"hello"}]},` +
				`
						{"role":"model","parts":[{"text":"answer",` +
				`"thoughtSignature":"signature"}]},
						{"role":"user",` +
				`"parts":null}
					]
				}
			}`),
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				index := newAntigravityReplayRequestIndex(test.payload)
				for beforeContentIndex := -1; beforeContentIndex <= len(index.contents)+1; beforeContentIndex++ {
					want := legacyAntigravityReplayContextFingerprint(test.payload, beforeContentIndex)
					if got := index.contextFingerprint(beforeContentIndex); got != want {
						t.Fatalf("contextFingerprint(%d) = %q, want %q", beforeContentIndex, got, want)
					}
				}
			},
		)
	}
}

func TestAntigravityReasoningReplayItemsFromIndexMatchLegacy(t *testing.T) {
	t.Run(
		"original inputs", func(t *testing.T) {
			payload := []byte(`{
		"request": {
			"systemInstruction":{"parts":[{"text":"system"}]},` +
				`
			"contents":[
				{"role":"user","parts":[{"text":"hello"}]},` +
				`
				{"role":"model","parts":[
					{"thought":true,"text":"same",` +
				`"thoughtSignature":"sig-thought"},
					{"text":"same",` +
				`"thoughtSignature":"sig-text"},
					{"functionCall":{"id":"call-1",` +
				`"name":"lookup","args":{"value":1}},"thoughtSignature":"sig-call"},` +
				`
					{"functionCall":{"id":"call-2","name":"lookup",` +
				`"args":{"value":1}}}
				]},
				{"role":"model","parts":[{"text":"same",` +
				`"thoughtSignature":"sig-text-2"}]}
			]
		}
	}`)

			want := legacyAntigravityReasoningReplayItemsFromRequestForTest(payload)
			got := antigravityReasoningReplayItemsFromRequest(payload)
			if len(got) != len(want) {
				t.Fatalf("items = %d, want %d", len(got), len(want))
			}
			for itemIndex := range want {
				if !bytes.Equal(got[itemIndex], want[itemIndex]) {
					t.Fatalf("item %d differs\n got: %s\nwant: %s", itemIndex, got[itemIndex], want[itemIndex])
				}
			}

		},
	)

	payload := []byte(`{
		"request":{"contents":[
			{"role":"user","parts":[{"text":"hello"}]},` +
		`
			{"role":"model","parts":[
				{"thought":true,"text":"same",` +
		`"thoughtSignature":"signature-thought"},
				{"text":"same",` +
		`"thoughtSignature":"signature-text"},
				{"functionCall":{"id":"call-1",` +
		`"name":"lookup","args":{"value":1}},"thoughtSignature":"signature-call"},` +
		`
				{"functionCall":{"id":"call-2","name":"lookup","args":{"value":1}}}
			]},` +
		`
			{"role":"model","parts":[{"text":"same",` +
		`"thoughtSignature":"signature-text-2"}]}
		]}
	}`)
	want := legacyAntigravityReasoningReplayItemsFromRequestForTest(payload)
	got := antigravityReasoningReplayItemsFromRequest(payload)
	if len(got) != len(want) {
		t.Fatalf("items = %d, want %d", len(got), len(want))
	}
	for itemIndex := range want {
		if !bytes.Equal(got[itemIndex], want[itemIndex]) {
			t.Fatalf("item %d differs\n got: %s\nwant: %s", itemIndex, got[itemIndex], want[itemIndex])
		}
	}
}

func TestAntigravityReplayRequestIndexRandomizedDifferential(t *testing.T) {
	t.Run(
		"original inputs", func(t *testing.T) {
			const randomSeed = 20260810
			randomSource := rand.New(rand.NewSource(randomSeed))
			basePayload := syntheticAntigravityReplayBenchmarkPayload(256, 8)
			items := legacyAntigravityReasoningReplayItemsFromRequestForTest(basePayload)
			if len(items) != 8 {
				t.Fatalf("items = %d, want 8", len(items))
			}

			for caseIndex := range 100 {
				payload := bytes.Clone(basePayload)
				mutationCount := 1 + randomSource.Intn(4)
				for range mutationCount {
					turn := randomSource.Intn(8)
					callContentIndex := 1 + turn*2
					responseContentIndex := callContentIndex + 1
					var errSet error
					switch randomSource.Intn(7) {
					case 0:
						payload, errSet = sjson.DeleteBytes(
							payload,
							fmt.Sprintf("request.contents.%d.parts.0.thoughtSignature", callContentIndex),
						)
					case 1:
						payload, errSet = sjson.SetBytes(
							payload,
							fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
							fmt.Sprintf("changed-%d", turn),
						)
					case 2:
						payload, errSet = sjson.SetBytes(
							payload,
							fmt.Sprintf("request.contents.%d.parts.0.functionCall.args.turn", callContentIndex),
							turn+100,
						)
					case 3:
						payload, errSet = sjson.DeleteBytes(
							payload,
							fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
						)
					case 4:
						payload, errSet = sjson.SetBytes(
							payload,
							fmt.Sprintf(
								"request.contents.%d.parts.0.functionResponse.id",
								responseContentIndex,
							),
							fmt.Sprintf("changed-%d", turn),
						)
					case 5:
						payload, errSet = sjson.SetBytes(
							payload, fmt.Sprintf("request.contents.%d.role", responseContentIndex), "user",
						)
					case 6:
						payload, errSet = sjson.SetBytes(
							payload,
							fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
							"call-0",
						)
					}
					if errSet != nil {
						t.Fatalf("case %d mutation failed: %v", caseIndex, errSet)
					}
				}

				wantFiltered := legacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(
					payload,
					items,
					nil,
				)
				gotFiltered := filterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
				if len(gotFiltered) != len(wantFiltered) {
					t.Fatalf(
						"seed=%d case=%d filtered=%d want=%d", randomSeed, caseIndex, len(gotFiltered),
						len(wantFiltered),
					)
				}
				for itemIndex := range wantFiltered {
					if !bytes.Equal(gotFiltered[itemIndex], wantFiltered[itemIndex]) {
						t.Fatalf("seed=%d case=%d filtered item %d differs", randomSeed, caseIndex, itemIndex)
					}
				}

				wantPayload, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(
					payload,
					items,
					nil,
				)
				gotPayload, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
				if gotChanged != wantChanged || !bytes.Equal(gotPayload, wantPayload) {
					t.Fatalf(
						"seed=%d case=%d apply differs: changed=%t want=%t", randomSeed, caseIndex, gotChanged,
						wantChanged,
					)
				}
			}

		},
	)

	const randomSeed = 20260923
	randomSource := rand.New(rand.NewSource(randomSeed))
	basePayload := syntheticAntigravityReplayIndexPayload(256, 8, true)
	items := legacyAntigravityReasoningReplayItemsFromRequestForTest(basePayload)
	if len(items) != 8 {
		t.Fatalf("items = %d, want 8", len(items))
	}

	for caseIndex := range 100 {
		payload := bytes.Clone(basePayload)
		mutationCount := 1 + randomSource.Intn(4)
		for range mutationCount {
			turn := randomSource.Intn(8)
			callContentIndex := 1 + turn*2
			responseContentIndex := callContentIndex + 1
			var errSet error
			switch randomSource.Intn(7) {
			case 0:
				payload, errSet = sjson.DeleteBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.thoughtSignature", callContentIndex),
				)
			case 1:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
					fmt.Sprintf("changed-%d", turn),
				)
			case 2:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.functionCall.args.turn", callContentIndex),
					turn+100,
				)
			case 3:
				payload, errSet = sjson.DeleteBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
				)
			case 4:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.functionResponse.id", responseContentIndex),
					fmt.Sprintf("changed-%d", turn),
				)
			case 5:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.role", responseContentIndex),
					"user",
				)
			case 6:
				payload, errSet = sjson.SetBytes(
					payload,
					fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex),
					"call-0",
				)
			}
			if errSet != nil {
				t.Fatalf("seed=%d case=%d mutation failed: %v", randomSeed, caseIndex, errSet)
			}
		}

		wantPayload, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
		gotPayload, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
		if gotChanged != wantChanged || !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf(
				"seed=%d case=%d apply differs: changed=%t want=%t\n got: %s\nwant: %s",
				randomSeed,
				caseIndex,
				gotChanged,
				wantChanged,
				gotPayload,
				wantPayload,
			)
		}
	}
}

func TestAntigravityReplayBatchRejectsUnknownPartOffset(t *testing.T) {
	base := syntheticAntigravityReplayIndexPayload(1<<10, 1, true)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayIndexPayload(1<<10, 1, false)
	index := newAntigravityReplayRequestIndex(payload)
	batch := newAntigravityReplayBatch(index)
	for _, item := range items {
		if !batch.apply(item, nil) {
			t.Fatal("batch did not accept replay item before offset validation")
		}
	}
	index.contents[1].parts[0].Index = 0
	if _, ok := batch.build(payload); ok {
		t.Fatal("batch accepted an unknown zero part offset")
	}
}

// noinspection GoUnusedGlobalVariable
var antigravityReplayIndexBenchmarkOutput []byte

func TestApplyAntigravityReasoningReplayItemsAllocationUpperBound(t *testing.T) {
	base := syntheticAntigravityReplayIndexPayload(64<<10, 32, true)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayIndexPayload(64<<10, 32, false)
	legacyAllocs := testing.AllocsPerRun(
		5, func() {
			antigravityReplayIndexBenchmarkOutput, _ = legacyApplyAntigravityReasoningReplayItemsForTest(
				payload,
				items,
				nil,
			)
		},
	)
	indexedAllocs := testing.AllocsPerRun(
		5, func() {
			antigravityReplayIndexBenchmarkOutput, _ = applyAntigravityReasoningReplayItems(payload, items, nil)
		},
	)
	if indexedAllocs > legacyAllocs {
		t.Fatalf(
			"indexed allocations = %.0f, legacy sequential allocations = %.0f",
			indexedAllocs,
			legacyAllocs,
		)
	}
}
