package helps

import (
	"math"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestParseDevinUsageFieldPreservesZeroPresenceAndHeaders(t *testing.T) {
	header := appendDevinStringField(nil, 1, "Request-Id")
	header = appendDevinStringField(header, 2, "request-123")

	data := appendDevinVarintField(nil, 2, 0)
	data = appendDevinVarintField(data, 4, 58)
	data = appendDevinVarintField(data, 3, 0)
	data = appendDevinVarintField(data, 5, 0)
	data = appendDevinVarintField(data, 6, 66)
	data = appendDevinBytesField(data, 8, header)
	data = appendDevinStringField(data, 9, "model-name")

	usage, err := parseDevinUsageField(data)
	if err != nil {
		t.Fatalf("parseDevinUsageField(): %v", err)
	}
	if usage.PromptTokens != 58 || usage.CompletionTokens != 0 || usage.CachedTokens != 0 {
		t.Fatalf("usage tokens = %d/%d/%d, want 58/0/0", usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens)
	}
	if !usage.PromptTokensPresent || !usage.CompletionTokensPresent || !usage.CachedTokensPresent {
		t.Fatalf("usage presence = %t/%t/%t, want all present", usage.PromptTokensPresent, usage.CompletionTokensPresent, usage.CachedTokensPresent)
	}
	if usage.RequestID != "request-123" || usage.Headers["Request-Id"] != "request-123" ||
		usage.ModelName != "model-name" || usage.StatusCode != 66 {
		t.Fatalf("usage metadata = %+v", usage)
	}
	if usage.InputTokens() != 58 || usage.TotalTokens() != 58 {
		t.Fatalf("usage totals = %d/%d, want 58/58", usage.InputTokens(), usage.TotalTokens())
	}
}

func TestDevinUsageFillsOnlyMissingMetrics(t *testing.T) {
	primary := &DevinUsage{
		PromptTokens:            0,
		PromptTokensPresent:     true,
		CompletionTokensPresent: false,
		CachedTokensPresent:     false,
	}
	fallback := &DevinUsage{
		PromptTokens:            100,
		CompletionTokens:        7,
		CachedTokens:            3,
		PromptTokensPresent:     true,
		CompletionTokensPresent: true,
		CachedTokensPresent:     true,
	}
	primary.FillMissingTokenMetrics(fallback)

	if primary.PromptTokens != 0 {
		t.Fatalf("explicit zero prompt tokens replaced with %d", primary.PromptTokens)
	}
	if primary.CompletionTokens != 7 || primary.CachedTokens != 3 {
		t.Fatalf("missing metrics = %d/%d, want 7/3", primary.CompletionTokens, primary.CachedTokens)
	}
	if primary.InputTokens() != 3 || primary.TotalTokens() != 10 {
		t.Fatalf("usage totals = %d/%d, want 3/10", primary.InputTokens(), primary.TotalTokens())
	}
}

func TestParseDevinUsageFieldRejectsMalformedMetrics(t *testing.T) {
	wrongWire := appendDevinStringField(nil, 2, "not a varint")
	overflow := protowire.AppendTag(nil, 3, protowire.VarintType)
	overflow = protowire.AppendVarint(overflow, uint64(math.MaxInt64)+1)
	malformedHeader := appendDevinBytesField(nil, 8, []byte{0x80})

	for _, data := range [][]byte{wrongWire, overflow, malformedHeader} {
		if _, err := parseDevinUsageField(data); err == nil {
			t.Fatalf("parseDevinUsageField(%x) error = nil", data)
		}
	}
}

func TestParseDevinResponseDimensionGroupsPreservesZeroPresence(t *testing.T) {
	group := testDevinDimensionGroup(
		testDevinDimensionMetric("input_tokens", 0),
		testDevinDimensionMetric("output_tokens", 5),
		testDevinDimensionMetric("cached_input_tokens", 0),
	)
	outer := appendDevinBytesField(nil, 28, group)

	usage, err := ParseDevinResponseDimensionGroups(outer)
	if err != nil {
		t.Fatalf("ParseDevinResponseDimensionGroups(): %v", err)
	}
	if usage.PromptTokens != 0 || usage.CompletionTokens != 5 || usage.CachedTokens != 0 {
		t.Fatalf("dimension tokens = %d/%d/%d, want 0/5/0", usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens)
	}
	if !usage.PromptTokensPresent || !usage.CompletionTokensPresent || !usage.CachedTokensPresent {
		t.Fatalf("dimension presence = %t/%t/%t, want all present", usage.PromptTokensPresent, usage.CompletionTokensPresent, usage.CachedTokensPresent)
	}

	unrelated := appendDevinStringField(nil, 1, "Latency Metrics")
	usage, err = ParseDevinResponseDimensionGroups(unrelated, group)
	if err != nil {
		t.Fatalf("ParseDevinResponseDimensionGroups(multiple): %v", err)
	}
	if !usage.CompletionTokensPresent || usage.CompletionTokens != 5 {
		t.Fatalf("multi-group usage = %+v", usage)
	}
}

func TestParseDevinResponseDimensionGroupsRejectsMalformedMetrics(t *testing.T) {
	fractional := testDevinDimensionGroup(testDevinDimensionMetric("input_tokens", 1.5))

	missingValueMetric := appendDevinStringField(nil, 5, "input_tokens")
	missingValue := testDevinDimensionGroup(missingValueMetric)

	wrongWireValue := protowire.AppendTag(nil, 2, protowire.VarintType)
	wrongWireValue = protowire.AppendVarint(wrongWireValue, 1)
	wrongWireMetric := appendDevinBytesField(nil, 4, wrongWireValue)
	wrongWireMetric = appendDevinStringField(wrongWireMetric, 5, "input_tokens")
	wrongWire := testDevinDimensionGroup(wrongWireMetric)

	trailingOuter := appendDevinBytesField(nil, 28, testDevinDimensionGroup(
		testDevinDimensionMetric("input_tokens", 1),
	))
	trailingOuter = append(trailingOuter, 0x08, 0x01)

	for _, group := range [][]byte{fractional, missingValue, wrongWire, trailingOuter, {0x80}} {
		if _, err := ParseDevinResponseDimensionGroups(group); err == nil {
			t.Fatalf("ParseDevinResponseDimensionGroups(%x) error = nil", group)
		}
	}
}

func testDevinDimensionGroup(metrics ...[]byte) []byte {
	group := appendDevinStringField(nil, 1, "Token Usage")
	for _, metric := range metrics {
		group = appendDevinBytesField(group, 2, metric)
	}
	return group
}

func testDevinDimensionMetric(key string, value float32) []byte {
	dimension := protowire.AppendTag(nil, 2, protowire.Fixed32Type)
	dimension = protowire.AppendFixed32(dimension, math.Float32bits(value))
	metric := appendDevinBytesField(nil, 4, dimension)
	return appendDevinStringField(metric, 5, key)
}
