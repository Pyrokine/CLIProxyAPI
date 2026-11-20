package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestConsumeClaudeStreamAcceptsClosedContentBlocks(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"fixture","model":"claude-test"}}`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"content_block_stop","index":3}`,
		`data: {"type":"content_block_start","index":7,"content_block":{"type":` +
			`"tool_use","id":"tool-1","name":"lookup","input":{}}}`,
		`data: {"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":7}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	}
	observer, chunks := consumeClaudeStreamForObserverTest(t, strings.Join(lines, "\n"))
	if len(chunks) != len(lines) || len(observer.outcomes) != 1 ||
		observer.outcomes[0] != claudeStreamOutcomeComplete {
		t.Fatalf("valid block lifecycle changed: chunks=%d outcomes=%v", len(chunks), observer.outcomes)
	}
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}

func TestClaudeContentBlockFailurePreservesObservedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	body := strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"fixture","model":` +
				`"claude-test","usage":{"input_tokens":` +
				`2}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	)
	_, chunks := consumeClaudeStreamForObserverTest(t, body)
	if len(chunks) == 0 || chunks[len(chunks)-1].Err == nil {
		t.Fatal("open-block completion did not fail")
	}
	select {
	case record := <-plugin.records:
		if !record.Failed || record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 7 ||
			record.Detail.TotalTokens != 9 {
			t.Fatalf("invalid content-block failure lost paid usage: %+v", record)
		}
		select {
		case duplicate := <-plugin.records:
			t.Fatalf("duplicate usage publication: %+v", duplicate)
		default:
		}
	case <-time.After(time.Second):
		t.Fatal("usage publication missing")
	}
}

func TestConsumeClaudeStreamRejectsInvalidContentBlockLifecycle(t *testing.T) {
	const start = `data: {"type":"message_start","message":{"id":"fixture","model":"claude-test"}}`
	const block = `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	const stopBlock = `data: {"type":"content_block_stop","index":0}`
	const delta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`
	const stop = `data: {"type":"message_stop"}`
	for _, tc := range []struct {
		name   string
		prefix []string
		bad    string
	}{
		{name: "start before message", bad: block},
		{name: "duplicate message start", prefix: []string{start}, bad: start},
		{name: "duplicate block start", prefix: []string{start, block}, bad: block},
		{name: "reused closed index", prefix: []string{start, block, stopBlock}, bad: block},
		{
			name: "orphan delta", prefix: []string{start},
			bad: `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"invalid"}}`,
		},
		{name: "orphan stop", prefix: []string{start}, bad: stopBlock},
		{name: "duplicate stop", prefix: []string{start, block, stopBlock}, bad: stopBlock},
		{
			name: "missing index", prefix: []string{start},
			bad: `data: {"type":"content_block_start","content_block":{"type":"text","text":""}}`,
		},
		{
			name: "negative index", prefix: []string{start},
			bad: `data: {"type":"content_block_start","index":-1,"content_block":{"type":"text","text":""}}`,
		},
		{
			name: "fractional index", prefix: []string{start},
			bad: `data: {"type":"content_block_start","index":1.5,"content_block":{"type":"text","text":""}}`,
		},
		{name: "open block at message delta", prefix: []string{start, block}, bad: delta},
		{name: "open block at message stop", prefix: []string{start, block}, bad: stop},
		{name: "block after message delta", prefix: []string{start, delta}, bad: block},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				lines := append(append([]string{}, tc.prefix...), tc.bad, delta, stop)
				observer, chunks := consumeClaudeStreamForObserverTest(t, strings.Join(lines, "\n"))
				var streamErr error
				payloads := 0
				for _, chunk := range chunks {
					if chunk.Err != nil {
						if streamErr != nil {
							t.Fatal("duplicate stream failure")
						}
						streamErr = chunk.Err
					} else {
						payloads++
					}
				}
				if streamErr == nil || payloads != len(tc.prefix) || len(observer.outcomes) != 1 ||
					observer.outcomes[0] != claudeStreamOutcomeValidationFailure {
					t.Fatalf(
						"invalid lifecycle reached consumer: error=%v payloads=%d want=%d outcomes=%v",
						streamErr,
						payloads,
						len(tc.prefix),
						observer.outcomes,
					)
				}
				var classified coreexecutor.ExecutionDispositionError
				if !errors.As(streamErr, &classified) {
					t.Fatalf("stream failure lost disposition: %T %v", streamErr, streamErr)
				}
				if disposition := classified.ExecutionDisposition(); disposition.DispatchPhase !=
					coreexecutor.DispatchPhaseResponseStarted ||
					disposition.Outcome != coreexecutor.ExecutionOutcomeUnknown ||
					disposition.RetryDirective != coreexecutor.RetryDirectiveNoRetry {
					t.Fatalf("invalid upstream stream became replayable: %+v", disposition)
				}
			},
		)
	}
}
