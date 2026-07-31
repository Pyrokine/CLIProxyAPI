package common

import (
	"bytes"
	"testing"
)

func TestSSEEventDataWritesCompleteFrame(t *testing.T) {
	got := SSEEventData(
		"response.completed",
		[]byte(`{"type":"response.completed"}`),
	)
	want := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("SSEEventData() = %q, want %q", got, want)
	}
}
