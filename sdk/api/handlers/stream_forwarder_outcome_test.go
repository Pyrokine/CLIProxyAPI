package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func newStreamOutcomeTestContext(t *testing.T) (*gin.Context, *internallogging.RequestOutcomeState) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	state := internallogging.NewRequestOutcomeState()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request = request.WithContext(internallogging.WithRequestOutcomeState(context.Background(), state))
	ginContext.Request = request
	return ginContext, state
}

type failingStreamResponseWriter struct {
	gin.ResponseWriter
	err     error
	writes  int
	flushes int
}

func (w *failingStreamResponseWriter) Write(_ []byte) (int, error) {
	w.writes++
	return 0, w.err
}

func (w *failingStreamResponseWriter) Flush() {
	w.flushes++
}

type failingStreamFlushWriter struct {
	gin.ResponseWriter
	err error
}

func (w *failingStreamFlushWriter) FlushError() error { return w.err }

func TestForwardStreamDeliveryFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		writer func(*gin.Context, error) gin.ResponseWriter
	}{
		{
			name: "write failure",
			writer: func(c *gin.Context, err error) gin.ResponseWriter {
				return &failingStreamResponseWriter{ResponseWriter: c.Writer, err: err}
			},
		},
		{
			name: "flush failure",
			writer: func(c *gin.Context, err error) gin.ResponseWriter {
				return &failingStreamFlushWriter{ResponseWriter: c.Writer, err: err}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, state := newStreamOutcomeTestContext(t)
			failure := errors.New(test.name)
			c.Writer = test.writer(c, failure)
			data := make(chan []byte, 1)
			data <- []byte("event: message\n\n")
			close(data)
			keepAlive := time.Duration(0)
			var canceledWith error
			NewBaseAPIHandlers(nil, nil).ForwardStream(c, c.Writer, func(err error) { canceledWith = err }, data, nil,
				StreamForwardOptions{
					KeepAliveInterval: &keepAlive,
					WriteChunkError:   func(chunk []byte) error { return WriteStreamBytes(c.Writer, chunk) },
				})
			if !errors.Is(canceledWith, failure) {
				t.Fatalf("cancel error = %v, want %v", canceledWith, failure)
			}
			if outcome := state.Snapshot(); !outcome.Failed || outcome.Reason != failure.Error() {
				t.Fatalf("request outcome = %+v, want stream delivery failure", outcome)
			}
		})
	}
}

func TestForwardStreamHeartbeatWriteFailureCancelsWithoutFlush(t *testing.T) {
	ginContext, state := newStreamOutcomeTestContext(t)
	wantErr := errors.New("heartbeat write failed")
	writer := &failingStreamResponseWriter{ResponseWriter: ginContext.Writer, err: wantErr}
	ginContext.Writer = writer
	data := make(chan []byte)
	keepAlive := time.Millisecond
	var canceledWith error
	done := make(chan struct{})
	go func() {
		NewBaseAPIHandlers(nil, nil).ForwardStream(
			ginContext,
			ginContext.Writer,
			func(err error) { canceledWith = err },
			data,
			nil,
			StreamForwardOptions{KeepAliveInterval: &keepAlive},
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ForwardStream did not stop after heartbeat write failure")
	}
	if !errors.Is(canceledWith, wantErr) {
		t.Fatalf("cancel error = %v, want %v", canceledWith, wantErr)
	}
	if writer.writes != 1 || writer.flushes != 0 {
		t.Fatalf("writer writes=%d flushes=%d, want one write and no flush", writer.writes, writer.flushes)
	}
	outcome := state.Snapshot()
	if !outcome.Failed || outcome.Kind != internallogging.RequestOutcomeStreamError || outcome.Reason != wantErr.Error() {
		t.Fatalf("request outcome = %+v, want heartbeat stream failure", outcome)
	}
}

func TestForwardStreamMarksTerminalFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func() (<-chan []byte, <-chan *ErrorMessage, StreamForwardOptions, error)
	}{
		{
			name: "upstream error channel",
			configure: func() (<-chan []byte, <-chan *ErrorMessage, StreamForwardOptions, error) {
				data := make(chan []byte)
				errs := make(chan *ErrorMessage, 1)
				streamErr := errors.New("late upstream failure")
				errs <- &ErrorMessage{StatusCode: http.StatusBadGateway, Error: streamErr}
				return data, errs, StreamForwardOptions{}, streamErr
			},
		},
		{
			name: "clean close finalization error",
			configure: func() (<-chan []byte, <-chan *ErrorMessage, StreamForwardOptions, error) {
				data := make(chan []byte)
				close(data)
				streamErr := errors.New("incomplete stream")
				return data, nil, StreamForwardOptions{DoneError: func() error { return streamErr }}, streamErr
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				ginContext, state := newStreamOutcomeTestContext(t)
				data, errs, options, wantErr := test.configure()
				keepAlive := time.Duration(0)
				options.KeepAliveInterval = &keepAlive
				var canceledWith error

				NewBaseAPIHandlers(nil, nil).ForwardStream(
					ginContext,
					ginContext.Writer,
					func(err error) { canceledWith = err },
					data,
					errs,
					options,
				)

				if !errors.Is(canceledWith, wantErr) {
					t.Fatalf("cancel error = %v, want %v", canceledWith, wantErr)
				}
				outcome := state.Snapshot()
				if !outcome.Failed || outcome.Kind != internallogging.RequestOutcomeStreamError {
					t.Fatalf("request outcome = %+v, want stream failure", outcome)
				}
				if outcome.Reason != wantErr.Error() {
					t.Fatalf("request outcome reason = %q, want %q", outcome.Reason, wantErr.Error())
				}
			},
		)
	}
}

func TestForwardStreamLeavesCleanCloseSuccessful(t *testing.T) {
	ginContext, state := newStreamOutcomeTestContext(t)
	data := make(chan []byte)
	close(data)
	keepAlive := time.Duration(0)

	NewBaseAPIHandlers(nil, nil).ForwardStream(
		ginContext,
		ginContext.Writer,
		func(error) {},
		data,
		nil,
		StreamForwardOptions{KeepAliveInterval: &keepAlive},
	)

	if outcome := state.Snapshot(); outcome.Failed {
		t.Fatalf("request outcome = %+v, want success", outcome)
	}
}
