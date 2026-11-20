package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type accountedHomeExecutionDispatcher struct {
	calls atomic.Int32
	auths []Auth
}

func (*accountedHomeExecutionDispatcher) HeartbeatOK() bool { return true }
func (d *accountedHomeExecutionDispatcher) RPopAuth(_ context.Context, model, _ string, _ http.Header, _ int) (
	[]byte,
	error,
) {
	index := int(d.calls.Add(1)) - 1
	if index >= len(d.auths) {
		return nil, home.ErrAuthNotFound
	}
	auth := d.auths[index]
	return json.Marshal(
		struct {
			Concurrency homeConcurrencyTuple `json:"concurrency"`
			Model       string               `json:"model"`
			AuthIndex   string               `json:"auth_index"`
			Auth        Auth                 `json:"auth"`
		}{
			Concurrency: homeConcurrencyTuple{Accounted: true, CredentialID: auth.ID, Model: model},
			Model:       model,
			AuthIndex:   auth.ID,
			Auth:        auth,
		},
	)
}
func (*accountedHomeExecutionDispatcher) AbortAmbiguousDispatch() {}

type retainingHomeExecutionExecutor struct {
	openAICompatPoolExecutor
	calls atomic.Int32
}

func (*retainingHomeExecutionExecutor) Identifier() string { return "home-execution" }
func (e *retainingHomeExecutionExecutor) Execute(
	_ context.Context,
	_ *Auth,
	_ executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	e.calls.Add(1)
	if lifecycle, ok := opts.ExecutionLifecycle.(interface{ Retain() }); ok {
		lifecycle.Retain()
	}
	return executor.Response{Payload: []byte("ok")}, nil
}

type resultCaptureHook struct {
	NoopHook
	mu      sync.Mutex
	results []Result
}

func (h *resultCaptureHook) OnResult(_ context.Context, result Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, result)
}

func (h *resultCaptureHook) Results() []Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Result(nil), h.results...)
}

func readOpenAICompatStreamPayload(t *testing.T, result *executor.StreamResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("expected stream result")
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	return string(payload)
}
