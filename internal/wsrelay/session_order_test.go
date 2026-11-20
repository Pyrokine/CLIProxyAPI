package wsrelay

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

type receiveDispatchContext struct {
	context.Context
	fired   atomic.Bool
	publish func()
}

func (ctx *receiveDispatchContext) Done() <-chan struct{} {
	if ctx.fired.CompareAndSwap(false, true) {
		ctx.publish()
	}
	return ctx.Context.Done()
}

func TestStreamTerminalFollowsEveryAcceptedChunk(t *testing.T) {
	for _, test := range []struct {
		name       string
		duringWait bool
		closed     bool
		chunks     int
	}{
		{name: "buffered", chunks: 1},
		{name: "arrives during wait", duringWait: true, chunks: 1},
		{name: "buffered before close", closed: true, chunks: 1},
		{name: "arrives during wait before close", duringWait: true, closed: true, chunks: 1},
		{name: "full regular buffer during wait", duringWait: true, chunks: pendingMessageBuffer},
		{name: "full regular buffer before close", duringWait: true, closed: true, chunks: pendingMessageBuffer},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				const attempts = 128
				lost := 0
				for range attempts {
					s := &session{closed: make(chan struct{})}
					var ctx = t.Context()
					controlled := &receiveDispatchContext{Context: ctx}
					if test.duringWait {
						ctx = controlled
					}
					pending := newPendingRequest(ctx, s.closed)
					s.pending.Store("ordered", pending)
					publish := func() {
						for range test.chunks {
							s.dispatch(
								message{
									ID: "ordered", Type: MessageTypeStreamChunk,
									Payload: map[string]any{"data": "complete"},
								},
							)
						}
						s.dispatch(message{ID: "ordered", Type: MessageTypeStreamEnd})
						if test.closed {
							s.cleanup(errClosed)
						}
					}
					if test.duringWait {
						controlled.publish = publish
					} else {
						publish()
					}
					body, ended := "", false
					for event := range streamPendingEvents(ctx, pending) {
						if event.Err != nil {
							t.Fatalf("unexpected stream error: %v", event.Err)
						}
						body += string(event.Payload)
						ended = ended || event.Type == MessageTypeStreamEnd
					}
					if !ended {
						t.Fatal("stream did not deliver its terminal event")
					}
					if body != strings.Repeat("complete", test.chunks) {
						lost++
					}
				}
				t.Logf("attempts=%d terminal_before_accepted_chunks=%d", attempts, lost)
				if lost != 0 {
					t.Fatalf("terminal event discarded accepted chunks in %d/%d attempts", lost, attempts)
				}
			},
		)
	}
}
