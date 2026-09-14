package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type recordingClaudeStreamObserver struct {
	lines    []string
	outcomes []claudeStreamOutcome
}

func (o *recordingClaudeStreamObserver) ObserveClaudeSSELine(line []byte) {
	o.lines = append(o.lines, string(append([]byte(nil), line...)))
}

func (o *recordingClaudeStreamObserver) FinishClaudeStream(outcome claudeStreamOutcome) {
	o.outcomes = append(o.outcomes, outcome)
}

func consumeClaudeStreamForObserverTest(
	t *testing.T,
	body string,
) (*recordingClaudeStreamObserver, []cliproxyexecutor.StreamChunk) {
	t.Helper()
	return consumeClaudeStreamReaderForObserverTest(
		t,
		context.Background(),
		strings.NewReader(body),
	)
}

func consumeClaudeStreamReaderForObserverTest(
	t *testing.T,
	ctx context.Context,
	body io.Reader,
) (*recordingClaudeStreamObserver, []cliproxyexecutor.StreamChunk) {
	t.Helper()
	observer := &recordingClaudeStreamObserver{}
	out := make(chan cliproxyexecutor.StreamChunk, 32)
	consumeClaudeStream(
		ctx,
		&config.Config{},
		body,
		out,
		newUsageReporter(ctx, NewClaudeExecutor(&config.Config{}), "claude-test", nil),
		false,
		nil,
		false,
		http.StatusOK,
		func(line []byte) [][]byte {
			return [][]byte{append([]byte(nil), line...)}
		},
		observer,
	)
	close(out)
	chunks := make([]cliproxyexecutor.StreamChunk, 0, len(out))
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	return observer, chunks
}

func TestConsumeClaudeStreamObserverSeesUpstreamErrorBeforeValidator(t *testing.T) {
	observer, chunks := consumeClaudeStreamForObserverTest(
		t,
		`data: {"type":"error","error":{"message":"bad signature"}}`,
	)
	if len(observer.lines) != 1 || !strings.Contains(observer.lines[0], `"type":"error"`) {
		t.Fatalf("observer lines = %q", observer.lines)
	}
	if len(observer.outcomes) != 1 || observer.outcomes[0] != claudeStreamOutcomeUpstreamError {
		t.Fatalf("observer outcomes = %v", observer.outcomes)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
		t.Fatalf("stream chunks = %+v", chunks)
	}
}

func TestConsumeClaudeStreamObserverDistinguishesValidationFailure(t *testing.T) {
	observer, chunks := consumeClaudeStreamForObserverTest(t, `data: {not-json}`)
	if len(observer.outcomes) != 1 || observer.outcomes[0] != claudeStreamOutcomeValidationFailure {
		t.Fatalf("observer outcomes = %v", observer.outcomes)
	}
	if len(chunks) != 1 || chunks[0].Err == nil {
		t.Fatalf("stream chunks = %+v", chunks)
	}
}

func TestConsumeClaudeStreamObserverReportsIncompleteTerminalState(t *testing.T) {
	body := strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg","model":"claude-test"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		}, "\n",
	)
	observer, chunks := consumeClaudeStreamForObserverTest(t, body)
	if len(observer.outcomes) != 1 || observer.outcomes[0] != claudeStreamOutcomeIncomplete {
		t.Fatalf("observer outcomes = %v", observer.outcomes)
	}
	if len(chunks) != 3 || chunks[len(chunks)-1].Err == nil {
		t.Fatalf("stream chunks = %+v", chunks)
	}
}

func TestConsumeClaudeStreamObserverDistinguishesReadFailureAndCancellation(t *testing.T) {
	readObserver, readChunks := consumeClaudeStreamReaderForObserverTest(
		t,
		context.Background(),
		&terminalErrorReader{err: io.ErrUnexpectedEOF},
	)
	if len(readObserver.outcomes) != 1 || readObserver.outcomes[0] != claudeStreamOutcomeReadFailure {
		t.Fatalf("read failure outcomes = %v", readObserver.outcomes)
	}
	if len(readChunks) != 1 || !errors.Is(readChunks[0].Err, io.ErrUnexpectedEOF) {
		t.Fatalf("read failure chunks = %+v", readChunks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelObserver, cancelChunks := consumeClaudeStreamReaderForObserverTest(
		t,
		ctx,
		&terminalErrorReader{err: io.ErrUnexpectedEOF},
	)
	if len(cancelObserver.outcomes) != 1 || cancelObserver.outcomes[0] != claudeStreamOutcomeCanceled {
		t.Fatalf("cancellation outcomes = %v", cancelObserver.outcomes)
	}
	if len(cancelChunks) > 1 ||
		(len(cancelChunks) == 1 && !errors.Is(cancelChunks[0].Err, io.ErrUnexpectedEOF)) {
		t.Fatalf("cancellation chunks = %+v", cancelChunks)
	}
}

func TestConsumeClaudeStreamObserverReportsOneCompleteOutcome(t *testing.T) {
	body := strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg","model":"claude-test"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	)
	observer, chunks := consumeClaudeStreamForObserverTest(t, body)
	if len(observer.lines) != 3 {
		t.Fatalf("observer lines = %d, want 3", len(observer.lines))
	}
	if len(observer.outcomes) != 1 || observer.outcomes[0] != claudeStreamOutcomeComplete {
		t.Fatalf("observer outcomes = %v", observer.outcomes)
	}
	if len(chunks) != 3 {
		t.Fatalf("stream chunks = %d, want 3", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("complete stream error = %v", chunk.Err)
		}
	}
}
