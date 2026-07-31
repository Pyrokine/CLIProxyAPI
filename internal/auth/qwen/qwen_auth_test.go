package qwen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type qwenRoundTripFunc func(*http.Request) (*http.Response, error)

func (f qwenRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type completeResponseAndCancelBody struct {
	cancel context.CancelFunc
	sent   bool
}

func (b *completeResponseAndCancelBody) Read(buffer []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	b.cancel()
	return copy(buffer, `{"error":"access_denied","error_description":"denied"}`), io.EOF
}

func (*completeResponseAndCancelBody) Close() error { return nil }

type cancellationBody struct {
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
}

func (b *cancellationBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*cancellationBody) Close() error { return nil }

func TestPollForTokenCancellationWinsCompletedErrorResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	auth := &Auth{
		httpClient: &http.Client{
			Transport: qwenRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Body:       &completeResponseAndCancelBody{cancel: cancel},
					}, nil
				},
			),
		},
	}
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = originalStdout
		_ = reader.Close()
		_ = writer.Close()
	})

	_, err = auth.PollForToken(ctx, "device-code", "code-verifier")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	os.Stdout = originalStdout
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if strings.Contains(string(output), "Polling attempt") {
		t.Fatalf("cancellation logged polling failure: %q", output)
	}
}

func TestPollForTokenCancellationDuringBodyReadDoesNotLogFailure(t *testing.T) {
	entered := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: qwenRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: &cancellationBody{
							ctx:     req.Context(),
							entered: entered,
						},
					}, nil
				},
			),
		},
	}
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = originalStdout
		_ = reader.Close()
		_ = writer.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, errPoll := auth.PollForToken(ctx, "device-code", "code-verifier")
		done <- errPoll
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("response body read did not start")
	}
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	os.Stdout = originalStdout
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if strings.Contains(string(output), "Polling attempt") {
		t.Fatalf("cancellation logged polling failure: %q", output)
	}
}

func TestPollForTokenStopsWhenContextIsCancelled(t *testing.T) {
	entered := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: qwenRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					close(entered)
					<-req.Context().Done()
					return nil, req.Context().Err()
				},
			),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := auth.PollForToken(ctx, "device-code", "code-verifier")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("poll request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("PollForToken() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PollForToken did not stop after cancellation")
	}
}
