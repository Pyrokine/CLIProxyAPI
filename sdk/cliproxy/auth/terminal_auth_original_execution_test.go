package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type continuation152TerminalExecutor struct {
	terminalUnauthorizedRefreshExecutor
	dispatched int
}

func (e *continuation152TerminalExecutor) Execute(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.dispatched++
	return cliproxyexecutor.Response{}, nil
}

func (e *continuation152TerminalExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	request cliproxyexecutor.Request,
	options cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, request, options)
}

func (e *continuation152TerminalExecutor) ExecuteStream(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.dispatched++
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestContinuation152AllRejectedSelectionKeepsOriginalGateway503(t *testing.T) {
	for _, method := range []string{"execute", "count", "stream"} {
		t.Run(
			method, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				defer func(manager *Manager) {
					//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
					if err := manager.StopAutoRefresh(nil); err != nil {
						t.Errorf("stop auto refresh: %v", err)
					}
				}(manager)
				executor := &continuation152TerminalExecutor{
					err: &Error{
						HTTPStatus: http.StatusBadRequest, Message: "invalid_grant",
					},
				}
				manager.RegisterExecutor(executor)
				provider := executor.Identifier()
				model := "continuation152-terminal-" + method
				id := "continuation152-rejected-" + method
				registerSchedulerModels(t, provider, model, id)
				registered, err := manager.Register(
					t.Context(), &Auth{
						ID: id, Provider: provider, Status: StatusActive,
						Metadata: map[string]any{
							"access_token": "continuation152-rejected", "refresh_token": "continuation152-invalid",
							"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				manager.tryRefreshAfterUnauthorized(
					t.Context(), registered, executor,
					&Error{HTTPStatus: http.StatusUnauthorized, Message: "synthetic access rejection"}, false,
				)
				current, _ := manager.GetByID(id)
				if !hasTerminalUnauthorizedAuthFailure(current) {
					t.Fatal("terminal credential precondition missing")
				}
				request := cliproxyexecutor.Request{Model: model}
				switch method {
				case "execute":
					_, err = manager.Execute(t.Context(), []string{provider}, request, cliproxyexecutor.Options{})
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{provider}, request, cliproxyexecutor.Options{})
				case "stream":
					_, err = manager.ExecuteStream(t.Context(), []string{provider}, request, cliproxyexecutor.Options{})
				}
				status := statusCodeFromError(err)
				if err == nil || executor.dispatched != 0 || executor.calls != 1 {
					t.Fatalf(
						"rejected credential was executed or refreshed again: error=%v dispatch=%d refresh=%d", err,
						executor.dispatched, executor.calls,
					)
				}
				if status != http.StatusServiceUnavailable {
					t.Errorf(
						"original scheduler all-unauthorized HTTP status=%d, want %d", status,
						http.StatusServiceUnavailable,
					)
				}
				var public, cause *Error
				if !IsTerminalAuthError(err) || !errors.As(err, &public) || public.Retryable ||
					!errors.As(
						errors.Unwrap(err), &cause,
					) || cause.HTTPStatus != http.StatusUnauthorized || cause.Message != "invalid_grant" {
					t.Fatalf("terminal public error=%+v, credential cause=%+v", public, cause)
				}
			},
		)
	}
}
