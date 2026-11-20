package executor

import "testing"

func TestOptionsEnsureMetadataInitializesAndPreservesMap(t *testing.T) {
	var options Options
	metadata := options.EnsureMetadata()
	if metadata == nil || options.Metadata == nil {
		t.Fatal("zero-value options did not initialize metadata")
	}
	metadata[RequestedModelMetadataKey] = "requested-model"
	if options.Metadata[RequestedModelMetadataKey] != "requested-model" {
		t.Fatal("EnsureMetadata returned an independent map")
	}
	options.Metadata[ReasoningEffortMetadataKey] = "high"
	if got := options.EnsureMetadata()[ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("existing metadata was replaced: %v", got)
	}
	metadata["request_counter"] = 0
	if got := options.EnsureMetadata()["request_counter"]; got != 0 {
		t.Fatalf("explicit zero metadata changed: %v", got)
	}
}
