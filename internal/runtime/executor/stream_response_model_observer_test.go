package executor

import (
	"strings"
	"testing"
)

func TestStreamResponseModelObserver_RepeatedEmptyDataBounded(t *testing.T) {
	reporter := &usageReporter{provider: "openai-compat", model: "dall-e-3"}
	buffer := streamUsageBuffer{modelReporter: reporter}
	emptyDataChunk := []byte(strings.Repeat("data:\n", 100))
	for range 50 {
		buffer.observeOpenAIStreamChunk(emptyDataChunk)
		if len(buffer.modelLines) > maxStreamModelEventLines || buffer.modelBytes > maxStreamModelEventBytes {
			t.Fatalf("model event exceeded budget: lines=%d bytes=%d", len(buffer.modelLines), buffer.modelBytes)
		}
	}
	if len(buffer.modelLines) != 0 || buffer.modelBytes != 0 || !buffer.modelOverflow {
		t.Fatalf(
			"overflow event retained state: lines=%d bytes=%d overflow=%t",
			len(buffer.modelLines), buffer.modelBytes, buffer.modelOverflow,
		)
	}

	buffer.observeOpenAIStreamChunk([]byte("\n\nevent: completion\ndata: {\"model\":\"dall-e-3\"}\n\n"))
	buffer.flushOpenAIStream()
	if served, _ := reporter.responseModelSnapshot(); served != "dall-e-3" {
		t.Fatalf("response model = %q, want dall-e-3", served)
	}
	if len(buffer.modelLines) != 0 || buffer.modelBytes != 0 || buffer.modelOverflow {
		t.Fatal("valid event did not clear the model observation overflow state")
	}
}

func TestStreamResponseModelObserver_EventOverflowDropsEventUntilBoundary(t *testing.T) {
	reporter := &usageReporter{provider: "openai-compat", model: "dall-e-3"}
	buffer := streamUsageBuffer{modelReporter: reporter}
	line := []byte("data: " + strings.Repeat("x", 1024) + "\n")
	for range 100 {
		buffer.observeOpenAIStreamChunk(line)
		if len(buffer.modelLines) > maxStreamModelEventLines || buffer.modelBytes > maxStreamModelEventBytes {
			t.Fatalf("model event exceeded budget: lines=%d bytes=%d", len(buffer.modelLines), buffer.modelBytes)
		}
	}
	if len(buffer.modelLines) != 0 || buffer.modelBytes != 0 || !buffer.modelOverflow {
		t.Fatalf(
			"overflow event retained state: lines=%d bytes=%d overflow=%t",
			len(buffer.modelLines), buffer.modelBytes, buffer.modelOverflow,
		)
	}
	buffer.observeOpenAIStreamChunk([]byte("data: {\"model\":\"wrong-before-boundary\"}\n"))
	if served, _ := reporter.responseModelSnapshot(); served != "" {
		t.Fatalf("model from overflowed event was observed before its boundary: %q", served)
	}

	buffer.observeOpenAIStreamChunk([]byte("\n\ndata: {\"model\":\"dall-e-3\"}\n\n"))
	buffer.flushOpenAIStream()
	if served, _ := reporter.responseModelSnapshot(); served != "dall-e-3" {
		t.Fatalf("response model = %q, want dall-e-3", served)
	}
	if len(buffer.modelLines) != 0 || buffer.modelBytes != 0 || buffer.modelOverflow {
		t.Fatal("event boundary did not restore model observation")
	}
}
