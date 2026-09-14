package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

type deliveryResponseWriter struct {
	gin.ResponseWriter
	events             *[]string
	writeErr           error
	flushErr           error
	writeDeadline      time.Time
	short              bool
	blockUntilDeadline bool
}

func (w *deliveryResponseWriter) Write(payload []byte) (int, error) {
	*w.events = append(*w.events, "write")
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.blockUntilDeadline {
		wait := time.Until(w.writeDeadline)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			<-timer.C
		}
		return 0, os.ErrDeadlineExceeded
	}
	if w.short && len(payload) > 0 {
		return len(payload) - 1, nil
	}
	return w.ResponseWriter.Write(payload)
}

func (w *deliveryResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

func (w *deliveryResponseWriter) FlushError() error {
	*w.events = append(*w.events, "flush")
	return w.flushErr
}

func newDeliveryForwardContext(t *testing.T, writer *deliveryResponseWriter) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	writer.ResponseWriter = context.Writer
	context.Writer = writer
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return context
}

func TestForwardStreamChunksAcknowledgesAfterWriteAndFlush(t *testing.T) {
	events := make([]string, 0, 6)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("payload"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("payload acknowledgement error = %v", err)
			}
			events = append(events, "payload_ack")
		},
	}
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("terminal acknowledgement error = %v", err)
			}
			events = append(events, "terminal_ack")
		},
	}
	close(data)
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			WriteChunk: func(chunk []byte) error {
				return WriteStreamBytes(context.Writer, chunk)
			},
			WriteDone: func() error {
				return WriteStreamBytes(context.Writer, []byte("done"))
			},
		},
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{"write", "flush", "payload_ack", "write", "flush", "terminal_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardStreamChunksWaitsForTerminalChannelCloseBeforeCancel(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk, 1)
	acknowledged := make(chan error, 1)
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			acknowledged <- err
		},
	}
	canceled := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
			context,
			context.Writer,
			func(err error) { canceled <- err },
			data,
			nil,
			StreamChunkForwardOptions{},
		)
		close(done)
	}()

	select {
	case err := <-acknowledged:
		if err != nil {
			t.Fatalf("terminal acknowledgement error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal chunk was not acknowledged")
	}
	select {
	case err := <-canceled:
		t.Fatalf("stream canceled before terminal channel closed: %v", err)
	default:
	}

	close(data)
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatalf("cancel error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream was not canceled after terminal channel closed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream forwarder did not return")
	}
}

func TestForwardStreamChunksRejectsCloseWithoutRequiredTerminal(t *testing.T) {
	events := make([]string, 0, 3)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	var canceledWith error
	writeDoneCalled := false

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			RequireTerminal: true,
			WriteTerminalError: func(*ErrorMessage) error {
				return WriteStreamBytes(context.Writer, []byte("error"))
			},
			WriteDone: func() error {
				writeDoneCalled = true
				return WriteStreamBytes(context.Writer, []byte("done"))
			},
		},
	)

	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "before transport terminal") {
		t.Fatalf("cancel error = %v, want missing transport terminal", canceledWith)
	}
	if writeDoneCalled {
		t.Fatal("success terminal was written after the transport channel closed")
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want error write and flush", events)
	}
}

func TestForwardStreamChunksRejectsDataAfterTerminal(t *testing.T) {
	events := make([]string, 0, 4)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk, 2)
	var terminalAck error
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			terminalAck = err
		},
	}
	var trailingAck error
	data <- coreexecutor.StreamChunk{
		Payload: []byte("late"),
		Acknowledge: func(err error) {
			trailingAck = err
		},
	}
	close(data)
	var canceledWith error
	writeDoneCalls := 0

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			RequireTerminal: true,
			WriteDone: func() error {
				writeDoneCalls++
				return WriteStreamBytes(context.Writer, []byte("done"))
			},
		},
	)

	if terminalAck != nil {
		t.Fatalf("terminal acknowledgement error = %v, want nil", terminalAck)
	}
	if trailingAck == nil || !strings.Contains(trailingAck.Error(), "after terminal") {
		t.Fatalf("trailing acknowledgement error = %v", trailingAck)
	}
	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "after terminal") {
		t.Fatalf("cancel error = %v", canceledWith)
	}
	if writeDoneCalls != 1 {
		t.Fatalf("WriteDone calls = %d, want 1", writeDoneCalls)
	}
}

func TestForwardStreamChunksReportsWriteAndFlushFailures(t *testing.T) {
	writeFailure := errors.New("write failed")
	flushFailure := errors.New("flush failed")
	tests := []struct {
		name      string
		writer    func(*[]string) *deliveryResponseWriter
		wantErr   error
		wantEvent []string
	}{
		{
			name: "write failure",
			writer: func(events *[]string) *deliveryResponseWriter {
				return &deliveryResponseWriter{events: events, writeErr: writeFailure}
			},
			wantErr:   writeFailure,
			wantEvent: []string{"write", "ack"},
		},
		{
			name: "short write",
			writer: func(events *[]string) *deliveryResponseWriter {
				return &deliveryResponseWriter{events: events, short: true}
			},
			wantErr:   io.ErrShortWrite,
			wantEvent: []string{"write", "ack"},
		},
		{
			name: "flush failure",
			writer: func(events *[]string) *deliveryResponseWriter {
				return &deliveryResponseWriter{events: events, flushErr: flushFailure}
			},
			wantErr:   flushFailure,
			wantEvent: []string{"write", "flush", "ack"},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				events := make([]string, 0, 3)
				writer := test.writer(&events)
				context := newDeliveryForwardContext(t, writer)
				data := make(chan coreexecutor.StreamChunk, 1)
				var acknowledgedWith error
				data <- coreexecutor.StreamChunk{
					Payload: []byte("payload"),
					Acknowledge: func(err error) {
						acknowledgedWith = err
						events = append(events, "ack")
					},
				}
				close(data)
				var canceledWith error

				NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
					context,
					context.Writer,
					func(err error) { canceledWith = err },
					data,
					nil,
					StreamChunkForwardOptions{
						WriteChunk: func(chunk []byte) error {
							return WriteStreamBytes(context.Writer, chunk)
						},
					},
				)

				if !errors.Is(acknowledgedWith, test.wantErr) {
					t.Fatalf("acknowledgement error = %v, want %v", acknowledgedWith, test.wantErr)
				}
				if !errors.Is(canceledWith, test.wantErr) {
					t.Fatalf("cancel error = %v, want %v", canceledWith, test.wantErr)
				}
				if !reflect.DeepEqual(events, test.wantEvent) {
					t.Fatalf("events = %v, want %v", events, test.wantEvent)
				}
			},
		)
	}
}

func TestForwardStreamChunksTimesOutBlockedDelivery(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events, blockUntilDeadline: true}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk, 1)
	var acknowledgedWith error
	data <- coreexecutor.StreamChunk{
		Payload: []byte("payload"),
		Acknowledge: func(err error) {
			acknowledgedWith = err
			events = append(events, "ack")
		},
	}
	close(data)
	deliveryTimeout := 5 * time.Millisecond
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			DeliveryTimeout: &deliveryTimeout,
			WriteChunk: func(chunk []byte) error {
				return WriteStreamBytes(context.Writer, chunk)
			},
		},
	)

	if !errors.Is(acknowledgedWith, os.ErrDeadlineExceeded) {
		t.Fatalf("acknowledgement error = %v, want deadline exceeded", acknowledgedWith)
	}
	if !errors.Is(canceledWith, os.ErrDeadlineExceeded) {
		t.Fatalf("cancel error = %v, want deadline exceeded", canceledWith)
	}
	want := []string{"write", "ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardStreamChunksWritesTerminalErrorStatusBeforeFirstBody(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	errs := make(chan *ErrorMessage, 1)
	errs <- &ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("quota exhausted"),
	}
	close(errs)

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(error) {},
		data,
		errs,
		StreamChunkForwardOptions{
			WriteTerminalError: func(*ErrorMessage) error {
				return WriteStreamBytes(context.Writer, []byte("error"))
			},
		},
	)

	if status := context.Writer.Status(); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want error write and flush", events)
	}
}

func TestForwardStreamChunksAcknowledgesTerminalErrorAfterFlush(t *testing.T) {
	events := make([]string, 0, 3)
	writer := &deliveryResponseWriter{events: &events}
	context := newDeliveryForwardContext(t, writer)
	upstreamErr := errors.New("upstream failed")
	errs := make(chan *ErrorMessage, 1)
	errs <- &ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      upstreamErr,
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("terminal error acknowledgement = %v", err)
			}
			events = append(events, "ack")
		},
	}
	close(errs)
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		context,
		context.Writer,
		func(err error) { canceledWith = err },
		make(chan coreexecutor.StreamChunk),
		errs,
		StreamChunkForwardOptions{
			WriteTerminalError: func(*ErrorMessage) error {
				return WriteStreamBytes(context.Writer, []byte("error"))
			},
		},
	)

	if !errors.Is(canceledWith, upstreamErr) {
		t.Fatalf("cancel error = %v, want %v", canceledWith, upstreamErr)
	}
	want := []string{"write", "flush", "ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardStreamChunksBoundsProtocolTerminalWaitWithoutHeartbeat(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	ginContext := newDeliveryForwardContext(t, writer)
	outcome := internallogging.NewRequestOutcomeState()
	ginContext.Request = ginContext.Request.WithContext(
		internallogging.WithRequestOutcomeState(ginContext.Request.Context(), outcome),
	)
	data := make(chan coreexecutor.StreamChunk)
	deliveryTimeout := 5 * time.Millisecond
	heartbeats := 0
	protocolTerminal := false
	acknowledged := 0
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		ginContext,
		ginContext.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			KeepAliveInterval: &deliveryTimeout,
			DeliveryTimeout:   &deliveryTimeout,
			ProtocolTerminal:  func() bool { return protocolTerminal },
			WriteChunk: func(chunk []byte) error {
				protocolTerminal = true
				return WriteStreamBytes(ginContext.Writer, chunk)
			},
			WriteKeepAlive: func() KeepAliveWriteResult {
				heartbeats++
				return KeepAliveWriteResult{}
			},
		},
		coreexecutor.StreamChunk{
			Payload: []byte("protocol-terminal"),
			Acknowledge: func(err error) {
				acknowledged++
				if err != nil {
					t.Errorf("protocol terminal acknowledgement error = %v", err)
				}
			},
		},
	)

	if !errors.Is(canceledWith, errStreamTransportTerminalTimeout) {
		t.Fatalf("cancel error = %v, want transport terminal timeout", canceledWith)
	}
	if acknowledged != 1 {
		t.Fatalf("protocol terminal acknowledgements = %d, want 1", acknowledged)
	}
	if heartbeats != 0 {
		t.Fatalf("heartbeats after protocol terminal = %d, want 0", heartbeats)
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want one protocol write and flush", events)
	}
	result := outcome.Snapshot()
	if !result.Failed || result.Kind != internallogging.RequestOutcomeStreamError ||
		result.Reason != errStreamTransportTerminalTimeout.Error() {
		t.Fatalf("request outcome = %+v, want transport terminal timeout", result)
	}
}

func TestForwardStreamChunksRejectsPayloadAfterProtocolTerminal(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	ginContext := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk, 1)
	var trailingAck error
	data <- coreexecutor.StreamChunk{
		Payload: []byte("late"),
		Acknowledge: func(err error) {
			trailingAck = err
		},
	}
	protocolTerminal := false
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		ginContext,
		ginContext.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			ProtocolTerminal: func() bool { return protocolTerminal },
			WriteChunk: func(chunk []byte) error {
				protocolTerminal = true
				return WriteStreamBytes(ginContext.Writer, chunk)
			},
		},
		coreexecutor.StreamChunk{Payload: []byte("protocol-terminal")},
	)

	if !errors.Is(trailingAck, errStreamChunkAfterProtocolTerminal) ||
		!errors.Is(canceledWith, errStreamChunkAfterProtocolTerminal) {
		t.Fatalf("trailing acknowledgement/cancel errors = %v/%v", trailingAck, canceledWith)
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want no trailing write", events)
	}
}

func TestForwardStreamChunksRejectsErrorAfterProtocolTerminalWithoutSecondTerminal(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	ginContext := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk)
	errs := make(chan *ErrorMessage, 1)
	upstreamErr := errors.New("late upstream error")
	var errorAck error
	errs <- &ErrorMessage{
		Error: upstreamErr,
		Acknowledge: func(err error) {
			errorAck = err
		},
	}
	protocolTerminal := false
	terminalWrites := 0
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		ginContext,
		ginContext.Writer,
		func(err error) { canceledWith = err },
		data,
		errs,
		StreamChunkForwardOptions{
			ProtocolTerminal: func() bool { return protocolTerminal },
			WriteChunk: func(chunk []byte) error {
				protocolTerminal = true
				return WriteStreamBytes(ginContext.Writer, chunk)
			},
			WriteTerminalError: func(*ErrorMessage) error {
				terminalWrites++
				return WriteStreamBytes(ginContext.Writer, []byte("second-terminal"))
			},
		},
		coreexecutor.StreamChunk{Payload: []byte("protocol-terminal")},
	)

	if !errors.Is(errorAck, errStreamErrorAfterProtocolTerminal) ||
		!errors.Is(errorAck, upstreamErr) ||
		!errors.Is(canceledWith, errStreamErrorAfterProtocolTerminal) {
		t.Fatalf("terminal error acknowledgement/cancel errors = %v/%v", errorAck, canceledWith)
	}
	if terminalWrites != 0 || strings.Contains(ginContext.Writer.Header().Get("Content-Type"), "second-terminal") {
		t.Fatalf("terminal writes = %d, want 0", terminalWrites)
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want no second terminal write", events)
	}
}

func TestForwardStreamChunksBoundsTransportTerminalDrain(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	ginContext := newDeliveryForwardContext(t, writer)
	data := make(chan coreexecutor.StreamChunk)
	deliveryTimeout := 5 * time.Millisecond
	terminalAcks := 0
	var terminalAckErr error
	var canceledWith error

	NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
		ginContext,
		ginContext.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		StreamChunkForwardOptions{
			DeliveryTimeout: &deliveryTimeout,
			WriteDone: func() error {
				return WriteStreamBytes(ginContext.Writer, []byte("done"))
			},
		},
		coreexecutor.StreamChunk{
			Terminal: true,
			Acknowledge: func(err error) {
				terminalAcks++
				terminalAckErr = err
			},
		},
	)

	if terminalAckErr != nil || terminalAcks != 1 {
		t.Fatalf("terminal acknowledgement = %v calls=%d, want nil once", terminalAckErr, terminalAcks)
	}
	if !errors.Is(canceledWith, errStreamChannelCloseTimeout) {
		t.Fatalf("cancel error = %v, want channel close timeout", canceledWith)
	}
	if !reflect.DeepEqual(events, []string{"write", "flush"}) {
		t.Fatalf("events = %v, want terminal write and flush", events)
	}
}

func TestForwardStreamChunksClientCancelStopsProtocolTerminalWait(t *testing.T) {
	events := make([]string, 0, 2)
	writer := &deliveryResponseWriter{events: &events}
	ginContext := newDeliveryForwardContext(t, writer)
	requestContext, cancelRequest := context.WithCancel(ginContext.Request.Context())
	ginContext.Request = ginContext.Request.WithContext(requestContext)
	data := make(chan coreexecutor.StreamChunk)
	protocolTerminal := false
	acknowledged := make(chan error, 1)
	canceled := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		NewBaseAPIHandlers(nil, nil).ForwardStreamChunks(
			ginContext,
			ginContext.Writer,
			func(err error) { canceled <- err },
			data,
			nil,
			StreamChunkForwardOptions{
				ProtocolTerminal: func() bool { return protocolTerminal },
				WriteChunk: func(chunk []byte) error {
					protocolTerminal = true
					return WriteStreamBytes(ginContext.Writer, chunk)
				},
			},
			coreexecutor.StreamChunk{
				Payload: []byte("protocol-terminal"),
				Acknowledge: func(err error) {
					acknowledged <- err
				},
			},
		)
		close(done)
	}()

	select {
	case err := <-acknowledged:
		if err != nil {
			t.Fatalf("protocol terminal acknowledgement error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("protocol terminal was not acknowledged")
	}
	cancelRequest()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not cancel with the client request")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream forwarder did not return after client cancellation")
	}
}
