package logging

import (
	"bytes"
	"sync"
)

const maxInMemoryRequestLogBytes = maxMaskedPayloadBytes

var truncatedInMemoryRequestLog = []byte(`{"log_capture":"truncated: exceeds safe capture limit"}`)

func cloneBoundedLogPayload(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInMemoryRequestLogBytes {
		return bytes.Clone(truncatedInMemoryRequestLog)
	}
	return bytes.Clone(data)
}

// BoundedLogBuffer stores request-log fallback data without allowing memory growth past the redaction limit.
type BoundedLogBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

// NewBoundedLogBuffer creates an empty bounded request-log buffer.
func NewBoundedLogBuffer() *BoundedLogBuffer {
	return &BoundedLogBuffer{}
}

// Write implements io.Writer. Input is accepted even when the retained log is replaced by the truncation marker.
func (b *BoundedLogBuffer) Write(data []byte) (int, error) {
	b.Append(data)
	return len(data), nil
}

// Append adds data unless doing so would exceed the request-log memory limit.
func (b *BoundedLogBuffer) Append(data []byte) {
	if b == nil || len(data) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return
	}
	if len(data) > maxInMemoryRequestLogBytes-b.buffer.Len() {
		b.buffer.Reset()
		_, _ = b.buffer.Write(truncatedInMemoryRequestLog)
		b.truncated = true
		return
	}
	_, _ = b.buffer.Write(data)
}

// Bytes returns a copy of the retained data.
func (b *BoundedLogBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}

// String returns the retained data as a string.
func (b *BoundedLogBuffer) String() string {
	return string(b.Bytes())
}

// HasSuffix reports whether the retained data ends with suffix.
func (b *BoundedLogBuffer) HasSuffix(suffix []byte) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.HasSuffix(b.buffer.Bytes(), suffix)
}

// Len returns the number of retained bytes.
func (b *BoundedLogBuffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

// Truncated reports whether the original payload exceeded the memory limit.
func (b *BoundedLogBuffer) Truncated() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
