package logging

import (
	"bytes"
	"testing"
)

func TestBoundedLogBufferAppendsWithoutCopyingState(t *testing.T) {
	buffer := NewBoundedLogBuffer()
	buffer.Append([]byte("first"))
	buffer.Append([]byte("-second"))

	if got := buffer.String(); got != "first-second" {
		t.Fatalf("buffer = %q, want %q", got, "first-second")
	}
	if buffer.Truncated() {
		t.Fatal("buffer was unexpectedly truncated")
	}
}

func TestBoundedLogBufferReplacesOversizedPayloadWithMarker(t *testing.T) {
	buffer := NewBoundedLogBuffer()
	buffer.Append(bytes.Repeat([]byte("x"), maxInMemoryRequestLogBytes))
	buffer.Append([]byte("overflow"))
	buffer.Append([]byte("ignored"))

	if got := buffer.Bytes(); !bytes.Equal(got, truncatedInMemoryRequestLog) {
		t.Fatalf("buffer = %q, want truncation marker", got)
	}
	if !buffer.Truncated() {
		t.Fatal("buffer did not report truncation")
	}
}

func TestCloneBoundedLogPayloadReplacesOversizedPayloadWithMarker(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), maxInMemoryRequestLogBytes+1)
	got := cloneBoundedLogPayload(payload)
	if !bytes.Equal(got, truncatedInMemoryRequestLog) {
		t.Fatalf("payload = %q, want truncation marker", got)
	}

	var masked bytes.Buffer
	if errWrite := writeMaskedPayload(&masked, got); errWrite != nil {
		t.Fatalf("writeMaskedPayload error: %v", errWrite)
	}
	if !bytes.Contains(masked.Bytes(), []byte("truncated: exceeds safe capture limit")) {
		t.Fatalf("masked payload removed truncation marker: %q", masked.Bytes())
	}
}
