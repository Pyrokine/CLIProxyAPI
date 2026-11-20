package executor

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func antigravityReplayItemContextHashForTest(item, payload []byte, contentIndex int) []byte {
	return antigravitySetReplayItemContextHashValue(
		item,
		newAntigravityReplayRequestIndex(payload).contextFingerprint(contentIndex),
	)
}

func filterAntigravityReasoningReplayItemsForRequestWithSchemas(
	payload []byte,
	items [][]byte,
	toolSchemas map[string]any,
) [][]byte {
	return filterAntigravityReasoningReplayItemsForRequestWithIndex(
		newAntigravityReplayRequestIndex(payload), items, toolSchemas,
	)
}

func applyAntigravityReplayItemsBatch(
	index *antigravityReplayRequestIndex,
	payload []byte,
	items [][]byte,
	toolSchemas map[string]any,
) ([]byte, bool, bool) {
	batch := newAntigravityReplayBatch(index)
	for _, item := range items {
		if !batch.apply(item, toolSchemas) {
			return nil, false, false
		}
	}
	updated, ok := batch.build(payload)
	return updated, batch.applied && ok, ok
}

func TestApplyAntigravityReasoningReplayItemsUsesUpdatedContextAfterIdentityRestore(t *testing.T) {
	const nativeID, name, args = "call-0", "lookup", `{"turn":0}`
	stableID := util.GeminiClaudeToolUseID(nativeID, name, args)
	base := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"text":"start"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"` + nativeID +
		`","name":"lookup","args":` + args + `},"thoughtSignature":"native-call-signature"}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"` + nativeID +
		`","name":"lookup","response":{"result":"ok"}}}]},` +
		`{"role":"model","parts":[{"text":"later"}]}]}}`)
	payload := bytes.ReplaceAll(base, []byte(nativeID), []byte(stableID))
	payload, err := sjson.DeleteBytes(payload, "request.contents.1.parts.0.thoughtSignature")
	if err != nil {
		t.Fatal(err)
	}
	kind, fingerprint := antigravityReplayPartFingerprint(gjson.Parse(`{"text":"later"}`))
	for _, test := range []struct {
		name    string
		context []byte
		want    string
	}{
		{"before identity restore", payload, ""},
		{"after identity restore", base, "later-signature"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				items := legacyAntigravityReasoningReplayItemsFromRequestForTest(base)
				item := buildAntigravityThoughtSignatureItem(3, 0, "later-signature", kind, fingerprint)
				item = legacyAntigravitySetReplayItemContextHash(item, test.context, 3)
				items = append(items, item)
				out, changed := applyAntigravityReasoningReplayItems(payload, items, nil)
				if !changed || gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.id").String() != nativeID {
					t.Fatalf("native identity was not restored: changed=%t body=%s", changed, out)
				}
				if got := gjson.GetBytes(
					out, "request.contents.3.parts.0.thoughtSignature",
				).String(); got != test.want {
					t.Fatalf("signature after identity restore = %q, want %q; body=%s", got, test.want, out)
				}
			},
		)
	}
}

func TestApplyAntigravityReasoningReplayItemsBatchesSignatureOnlyMutations(t *testing.T) {
	const turns = 4
	base := syntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, turns)

	got, gotChanged, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature-only replay did not use the batch path")
	}
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if gotChanged != wantChanged {
		t.Fatalf("changed = %t, want %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload differs\n got: %s\nwant: %s", got, want)
	}
}

func TestApplyAntigravityReasoningReplayItemsBatchPreservesSignatureMoveOrder(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"ask"}]},` +
		`{"role":"model","parts":[{"text":"first"},{"text":"second"}]}]}}`)
	const signature = "shared-native-signature-123456789"
	items := make([][]byte, 0, 2)
	for partIndex, text := range []string{"first", "second"} {
		kind, fingerprint := antigravityReplayPartFingerprint(gjson.Parse(`{"text":"` + text + `"}`))
		item := buildAntigravityThoughtSignatureItem(1, partIndex, signature, kind, fingerprint)
		items = append(items, antigravityReplayItemContextHashForTest(item, payload, 1))
	}

	got, gotChanged, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature move did not use the batch path")
	}
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("batch differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	if first := gjson.GetBytes(got, "request.contents.1.parts.0.thoughtSignature"); first.Exists() {
		t.Fatalf("signature remained on the first part: %s", got)
	}
	if second := gjson.GetBytes(got, "request.contents.1.parts.1.thoughtSignature").String(); second != signature {
		t.Fatalf("second signature = %q, want %q", second, signature)
	}
}

func TestApplyAntigravityReasoningReplayItemsBatchPreservesStickyChangedResult(t *testing.T) {
	const signature = "shared-native-signature-123456789"
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"ask"}]},` +
		`{"role":"model","parts":[{"text":"first","thoughtSignature":"` + signature + `"},{"text":"second"}]}]}}`)
	items := make([][]byte, 0, 2)
	for _, target := range []struct {
		partIndex int
		text      string
	}{{partIndex: 1, text: "second"}, {partIndex: 0, text: "first"}} {
		kind, fingerprint := antigravityReplayPartFingerprint(gjson.Parse(`{"text":"` + target.text + `"}`))
		item := buildAntigravityThoughtSignatureItem(1, target.partIndex, signature, kind, fingerprint)
		items = append(items, antigravityReplayItemContextHashForTest(item, payload, 1))
	}

	got, gotChanged, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature round trip did not use the batch path")
	}
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if !gotChanged || gotChanged != wantChanged {
		t.Fatalf("changed = %t, want sticky %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) || !bytes.Equal(got, payload) {
		t.Fatalf("signature round trip did not restore the original payload\n got: %s\nwant: %s", got, want)
	}
}

func TestApplyAntigravityReasoningReplayItemsBatchesStableProvenanceIDs(t *testing.T) {
	const turns = 4
	base := syntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(1<<10, turns)

	got, gotChanged, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("stable provenance replay did not use the batch path")
	}
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("batch differs from sequential stable-ID replay\n got: %s\nwant: %s", got, want)
	}
	for turn := range turns {
		callContentIndex := 1 + turn*2
		responseContentIndex := callContentIndex + 1
		wantID := fmt.Sprintf("call-%d", turn)
		if gotID := gjson.GetBytes(
			got, fmt.Sprintf(
				"request.contents.%d.parts.0.functionCall.id", callContentIndex,
			),
		).String(); gotID != wantID {
			t.Fatalf("call %d ID = %q, want %q", turn, gotID, wantID)
		}
		if gotID := gjson.GetBytes(
			got, fmt.Sprintf(
				"request.contents.%d.parts.0.functionResponse.id", responseContentIndex,
			),
		).String(); gotID != wantID {
			t.Fatalf("response %d ID = %q, want %q", turn, gotID, wantID)
		}
	}
}

func TestApplyAntigravityReasoningReplayItemsSegmentsDuplicateStableProvenanceIDs(t *testing.T) {
	const (
		nativeID = "reused-native-call"
		name     = "lookup"
		argsRaw  = `{"turn":0}`
	)
	stableID := util.GeminiClaudeToolUseID(nativeID, name, argsRaw)
	base := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"text":"start"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"` +
		nativeID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`},"thoughtSignature":"sig-a"}]},{"role":"user",` +
		`"parts":[{"functionResponse":{"id":"` +
		nativeID +
		`","name":"` +
		name +
		`","response":{"result":"a"}}}]},{"role":"model",` +
		`"parts":[{"functionCall":{"id":"` +
		nativeID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`},"thoughtSignature":"sig-b"}]},{"role":"user",` +
		`"parts":[{"functionResponse":{"id":"` + nativeID + `","name":"` + name + `","response":{"result":"b"}}}]}]}}`)
	payload := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"text":"start"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`}}]},{"role":"user","parts":[{"functionResponse":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","response":{"result":"a"}}}]},{"role":"model",` +
		`"parts":[{"functionCall":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`}}]},{"role":"user","parts":[{"functionResponse":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","response":{"result":"b"}}}]}]}}`)
	items := antigravityReasoningReplayItemsFromRequest(base)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if _, _, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload), payload, items, nil,
	); handled {
		t.Fatal("single batch accepted a non-unique stable provenance ID")
	}

	got, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("duplicate-ID segmentation differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	for _, contentIndex := range []int{1, 3} {
		path := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", contentIndex)
		if gotID := gjson.GetBytes(got, path).String(); gotID != nativeID {
			t.Fatalf("call at contents[%d] ID = %q, want %q", contentIndex, gotID, nativeID)
		}
		if gotID := gjson.GetBytes(
			got, fmt.Sprintf(
				"request.contents.%d.parts.0.functionResponse.id", contentIndex+1,
			),
		).String(); gotID != nativeID {
			t.Fatalf("response after contents[%d] ID = %q, want %q", contentIndex, gotID, nativeID)
		}
	}
	firstOnly, changed := applyAntigravityReasoningReplayItems(payload, items[:1], nil)
	if !changed {
		t.Fatal("first scoped occurrence was not restored")
	}
	for _, target := range []struct {
		path, want string
	}{
		{"request.contents.1.parts.0.functionCall.id", nativeID},
		{"request.contents.2.parts.0.functionResponse.id", nativeID},
		{"request.contents.3.parts.0.functionCall.id", stableID},
		{"request.contents.4.parts.0.functionResponse.id", stableID},
	} {
		if got := gjson.GetBytes(firstOnly, target.path).String(); got != target.want {
			t.Fatalf("scoped occurrence changed %s = %q, want %q", target.path, got, target.want)
		}
	}
	nativePayload, err := sjson.DeleteBytes(base, "request.contents.3.parts.0.thoughtSignature")
	if err != nil {
		t.Fatal(err)
	}
	nativeOut, changed := applyAntigravityReasoningReplayItems(nativePayload, items[1:], nil)
	if !changed || gjson.GetBytes(nativeOut, "request.contents.3.parts.0.thoughtSignature").String() != "sig-b" {
		t.Fatalf("second native occurrence did not recover its own signature: %s", nativeOut)
	}
	if first := gjson.GetBytes(nativeOut, "request.contents.1.parts.0.thoughtSignature").String(); first != "sig-a" {
		t.Fatalf("second occurrence changed the first signature: %q", first)
	}
}

func TestApplyAntigravityReasoningReplayItemsFallsBackAfterIdentityChangesContext(t *testing.T) {
	const (
		nativeID  = "call-0"
		name      = "lookup"
		argsRaw   = `{"turn":0}`
		signature = "native-call-signature"
	)
	base := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"text":"start"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"` +
		nativeID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`},"thoughtSignature":"` +
		signature +
		`"}]},{"role":"user","parts":[{"functionResponse":{"id":"` +
		nativeID +
		`","name":"` +
		name +
		`","response":{"result":"ok"}}}]},{"role":"model",` +
		`"parts":[{"text":"later"}]}]}}`)
	callItems := antigravityReasoningReplayItemsFromRequest(base)
	if len(callItems) != 1 {
		t.Fatalf("call items = %d, want 1", len(callItems))
	}
	stableID := util.GeminiClaudeToolUseID(nativeID, name, argsRaw)
	payload := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"text":"start"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","args":` +
		argsRaw +
		`}}]},{"role":"user","parts":[{"functionResponse":{"id":"` +
		stableID +
		`","name":"` +
		name +
		`","response":{"result":"ok"}}}]},{"role":"model",` +
		`"parts":[{"text":"later"}]}]}}`)
	legacyThoughtItem := buildAntigravityThoughtSignatureItem(3, 0, "later-signature", "", "")
	legacyThoughtItem = antigravityReplayItemContextHashForTest(legacyThoughtItem, payload, 3)
	items := append(callItems, legacyThoughtItem)

	if _, _, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload), payload, items, nil,
	); handled {
		t.Fatal("batch path reused a context fingerprint after restoring a stable ID")
	}
	got, gotChanged := applyAntigravityReasoningReplayItems(payload, items, nil)
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("fallback differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	if signature := gjson.GetBytes(got, "request.contents.3.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("stale-context signature was applied: %s", got)
	}
}

func TestApplyAntigravityReasoningReplayItemsSegmentsLateStructuralFallback(t *testing.T) {
	const turns = 16
	base := syntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, turns)
	lastCallPath := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", 1+(turns-1)*2)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, lastCallPath, "lookup-legacy-client-id")
	if errSet != nil {
		t.Fatalf("set legacy call ID: %v", errSet)
	}
	toolSchemas := map[string]any{"lookup": map[string]any{"type": "object"}}

	if _, _, handled := applyAntigravityReplayItemsBatch(
		newAntigravityReplayRequestIndex(payload), payload, items, toolSchemas,
	); handled {
		t.Fatal("all-or-nothing batch unexpectedly handled a legacy identity restore")
	}
	got, gotChanged := applyAntigravityReasoningReplayItems(payload, items, toolSchemas)
	want, wantChanged := legacyApplyAntigravityReasoningReplayItemsForTest(payload, items, toolSchemas)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("segmented fallback differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	wantLastID := fmt.Sprintf("call-%d", turns-1)
	if gotLastID := gjson.GetBytes(got, lastCallPath).String(); gotLastID != wantLastID {
		t.Fatalf("last call ID = %q, want %q", gotLastID, wantLastID)
	}
}

func TestApplyAntigravityReasoningReplayItemsBatchRejectsUnknownPartOffset(t *testing.T) {
	base := syntheticAntigravityReplayBenchmarkPayload(1<<10, 1)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, 1)
	index := newAntigravityReplayRequestIndex(payload)
	index.contents[1].parts[0].Index = 0

	if _, _, handled := applyAntigravityReplayItemsBatch(index, payload, items, nil); handled {
		t.Fatal("batch path accepted an unknown zero part offset")
	}
}
