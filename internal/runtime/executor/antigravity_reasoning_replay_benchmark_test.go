package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/sjson"
)

func syntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns int) []byte {
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
				`"args":{"turn":%d}},"thoughtSignature":"sig-%d"}]}`,
			turn,
			turn,
			turn,
		)
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

func syntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(inlineBytes, turns int) []byte {
	return syntheticAntigravityReplayIndexPayload(inlineBytes, turns, false)
}

func syntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(inlineBytes, turns int) []byte {
	var payload strings.Builder
	payload.Grow(inlineBytes + turns*320)
	payload.WriteString(
		`{"request":{"contents":[{"role":"user",` +
			`"parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"`,
	)
	payload.WriteString(strings.Repeat("a", inlineBytes))
	payload.WriteString(`"}}]}`)
	for turn := range turns {
		nativeID := fmt.Sprintf("call-%d", turn)
		argsRaw := fmt.Sprintf(`{"turn":%d}`, turn)
		stableID := util.GeminiClaudeToolUseID(nativeID, "lookup", argsRaw)
		// strings.Builder.Write always returns the full length and a nil error.
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionCall":{"id":%q,"name":"lookup","args":%s}}]}`,
			stableID,
			argsRaw,
		)
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionResponse":{"id":%q,"name":"lookup",`+
				`"response":{"result":"ok"}}}]}`,
			stableID,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func syntheticAntigravityReplayMixedPayload(turns int) []byte {
	var payload strings.Builder
	payload.WriteString(`{"request":{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[`)
	payload.WriteString(`{"role":"user","parts":[{"text":"start"}]}`)
	for turn := range turns {
		// strings.Builder.Write always returns the full length and a nil error.
		//goland:noinspection SpellCheckingInspection
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"thought":true,"text":"reason-%d","thoughtSignature":"tsig-%d"},`+
				`{"text":"say-%d","thoughtSignature":"xsig-%d"},`+
				`{"functionCall":{"id":"call-%d","name":"lookup","args":{"turn":%d}},`+
				`"thoughtSignature":"csig-%d"}`+
				`]}`,
			turn, turn, turn, turn, turn, turn, turn,
		)
		_, _ = fmt.Fprintf(
			&payload,
			`,{"role":"user","parts":[{"functionResponse":{"id":"call-%d","name":"lookup",`+
				`"response":{"result":"ok-%d"}}}]}`,
			turn, turn,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func BenchmarkApplyAntigravityReasoningReplayItemsLargeBatch(b *testing.B) {
	const (
		inlineBytes = 10 << 20
		turns       = 161
	)
	base := syntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns)
	items := antigravityReasoningReplayItemsFromRequest(base)
	if len(items) != turns {
		b.Fatalf("items = %d, want %d", len(items), turns)
	}
	payload := syntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(inlineBytes, turns)
	if _, changed := applyAntigravityReasoningReplayItems(payload, items, nil); !changed {
		b.Fatal("benchmark payload applies nothing")
	}

	for _, algorithm := range []struct {
		name  string
		apply func([]byte, [][]byte, map[string]any) ([]byte, bool)
	}{
		{"sequential", legacyApplyAntigravityReasoningReplayItemsForTest},
		{"batch", applyAntigravityReasoningReplayItems},
	} {
		b.Run(
			algorithm.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				for b.Loop() {
					antigravityReplayIndexBenchmarkOutput, _ = algorithm.apply(payload, items, nil)
				}
			},
		)
	}
}

func BenchmarkApplyAntigravityReasoningReplayItemsLargeSegmentedFallback(b *testing.B) {
	const (
		inlineBytes = 10 << 20
		turns       = 161
	)
	base := syntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns)
	items := antigravityReasoningReplayItemsFromRequest(base)
	payload := syntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(inlineBytes, turns)
	lastCallPath := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", 1+(turns-1)*2)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, lastCallPath, "lookup-legacy-client-id")
	if errSet != nil {
		b.Fatalf("set legacy call ID: %v", errSet)
	}
	toolSchemas := map[string]any{"lookup": map[string]any{"type": "object"}}
	if _, changed := applyAntigravityReasoningReplayItems(payload, items, toolSchemas); !changed {
		b.Fatal("benchmark payload applies nothing")
	}

	for _, algorithm := range []struct {
		name  string
		apply func([]byte, [][]byte, map[string]any) ([]byte, bool)
	}{
		{"sequential", legacyApplyAntigravityReasoningReplayItemsForTest},
		{"batch", applyAntigravityReasoningReplayItems},
	} {
		b.Run(
			algorithm.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				for b.Loop() {
					antigravityReplayIndexBenchmarkOutput, _ = algorithm.apply(payload, items, toolSchemas)
				}
			},
		)
	}
}
