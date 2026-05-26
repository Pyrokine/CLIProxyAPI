package executor

import (
	"context"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func sendStreamChunk(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	chunk cliproxyexecutor.StreamChunk,
) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendStreamPayloads(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, payloads [][]byte) bool {
	for i := range payloads {
		if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: payloads[i]}) {
			return false
		}
	}
	return true
}
