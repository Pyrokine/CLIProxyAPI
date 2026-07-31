package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type replaceAwareExecutor struct {
	id string

	mu               sync.Mutex
	closedSessionIDs []string
}

func (e *replaceAwareExecutor) Identifier() string {
	return e.id
}

func (e *replaceAwareExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *replaceAwareExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *replaceAwareExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *replaceAwareExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *replaceAwareExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *replaceAwareExecutor) CloseExecutionSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closedSessionIDs = append(e.closedSessionIDs, sessionID)
}

func (e *replaceAwareExecutor) ClosedSessionIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.closedSessionIDs))
	copy(out, e.closedSessionIDs)
	return out
}

func TestManagerRegisterExecutorClosesReplacedExecutionSessions(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	replaced := &replaceAwareExecutor{id: "codex"}
	current := &replaceAwareExecutor{id: "codex"}

	manager.RegisterExecutor(replaced)
	manager.RegisterExecutor(current)

	closed := replaced.ClosedSessionIDs()
	if len(closed) != 1 {
		t.Fatalf("expected replaced executor close calls = 1, got %d", len(closed))
	}
	if closed[0] != CloseAllExecutionSessionsID {
		t.Fatalf("expected close marker %q, got %q", CloseAllExecutionSessionsID, closed[0])
	}
	if len(current.ClosedSessionIDs()) != 0 {
		t.Fatalf("expected current executor to stay open")
	}
}

func TestManagerExecutorCompareAndSwap(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	plugin := &replaceAwareExecutor{id: "codex"}
	native := &replaceAwareExecutor{id: "codex"}
	replacement := &replaceAwareExecutor{id: "codex"}

	if !manager.RegisterExecutorIfAbsent(plugin) {
		t.Fatal("RegisterExecutorIfAbsent() rejected an empty provider")
	}
	if manager.RegisterExecutorIfAbsent(native) {
		t.Fatal("RegisterExecutorIfAbsent() replaced an existing executor")
	}
	if manager.ReplaceExecutorIfSame(native, replacement) {
		t.Fatal("ReplaceExecutorIfSame() accepted a stale expected executor")
	}
	if manager.UnregisterExecutorIfSame("codex", native) {
		t.Fatal("UnregisterExecutorIfSame() accepted a stale expected executor")
	}
	resolved, okResolved := manager.Executor("codex")
	if !okResolved || resolved != plugin {
		t.Fatalf("stale CAS changed executor: %#v", resolved)
	}

	if !manager.ReplaceExecutorIfSame(plugin, replacement) {
		t.Fatal("ReplaceExecutorIfSame() rejected the current executor")
	}
	if closed := plugin.ClosedSessionIDs(); len(closed) != 1 || closed[0] != CloseAllExecutionSessionsID {
		t.Fatalf("replaced executor close calls = %v", closed)
	}
	if !manager.UnregisterExecutorIfSame("codex", replacement) {
		t.Fatal("UnregisterExecutorIfSame() rejected the current executor")
	}
	if _, okResolved = manager.Executor("codex"); okResolved {
		t.Fatal("executor remains after successful conditional unregister")
	}
}

func TestManagerExecutorReturnsRegisteredExecutor(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	current := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(current)

	resolved, okResolved := manager.Executor("CODEX")
	if !okResolved {
		t.Fatal("expected registered executor to be found")
	}
	resolvedExecutor, okResolvedExecutor := resolved.(*replaceAwareExecutor)
	if !okResolvedExecutor {
		t.Fatalf("expected resolved executor type %T, got %T", current, resolved)
	}
	if resolvedExecutor != current {
		t.Fatal("expected resolved executor to match registered executor")
	}

	_, okMissing := manager.Executor("unknown")
	if okMissing {
		t.Fatal("expected unknown provider lookup to fail")
	}
}
