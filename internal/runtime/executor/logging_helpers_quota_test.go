package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type loggingQuotaBridgeExecutor struct {
	websocket bool
	headers   http.Header
}

func (*loggingQuotaBridgeExecutor) Identifier() string { return "codex" }

func (e *loggingQuotaBridgeExecutor) Execute(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e.websocket {
		RecordAPIWebsocketHandshake(ctx, nil, http.StatusSwitchingProtocols, e.headers)
	} else {
		RecordAPIResponseMetadata(ctx, nil, http.StatusOK, e.headers)
	}
	e.headers.Set("X-Codex-Primary-Used-Percent", "mutated-after-record")
	return cliproxyexecutor.Response{}, nil
}

func (*loggingQuotaBridgeExecutor) ExecuteStream(
	context.Context,
	*cliproxyauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("unexpected ExecuteStream call")
}

func (*loggingQuotaBridgeExecutor) Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return nil, errors.New("unexpected Refresh call")
}

func (*loggingQuotaBridgeExecutor) CountTokens(
	context.Context,
	*cliproxyauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected CountTokens call")
}

func (*loggingQuotaBridgeExecutor) HttpRequest(
	context.Context,
	*cliproxyauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func TestResponseLoggingHelpersBridgeHeadersToCurrentAuthAttempt(t *testing.T) {
	for _, test := range []struct {
		name      string
		websocket bool
	}{
		{name: "http"},
		{name: "websocket handshake", websocket: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := "logging-quota-bridge-" + test.name
			authID := "logging-quota-auth-" + test.name
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			headers := http.Header{
				"X-Codex-Plan-Type":            {"pro"},
				"X-Codex-Primary-Used-Percent": {"73"},
			}
			executor := &loggingQuotaBridgeExecutor{websocket: test.websocket, headers: headers}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
			manager.RegisterExecutor(executor)
			if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
				ID: authID, Provider: "codex", Status: cliproxyauth.StatusActive,
			}); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			if _, errExecute := manager.Execute(
				context.Background(),
				[]string{"codex"},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{},
			); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("auth disappeared after execution")
			}
			if updated.Quota.Signals["X-Codex-Plan-Type"] != "pro" ||
				updated.Quota.Signals["X-Codex-Primary-Used-Percent"] != "73" {
				t.Fatalf("quota observation = %#v", updated.Quota.Signals)
			}
		})
	}
}
