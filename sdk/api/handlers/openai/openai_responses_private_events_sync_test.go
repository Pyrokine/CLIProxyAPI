package openai

import (
	"bytes"
	"testing"
)

func TestResponsesSSEFiltersPrivateFramesForStandardClients(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	chunks := [][]byte{
		[]byte("event: responsesapi.websocket_timing\ndata: {\"type\":\"responsesapi.websocket_timing\"}\n\n"),
		[]byte("event: codex.response.metadata\ndata: {\"type\":\"codex.response.metadata\",\"secret\":\"hidden\"}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"codex.rate_limits\"}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"),
	}
	var frames [][]byte
	for _, chunk := range chunks {
		frames = append(frames, acc.AddChunk(chunk)...)
	}
	frames = append(frames, acc.Finalize()...)
	if err := acc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !bytes.Contains(frames[0], []byte("visible")) ||
		!bytes.Contains(frames[1], []byte("response.completed")) {
		t.Fatalf("public frames = %q, want text delta and completion only", frames)
	}
}

func TestResponsesSSEPreservesCodexMetadataButNotRateLimits(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	acc.isCodexClient = true
	var frames [][]byte
	frames = append(frames, acc.AddChunk([]byte("event: codex.response.metadata\ndata: {\"type\":\"codex.response.metadata\"}\n\n"))...)
	frames = append(frames, acc.AddChunk([]byte("event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\"}\n\n"))...)
	frames = append(frames, acc.AddChunk([]byte("event: responsesapi.timing\ndata: invalid-json\n\n"))...)
	frames = append(frames, acc.AddChunk([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"error\":{\"message\":\"denied\"}}\n\n"))...)
	if err := acc.Err(); err != nil {
		t.Fatalf("private telemetry caused a malformed-frame failure: %v", err)
	}
	if len(frames) != 2 || !bytes.Contains(frames[0], []byte("codex.response.metadata")) ||
		!bytes.Contains(frames[1], []byte("response.failed")) {
		t.Fatalf("Codex client frames = %q, want metadata and failure only", frames)
	}
}
