package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestPickNextViaHomeDoesNotReusePinnedWebsocketAuthWithoutSelection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})

	auth := &Auth{
		ID:       "home-auth-1",
		Provider: "test",
		Status:   StatusActive,
		Attributes: map[string]string{
			"websockets":                  "true",
			homeUpstreamModelAttributeKey: "upstream-model",
		},
		Metadata: map[string]any{"email": "home@example.com"},
	}
	auth.EnsureIndex()
	manager.homeRuntimeAuths["session-1"] = map[string]*Auth{auth.ID: auth}
	cachedAuth, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth-1")
	if ok || cachedAuth != nil {
		t.Fatalf("ownerless websocket credential is readable: auth=%#v ok=%v", cachedAuth, ok)
	}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
		},
		Headers: http.Header{"Authorization": {"Bearer client-key"}},
	}

	got, executor, provider, errPick := manager.pickNextViaHome(ctx, "gpt-5.4", opts, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() unexpectedly reused an auth without a Home selection")
	}
	if got != nil || executor != nil || provider != "" {
		t.Fatalf(
			"pickNextViaHome() returned unbound execution target: auth=%#v executor=%#v provider=%q", got, executor,
			provider,
		)
	}
}

func TestPickNextViaHomeRejectsSessionScopedAuthCache(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})

	manager.homeRuntimeAuths["session-1"] = map[string]*Auth{
		"home-auth-1": {
			ID:       "home-auth-1",
			Provider: "test",
			Status:   StatusActive,
			Attributes: map[string]string{
				"websockets":                  "true",
				homeUpstreamModelAttributeKey: "upstream-model-a",
			},
		},
	}
	manager.homeRuntimeAuths["session-2"] = map[string]*Auth{
		"home-auth-1": {
			ID:       "home-auth-1",
			Provider: "test",
			Status:   StatusActive,
			Attributes: map[string]string{
				"websockets":                  "true",
				homeUpstreamModelAttributeKey: "upstream-model-b",
			},
		},
	}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	optsSession1 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
		},
	}
	optsSession2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-2",
			cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
		},
	}

	if _, _, _, errSession1 := manager.pickNextViaHome(ctx, "gpt-5.4", optsSession1, nil); errSession1 == nil {
		t.Fatal("pickNextViaHome(session-1) unexpectedly reused a session auth cache")
	}
	if _, _, _, errSession2 := manager.pickNextViaHome(ctx, "gpt-5.4", optsSession2, nil); errSession2 == nil {
		t.Fatal("pickNextViaHome(session-2) unexpectedly reused a session auth cache")
	}
}

func TestPickNextViaHomeDoesNotReuseTriedPinnedWebsocketAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})

	auth := &Auth{
		ID:       "home-auth-1",
		Provider: "test",
		Status:   StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	manager.homeRuntimeAuths["session-1"] = map[string]*Auth{auth.ID: auth}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
		},
	}
	tried := map[string]struct{}{"home-auth-1": {}}

	got, executor, provider, errPick := manager.pickNextViaHome(ctx, "gpt-5.4", opts, tried)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error is nil, want home unavailable error")
	}
	var authErr *Error
	if !errors.As(
		errPick, &authErr,
	) || authErr.Code != homeRequestRetryExceededErrorCode || authErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("pickNextViaHome() error = %v, want current pinned credential rejection", errPick)
	}
	if got != nil || executor != nil || provider != "" {
		t.Fatalf("pickNextViaHome() reused tried auth: auth=%#v executor=%#v provider=%q", got, executor, provider)
	}
}

func TestPickNextViaHomeDoesNotReusePinnedWebsocketAuthAfterFirstHomeAttempt(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})

	auth := &Auth{
		ID:       "home-auth-1",
		Provider: "test",
		Status:   StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	manager.homeRuntimeAuths["session-1"] = map[string]*Auth{auth.ID: auth}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	opts := withHomeAuthCount(
		cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
				cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
			},
		}, 2,
	)

	got, executor, provider, errPick := manager.pickNextViaHome(ctx, "gpt-5.4", opts, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error is nil, want home unavailable error")
	}
	var authErr *Error
	if !errors.As(
		errPick, &authErr,
	) || authErr.Code != "pinned_auth_not_found" || authErr.StatusCode() != http.StatusConflict {
		t.Fatalf("pickNextViaHome() error = %v, want current pinned credential rejection", errPick)
	}
	if got != nil || executor != nil || provider != "" {
		t.Fatalf(
			"pickNextViaHome() reused auth after first home attempt: auth=%#v executor=%#v provider=%q", got, executor,
			provider,
		)
	}
}

func TestPickNextViaHomeDoesNotReusePinnedNonWebsocketAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})

	manager.mu.Lock()
	manager.homeRuntimeAuths["session-1"] = map[string]*Auth{
		"home-auth-1": {
			ID:       "home-auth-1",
			Provider: "test",
			Status:   StatusActive,
		},
	}
	manager.mu.Unlock()

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "home-auth-1",
		},
		Headers: http.Header{"Authorization": {"Bearer client-key"}},
	}

	got, executor, provider, errPick := manager.pickNextViaHome(ctx, "gpt-5.4", opts, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error is nil, want home unavailable error")
	}
	var authErr *Error
	if !errors.As(
		errPick, &authErr,
	) || authErr.Code != "pinned_auth_not_found" || authErr.StatusCode() != http.StatusConflict {
		t.Fatalf("pickNextViaHome() error = %v, want current pinned credential rejection", errPick)
	}
	if got != nil || executor != nil || provider != "" {
		t.Fatalf(
			"pickNextViaHome() reused non-websocket auth: auth=%#v executor=%#v provider=%q", got, executor, provider,
		)
	}
}

// noinspection GoErrorStringFormat
func originalHomeAmbiguousTransportError(t *testing.T) error {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		reader := bufio.NewReader(conn)
		readSize := func(prefix byte) (int, error) {
			line, err := reader.ReadString('\n')
			if err != nil {
				return 0, err
			}
			if len(line) < 3 || line[0] != prefix {
				return 0, fmt.Errorf("unexpected Redis frame %q", line)
			}
			return strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(line[1:], "\n"), "\r"))
		}
		for {
			count, err := readSize('*')
			if err != nil || count <= 0 {
				done <- fmt.Errorf("Redis argument count=%d: %v", count, err)
				return
			}
			args := make([]string, count)
			for index := range count {
				size, err := readSize('$')
				if err != nil || size < 0 {
					done <- fmt.Errorf("Redis argument size=%d: %v", size, err)
					return
				}
				value := make([]byte, size+2)
				if _, err := io.ReadFull(reader, value); err != nil {
					done <- err
					return
				}
				args[index] = string(value[:size])
			}
			response := "+OK\r\n"
			switch strings.ToUpper(args[0]) {
			case "HELLO":
				response = "-ERR unknown command 'HELLO'\r\n"
			case "PING":
				response = "+PONG\r\n"
			case "RPOP":
				done <- nil
				return
			}
			if _, err := io.WriteString(conn, response); err != nil {
				done <- err
				return
			}
		}
	}()
	client := home.New(
		internalconfig.HomeConfig{
			Enabled: true, Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
		},
	)
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, dispatchErr := client.RPopAuth(ctx, "gpt-5.4", "session-1", nil, 1)
	if err := <-done; err != nil {
		t.Fatalf("synthetic Home transport: %v", err)
	}
	if !home.IsAmbiguousDispatchError(dispatchErr) {
		t.Fatalf("post-RPOP disconnect=%v, want ambiguous dispatch", dispatchErr)
	}
	return fmt.Errorf("response connection closed: %w", dispatchErr)
}

type homeAuthTransportErrorDispatcher struct {
	err     error
	aborts  atomic.Int32
	onAbort func()
}

func (d *homeAuthTransportErrorDispatcher) HeartbeatOK() bool {
	return true
}

func (d *homeAuthTransportErrorDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	return nil, d.err
}

func (d *homeAuthTransportErrorDispatcher) AbortAmbiguousDispatch() {
	d.aborts.Add(1)
	if d.onAbort != nil {
		d.onAbort()
	}
}

func TestPickNextViaHomeClassifiesTransportErrorsAsHomeUnavailable(t *testing.T) {
	dispatcher := &homeAuthTransportErrorDispatcher{
		err: errors.New("read tcp 127.0.0.1:46704->127.0.0.1:8327: i/o timeout"),
	}

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error is nil, want home unavailable error")
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) {
		t.Fatalf("pickNextViaHome() error = %T, want *Error", errPick)
	}
	if authErr.Code != "home_unavailable" {
		t.Fatalf("pickNextViaHome() error code = %q, want home_unavailable (%v)", authErr.Code, errPick)
	}
	if authErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("pickNextViaHome() status = %d, want %d", authErr.StatusCode(), http.StatusServiceUnavailable)
	}
	if !authErr.Retryable {
		t.Fatal("pickNextViaHome() retryable = false, want true")
	}
}

func TestPickNextViaHomeAbortsBeforeEndingPendingDispatch(t *testing.T) {
	registry := executionregistry.New()
	abortSawPending := make(chan bool, 1)
	dispatcher := &homeAuthTransportErrorDispatcher{
		err: originalHomeAmbiguousTransportError(t),
		onAbort: func() {
			cancelledCtx, cancel := context.WithCancel(context.Background())
			cancel()
			abortSawPending <- errors.Is(registry.Drain(cancelledCtx), context.Canceled)
		},
	}

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, registry, 1)

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error = nil, want home unavailable")
	}
	if sawPending := <-abortSawPending; !sawPending {
		t.Fatal("AbortAmbiguousDispatch() observed an already-ended pending dispatch")
	}
}

func TestPickNextViaHomeDoesNotAbortDeterministicDispatchFailure(t *testing.T) {
	dispatcher := &homeAuthTransportErrorDispatcher{err: home.ErrNotConnected}

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error = nil, want home unavailable")
	}
	if got := dispatcher.aborts.Load(); got != 0 {
		t.Fatalf("AbortAmbiguousDispatch() calls = %d, want 0 for deterministic failure", got)
	}
}

func TestPickNextViaHomeAbortsAmbiguousTransport(t *testing.T) {
	dispatcher := &homeAuthTransportErrorDispatcher{err: originalHomeAmbiguousTransportError(t)}

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, registry, 1)

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatal("pickNextViaHome() error = nil, want home unavailable")
	}
	if got := dispatcher.aborts.Load(); got != 1 {
		t.Fatalf("AbortAmbiguousDispatch() calls = %d, want 1", got)
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if errDrain := registry.Drain(drainCtx); errDrain != nil {
		t.Fatalf("Drain() error = %v, ambiguous pending dispatch was not ended", errDrain)
	}
}

func TestHomeRuntimeAuthsClearWhenHomeDisabled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	t.Cleanup(func() { manager.CloseExecutionSession(CloseAllExecutionSessionsID) })
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.rememberHomeRuntimeAuth(
		"session-1", &Auth{
			ID:       "home-auth-1",
			Provider: "test",
			Attributes: map[string]string{
				"websockets": "true",
			},
		},
	)

	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth-1"); !ok {
		t.Fatal("expected remembered home auth before disabling home")
	}

	manager.SetConfig(&internalconfig.Config{})
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth-1"); ok {
		t.Fatal("remembered home auth was not cleared when home was disabled")
	}
}

func TestCloseExecutionSessionClearsHomeRuntimeAuthForSession(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "home-auth-1",
		Provider: "test",
		Attributes: map[string]string{
			"websockets": "true",
		},
	}

	manager.rememberHomeRuntimeAuth("session-1", auth)
	manager.rememberHomeRuntimeAuth("session-2", auth)

	manager.CloseExecutionSession("session-1")
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth-1"); ok {
		t.Fatal("home auth for closed session was not cleared")
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-2", "home-auth-1"); !ok {
		t.Fatal("home auth for another session was cleared")
	}

	manager.CloseExecutionSession("session-2")
	if _, ok := manager.GetExecutionSessionAuthByID("session-2", "home-auth-1"); ok {
		t.Fatal("home auth was not cleared when its last session closed")
	}
}
