package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type headerSessionExecutor struct {
	schedulerTestExecutor
	headers chan string
}

func (e *headerSessionExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	util.ApplyCustomHeadersFromAttrs(req, map[string]string{"header:X-Session": "$CPA-SESSION-ID"})
	e.headers <- req.Header.Get("X-Session")
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *headerSessionExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *headerSessionExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if _, err := e.Execute(ctx, auth, req, opts); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestManagerPropagatesCanonicalSessionToCustomHeaders(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, explicit := range []string{"absent", "explicit", ""} {
			t.Run(mode+"/"+explicit, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				executor := &headerSessionExecutor{headers: make(chan string, 1)}
				manager.RegisterExecutor(executor)
				auth := &Auth{ID: "session-header-test", Provider: "test", Status: StatusActive}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, "test", []*registry.ModelInfo{{ID: "test-model"}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				manager.RefreshSchedulerEntry(auth.ID)
				ctx := context.Background()
				want := "caller-session"
				if explicit != "absent" {
					ctx = util.WithSessionID(util.WithSessionID(ctx, "parent"), explicit)
					want = explicit
				}
				req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{}`)}
				opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"caller-session"}}}
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(ctx, []string{"test"}, req, opts)
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{"test"}, req, opts)
				case "stream":
					var result *cliproxyexecutor.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"test"}, req, opts)
					if err == nil {
						for range result.Chunks {
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-executor.headers:
					if got != want {
						t.Fatalf("dynamic session = %q, want %q", got, want)
					}
				default:
					t.Fatal("executor was not called")
				}
			})
		}
	}
}
