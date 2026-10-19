package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestTransientTransportErrorRetriesWithoutCooldown(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "dial refused",
			err: &url.Error{Op: "Post", URL: "https://example.invalid", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED,
			}},
		},
		{
			name: "temporary dns",
			err: &url.Error{Op: "Post", URL: "https://example.invalid", Err: &net.DNSError{
				Err: "temporary lookup failure", Name: "example.invalid", IsTemporary: true,
			}},
		},
		{
			name: "tls handshake timeout",
			err: &url.Error{Op: "Post", URL: "https://example.invalid", Err: errors.New("net/http: TLS handshake timeout")},
		},
	}
	for _, test := range tests {
		for _, kind := range []string{"execute", "count", "stream"} {
			t.Run(test.name+"/"+kind, func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				executor := &roundExecutor{failure: test.err}
				manager.RegisterExecutor(executor)
				manager.SetRetryConfig(1, 0, 1)
				if _, err := manager.Register(context.Background(), &Auth{ID: "transport", Provider: "test", Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				registerReservationContextModels(t, "test", "round-model", "transport")
				if err := invokeRetryRound(manager, kind, apiKeyReservationTestContext(), coreexecutor.Options{}); err == nil {
					t.Fatal("expected terminal transport error")
				}
				if len(executor.calls) != 2 {
					t.Fatalf("transport attempts = %d, want two with request-retry=1", len(executor.calls))
				}
				assertNoCooldown(t, manager, "transport", "round-model")
			})
		}
	}
}

func TestTransientTransportErrorRejectsUnknownOutcomes(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	cases := []error{
		context.Canceled,
		context.DeadlineExceeded,
		&url.Error{Op: "Post", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
		&Error{Message: "already sent", DispatchPhase: coreexecutor.DispatchPhaseMaybeSent,
			Outcome: coreexecutor.ExecutionOutcomeUnknown, cause: dial},
		&statusBearingError{status: http.StatusTooManyRequests, msg: "dial tcp: connection refused"},
	}
	for _, err := range cases {
		if isTransientTransportError(err) {
			t.Fatalf("unsafe error was classified as pre-HTTP: %v", err)
		}
		if retryDirectiveAllowsRedispatch(executionDispositionForError(err, coreexecutor.DispatchPhaseMaybeSent)) &&
			statusCodeFromError(err) == 0 {
			t.Fatalf("unsafe error was made retryable: %v", err)
		}
	}
}
