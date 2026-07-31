package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type streamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[string]*streamEntry
}

type streamInput struct {
	chunk    pluginapi.ExecutorStreamChunk
	accepted chan bool
}

type streamEntry struct {
	input     chan streamInput
	chunks    chan pluginapi.ExecutorStreamChunk
	done      chan struct{}
	abort     chan struct{}
	closeOnce sync.Once
	abortOnce sync.Once
	onDone    func()
	mu        sync.Mutex
	closed    bool
	closeErr  error
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func newStreamBridge() *streamBridge {
	return &streamBridge{streams: make(map[string]*streamEntry)}
}

func newStreamEntry(onDone func()) *streamEntry {
	entry := &streamEntry{
		input:  make(chan streamInput),
		chunks: make(chan pluginapi.ExecutorStreamChunk, 16),
		done:   make(chan struct{}),
		abort:  make(chan struct{}),
		onDone: onDone,
	}
	go entry.forward()
	return entry
}

func (e *streamEntry) forward() {
	defer close(e.chunks)
	defer func() {
		if e.onDone != nil {
			e.onDone()
		}
	}()
	pending := make([]pluginapi.ExecutorStreamChunk, 0, 16)
	closing := false
	for {
		if closing && len(pending) == 0 {
			return
		}
		var input chan streamInput
		if len(pending) < 1 {
			input = e.input
		}
		var output chan pluginapi.ExecutorStreamChunk
		var next pluginapi.ExecutorStreamChunk
		if len(pending) > 0 {
			output = e.chunks
			next = pending[0]
		}
		if closing {
			select {
			case <-e.abort:
				return
			case output <- next:
				pending = pending[1:]
			}
			continue
		}
		select {
		case <-e.abort:
			return
		case <-e.done:
			e.mu.Lock()
			closeErr := e.closeErr
			e.mu.Unlock()
			if closeErr != nil {
				pending = append(pending, pluginapi.ExecutorStreamChunk{Err: closeErr})
			}
			closing = true
		case request := <-input:
			e.mu.Lock()
			if e.closed {
				request.accepted <- false
				e.mu.Unlock()
				continue
			}
			pending = append(pending, request.chunk)
			request.accepted <- true
			e.mu.Unlock()
		case output <- next:
			pending = pending[1:]
		}
	}
}

func (e *streamEntry) emit(ctx context.Context, chunk pluginapi.ExecutorStreamChunk) error {
	if ctx == nil {
		ctx = context.Background()
	}
	request := streamInput{chunk: chunk, accepted: make(chan bool, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return fmt.Errorf("stream is closed")
	case <-e.abort:
		return fmt.Errorf("stream is closed")
	case e.input <- request:
	}
	select {
	case accepted := <-request.accepted:
		if accepted {
			return nil
		}
		return fmt.Errorf("stream is closed")
	case <-ctx.Done():
		return ctx.Err()
	case <-e.abort:
		return fmt.Errorf("stream is closed")
	case <-e.done:
		select {
		case accepted := <-request.accepted:
			if accepted {
				return nil
			}
		default:
		}
		return fmt.Errorf("stream is closed")
	}
}

func (e *streamEntry) close(errorMessage string) {
	if e == nil {
		return
	}
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		if errorMessage != "" {
			e.closeErr = fmt.Errorf("%s", errorMessage)
		}
		close(e.done)
		e.mu.Unlock()
	})
}

func (e *streamEntry) cancel() {
	if e == nil {
		return
	}
	e.abortOnce.Do(func() {
		close(e.abort)
	})
}

func (b *streamBridge) open(
	ctx context.Context,
	onDone ...func(),
) (string, <-chan pluginapi.ExecutorStreamChunk, func()) {
	if b == nil {
		chunks := make(chan pluginapi.ExecutorStreamChunk)
		close(chunks)
		return "", chunks, func() {}
	}
	id := strconv.FormatUint(b.next.Add(1), 10)
	var entry *streamEntry
	entry = newStreamEntry(func() {
		b.remove(id, entry)
		if len(onDone) > 0 && onDone[0] != nil {
			onDone[0]()
		}
	})
	b.mu.Lock()
	b.streams[id] = entry
	b.mu.Unlock()
	cleanup := func() {
		b.abort(id)
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			b.abort(id)
		}()
	}
	return id, entry.chunks, cleanup
}

func (b *streamBridge) emit(ctx context.Context, id string, chunk pluginapi.ExecutorStreamChunk) error {
	if b == nil || id == "" {
		return fmt.Errorf("stream id is required")
	}
	b.mu.Lock()
	entry := b.streams[id]
	b.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("stream %s is not open", id)
	}
	if err := entry.emit(ctx, chunk); err != nil {
		return fmt.Errorf("stream %s: %w", id, err)
	}
	return nil
}

func (b *streamBridge) close(id string, errorMessage string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	entry := b.streams[id]
	b.mu.Unlock()
	entry.close(errorMessage)
}

func (b *streamBridge) abort(id string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	entry := b.streams[id]
	delete(b.streams, id)
	b.mu.Unlock()
	entry.cancel()
}

func (b *streamBridge) remove(id string, expected *streamEntry) {
	if b == nil || id == "" || expected == nil {
		return
	}
	b.mu.Lock()
	if b.streams[id] == expected {
		delete(b.streams, id)
	}
	b.mu.Unlock()
}
