package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func captureExecutionWarnings(t *testing.T) func() []*log.Entry {
	t.Helper()
	logger := log.StandardLogger()
	level, output := logger.GetLevel(), logger.Out
	hooks := logger.ReplaceHooks(make(log.LevelHooks))
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.WarnLevel)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() {
		logger.ReplaceHooks(hooks)
		logger.SetLevel(level)
		logger.SetOutput(output)
	})
	return func() []*log.Entry {
		var entries []*log.Entry
		for _, entry := range hook.AllEntries() {
			if entry.Message == "upstream execution failed" {
				entries = append(entries, entry)
			}
		}
		return entries
	}
}

type markedExecutionDiagnostic struct{ diagnostic string }

func (e markedExecutionDiagnostic) Error() string         { return "private free-form control error" }
func (e markedExecutionDiagnostic) StatusCode() int       { return 502 }
func (e markedExecutionDiagnostic) LogDiagnostic() string { return e.diagnostic }
func (e markedExecutionDiagnostic) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{DispatchPhase: cliproxyexecutor.DispatchPhaseNotSent}
}

func TestWarnLogUpstreamFailurePreservesSafeFields(t *testing.T) {
	warnings := captureExecutionWarnings(t)
	ctx := logging.WithRequestID(context.Background(), "full-synthetic-request-id")
	err := fmt.Errorf("wrapped: %w", markedExecutionDiagnostic{diagnostic: "access token expired\nAuthorization: Bearer synthetic-secret"})
	auth := &Auth{ID: "private-filename.json", Index: "synthetic-index", Metadata: map[string]any{"access_token": "never-log-auth"}}
	warnLogUpstreamFailure(ctx, auth, "provider", "model", time.Now(), err, cliproxyexecutor.DispatchPhaseNotSent)
	entries := warnings()
	if len(entries) != 1 {
		t.Fatalf("warnings=%d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Data["status"] != 502 || entry.Data["phase"] != cliproxyexecutor.DispatchPhaseNotSent || entry.Data["auth_index"] != auth.Index {
		t.Fatalf("warning fields=%v", entry.Data)
	}
	wire, errFormat := (&logging.LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	line := string(wire)
	for _, want := range []string{"status=502", "phase=not_sent", "duration=", "provider=provider", "model=model", "synthetic-index", "full-synthetic-request-id", "access token expired", "REDACTED"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log missing %q: %q", want, line)
		}
	}
	for _, secret := range []string{"private free-form", "synthetic-secret", "private-filename", "never-log-auth"} {
		if strings.Contains(line, secret) {
			t.Fatalf("log retained %q: %q", secret, line)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("multiline warning: %q", line)
	}
}

func TestWarnLogUpstreamFailureSuppressesNonUpstreamFailures(t *testing.T) {
	warnings := captureExecutionWarnings(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		ctx context.Context
		err error
	}{
		{context.Background(), nil},
		{context.Background(), fmt.Errorf("wrapped: %w", context.Canceled)},
		{ctx, errors.New("upstream failure after caller cancellation")},
		{context.Background(), &Error{Code: requestScopedErrorCode, HTTPStatus: 400, Message: "invalid request"}},
	} {
		warnLogUpstreamFailure(test.ctx, nil, "provider", "model", time.Now(), test.err, cliproxyexecutor.DispatchPhaseMaybeSent)
	}
	if got := len(warnings()); got != 0 {
		t.Fatalf("warnings=%d, want 0", got)
	}
}

func TestHomeExecutionWarningsOncePerFailedAttempt(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream", "bootstrap", "started"} {
		t.Run(mode, func(t *testing.T) {
			warnings := captureExecutionWarnings(t)
			executor := &c08HomeRefreshExecutor{streamMode: mode}
			manager := newC08HomeRefreshManager(&c08HomeRefreshDispatcher{}, executor)
			request := cliproxyexecutor.Request{Model: "model-a"}
			var err error
			switch mode {
			case "execute":
				_, err = manager.Execute(context.Background(), []string{c08HomeRefreshProvider}, request, cliproxyexecutor.Options{})
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{c08HomeRefreshProvider}, request, cliproxyexecutor.Options{})
			default:
				var stream *cliproxyexecutor.StreamResult
				stream, err = manager.ExecuteStream(context.Background(), []string{c08HomeRefreshProvider}, request, cliproxyexecutor.Options{Stream: true})
				if err == nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			if statusCodeFromError(err) != 401 {
				t.Fatalf("error=%v", err)
			}
			entries := warnings()
			if len(entries) != 1 {
				t.Fatalf("warnings=%d, want 1", len(entries))
			}
			if mode == "started" && entries[0].Data["phase"] != cliproxyexecutor.DispatchPhaseResponseStarted {
				t.Fatalf("phase=%v", entries[0].Data["phase"])
			}
			if executor.refreshCalls.Load() != 0 {
				t.Fatal("Home credential was refreshed")
			}
		})
	}
}

type diagnosticRefreshExecutor struct{ *c08HomeRefreshExecutor }

func (*diagnosticRefreshExecutor) ShouldRefreshAfterUnauthorized(*Auth) bool { return true }

func TestManagedExecutionWarningsRetainRecoveredUnauthorized(t *testing.T) {
	warnings := captureExecutionWarnings(t)
	manager := NewManager(nil, nil, nil)
	executor := &diagnosticRefreshExecutor{&c08HomeRefreshExecutor{}}
	manager.RegisterExecutor(executor)
	credential, err := manager.Register(context.Background(), &Auth{ID: "diagnostic-managed", Provider: c08HomeRefreshProvider, Metadata: map[string]any{"access_token": "stale-access-token", "refresh_token": "synthetic-refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "model-a"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	_, err = manager.Execute(context.Background(), []string{credential.Provider}, cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if executor.refreshCalls.Load() != 1 || executor.executeCalls.Load() != 2 || len(warnings()) != 1 {
		t.Fatalf("refresh=%d execute=%d warnings=%d", executor.refreshCalls.Load(), executor.executeCalls.Load(), len(warnings()))
	}
}
